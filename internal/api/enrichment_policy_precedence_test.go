package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Re-enrich precedence when a Library's Enrichment policy changes (ADR-0027, issue
// 06): Locked fields survive, a per-item Enrichment override keeps winning while its
// record's provider is reachable, and an override orphaned by a policy change (its
// provider made unreachable) is filed to the attention list — never silently
// dropped. Driven through the settings-driven provider builder so the per-Library
// resolver is live, with the fake provider (zero network).

// TestPolicyReEnrichPreservesLockedFields: changing a Library's policy re-enriches
// it, and a hand-edited Locked field is never overwritten by that pass (only
// unlocked fields refresh).
func TestPolicyReEnrichPreservesLockedFields(t *testing.T) {
	requireFixtures(t)
	// version flips the provider payload so a refresh is observable across the pass.
	version := 1
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		m := richMeta()
		if version == 2 {
			m.Overview = "A wholly different provider overview."
			m.Genres = []string{"Adventure"}
		}
		return m, nil
	}}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("tmdb-key"),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// Hand-edit + lock the overview; leave genres unlocked.
	var edited lockedDetailResp
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token,
		map[string]any{"overview": "My hand-written summary."}, &edited); status != http.StatusOK {
		t.Fatalf("PUT metadata = %d; body: %s", status, body)
	}
	if !hasLock(edited, "overview") {
		t.Fatalf("overview not locked: %v", edited.LockedFields)
	}

	// A policy change (language) re-enriches the Library in the background with the
	// NEW provider payload. The locked overview must survive; unlocked genres refresh.
	version = 2
	putPolicy(t, srv, token, libID, map[string]any{"metadataLanguage": "ja-JP"}, http.StatusOK)
	waitFor(t, "the policy re-enrich to refresh the unlocked genres", func() bool {
		d := getLockedDetail(t, srv, token, id)
		return len(d.Genres) == 1 && d.Genres[0] == "Adventure"
	})
	d := getLockedDetail(t, srv, token, id)
	if d.Overview != "My hand-written summary." {
		t.Errorf("locked overview overwritten by the policy re-enrich: %q", d.Overview)
	}
}

// TestPolicyReEnrichOverrideWinsWhileReachable: a per-item Enrichment override (a
// Title pinned to a specific TMDB record) keeps resolving that record across a
// policy change that leaves its provider reachable — most-specific-wins.
func TestPolicyReEnrichOverrideWinsWhileReachable(t *testing.T) {
	requireFixtures(t)
	// The provider only matches the PINNED id (555); anything else no-matches. So a
	// 'matched' Dune after the policy change proves it resolved via its pinned record.
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		if ref.TMDBID == "555" {
			return richMeta(), nil
		}
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("tmdb-key"),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	// TheTVDB available to force ON (a policy change that keeps TMDB the leader).
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "thetvdb", "enabled": false, "apiKey": "tvdb-key"},
	}}, http.StatusOK)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// Pin Dune to TMDB record 555 (a per-item Enrichment override) → matched.
	var matched enrichedDetailResp
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentMatch", token,
		map[string]any{"tmdbId": "555"}, &matched); status != http.StatusOK {
		t.Fatalf("PUT enrichmentMatch = %d; body: %s", status, body)
	}
	if matched.EnrichmentStatus != "matched" {
		t.Fatalf("pinned Dune status = %q, want matched", matched.EnrichmentStatus)
	}

	// A policy change that keeps TMDB the authoritative (force ON a supplement) still
	// re-enriches the Library. The override keeps winning: Dune stays matched.
	putPolicy(t, srv, token, libID, map[string]any{
		"providerOverrides": map[string]any{"thetvdb": true},
	}, http.StatusOK)
	// The re-enrich is ModeFull; wait until Dune has been reprocessed and is matched.
	waitFor(t, "the override to keep resolving its pinned record", func() bool {
		return getEnrichedDetail(t, srv, token, id).EnrichmentStatus == "matched"
	})
}

// TestPolicyReEnrichOrphansOverrideToAttention: when a policy change makes a pinned
// Title's provider UNREACHABLE (repoint the authoritative away from TMDB and mute
// TMDB), the orphaned override is filed to the attention list — not silently
// dropped, and never re-resolved against the wrong leader.
func TestPolicyReEnrichOrphansOverrideToAttention(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		if ref.TMDBID == "555" {
			return richMeta(), nil
		}
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("tmdb-key"),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	// OMDb keyed globally so it can lead; TMDB stays keyed globally (we mute it
	// per-Library, making it unreachable for THIS Library only).
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "omdb", "enabled": true, "apiKey": "omdb-key"},
	}}, http.StatusOK)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// Pin Dune to a TMDB record → matched, and off the attention list.
	var matched enrichedDetailResp
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/enrichmentMatch", token,
		map[string]any{"tmdbId": "555"}, &matched); status != http.StatusOK {
		t.Fatalf("PUT enrichmentMatch = %d; body: %s", status, body)
	}
	if _, on := attentionHas(listEnrichmentAttention(t, srv, token, libID), "Dune"); on {
		t.Fatalf("pinned+matched Dune should not be on the attention list yet")
	}

	// Repoint the authoritative to OMDb AND mute TMDB for this Library: TMDB (the
	// pin's provider) is now unreachable while video stays ON (OMDb leads). The
	// pinned Dune is orphaned → filed to the attention list as 'unmatched'.
	putPolicy(t, srv, token, libID, map[string]any{
		"authoritativeProvider": "omdb",
		"providerOverrides":     map[string]any{"tmdb": false},
	}, http.StatusOK)
	waitFor(t, "the orphaned override to surface on the attention list", func() bool {
		it, on := attentionHas(listEnrichmentAttention(t, srv, token, libID), "Dune")
		return on && it.EnrichmentStatus == "unmatched"
	})

	// It is NOT marked 'disabled' (video is still on via OMDb) — the override is
	// surfaced for the Admin, not silently swept as a switched-off Library would be.
	if d := getEnrichedDetail(t, srv, token, id); d.EnrichmentStatus != "unmatched" {
		t.Errorf("orphaned Dune status = %q, want unmatched (attention), not disabled/dropped", d.EnrichmentStatus)
	}
}
