package rotation

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testEncKey returns a fresh base64 AES-256 key for a test.
func testEncKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// TestEncryptDecryptRoundTrip is the core guarantee: what Encrypt seals under a key
// Decrypt opens back to the same Keys, and a fresh nonce makes two encryptions of
// the same input differ (so a scraper can't fingerprint an unchanged payload).
func TestEncryptDecryptRoundTrip(t *testing.T) {
	enc := testEncKey(t)
	in := Keys{TMDB: "tmdb-secret", Fanart: "fanart-secret"}

	payload, err := Encrypt(enc, in)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(enc, payload)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip = %+v, want %+v", got, in)
	}

	payload2, err := Encrypt(enc, in)
	if err != nil {
		t.Fatalf("Encrypt (2): %v", err)
	}
	if payload == payload2 {
		t.Fatalf("two encryptions produced identical payloads — nonce is not random")
	}
}

// TestDecryptRejectsWrongKey: a payload sealed under one key must not decrypt under
// another — GCM's authentication tag fails, and Decrypt reports an error rather than
// returning garbage keys (which would then fail every provider call silently).
func TestDecryptRejectsWrongKey(t *testing.T) {
	payload, err := Encrypt(testEncKey(t), Keys{TMDB: "x"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt(testEncKey(t), payload); err == nil {
		t.Fatal("Decrypt with the wrong key succeeded, want an error")
	}
}

// TestDecryptRejectsMalformed covers the hostile-input surface: non-base64, a too-
// short blob that can't hold a nonce, and a wrong-length key. None may panic; each
// is a plain error so the caller falls through to bootstrap.
func TestDecryptRejectsMalformed(t *testing.T) {
	enc := testEncKey(t)
	cases := []struct {
		name    string
		key     string
		payload string
	}{
		{"not base64 payload", enc, "!!! not base64 !!!"},
		{"payload too short", enc, base64.StdEncoding.EncodeToString([]byte("short"))},
		{"key not base64", "!!!", "AAAA"},
		{"key wrong length", base64.StdEncoding.EncodeToString([]byte("too-short-key")), "AAAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decrypt(tc.key, tc.payload); err == nil {
				t.Fatalf("Decrypt(%q) succeeded, want an error", tc.name)
			}
		})
	}
}

// serveEnvelope spins up a stub rotation endpoint returning the given envelope JSON
// (or raw body when env is nil). It returns the URL and a pointer to a hit counter.
func serveEnvelope(t *testing.T, env *Envelope, raw string) (url string, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if env != nil {
			_ = json.NewEncoder(w).Encode(env)
			return
		}
		_, _ = io.WriteString(w, raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// TestFetchHappyPath drives the full client path against a stub: an encrypted v=1
// envelope round-trips to the plaintext keys.
func TestFetchHappyPath(t *testing.T) {
	enc := testEncKey(t)
	payload, err := Encrypt(enc, Keys{TMDB: "rotated-tmdb", Fanart: "rotated-fanart"})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	url, hits := serveEnvelope(t, &Envelope{V: 1, MinAppVersion: "0.1.0", Payload: payload}, "")

	c := Client{URL: url, EncKeyB64: enc, AppVersion: "0.1.0", UserAgent: "juicebox/test"}
	keys, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if keys.TMDB != "rotated-tmdb" || keys.Fanart != "rotated-fanart" {
		t.Fatalf("Fetch keys = %+v, want rotated-tmdb/rotated-fanart", keys)
	}
	if *hits != 1 {
		t.Fatalf("endpoint hit %d times, want 1", *hits)
	}
}

// TestFetchRejectsUnsupportedVersion: an envelope announcing an unknown v is an
// explicit error (not mis-parsed), so a future format change is detectable.
func TestFetchRejectsUnsupportedVersion(t *testing.T) {
	enc := testEncKey(t)
	payload, _ := Encrypt(enc, Keys{TMDB: "x"})
	url, _ := serveEnvelope(t, &Envelope{V: 2, Payload: payload}, "")

	c := Client{URL: url, EncKeyB64: enc, AppVersion: "0.1.0"}
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch of a v2 envelope succeeded, want an unsupported-version error")
	}
}

// TestFetchRespectsMinAppVersion: a payload marked for a newer build than ours is
// skipped, not adopted; an equal-or-older minAppVersion is accepted.
func TestFetchRespectsMinAppVersion(t *testing.T) {
	enc := testEncKey(t)
	payload, _ := Encrypt(enc, Keys{TMDB: "x"})

	tooNewURL, _ := serveEnvelope(t, &Envelope{V: 1, MinAppVersion: "0.9.0", Payload: payload}, "")
	if _, err := (Client{URL: tooNewURL, EncKeyB64: enc, AppVersion: "0.1.0"}).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch of a too-new payload succeeded, want a version-skip error")
	}

	okURL, _ := serveEnvelope(t, &Envelope{V: 1, MinAppVersion: "0.1.0", Payload: payload}, "")
	if _, err := (Client{URL: okURL, EncKeyB64: enc, AppVersion: "0.1.0"}).Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch of an equal-version payload failed: %v", err)
	}
}

