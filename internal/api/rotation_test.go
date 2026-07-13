package api_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	cryptorand "crypto/rand"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/rotation"
	"github.com/marioquake/juicebox/internal/testharness"
)

// rotationStub is a controllable stub for the maintainer-hosted rotation endpoint
// (ADR-0032): it serves an encrypted v=1 envelope for whatever Keys the test has
// set, encrypting with the shared enc key, and counts hits so a test can assert the
// consent gate suppressed the fetch. Safe for the async poll goroutine + the
// synchronous test driver to touch concurrently.
type rotationStub struct {
	mu     sync.Mutex
	encKey string
	keys   rotation.Keys
	hits   int
}

func (s *rotationStub) setKeys(k rotation.Keys) {
	s.mu.Lock()
	s.keys = k
	s.mu.Unlock()
}

func (s *rotationStub) hitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func (s *rotationStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		keys := s.keys
		s.mu.Unlock()
		payload, err := rotation.Encrypt(s.encKey, keys)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rotation.Envelope{V: rotation.SupportedVersion, MinAppVersion: "0.1.0", Payload: payload})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRotationEncKey mints a fresh base64 AES-256 key for a test's stub + app.
func newRotationEncKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, key); err != nil {
		t.Fatalf("generating enc key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

// recordingBuilder is a provider builder that captures the TMDB key it was last
// composed with, so a test can observe the resolved default credential reaching the
// rebuilt provider (the rotation-layer effect). It wraps a shared fake provider so
// an actual enrich pass still runs with zero network.
type recordingBuilder struct {
	mu          sync.Mutex
	prov        *fakeProvider
	lastTMDBKey string
	builds      int
}

func (rb *recordingBuilder) build() enrich.BuildFunc {
	return func(cfg enrich.ProviderConfig) (enrich.MetadataProvider, enrich.Enablement) {
		rb.mu.Lock()
		rb.lastTMDBKey = cfg.TMDBAPIKey
		rb.builds++
		rb.mu.Unlock()
		return enrich.CompositeProvider{Video: rb.prov, Music: rb.prov}, enrich.DeriveEnablement(cfg)
	}
}

func (rb *recordingBuilder) tmdbKey() string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.lastTMDBKey
}

// TestKeyRotationPickedUpOnNextPoll is the core issue-03 acceptance test: with
// consent granted and no operator key, the server fetches the rotation endpoint,
// decrypts the payload, and adopts the default key into the running provider — and
// a SUBSEQUENT poll picks up a ROTATED key without a restart (ADR-0032, layer 2).
func TestKeyRotationPickedUpOnNextPoll(t *testing.T) {
	requireFixtures(t)
	encKey := newRotationEncKey(t)
	stub := &rotationStub{encKey: encKey, keys: rotation.Keys{TMDB: "rot-A", Fanart: "fan-A"}}
	endpoint := stub.serve(t)

	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	rb := &recordingBuilder{prov: prov}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(rb.build()),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		// interval 0 → no periodic timer; the test drives polls deterministically.
		testharness.WithKeyRotation(endpoint.URL, encKey, 0),
		// Consent granted (the harness default) so the fetch is permitted.
	)
	token := adminToken(t, srv)

	// First poll: the bundled default "rot-A" is fetched, cached, and propagated into
	// the rebuilt provider — video enrichment now works out of the box.
	srv.RefreshRotationKeys()
	if got := rb.tmdbKey(); got != "rot-A" {
		t.Fatalf("provider TMDB key after first poll = %q, want rot-A", got)
	}

	// The keys are cached under the data dir with the fetch time (ADR-0007).
	cache := readKeyCache(t, srv.DataDir)
	if cache.TMDB != "rot-A" || cache.Fanart != "fan-A" || cache.FetchedAt.IsZero() {
		t.Fatalf("cache after first poll = %+v, want rot-A/fan-A with a timestamp", cache)
	}

	// End-to-end: the adopted key actually enables + drives enrichment.
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	if res := enrichLib(t, srv, token, libID, "full"); res.Matched == 0 {
		t.Fatalf("enrich with rotation key result = %+v, want some matched", res)
	}
	if prov.calls() == 0 {
		t.Fatal("provider never called after adopting the rotation key")
	}

	// Rotate the key at the endpoint and poll again: the NEW key supersedes the old
	// one in the running provider without a restart — the whole point of the channel.
	stub.setKeys(rotation.Keys{TMDB: "rot-B", Fanart: "fan-B"})
	srv.RefreshRotationKeys()
	if got := rb.tmdbKey(); got != "rot-B" {
		t.Fatalf("provider TMDB key after rotation = %q, want rot-B (rotated key not picked up)", got)
	}
	if cache := readKeyCache(t, srv.DataDir); cache.TMDB != "rot-B" {
		t.Fatalf("cache after rotation = %+v, want rot-B", cache)
	}
}

