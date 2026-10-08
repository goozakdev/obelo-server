package plugins_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// headerOnlyPNG is a PNG that declares w by h and carries no pixels: a few dozen
// bytes, enough for the header check an install makes and for no decoder to build.
func headerOnlyPNG(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		body := append([]byte(kind), data...)
		b.Write(body)
		_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(body))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return b.Bytes()
}

func TestAnIconIsRefusedPastTheMaxSideAndAcceptedAtIt(t *testing.T) {
	t.Parallel()
	manifest, module := packageParts(t)

	_, err := plugins.UnpackPackage(iconPackage(t, manifest, module, headerOnlyPNG(1025, 1025), nil))
	if refusalReason(err) != plugins.ReasonPackage || !strings.Contains(err.Error(), "icon.png") ||
		!strings.Contains(err.Error(), "1024") {
		t.Fatalf("a 1025x1025 icon: err = %v, want a package refusal naming icon.png and 1024", err)
	}
	if _, err := plugins.UnpackPackage(iconPackage(t, manifest, module, headerOnlyPNG(1024, 1024), nil)); err != nil {
		t.Fatalf("a 1024x1024 icon: %v", err)
	}
}

func TestTheBadSignatureSentenceMentionsTheIconOnlyWhenThePackageHasOne(t *testing.T) {
	t.Parallel()
	f := newManagerFixture(t)
	manifest, module := packageParts(t)
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.PinPublisher("Example Publisher", signing.EncodeKey(pub)); err != nil {
		t.Fatal(err)
	}
	icon := iconPNG(t, 32, 32)
	doc := func(covered []byte) []byte {
		sig, err := signing.SignWithIcon(priv, "Example Publisher", manifest, module, covered)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := signing.Encode(sig)
		return d
	}
	ctx := context.Background()

	// An icon the signature does not cover: the sentence names the icon.
	_, err = f.manager.InstallPackage(ctx, iconPackage(t, manifest, module, icon, doc(nil)), plugins.SourceUpload)
	if refusalReason(err) != plugins.ReasonSignature || !strings.Contains(err.Error(), "icon.png") {
		t.Fatalf("package with an icon: err = %v, want a signature refusal naming icon.png", err)
	}
	// No icon in the package: nothing about icons.
	_, err = f.manager.InstallPackage(ctx, plugintest.PackageZip(t, manifest, module, doc(icon)), plugins.SourceUpload)
	if refusalReason(err) != plugins.ReasonSignature || strings.Contains(err.Error(), "icon") {
		t.Fatalf("package with no icon: err = %v, want a signature refusal that does not mention an icon", err)
	}
}
