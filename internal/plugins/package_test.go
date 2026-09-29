package plugins_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Installing a Plugin package: a zip of the manifest, the module and an optional
// signature. The archive is untrusted input, so most of this file is the ways one
// can be wrong and the sentence each earns.

// --- installs -----------------------------------------------------------------------

func TestInstallingAPackageStoresTheThreeFilesAndNotTheArchive(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	manifest := plugintest.ManifestJSON(t, plugintest.SinkManifest("pkg-sink"))
	archive := plugintest.PackageZip(t, manifest, plugintest.Guest(t), nil)

	got, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload)
	if err != nil {
		t.Fatalf("installing a well-formed package: %v", err)
	}
	if got.ID != "pkg-sink" || got.Source != plugins.SourceUpload {
		t.Fatalf("installed %+v, want pkg-sink from an upload", got)
	}
	dir := filepath.Join(f.dir, "pkg-sink")
	stored, err := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if err != nil || !bytes.Equal(stored, manifest) {
		t.Fatalf("the stored manifest differs from the packaged one (err = %v)", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !contains(names, plugins.ManifestFile) || !contains(names, plugins.DefaultModuleFile) {
		t.Fatalf("the plugin directory holds %v, want just the manifest and the module", names)
	}
}

func TestInstallingASignedPackageKeepsTheSignatureAndRecordsThePinnedPublisher(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	manifest := plugintest.ManifestJSON(t, plugintest.SinkManifest("signed-pkg"))
	module := plugintest.Guest(t)
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	sig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatalf("encoding the signature: %v", err)
	}
	archive := plugintest.PackageZip(t, manifest, module, doc)

	// Nothing pinned: the signature is stored as provenance and nothing is recorded.
	got, err := f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload)
	if err != nil {
		t.Fatalf("installing a signed package with nothing pinned: %v", err)
	}
	if got.Publisher != "" {
		t.Fatalf("publisher = %q with nothing pinned, want none recorded", got.Publisher)
	}
	if kept, err := os.ReadFile(filepath.Join(f.dir, "signed-pkg", pluginapi.SignatureFile)); err != nil || !bytes.Equal(kept, doc) {
		t.Fatalf("the signature inside the package was not stored beside the manifest (err = %v)", err)
	}
	if err := f.manager.Uninstall(context.Background(), "signed-pkg"); err != nil {
		t.Fatalf("uninstalling: %v", err)
	}

	// Pinned: the signature inside the package is what verifies.
	if err := f.manager.PinPublisher("Example Publisher", signing.EncodeKey(pub)); err != nil {
		t.Fatalf("pinning: %v", err)
	}
	got, err = f.manager.InstallPackage(context.Background(), archive, plugins.SourceUpload)
	if err != nil {
		t.Fatalf("installing a signed package against a pinned key: %v", err)
	}
	if got.Publisher != "Example Publisher" {
		t.Fatalf("publisher = %q, want the pinned spelling", got.Publisher)
	}

	// And an unsigned package is now refused by name, exactly as loose files were.
	unsigned := plugintest.PackageZip(t, plugintest.ManifestJSON(t, plugintest.SinkManifest("unsigned-pkg")), module, nil)
	_, err = f.manager.InstallPackage(context.Background(), unsigned, plugins.SourceUpload)
	if refusalReason(err) != plugins.ReasonSignature {
		t.Fatalf("an unsigned package against a pinned key: err = %v, want a signature refusal", err)
	}
}

// newURLManager is a Manager allowed to fetch from the loopback address the
// fixture is served on, which production refuses.
func newURLManager(t *testing.T) *plugins.Manager {
	t.Helper()
	dir := filepath.Join(t.TempDir(), plugins.DirName)
	set, err := plugins.Load(context.Background(), dir, plugins.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("loading an empty plugins directory: %v", err)
	}
	m := plugins.NewManager(plugins.ManagerConfig{
		Dir: dir, Registry: pluginapi.NewRegistry(), Set: set, Store: newMemStore(),
		Loader: plugins.Options{Logf: func(string, ...any) {}}, Logf: func(string, ...any) {},
		AllowPrivateSources: true,
	})
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func serveBytes(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallingFromAURLFetchesOnePackage(t *testing.T) {
	t.Parallel()
	m := newURLManager(t)
	requests := 0
	archive := plugintest.PackageZip(t, plugintest.ManifestJSON(t, plugintest.SinkManifest("url-pkg")), plugintest.Guest(t), nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	got, err := m.InstallFromURL(context.Background(), srv.URL+"/url-pkg.zip")
	if err != nil {
		t.Fatalf("installing from a package URL: %v", err)
	}
	if got.ID != "url-pkg" || got.Source != srv.URL+"/url-pkg.zip" {
		t.Fatalf("installed %+v, want url-pkg with the URL as its source", got)
	}
	if requests != 1 {
		t.Fatalf("the install made %d requests, want exactly 1", requests)
	}
}

func TestAFetchedPackageOverTheCapIsRefusedNotTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a package-sized buffer")
	}
	t.Parallel()
	m := newURLManager(t)
	srv := serveBytes(t, make([]byte, plugins.MaxPackageBytes+1))
	_, err := m.InstallFromURL(context.Background(), srv.URL+"/big.zip")
	if refusalReason(err) != plugins.ReasonSource || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want a source refusal saying the package is too large", err)
	}
}

