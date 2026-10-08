package plugins_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// The optional icon.png member of a Plugin package (ADR-0068 decision 12): PNG,
// square, at most 64 KiB, signed with the rest when present, and refused with a
// sentence naming the problem when it is anything else.

// iconPNG is a w-by-h PNG, the real thing an install decodes the header of.
func iconPNG(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encoding a %dx%d PNG: %v", w, h, err)
	}
	return buf.Bytes()
}

func iconPackage(t *testing.T, manifest, module, icon, signature []byte) []byte {
	t.Helper()
	members := []plugintest.ZipMember{
		{Name: plugins.ManifestFile, Body: manifest},
		{Name: plugins.DefaultModuleFile, Body: module},
		{Name: plugins.IconFile, Body: icon},
	}
	if len(signature) > 0 {
		members = append(members, plugintest.ZipMember{Name: "plugin.sig.json", Body: signature})
	}
	return plugintest.Zip(t, members...)
}

func TestAPackageMayCarryAnIconAndInstallStoresIt(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	manifest, module := packageParts(t)
	icon := iconPNG(t, 48, 48)

	pkg, err := plugins.UnpackPackage(iconPackage(t, manifest, module, icon, nil))
	if err != nil || !bytes.Equal(pkg.Icon, icon) {
		t.Fatalf("unpacking a package with a valid icon: err = %v, icon equal = %v", err, bytes.Equal(pkg.Icon, icon))
	}
	if _, err := f.manager.InstallPackage(context.Background(), iconPackage(t, manifest, module, icon, nil), plugins.SourceUpload); err != nil {
		t.Fatalf("installing a package with a valid icon: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(f.dir, "pkg-sink", plugins.IconFile))
	if err != nil || !bytes.Equal(stored, icon) {
		t.Fatalf("the icon was not stored beside the manifest byte for byte (err = %v)", err)
	}
}

func TestAnIconThatIsNotAValidSquarePNGUnderTheCapIsRefusedWithASentenceNamingTheProblem(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)

	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 32, 32)), nil); err != nil {
		t.Fatal(err)
	}
	// A PNG signature followed by nothing decodable: the header check cannot be
	// satisfied by the eight magic bytes alone.
	truncated := []byte("\x89PNG\r\n\x1a\n")
	// Small once compressed, over the cap once unpacked: the cap reads the
	// decompressed bytes, never the size the header claims.
	oversize := append(iconPNG(t, 8, 8), make([]byte, plugins.MaxIconBytes)...)

	for name, tc := range map[string]struct {
		icon []byte
		want string
	}{
		"a JPEG renamed icon.png": {jpg.Bytes(), "not a PNG"},
		"a truncated PNG":         {truncated, "not a PNG"},
		"a non-square PNG":        {iconPNG(t, 64, 32), "square"},
		"a PNG over 64 KiB":       {oversize, "larger than the 65536 bytes"},
		"an empty icon":           {nil, "empty"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := plugins.UnpackPackage(iconPackage(t, manifest, module, tc.icon, nil))
			if refusalReason(err) != plugins.ReasonPackage || !strings.Contains(err.Error(), "icon.png") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a package refusal naming icon.png and %q", err, tc.want)
			}
		})
	}
}

func TestADuplicateIconAndOtherUnknownMembersAreStillRefused(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)
	icon := iconPNG(t, 16, 16)
	base := []plugintest.ZipMember{
		{Name: plugins.ManifestFile, Body: manifest},
		{Name: plugins.DefaultModuleFile, Body: module},
	}
	for name, extra := range map[string][]plugintest.ZipMember{
		"icon.png twice":      {{Name: "icon.png", Body: icon}, {Name: "icon.png", Body: icon}},
		"a wrapped icon":      {{Name: "assets/icon.png", Body: icon}},
		"a different image":   {{Name: "icon.jpg", Body: icon}},
		"a differently-cased": {{Name: "Icon.PNG", Body: icon}},
		"an icon symlink":     {{Name: "icon.png", Body: []byte("target"), Mode: 0o120777}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			members := append(append([]plugintest.ZipMember(nil), base...), extra...)
			_, err := plugins.UnpackPackage(plugintest.Zip(t, members...))
			if refusalReason(err) != plugins.ReasonPackage {
				t.Fatalf("err = %v, want a package refusal", err)
			}
		})
	}
}

