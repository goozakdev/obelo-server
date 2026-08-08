package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marioquake/obelo-server/internal/rotation"
)

// testEncKey returns a fresh base64 AES-256 key.
func testEncKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// TestBuildEnvelopeRoundTripThroughClient is the acceptance guarantee of issue 04:
// an envelope keytool seals is fetched, decrypted, and used by the REAL rotation
// client (issue 03) — the two ends of the same contract, proven to agree without any
// deployed Worker. keytool's buildEnvelope stands in for the CLI; rotation.Client
// stands in for the server's fetch path.
func TestBuildEnvelopeRoundTripThroughClient(t *testing.T) {
	enc := testEncKey(t)
	env, err := buildEnvelope(options{
		tmdb:          "rotated-tmdb",
		fanart:        "rotated-fanart",
		encKeyB64:     enc,
		minAppVersion: "0.x",
	})
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	if env.V != rotation.SupportedVersion {
		t.Fatalf("envelope v = %d, want %d", env.V, rotation.SupportedVersion)
	}

	// Serve the sealed envelope from a stub and consume it with the client the server
	// runs — closing the loop keytool → wire → server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
	}))
	defer srv.Close()

	keys, err := (rotation.Client{URL: srv.URL, EncKeyB64: enc, AppVersion: "0.1.0", UserAgent: "obelo/test"}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("client Fetch of keytool envelope: %v", err)
	}
	if keys.TMDB != "rotated-tmdb" || keys.Fanart != "rotated-fanart" {
		t.Fatalf("round-trip keys = %+v, want rotated-tmdb/rotated-fanart", keys)
	}
}

// TestBuildEnvelopeFreshNonce: two runs over identical inputs must produce different
// payloads (a fresh nonce per run), so a scraper can't fingerprint an unchanged
// payload and the maintainer can re-publish safely.
func TestBuildEnvelopeFreshNonce(t *testing.T) {
	opt := options{tmdb: "k", encKeyB64: testEncKey(t)}
	a, err := buildEnvelope(opt)
	if err != nil {
		t.Fatalf("buildEnvelope a: %v", err)
	}
	b, err := buildEnvelope(opt)
	if err != nil {
		t.Fatalf("buildEnvelope b: %v", err)
	}
	if a.Payload == b.Payload {
		t.Fatal("two runs produced identical payloads — nonce is not fresh per run")
	}
}

// TestBuildEnvelopeRejectsBadInput: the two footguns keytool must refuse — no enc key
// (nothing to seal under) and an all-empty key set (publishing it would strip every
// install's default keys) — are usage errors, not silently-shipped envelopes.
func TestBuildEnvelopeRejectsBadInput(t *testing.T) {
	if _, err := buildEnvelope(options{tmdb: "k"}); err == nil {
		t.Fatal("buildEnvelope with no enc key succeeded, want an error")
	}
	if _, err := buildEnvelope(options{encKeyB64: testEncKey(t)}); err == nil {
		t.Fatal("buildEnvelope with no provider keys succeeded, want an error")
	}
}

// TestRunWritesEnvelopeToStdout drives the full CLI path (flag parsing → sealed
// envelope on stdout) and confirms the output is the versioned envelope shape, so a
// maintainer piping keytool into `wrangler kv key put` gets exactly the client's
// contract.
func TestRunWritesEnvelopeToStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	args := []string{"-tmdb", "t", "-fanart", "f", "-enc-key", testEncKey(t)}
	if err := run(args, &out, &errBuf); err != nil {
		t.Fatalf("run: %v (stderr: %s)", err, errBuf.String())
	}
	var env rotation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, out.String())
	}
	if env.V != rotation.SupportedVersion || env.MinAppVersion != "0.x" || strings.TrimSpace(env.Payload) == "" {
		t.Fatalf("stdout envelope = %+v, want v%d / 0.x / non-empty payload", env, rotation.SupportedVersion)
	}
}

// TestRunReportsBadInput: a run missing the enc key fails (non-nil error) and writes
// nothing to stdout, so a broken invocation can't be piped into KV as an empty file.
func TestRunReportsBadInput(t *testing.T) {
	var out, errBuf bytes.Buffer
	if err := run([]string{"-tmdb", "t"}, &out, &errBuf); err == nil {
		t.Fatal("run with no enc key succeeded, want an error")
	}
	if out.Len() != 0 {
		t.Fatalf("run wrote %q to stdout on failure, want nothing", out.String())
	}
}