func TestAFetchedFileThatIsNotAZipIsAPackageRefusal(t *testing.T) {
	t.Parallel()
	m := newURLManager(t)
	srv := serveBytes(t, []byte("<html>a login page</html>"))
	_, err := m.InstallFromURL(context.Background(), srv.URL+"/x.zip")
	if refusalReason(err) != plugins.ReasonPackage {
		t.Fatalf("err = %v, want a package refusal", err)
	}
}

// --- refusals: one test per way a package can be wrong ---------------------------------

// packageParts is the good pair of files every malformed fixture starts from.
func packageParts(t *testing.T) (manifest, module []byte) {
	t.Helper()
	return plugintest.ManifestJSON(t, plugintest.SinkManifest("pkg-sink")), plugintest.Guest(t)
}

// patchCentralEntry rewrites one member's central directory record in place, so a
// test can hand over an archive whose header says something its stream does not.
func patchCentralEntry(t *testing.T, archive []byte, name string, patch func(record []byte)) []byte {
	t.Helper()
	out := append([]byte(nil), archive...)
	patched := false
	for i := 0; i+46 < len(out); i++ {
		if binary.LittleEndian.Uint32(out[i:]) != 0x02014b50 {
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(out[i+28:]))
		if string(out[i+46:i+46+nameLen]) != name {
			continue
		}
		patch(out[i:])
		patched = true
	}
	if !patched {
		t.Fatalf("no central directory entry for %q to falsify", name)
	}
	return out
}

// lieAboutUncompressedSize rewrites the size the archive's central directory
// claims for one member, so a test can hand over a member whose header says it is
// small and whose stream is not.
func lieAboutUncompressedSize(t *testing.T, archive []byte, name string, claimed uint32) []byte {
	t.Helper()
	return patchCentralEntry(t, archive, name, func(record []byte) {
		binary.LittleEndian.PutUint32(record[24:], claimed)
	})
}

func TestAMalformedPackageIsRefusedWithASentenceNamingWhatWasFound(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)
	zeros := func(n int) []byte { return make([]byte, n) }

	cases := []struct {
		name    string
		archive []byte
		wantIn  string
	}{
		{
			name: "a wrapping folder",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "pkg-sink/manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "pkg-sink/plugin.wasm", Body: module}),
			wantIn: `"pkg-sink/manifest.json" inside a folder`,
		},
		{
			name: "a folder entry",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "pkg-sink/", Mode: fs.ModeDir | 0o755},
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}),
			wantIn: "holds a folder",
		},
		{
			name: "a __MACOSX folder",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module},
				plugintest.ZipMember{Name: "__MACOSX/._manifest.json", Body: []byte("x")}),
			wantIn: "macOS resource-fork folder",
		},
		{
			name: "an extra member",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module},
				plugintest.ZipMember{Name: "README.md", Body: []byte("hi")}),
			wantIn: `"README.md", which is not part of a plugin`,
		},
		{
			name: "no module",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest}),
			wantIn: "has no module",
		},
		{
			name: "no manifest",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}),
			wantIn: "has no manifest.json at its root",
		},
		{
			name: "a duplicate name",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}),
			wantIn: `"manifest.json" twice`,
		},
		{
			name: "a path that escapes the package",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module},
				plugintest.ZipMember{Name: "../../obelo.db", Body: []byte("x")}),
			wantIn: "points outside the package",
		},
		{
			name: "a symbolic link",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: []byte("/etc/passwd"), Mode: fs.ModeSymlink | 0o777}),
			wantIn: "symbolic link",
		},
		{
			name: "a manifest larger than its cap once unpacked",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: zeros(plugins.MaxManifestBytes + 1)},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}),
			wantIn: "manifest (manifest.json) is larger than",
		},
		{
			name: "a manifest whose header claims it is small",
			archive: lieAboutUncompressedSize(t, plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: zeros(plugins.MaxManifestBytes + 1)},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}), "manifest.json", 100),
			wantIn: "manifest (manifest.json) could not be read",
		},
		{
			name: "a signature larger than its cap once unpacked",
			archive: plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module},
				plugintest.ZipMember{Name: "plugin.sig.json", Body: zeros(plugins.MaxSignatureBytes + 1)}),
			wantIn: "signature (plugin.sig.json) is larger than",
		},
		{
			name: "a signature whose header claims it is small",
			archive: lieAboutUncompressedSize(t, plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module},
				plugintest.ZipMember{Name: "plugin.sig.json", Body: zeros(plugins.MaxSignatureBytes + 1)}), "plugin.sig.json", 10),
			wantIn: "signature (plugin.sig.json) could not be read",
		},
		{
			name:    "not a zip",
			archive: []byte("MZ this is a windows executable"),
			wantIn:  "not a readable .zip",
		},
		{
			name: "an unsupported compression method",
			archive: patchCentralEntry(t, plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}), "manifest.json", func(record []byte) {
				binary.LittleEndian.PutUint16(record[10:], 12)
			}),
			wantIn: "compression method 12",
		},
		{
			name: "an encrypted member",
			archive: patchCentralEntry(t, plugintest.Zip(t,
				plugintest.ZipMember{Name: "manifest.json", Body: manifest},
				plugintest.ZipMember{Name: "plugin.wasm", Body: module}), "plugin.wasm", func(record []byte) {
				flags := binary.LittleEndian.Uint16(record[8:])
				binary.LittleEndian.PutUint16(record[8:], flags|0x1)
			}),
			wantIn: `"plugin.wasm" is encrypted`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newManagerFixture(t)
			_, err := f.manager.InstallPackage(context.Background(), tc.archive, plugins.SourceUpload)
			if refusalReason(err) != plugins.ReasonPackage {
				t.Fatalf("err = %v, want a package refusal", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("message = %q, want it to contain %q", err.Error(), tc.wantIn)
			}
			if _, statErr := os.Stat(filepath.Join(f.dir, "pkg-sink")); !os.IsNotExist(statErr) {
				t.Fatalf("a refused package left a plugin directory behind (stat err = %v)", statErr)
			}
		})
	}
}

