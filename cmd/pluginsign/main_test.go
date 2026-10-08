package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
)

// The signature document names the public key that made it (ADR-0069, Q2b), so a
// server with nothing pinned can record who signed.
func TestSignWritesThePublicKeyAndTheVerifierAcceptsIt(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "k.key")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"keygen", "-out", keyFile}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	manifest, module := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "plugin.wasm")
	if err := os.WriteFile(manifest, []byte(`{"id":"x","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("wasm"), 0o644); err != nil {
		t.Fatal(err)
	}
	sigFile := filepath.Join(dir, "plugin.sig.json")
	if err := run([]string{"sign", "-key", keyFile, "-publisher", "P", "-manifest", manifest, "-module", module, "-out", sigFile}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := readPrivateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["publicKey"] != signing.EncodeKey(pub) {
		t.Fatalf("publicKey = %v, want the signing key %s", doc["publicKey"], signing.EncodeKey(pub))
	}
	verify := []string{"verify", "-sig", sigFile, "-pub", signing.EncodeKey(pub), "-manifest", manifest, "-module", module}
	if err := run(verify, &stdout, &stderr); err != nil {
		t.Fatalf("the verifier rejected a document with the key: %v", err)
	}

	// A document without the field (everything signed before this) is still accepted.
	delete(doc, "publicKey")
	old, _ := json.Marshal(doc)
	if err := os.WriteFile(sigFile, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(verify, &stdout, &stderr); err != nil {
		t.Fatalf("the verifier rejected a document without the key: %v", err)
	}
}
