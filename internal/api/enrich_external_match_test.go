package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Regression test (movie-library bug: every Edit-item image tab said "No images
// offered" for a search-matched Movie): the enrichment pass must PERSIST the
// resolved external id on a leaf Title, because artwork candidates are queried
// LIVE keyed on the stored id — the real TMDB provider offers nothing for a ref
// with no resolved id. The fake mirrors that exact behavior (unlike the other
// enrich fakes, which answer any ref), so this goes red if the pass stops
// persisting the id. The entity path (Shows) already persists it; this pins the
// leaf path to the same rule.
func TestSearchResolvedEnrichPersistsExternalIDForCandidates(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{
		fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil },
		artworkFn: func(ref enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
			// Mirror TMDBProvider.ArtworkCandidates: no resolved id → nothing to list.
			if ref.TMDBID == "" {
				return nil, nil
			}
			return []enrich.ArtworkCandidate{
				{URL: "https://img.example/" + role + "-1.jpg", Width: 1000, Height: 1500, Source: "tmdb"},
			}, nil
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("img")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// Dune has no {tmdb-…} token, so its record was resolved by search — the id
	// must come from the pass itself, not the parse.
	id := titleIDByName(t, srv, token, libID, "Dune")
	if d := getEnrichedDetail(t, srv, token, id); d.TMDBID != "999" {
		t.Errorf("detail tmdbId = %q, want 999 (the search-resolved record id must persist)", d.TMDBID)
	}

	// …and with the id persisted, every Edit-item image tab lists candidates
	// instead of "No images offered for this item."
	for _, role := range []string{"poster", "background", "logo"} {
		var cands artworkCandidatesResp
		if st, body := srv.AuthGET("/api/v1/titles/"+id+"/artworkCandidates?role="+role, token, &cands); st != http.StatusOK {
			t.Fatalf("GET %s artworkCandidates = %d; body: %s", role, st, body)
		}
		if len(cands.Candidates) == 0 {
			t.Errorf("%s candidates empty — the live lookup was handed a ref with no persisted external id", role)
		}
	}
}