func TestAnArchiveOverThePackageCapIsRefusedBeforeItIsRead(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a package-sized buffer")
	}
	t.Parallel()
	_, err := plugins.UnpackPackage(make([]byte, plugins.MaxPackageBytes+1))
	if refusalReason(err) != plugins.ReasonPackage || !strings.Contains(err.Error(), "will not accept one larger than") {
		t.Fatalf("err = %v, want a package refusal naming the cap", err)
	}
}

func TestAModuleLargerThanItsCapOnceUnpackedIsRefusedEvenWhenTheArchiveIsSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("compresses a module-sized buffer")
	}
	t.Parallel()
	manifest, _ := packageParts(t)
	archive := plugintest.Zip(t,
		plugintest.ZipMember{Name: "manifest.json", Body: manifest},
		plugintest.ZipMember{Name: "plugin.wasm", Body: make([]byte, plugins.MaxModuleBytes+1)})
	if len(archive) > 1<<20 {
		t.Fatalf("the fixture archive is %d bytes; the point is that it is small", len(archive))
	}
	_, err := plugins.UnpackPackage(archive)
	if refusalReason(err) != plugins.ReasonPackage || !strings.Contains(err.Error(), "module (plugin.wasm) is larger than") {
		t.Fatalf("err = %v, want a package refusal naming the module", err)
	}
}

func TestAPackageNamesItsModuleWithTheManifestsModuleField(t *testing.T) {
	t.Parallel()
	m := plugintest.SinkManifest("named-module")
	m.Module = "guest.wasm"
	manifest := plugintest.ManifestJSON(t, m)
	pkg, err := plugins.UnpackPackage(plugintest.Zip(t,
		plugintest.ZipMember{Name: "manifest.json", Body: manifest},
		plugintest.ZipMember{Name: "guest.wasm", Body: []byte("wasm")}))
	if err != nil || string(pkg.Module) != "wasm" {
		t.Fatalf("unpacking a package with a named module: %v, %q", err, pkg.Module)
	}
	// The default name is then an unknown member.
	_, err = plugins.UnpackPackage(plugintest.Zip(t,
		plugintest.ZipMember{Name: "manifest.json", Body: manifest},
		plugintest.ZipMember{Name: "plugin.wasm", Body: []byte("wasm")}))
	if refusalReason(err) != plugins.ReasonPackage {
		t.Fatalf("err = %v, want plugin.wasm refused when the manifest names guest.wasm", err)
	}
}

func TestPackPackageRefusesWhatInstallingWould(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)
	archive, err := plugins.PackPackage(manifest, module, nil)
	if err != nil {
		t.Fatalf("packing good files: %v", err)
	}
	again, _ := plugins.PackPackage(manifest, module, nil)
	if !bytes.Equal(archive, again) {
		t.Fatal("packing the same files twice gave different bytes")
	}
	if _, err := plugins.UnpackPackage(archive); err != nil {
		t.Fatalf("a packed package does not unpack: %v", err)
	}
	if _, err := plugins.PackPackage([]byte("{nope"), module, nil); refusalReason(err) != plugins.ReasonManifest {
		t.Fatalf("packing a manifest that is not JSON: err = %v, want a manifest refusal", err)
	}
	if _, err := plugins.PackPackage(manifest, nil, nil); err == nil {
		t.Fatal("packing an empty module succeeded")
	}
}
