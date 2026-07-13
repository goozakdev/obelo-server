package api_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// External-metadata-enrichment issue 05 black-box tests: enrichment-match
// correction + the attention surface. An Admin re-points a Title's external
// metadata id via PUT /titles/{id}/enrichmentMatch and the Title re-enriches
// immediately WITHOUT disturbing identity or watch state (ADR-0014); the Titles
// that enrichment could not match (unmatched/failed) surface for the Admin on a
// new attention dimension, distinct from the identity Unmatched bucket. All driven
// through the HTTP API with the FAKE MetadataProvider + ArtworkFetcher (zero
// network), like the other enrichment specs.

// --- Wire shapes ------------------------------------------------------------

type attentionTitleResp struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Year             int    `json:"year"`
	EnrichmentStatus string `json:"enrichmentStatus"`
}

type attentionResp struct {
	Titles []attentionTitleResp `json:"titles"`
}

func listEnrichmentAttention(t *testing.T, srv *testharness.Server, token, libID string) attentionResp {
	t.Helper()
	var res attentionResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/enrichment-attention", token, &res)
	if status != http.StatusOK {
		t.Fatalf("enrichment-attention = %d, want 200; body: %s", status, body)
	}
	return res
}

func attentionHas(res attentionResp, title string) (attentionTitleResp, bool) {
	for _, ti := range res.Titles {
		if ti.Title == title {
			return ti, true
		}
	}
	return attentionTitleResp{}, false
}

// --- Tests ------------------------------------------------------------------

// TestEnrichmentMatchCorrectsAndLeavesAttention is the core acceptance loop: a
// Title that no-matched appears on the attention surface; an enrichmentMatch with
// a valid id (faked provider) re-enriches it; it becomes 'matched', keeps its
// identity + watch state, and leaves the attention list. A separate provider-error
// Title ('failed') stays on the list, proving the list tracks both states.
func TestEnrichmentMatchCorrectsAndLeavesAttention(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		// A by-id lookup (the Admin's correcting id) always resolves.
		if ref.TMDBID == "999" {
			return richMeta(), nil
		}
		switch ref.Title {
		case "Blade Runner":
			return enrich.TitleMetadata{}, enrich.ErrNoMatch // no-match → unmatched
		case "Sample Movie":
			return enrich.TitleMetadata{}, errors.New("provider exploded") // error → failed
		default:
			return richMeta(), nil
		}
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("POSTERBYTES")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// The attention surface lists the un-matched (Blade Runner) + failed (Sample
	// Movie) Titles, and only those — Dune matched, so it is absent.
	att := listEnrichmentAttention(t, srv, token, libID)
	if len(att.Titles) != 2 {
		t.Fatalf("attention list = %+v, want 2 (Blade Runner unmatched + Sample Movie failed)", att.Titles)
	}
	br, ok := attentionHas(att, "Blade Runner")
	if !ok || br.EnrichmentStatus != "unmatched" {
		t.Fatalf("Blade Runner not unmatched on attention list: %+v", att.Titles)
	}
	if sm, ok := attentionHas(att, "Sample Movie"); !ok || sm.EnrichmentStatus != "failed" {
		t.Fatalf("Sample Movie not failed on attention list: %+v", att.Titles)
	}
	if _, ok := attentionHas(att, "Dune"); ok {
		t.Errorf("matched Title Dune leaked onto the attention list: %+v", att.Titles)
	}

	// Capture identity + set a watch state BEFORE the match; both must survive.
	before := getEnrichedDetail(t, srv, token, br.ID)
	if before.EnrichmentStatus != "unmatched" || before.Overview != "" {
		t.Fatalf("Blade Runner detail before match unexpected: %+v", before)
	}
	srv.JSON(http.MethodPut, "/api/v1/titles/"+br.ID+"/watchState", token,
		map[string]any{"watched": true}, nil)

	// Correct the match: re-point to tmdbId 999 and re-enrich just this Title.
	var matched enrichedDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+br.ID+"/enrichmentMatch", token,
		map[string]any{"tmdbId": "999"}, &matched)
	if status != http.StatusOK {
		t.Fatalf("PUT enrichmentMatch = %d, want 200; body: %s", status, body)
	}
	if matched.EnrichmentStatus != "matched" {
		t.Errorf("status after match = %q, want matched", matched.EnrichmentStatus)
	}
	if matched.Overview == "" || matched.TMDBID != "999" {
		t.Errorf("re-enriched detail not decorated / id not set: %+v", matched)
	}
	// The corrected detail must carry an artwork cache-bust token: the detail hero
	// busts its Logo/Background <img> on this (the fetched-artwork cache filename is
	// stable per (Title, role), so the row `path` can't bust a replaced image).
	// Without it a re-matched Title's hero art would stay stale in the browser.
	if matched.ArtworkVersion == "" {
		t.Errorf("re-enriched detail missing artworkVersion (hero art can't cache-bust): %+v", matched)
	}

	// Identity unchanged (year), watch state preserved (ADR-0014).
	after := getEnrichedDetail(t, srv, token, br.ID)
	if after.Year != before.Year {
		t.Errorf("year changed by enrichment match: before=%d after=%d", before.Year, after.Year)
	}
	if !after.Watched {
		t.Errorf("watch state lost across enrichment match (identity must be preserved)")
	}
	if after.EnrichmentStatus != "matched" {
		t.Errorf("status after match (GET) = %q, want matched", after.EnrichmentStatus)
	}

	// Blade Runner has left the attention list; Sample Movie (failed) remains.
	att2 := listEnrichmentAttention(t, srv, token, libID)
	if _, ok := attentionHas(att2, "Blade Runner"); ok {
		t.Errorf("matched Title still on attention list: %+v", att2.Titles)
	}
	if _, ok := attentionHas(att2, "Sample Movie"); !ok {
		t.Errorf("failed Title dropped from attention list: %+v", att2.Titles)
	}
}

