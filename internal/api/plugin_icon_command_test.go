package api_test

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/discordtest"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// `pluginsign` and the optional icon.png (issue 08): pack includes an icon.png that
// sits beside the manifest, sign covers it, and the package that results installs
// against the pinned key; an icon swapped after signing does not.
func TestThePluginsignCommandPacksAndSignsAnIconBesideTheManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the pluginsign command")
	}
	t.Parallel()
	const publisher = "Example Publisher"
	dir := t.TempDir()
	root := discordtest.RepoRoot(t)

	bin := filepath.Join(dir, "pluginsign")
	build := exec.Command("go", "build", "-o", bin, "./cmd/pluginsign")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building pluginsign: %v\n%s", err, out)
	}
	run := func(wantOK bool, args ...string) string {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if (err == nil) != wantOK {
			t.Fatalf("pluginsign %v: err = %v, want ok = %v\n%s", args, err, wantOK, out)
		}
		return string(out)
	}

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	icon := onlineIconPNG(t)
	manifestPath := filepath.Join(src, plugins.ManifestFile)
	modulePath := filepath.Join(src, plugins.DefaultModuleFile)
	iconPath := filepath.Join(src, plugins.IconFile)
	keyPath := filepath.Join(dir, "publisher.key")
	sigPath := filepath.Join(dir, "plugin.sig.json")
	for path, body := range map[string][]byte{
		manifestPath: plugintest.ManifestJSON(t, plugintest.SinkManifest("icon-sink")),
		modulePath:   plugintest.Guest(t),
		iconPath:     icon,
	} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var publicKey string
	for _, line := range strings.Split(run(true, "keygen", "-out", keyPath), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "public key:"); ok {
			publicKey = strings.TrimSpace(rest)
		}
	}
	run(true, "sign", "-key", keyPath, "-publisher", publisher,
		"-manifest", manifestPath, "-module", modulePath, "-out", sigPath)
	run(true, "verify", "-sig", sigPath, "-pub", publicKey, "-manifest", manifestPath, "-module", modulePath)

	zipPath := filepath.Join(dir, "icon-sink.zip")
	run(true, "pack", "-manifest", manifestPath, "-module", modulePath, "-signature", sigPath, "-out", zipPath)
	if got := zipMembers(t, zipPath); !strings.Contains(got, plugins.IconFile) {
		t.Fatalf("pack left icon.png out of the package: members = %s", got)
	}
	run(true, "verify", "-package", zipPath, "-pub", publicKey)

	// The same sources signed WITHOUT the icon do not verify once the icon is
	// packed, and the server says so: the icon is under the signature.
	stale := filepath.Join(dir, "stale.sig.json")
	other := filepath.Join(dir, "other-icon.png")
	var otherBuf bytes.Buffer
	if err := png.Encode(&otherBuf, image.NewRGBA(image.Rect(0, 0, 24, 24))); err != nil {
		t.Fatal(err)
	}
	otherIcon := otherBuf.Bytes()
	if err := os.WriteFile(other, otherIcon, 0o644); err != nil {
		t.Fatal(err)
	}
	run(true, "sign", "-key", keyPath, "-publisher", publisher, "-icon", other,
		"-manifest", manifestPath, "-module", modulePath, "-out", stale)
	swapped := filepath.Join(dir, "swapped.zip")
	run(true, "pack", "-manifest", manifestPath, "-module", modulePath, "-signature", stale, "-out", swapped)
	run(false, "verify", "-package", swapped, "-pub", publicKey)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	pub, err := signing.ParsePublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	pinPublisher(t, srv, token, publisher, pub)

	archive, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := uploadPackage(t, srv, token, archive); status != http.StatusCreated {
		t.Fatalf("the command's package with an icon was refused: %d %s", status, body)
	}
	if got := signerOf(t, srv, token, "icon-sink"); got.Publisher != publisher {
		t.Fatalf("publisher = %q, want %q", got.Publisher, publisher)
	}
	swappedArchive, err := os.ReadFile(swapped)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := testharness.New(t)
	token2 := adminToken(t, srv2)
	pinPublisher(t, srv2, token2, publisher, pub)
	if status, body := uploadPackage(t, srv2, token2, swappedArchive); status != http.StatusUnprocessableEntity ||
		!bytes.Contains(body, []byte("PLUGIN_SIGNATURE")) {
		t.Fatalf("a package whose icon the signature does not cover: %d %s, want 422 PLUGIN_SIGNATURE", status, body)
	}

	// No icon beside the manifest: nothing is added, and the old shape is unchanged.
	if err := os.Remove(iconPath); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.zip")
	run(true, "pack", "-manifest", manifestPath, "-module", modulePath, "-out", plain)
	if got := zipMembers(t, plain); strings.Contains(got, plugins.IconFile) {
		t.Fatalf("pack invented an icon: members = %s", got)
	}
}

func zipMembers(t *testing.T, path string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return strings.Join(names, ",")
}
