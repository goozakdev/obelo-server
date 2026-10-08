package signing_test

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The optional icon.png joins the signed message only when there is one, so a
// signature made before icons existed is the same bytes it always was.

func TestASignatureOverNoIconIsTheSignatureItAlwaysWas(t *testing.T) {
	t.Parallel()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	manifest, module := []byte(`{"id":"x"}`), []byte("wasm")

	// The pre-icon message, built here from its definition and not from the package.
	m, w := sha256.Sum256(manifest), sha256.Sum256(module)
	legacyMessage := append(append([]byte(pluginapi.SignatureDomain), m[:]...), w[:]...)

	sig, err := signing.SignWithIcon(priv, "P", manifest, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := signing.Sign(priv, "P", manifest, module)
	if err != nil {
		t.Fatal(err)
	}
	if sig != old {
		t.Fatalf("SignWithIcon(nil) = %+v, want exactly Sign's %+v", sig, old)
	}
	if string(signing.Message(manifest, module)) != string(legacyMessage) {
		t.Fatal("the no-icon signed message changed")
	}
	if err := signing.Verify(sig, pub, manifest, module); err != nil {
		t.Fatalf("a no-icon signature stopped verifying: %v", err)
	}
	if err := signing.VerifyWithIcon(sig, pub, manifest, module, nil); err != nil {
		t.Fatalf("a no-icon signature stopped verifying with no icon: %v", err)
	}
}

func TestAnIconIsCoveredWhenPresent(t *testing.T) {
	t.Parallel()
	pub, priv, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	manifest, module, icon := []byte(`{"id":"x"}`), []byte("wasm"), []byte("icon-bytes")
	sig, err := signing.SignWithIcon(priv, "P", manifest, module, icon)
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyWithIcon(sig, pub, manifest, module, icon); err != nil {
		t.Fatalf("the icon it was signed with does not verify: %v", err)
	}
	for name, got := range map[string][]byte{"swapped": []byte("other-icon"), "removed": nil} {
		if err := signing.VerifyWithIcon(sig, pub, manifest, module, got); !errors.Is(err, signing.ErrBadSignature) {
			t.Fatalf("icon %s: err = %v, want ErrBadSignature", name, err)
		}
	}
	// An icon on a package whose signature never covered one is a change too.
	plain, _ := signing.Sign(priv, "P", manifest, module)
	if err := signing.VerifyWithIcon(plain, pub, manifest, module, icon); !errors.Is(err, signing.ErrBadSignature) {
		t.Fatalf("an icon added to a no-icon signature: err = %v, want ErrBadSignature", err)
	}
}
