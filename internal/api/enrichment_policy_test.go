package api_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for the per-Library Enrichment policy (ADR-0027, slice 01):
// the enrich on/off key end-to-end — sparse storage (inherit vs. overridden),
// the derived effective/inherited enablement in the view, Admin-only enforcement,
// and the behavioral contract that enrich_enabled=false stops a Library enriching
// (zero provider calls, Titles re-marked 'disabled') without touching any other
// Library. Driven through the fake provider-builder path so the per-Library
// resolver is live (a fixed injected provider bypasses it) with zero network.

type enrichmentPolicyView struct {
	EnrichEnabled          *bool `json:"enrichEnabled"`
	InheritedEnrichEnabled bool  `json:"inheritedEnrichEnabled"`
	Effective              struct {
		Video bool `json:"video"`
		Music bool `json:"music"`
	} `json:"effective"`
	MetadataLanguage          *string `json:"metadataLanguage"`
	InheritedMetadataLanguage string  `json:"inheritedMetadataLanguage"`
	AuthoritativeProvider     *string `json:"authoritativeProvider"`
	InheritedAuthoritative    struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"inheritedAuthoritative"`
	EffectiveAuthoritative struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"effectiveAuthoritative"`
	AuthoritativeUnreachable *string `json:"authoritativeUnreachable"`
	AuthoritativeCandidates  []struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"authoritativeCandidates"`
}

func policyPath(libID string) string { return "/api/v1/libraries/" + libID + "/enrichment-policy" }

// countingBuilder composes a shared call-counting fake provider so a test can
// assert a disabled Library made ZERO outbound calls. Enablement is the real
// settings-derived one (DeriveEnablement), so a keyed/enabled global enables video.
func countingBuilder(prov *fakeProvider) enrich.BuildFunc {
	return func(cfg enrich.ProviderConfig) (enrich.MetadataProvider, enrich.Enablement) {
		return enrich.CompositeProvider{Video: prov, Music: prov}, enrich.DeriveEnablement(cfg)
	}
}

func getPolicy(t *testing.T, srv *testharness.Server, token, libID string) enrichmentPolicyView {
	t.Helper()
	var v enrichmentPolicyView
	status, body := srv.AuthGET(policyPath(libID), token, &v)
	if status != http.StatusOK {
		t.Fatalf("GET policy = %d, want 200; body: %s", status, body)
	}
	return v
}

func putPolicy(t *testing.T, srv *testharness.Server, token, libID string, body map[string]any, wantStatus int) enrichmentPolicyView {
	t.Helper()
	var v enrichmentPolicyView
	status, raw := srv.JSON(http.MethodPut, policyPath(libID), token, body, &v)
	if status != wantStatus {
		t.Fatalf("PUT policy = %d, want %d; body: %s", status, wantStatus, raw)
	}
	return v
}

// TestEnrichmentPolicyDefaultInherits: an untouched Library reports an empty
// policy (enrichEnabled null = inherit) and the derived effective enablement
// tracks the global config — the "no observable change for existing Libraries"
// acceptance criterion.
func TestEnrichmentPolicyDefaultInherits(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("test-key"), // video enabled globally
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	v := getPolicy(t, srv, token, libID)
	if v.EnrichEnabled != nil {
		t.Errorf("default enrichEnabled = %v, want null (inherit)", *v.EnrichEnabled)
	}
	if !v.Effective.Video || !v.InheritedEnrichEnabled {
		t.Errorf("default view = %+v, want effective video on + inherited on (global enabled)", v)
	}
}

// TestEnrichmentPolicyDisableStopsEnrichment is the core behavioral test: setting
// enrich_enabled=false re-enriches the Library to 'disabled' with ZERO provider
// calls, other Libraries keep enriching, and clearing back to inherit restores it.
func TestEnrichmentPolicyDisableStopsEnrichment(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Baseline: a manual pass matches the Titles (provider is consulted).
	enrichLib(t, srv, token, libID, "")
	duneID := titleIDByName(t, srv, token, libID, "Dune")
	if d := getEnrichedDetail(t, srv, token, duneID); d.EnrichmentStatus != "matched" {
		t.Fatalf("precondition: Dune status = %q, want matched", d.EnrichmentStatus)
	}

	// Turn the Library off. The PUT kicks an immediate background full re-enrich;
	// wait for its effect: every Title becomes 'disabled'.
	v := putPolicy(t, srv, token, libID, map[string]any{"enrichEnabled": false}, http.StatusOK)
	if v.EnrichEnabled == nil || *v.EnrichEnabled != false || v.Effective.Video {
		t.Errorf("after disable view = %+v, want stored false + effective video off", v)
	}
	waitFor(t, "disabled re-enrich to mark Dune disabled", func() bool {
		return getEnrichedDetail(t, srv, token, duneID).EnrichmentStatus == "disabled"
	})

	// A manual pass over the switched-off Library makes ZERO provider calls.
	callsBefore := prov.calls()
	res := enrichLib(t, srv, token, libID, "full")
	if prov.calls() != callsBefore {
		t.Errorf("provider called %d times for a switched-off Library, want 0", prov.calls()-callsBefore)
	}
	if res.Matched != 0 || res.Disabled == 0 {
		t.Errorf("switched-off pass result = %+v, want 0 matched + some disabled", res)
	}

	// Another Library (its own root, no overlap) is unaffected — it still enriches.
	otherRoot := t.TempDir()
	makeMovie(t, otherRoot+"/Arrival (2016)/Arrival (2016).mkv")
	otherID := createMovieLibrary(t, srv, token, otherRoot)
	scanLib(t, srv, token, otherID, "")
	enrichLib(t, srv, token, otherID, "")
	otherMovie := titleIDByName(t, srv, token, otherID, "Arrival")
	if d := getEnrichedDetail(t, srv, token, otherMovie); d.EnrichmentStatus != "matched" {
		t.Errorf("other Library title status = %q, want matched (unaffected by lib %s policy)", d.EnrichmentStatus, libID)
	}

	// Clear back to inherit: null override, and a full re-enrich re-matches.
	cleared := putPolicy(t, srv, token, libID, map[string]any{"enrichEnabled": nil}, http.StatusOK)
	if cleared.EnrichEnabled != nil || !cleared.Effective.Video {
		t.Errorf("after clear view = %+v, want null (inherit) + effective video on", cleared)
	}
	waitFor(t, "cleared policy re-enrich to re-match Dune", func() bool {
		return getEnrichedDetail(t, srv, token, duneID).EnrichmentStatus == "matched"
	})
}

// TestEnrichmentPolicyLanguageOverride: a Library can override its metadata
// language distinct from the server-wide default; the view reports the stored
// override + inherited language, the override reaches the composed provider on the
// immediate re-enrich, and clearing it tracks the global language again.
func TestEnrichmentPolicyLanguageOverride(t *testing.T) {
	requireFixtures(t)

	// A builder that records the metadata language it was asked to compose with, so
	// the test can assert the per-Library override actually reached the chain.
	var (
		mu   sync.Mutex
		seen []string
	)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	recording := func(cfg enrich.ProviderConfig) (enrich.MetadataProvider, enrich.Enablement) {
		mu.Lock()
		seen = append(seen, cfg.MetadataLanguage)
		mu.Unlock()
		return enrich.CompositeProvider{Video: prov, Music: prov}, enrich.DeriveEnablement(cfg)
	}
	sawLanguage := func(lang string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range seen {
			if l == lang {
				return true
			}
		}
		return false
	}

	srv := testharness.New(t,
		testharness.WithProviderBuilder(recording),
		testharness.WithEnrichmentKey("test-key"), // video enabled globally (en-US default)
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Default: language inherits (null), inherited value is the server default.
	v := getPolicy(t, srv, token, libID)
	if v.MetadataLanguage != nil {
		t.Errorf("default metadataLanguage = %q, want null (inherit)", *v.MetadataLanguage)
	}
	if v.InheritedMetadataLanguage != "en-US" {
		t.Errorf("inheritedMetadataLanguage = %q, want en-US", v.InheritedMetadataLanguage)
	}

	// Override to ja-JP: the stored value round-trips and the immediate re-enrich
	// composes the chain in that language.
	v = putPolicy(t, srv, token, libID, map[string]any{"metadataLanguage": "ja-JP"}, http.StatusOK)
	if v.MetadataLanguage == nil || *v.MetadataLanguage != "ja-JP" {
		t.Errorf("after override metadataLanguage = %v, want ja-JP", v.MetadataLanguage)
	}
	waitFor(t, "the re-enrich to compose in the overridden language", func() bool {
		return sawLanguage("ja-JP")
	})

	// Clear back to inherit: null, and the inherited value still tracks the global.
	cleared := putPolicy(t, srv, token, libID, map[string]any{"metadataLanguage": nil}, http.StatusOK)
	if cleared.MetadataLanguage != nil {
		t.Errorf("after clear metadataLanguage = %v, want null (inherit)", cleared.MetadataLanguage)
	}
	if cleared.InheritedMetadataLanguage != "en-US" {
		t.Errorf("inheritedMetadataLanguage after clear = %q, want en-US", cleared.InheritedMetadataLanguage)
	}
}

// TestEnrichmentPolicyAuthoritativePointer: the authoritative dropdown lists usable
// Full providers of the Library's kind, a Library can repoint its authoritative at a
// keyed OMDb (effective reflects it), an unusable slug is rejected 422, and a chosen
// authoritative that later becomes unreachable falls back to the default and is
// surfaced (never silently dropped) — ADR-0027.
func TestEnrichmentPolicyAuthoritativePointer(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("tmdb-key"), // TMDB keyed globally
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	// Make OMDb a usable Full video provider (globally enabled + keyed). TheTVDB stays
	// unconfigured (unkeyed) so it must NOT appear in the candidate list.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{{"slug": "omdb", "enabled": true, "apiKey": "omdb-key"}}}, http.StatusOK)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// Default view: inherits the kind default (TMDB); candidates are the keyed Full
	// video providers (TMDB + OMDb), NOT the unkeyed TheTVDB nor artwork-only sources.
	v := getPolicy(t, srv, token, libID)
	if v.AuthoritativeProvider != nil {
		t.Errorf("default authoritativeProvider = %q, want null (inherit)", *v.AuthoritativeProvider)
	}
	if v.InheritedAuthoritative.Slug != "tmdb" || v.EffectiveAuthoritative.Slug != "tmdb" {
		t.Errorf("default authoritative = inherited %q / effective %q, want tmdb/tmdb", v.InheritedAuthoritative.Slug, v.EffectiveAuthoritative.Slug)
	}
	if !hasCandidate(v.AuthoritativeCandidates, "tmdb") || !hasCandidate(v.AuthoritativeCandidates, "omdb") {
		t.Errorf("candidates = %+v, want tmdb + omdb", v.AuthoritativeCandidates)
	}
	if hasCandidate(v.AuthoritativeCandidates, "thetvdb") || hasCandidate(v.AuthoritativeCandidates, "fanarttv") {
		t.Errorf("candidates = %+v, want no unkeyed/artwork-only sources", v.AuthoritativeCandidates)
	}

	// Repoint the authoritative at OMDb: stored + effective reflect it.
	v = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "omdb"}, http.StatusOK)
	if v.AuthoritativeProvider == nil || *v.AuthoritativeProvider != "omdb" {
		t.Errorf("stored authoritative = %v, want omdb", v.AuthoritativeProvider)
	}
	if v.EffectiveAuthoritative.Slug != "omdb" {
		t.Errorf("effective authoritative = %q, want omdb (leads now)", v.EffectiveAuthoritative.Slug)
	}
	if v.AuthoritativeUnreachable != nil {
		t.Errorf("unreachable = %v, want none (OMDb keyed)", *v.AuthoritativeUnreachable)
	}

	// An artwork-only source can never lead → 422; an unkeyed Full provider likewise.
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "fanarttv"}, http.StatusUnprocessableEntity)
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "thetvdb"}, http.StatusUnprocessableEntity)

	// OMDb becomes unreachable AFTER selection (its key is cleared globally): the
	// Library falls back to the default authoritative and surfaces the degradation.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{{"slug": "omdb", "enabled": false, "apiKey": ""}}}, http.StatusOK)
	v = getPolicy(t, srv, token, libID)
	if v.AuthoritativeProvider == nil || *v.AuthoritativeProvider != "omdb" {
		t.Errorf("stored pointer = %v, want it PRESERVED as omdb (a surviving delta)", v.AuthoritativeProvider)
	}
	if v.EffectiveAuthoritative.Slug != "tmdb" {
		t.Errorf("effective authoritative = %q, want tmdb (fell back, never stalls)", v.EffectiveAuthoritative.Slug)
	}
	if v.AuthoritativeUnreachable == nil || *v.AuthoritativeUnreachable != "omdb" {
		t.Errorf("unreachable = %v, want omdb surfaced (never silently dropped)", v.AuthoritativeUnreachable)
	}
}

func hasCandidate(cands []struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}, slug string) bool {
	for _, c := range cands {
		if c.Slug == slug {
			return true
		}
	}
	return false
}

// TestEnrichmentPolicyAdminOnly: the whole surface is Admin-only (Member → 403 on
// GET and PUT) and an unknown Library is 404 (hide-existence).
func TestEnrichmentPolicyAdminOnly(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	if status, _ := srv.AuthGET(policyPath(libID), mTok, nil); status != http.StatusForbidden {
		t.Errorf("member GET policy = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPut, policyPath(libID), mTok, map[string]any{"enrichEnabled": false}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT policy = %d, want 403", status)
	}

	// Unknown Library → 404 for the Admin.
	if status, _ := srv.AuthGET(policyPath("no-such-lib"), token, nil); status != http.StatusNotFound {
		t.Errorf("GET policy for unknown library = %d, want 404", status)
	}
}
