package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// artwork-management issue 04 black-box tests: the per-session candidate cache —
// a lightweight optimization so that auto-search-on-tab-open doesn't re-hit the
// metadata providers on every toggle (protecting TMDB/fanart/etc. rate-limits).
// Driven through the real HTTP surface with a COUNTING fake MetadataProvider
// (zero network). The invariants: two candidate requests for the same (entity,
// role) within the TTL hit the provider ONCE; picking or uploading a new image
// invalidates that entry so the next request re-queries; after the TTL a request
// re-queries; and with the cache disabled every request re-queries (correctness
// is independent of the cache). Prior art: fixlabel_test.go, artwork_upload_test.go.

// cacheProbeProvider builds a fake provider that resolves a Movie to a pinned id
// and offers a fixed two-image poster grid, recording each ArtworkCandidates call
// so a test can count provider hits.
func cacheProbeProvider() *fakeProvider {
	return &fakeProvider{
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
		artworkFn: func(_ enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			return []enrich.ArtworkCandidate{
				{URL: "https://img.example/" + role + "-1.jpg", Width: 1000, Height: 1500, Source: "tmdb"},
				{URL: "https://img.example/" + role + "-2.jpg", Width: 680, Height: 1000, Source: "tmdb"},
			}, nil
		},
	}
}

// getPosterCandidates lists a Movie's poster candidates, asserting a clean 200 +
// the expected grid, and returns the parsed response.
func getPosterCandidates(t *testing.T, srv *testharness.Server, token, titleID string) artworkCandidatesResp {
	t.Helper()
	var cands artworkCandidatesResp
	if st, body := srv.AuthGET("/api/v1/titles/"+titleID+"/artworkCandidates?role=poster", token, &cands); st != http.StatusOK {
		t.Fatalf("GET poster artworkCandidates = %d; body: %s", st, body)
	}
	if len(cands.Candidates) != 2 {
		t.Fatalf("poster candidates = %d, want 2 (the cache must return the same grid): %+v", len(cands.Candidates), cands)
	}
	return cands
}

// TestArtworkCandidateCacheHitAndInvalidation: two successive candidate requests
// within the TTL hit the provider once (a cache hit); picking a candidate and,
// separately, uploading an image each invalidate that (entity, role) entry so the
// next request re-queries — all with the production-default TTL.
func TestArtworkCandidateCacheHitAndInvalidation(t *testing.T) {
	requireNamingFixtures(t)
	prov := cacheProbeProvider()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("PICKED")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Pinned Movie") // no local poster

	// Baseline: enrichment does not use the ArtworkCandidates seam, so provider
	// hits are counted purely from the picker requests below.
	base := prov.artworkCalls()

	// (AC) Two requests within the TTL → the provider is hit exactly once.
	cands := getPosterCandidates(t, srv, token, id)
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 1 {
		t.Fatalf("provider hits after two cached requests = %d, want 1 (second served from cache)", got)
	}

	// (AC) Picking a candidate invalidates the entry → the next request re-queries.
	if st, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/artwork", token,
		map[string]any{"role": "poster", "url": cands.Candidates[0].URL}, nil); st != http.StatusOK {
		t.Fatalf("PUT pick poster = %d; body: %s", st, body)
	}
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 2 {
		t.Fatalf("provider hits after pick-then-request = %d, want 2 (pick invalidated the cache)", got)
	}
	// The re-queried result is now cached again (a second request does not re-hit).
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 2 {
		t.Fatalf("provider hits after re-cache = %d, want 2 (post-pick result should cache)", got)
	}

	// (AC) Uploading an image invalidates the entry too → the next request re-queries.
	if st := uploadArtwork(t, srv, token, "/api/v1/titles/"+id+"/artworkUpload?role=poster",
		"image/jpeg", jpegImage("UPLOADED"), nil); st != http.StatusOK {
		t.Fatalf("upload poster = %d, want 200", st)
	}
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 3 {
		t.Fatalf("provider hits after upload-then-request = %d, want 3 (upload invalidated the cache)", got)
	}
}

// TestArtworkCandidateCacheExpiryReQueries: with a tiny TTL, a request after the
// TTL elapses re-queries the provider (the cache is short-lived, not permanent).
func TestArtworkCandidateCacheExpiryReQueries(t *testing.T) {
	requireNamingFixtures(t)
	prov := cacheProbeProvider()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		testharness.WithArtworkCandidateCacheTTL(40*time.Millisecond),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Pinned Movie")

	base := prov.artworkCalls()
	getPosterCandidates(t, srv, token, id)
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 1 {
		t.Fatalf("provider hits within TTL = %d, want 1 (second served from cache)", got)
	}

	time.Sleep(80 * time.Millisecond) // let the TTL elapse
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 2 {
		t.Fatalf("provider hits after TTL = %d, want 2 (a request past the TTL re-queries)", got)
	}
}

// TestArtworkCandidateCacheDisabledReQueriesEveryTime: with the cache disabled
// (TTL 0), every request re-queries — correctness/behavior is identical to having
// no cache at all, proving the cache is a pure optimization.
func TestArtworkCandidateCacheDisabledReQueriesEveryTime(t *testing.T) {
	requireNamingFixtures(t)
	prov := cacheProbeProvider()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		testharness.WithArtworkCandidateCacheTTL(0), // cache OFF
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Pinned Movie")

	base := prov.artworkCalls()
	getPosterCandidates(t, srv, token, id)
	getPosterCandidates(t, srv, token, id)
	getPosterCandidates(t, srv, token, id)
	if got := prov.artworkCalls() - base; got != 3 {
		t.Fatalf("provider hits with cache disabled = %d, want 3 (every request re-queries)", got)
	}
}