// TestKeyRotationNoFetchBeforeConsent proves the consent gate covers the rotation
// endpoint (ADR-0032): an unconsented server makes ZERO calls to it; granting
// consent then lets the fetch proceed and the default key is adopted.
func TestKeyRotationNoFetchBeforeConsent(t *testing.T) {
	encKey := newRotationEncKey(t)
	stub := &rotationStub{encKey: encKey, keys: rotation.Keys{TMDB: "rot-A"}}
	endpoint := stub.serve(t)

	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	rb := &recordingBuilder{prov: prov}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(rb.build()),
		testharness.WithKeyRotation(endpoint.URL, encKey, 0),
		testharness.WithoutEnrichmentConsent(), // undecided → the endpoint must not be contacted
	)
	token := adminToken(t, srv)

	// Undecided consent: an explicit poll makes NO call to the rotation endpoint and
	// propagates nothing.
	srv.RefreshRotationKeys()
	if h := stub.hitCount(); h != 0 {
		t.Fatalf("rotation endpoint hit %d times before consent, want 0", h)
	}
	if got := rb.tmdbKey(); got != "" {
		t.Fatalf("provider TMDB key before consent = %q, want empty", got)
	}

	// Grant consent, then poll: now the endpoint IS contacted and the key adopted.
	putConsent(t, srv, token, true)
	srv.RefreshRotationKeys()
	if h := stub.hitCount(); h == 0 {
		t.Fatal("rotation endpoint never hit after consent granted")
	}
	if got := rb.tmdbKey(); got != "rot-A" {
		t.Fatalf("provider TMDB key after consent = %q, want rot-A", got)
	}
}

// TestKeyRotationOperatorKeyWins proves BYOK bypasses the channel (ADR-0032): with
// an operator TMDB key set, the rotation payload never overrides it — the running
// provider keeps the operator's key.
func TestKeyRotationOperatorKeyWins(t *testing.T) {
	encKey := newRotationEncKey(t)
	stub := &rotationStub{encKey: encKey, keys: rotation.Keys{TMDB: "rot-A"}}
	endpoint := stub.serve(t)

	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	rb := &recordingBuilder{prov: prov}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(rb.build()),
		testharness.WithEnrichmentKey("operator-key"), // BYOK: operator's own TMDB key
		testharness.WithKeyRotation(endpoint.URL, encKey, 0),
	)
	token := adminToken(t, srv)
	_ = token

	srv.RefreshRotationKeys()
	if got := rb.tmdbKey(); got != "operator-key" {
		t.Fatalf("provider TMDB key = %q, want operator-key (BYOK must win over rotation)", got)
	}
}

// readKeyCache reads the durable rotation-key cache from a server's data dir.
func readKeyCache(t *testing.T, dataDir string) rotation.Cache {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, "metadata-keys.json"))
	if err != nil {
		t.Fatalf("reading rotation cache: %v", err)
	}
	var c rotation.Cache
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decoding rotation cache: %v", err)
	}
	return c
}
