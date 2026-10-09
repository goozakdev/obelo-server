package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/bundled"
	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// The guard supplies every id it can from a throwaway module so it never depends on
// what `make plugins` last wrote. Only ids this binary embeds are checked by the
// guard, so the test checks the one it supplies.
func supply(t *testing.T, signer ed25519.PrivateKey) {
	t.Helper()
	id := bundled.IDs()[0]
	manifest := []byte(`{"id":"` + id + `","name":"x","version":"1.0.0","apiVersion":1}`)
	module := []byte("\x00asm module")
	if err := bundled.SupplyForTests(id, manifest, module); err != nil {
		t.Fatal(err)
	}
	if signer != nil {
		sig, err := signing.Sign(signer, "Obelo", manifest, module)
		if err != nil {
			t.Fatal(err)
		}
		doc, _ := signing.Encode(sig)
		if err := bundled.SupplySignatureForTests(id, doc); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGuardFailsWhenTheKeyIsCompiledInAndAModuleIsUnsigned(t *testing.T) {
	pub, _, _ := signing.GenerateKey()
	supply(t, nil)
	var out bytes.Buffer
	if code := check(signing.EncodeKey(pub), bundled.IDs()[:1], &out); code != 1 {
		t.Fatalf("exit %d, want 1 for an unsigned module under a compiled-in key", code)
	}
	if !strings.Contains(out.String(), bundled.IDs()[0]) {
		t.Errorf("the failure does not name the module: %q", out.String())
	}
}

func TestGuardPassesWhenEveryEmbeddedModuleIsSigned(t *testing.T) {
	pub, priv, _ := signing.GenerateKey()
	supply(t, priv)
	if code := check(signing.EncodeKey(pub), bundled.IDs()[:1], &bytes.Buffer{}); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
}

func TestGuardIsSilentOnADevBuild(t *testing.T) {
	supply(t, nil)
	var out bytes.Buffer
	if code := check("", bundled.IDs()[:1], &out); code != 0 {
		t.Fatalf("exit %d, want 0 when no key is compiled in", code)
	}
}

// main must check what the build embeds (Present), not a list of its own.
func TestMainChecksPresent(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "bundled.Present()") {
		t.Error("main no longer passes bundled.Present() to check")
	}
}