// TestEnrichmentMatchDistinctFromIdentityUnmatched: the enrichment attention list
// is a Title-level surface (it lists real, browsable Titles by enrichmentStatus),
// kept separate from the identity Unmatched FILES list, which is unaffected by an
// enrichment no-match.
func TestEnrichmentMatchDistinctFromIdentityUnmatched(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		if ref.Title == "Blade Runner" {
			return enrich.TitleMetadata{}, enrich.ErrNoMatch
		}
		return richMeta(), nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	// The enrichment-attention Title is a real, browsable Title (resolvable by id).
	att := listEnrichmentAttention(t, srv, token, libID)
	br, ok := attentionHas(att, "Blade Runner")
	if !ok {
		t.Fatalf("Blade Runner not on enrichment attention list: %+v", att.Titles)
	}
	if status, _ := srv.AuthGET("/api/v1/titles/"+br.ID, token, nil); status != http.StatusOK {
		t.Errorf("enrichment-attention Title not browsable: GET title = %d, want 200", status)
	}

	// The identity Unmatched FILES list does NOT carry this Title (it tracks media
	// with no extractable identity, a separate concern).
	var um unmatchedListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/unmatched", token, &um)
	for _, f := range um.Files {
		if f.ID == br.ID {
			t.Errorf("enrichment-unmatched Title leaked into the identity Unmatched bucket")
		}
	}
}

// TestEnrichmentMatchRequiresAdmin: a Member cannot read the enrichment attention
// list or correct a Title's match (both 403).
func TestEnrichmentMatchRequiresAdmin(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	if status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/enrichment-attention", mTok, nil); status != http.StatusForbidden {
		t.Errorf("member GET enrichment-attention = %d, want 403", status)
	}
	status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentMatch", mTok,
		map[string]any{"tmdbId": "999"}, nil)
	if status != http.StatusForbidden {
		t.Errorf("member PUT enrichmentMatch = %d, want 403", status)
	}
}

// TestEnrichmentMatchValidation: an empty body (no external id) is 400; an unknown
// Title is 404 (hide existence).
func TestEnrichmentMatchValidation(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	if status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentMatch", token,
		map[string]any{}, nil); status != http.StatusBadRequest {
		t.Errorf("empty enrichmentMatch body = %d, want 400", status)
	}
	if status, _ := srv.JSON(http.MethodPut, "/api/v1/titles/does-not-exist/enrichmentMatch", token,
		map[string]any{"tmdbId": "999"}, nil); status != http.StatusNotFound {
		t.Errorf("enrichmentMatch on unknown Title = %d, want 404", status)
	}
}