// TestFetchTransportAndBodyErrors: a non-200, a garbage body, and a missing enc key
// all return errors (fail-safe) and — for the no-key case — make no network call.
func TestFetchTransportAndBodyErrors(t *testing.T) {
	enc := testEncKey(t)

	// Non-200.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if _, err := (Client{URL: down.URL, EncKeyB64: enc, AppVersion: "0.1.0"}).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch of a 503 succeeded, want an error")
	}

	// Garbage (non-JSON) body.
	garbageURL, _ := serveEnvelope(t, nil, "this is not json")
	if _, err := (Client{URL: garbageURL, EncKeyB64: enc, AppVersion: "0.1.0"}).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch of a non-JSON body succeeded, want an error")
	}

	// No enc key (build-from-source): errors WITHOUT hitting the network.
	url, hits := serveEnvelope(t, &Envelope{V: 1}, "")
	if _, err := (Client{URL: url, EncKeyB64: "", AppVersion: "0.1.0"}).Fetch(context.Background()); err == nil {
		t.Fatal("Fetch with no enc key succeeded, want an error")
	}
	if *hits != 0 {
		t.Fatalf("Fetch with no enc key made %d network calls, want 0", *hits)
	}
}

// TestVersionAtLeast pins the lenient comparison semantics the min-version gate
// relies on, including the "0.x" wildcard and missing-segment handling.
func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, min string
		want      bool
	}{
		{"0.1.0", "", true},      // no minimum → always ok
		{"0.1.0", "0.1.0", true}, // equal
		{"0.2.0", "0.1.0", true}, // newer
		{"0.1.0", "0.2.0", false},
		{"1.0.0", "0.9.9", true},
		{"0.1.0", "0.x", true}, // wildcard segment counts as 0
		{"0.1", "0.1.0", true}, // missing trailing segment counts as 0
		{"0.1.0", "0.1.5", false},
		{"v0.1.0", "0.1.0", true}, // leading v tolerated
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.have, tc.min); got != tc.want {
			t.Errorf("versionAtLeast(%q, %q) = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

// TestCacheRoundTrip: a saved cache loads back identically, a missing file is a
// clean not-found (no error), and a corrupt file is reported (treated as absent by
// the caller).
func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata-keys.json")

	// Missing file → found=false, no error.
	if _, found, err := LoadCache(path); err != nil || found {
		t.Fatalf("LoadCache(missing) = found %v err %v, want (false, nil)", found, err)
	}

	want := Cache{TMDB: "t", Fanart: "f", FetchedAt: time.Unix(1_700_000_000, 0).UTC(), V: 1}
	if err := SaveCache(path, want); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	got, found, err := LoadCache(path)
	if err != nil || !found {
		t.Fatalf("LoadCache(saved) = found %v err %v, want (true, nil)", found, err)
	}
	if got.TMDB != want.TMDB || got.Fanart != want.Fanart || got.V != want.V || !got.FetchedAt.Equal(want.FetchedAt) {
		t.Fatalf("LoadCache = %+v, want %+v", got, want)
	}
	if got.Keys() != (Keys{TMDB: "t", Fanart: "f"}) {
		t.Fatalf("Keys() = %+v, want t/f", got.Keys())
	}

	// Corrupt file → reported as an error (caller degrades to bootstrap).
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("writing corrupt cache: %v", err)
	}
	if _, found, err := LoadCache(path); err == nil || found {
		t.Fatalf("LoadCache(corrupt) = found %v err %v, want (false, error)", found, err)
	}
}