func TestPackPackageWithIconRoundTripsAndRefusesABadIcon(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)
	icon := iconPNG(t, 32, 32)
	archive, err := plugins.PackPackageWithIcon(manifest, module, nil, icon)
	if err != nil {
		t.Fatalf("packing a good icon: %v", err)
	}
	pkg, err := plugins.UnpackPackage(archive)
	if err != nil || !bytes.Equal(pkg.Icon, icon) {
		t.Fatalf("a packed icon does not come back (err = %v)", err)
	}
	if _, err := plugins.PackPackageWithIcon(manifest, module, nil, iconPNG(t, 32, 16)); refusalReason(err) != plugins.ReasonPackage {
		t.Fatalf("packing a non-square icon: err = %v, want a package refusal", err)
	}
	// No icon is the old package, byte for byte.
	plain, _ := plugins.PackPackage(manifest, module, nil)
	same, _ := plugins.PackPackageWithIcon(manifest, module, nil, nil)
	if !bytes.Equal(plain, same) {
		t.Fatal("packing with no icon changed the package")
	}
}

// --- signature coverage ---------------------------------------------------------------

func signedIconPackage(t *testing.T, priv ed25519.PrivateKey, manifest, module, icon []byte) []byte {
	t.Helper()
	sig, err := signing.SignWithIcon(priv, "Example Publisher", manifest, module, icon)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	doc, err := signing.Encode(sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(icon) == 0 {
		return plugintest.PackageZip(t, manifest, module, doc)
	}
	return iconPackage(t, manifest, module, icon, doc)
}

func TestTheSignatureCoversTheIconWhenAPublisherIsPinned(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	manifest, module := packageParts(t)
	icon := iconPNG(t, 32, 32)
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.PinPublisher("Example Publisher", signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	signed := signedIconPackage(t, priv, manifest, module, icon)
	if got, err := f.manager.InstallPackage(ctx, signed, plugins.SourceUpload); err != nil || got.Publisher != "Example Publisher" {
		t.Fatalf("installing the package as signed: publisher = %q, err = %v", got.Publisher, err)
	}
	if err := f.manager.Uninstall(ctx, "pkg-sink"); err != nil {
		t.Fatal(err)
	}

	docOf := func(archive []byte) []byte {
		pkg, err := plugins.UnpackPackage(archive)
		if err != nil {
			t.Fatal(err)
		}
		return pkg.Signature
	}
	withIconDoc := docOf(signed)
	noIconDoc := docOf(signedIconPackage(t, priv, manifest, module, nil))
	for name, archive := range map[string][]byte{
		"icon swapped after signing": iconPackage(t, manifest, module, iconPNG(t, 48, 48), withIconDoc),
		"icon added after signing":   iconPackage(t, manifest, module, icon, noIconDoc),
	} {
		_, err := f.manager.InstallPackage(ctx, archive, plugins.SourceUpload)
		if refusalReason(err) != plugins.ReasonSignature || !strings.Contains(err.Error(), "icon.png") {
			t.Fatalf("%s: err = %v, want a signature refusal naming icon.png", name, err)
		}
	}

	// An icon removed after signing is still refused (the sentence says nothing of
	// icons: the package has none to name).
	_, err = f.manager.InstallPackage(ctx, plugintest.PackageZip(t, manifest, module, withIconDoc), plugins.SourceUpload)
	if refusalReason(err) != plugins.ReasonSignature {
		t.Fatalf("icon removed after signing: err = %v, want a signature refusal", err)
	}

	// Back-compat: a package with no icon, signed the old way, still verifies.
	oldSig, err := signing.Sign(priv, "Example Publisher", manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	oldDoc, _ := signing.Encode(oldSig)
	if got, err := f.manager.InstallPackage(ctx, plugintest.PackageZip(t, manifest, module, oldDoc), plugins.SourceUpload); err != nil || got.Publisher != "Example Publisher" {
		t.Fatalf("a signed package with no icon: publisher = %q, err = %v", got.Publisher, err)
	}
}
