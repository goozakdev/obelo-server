package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Admin-scope metadata-provider settings black-box tests (metadata-providers 02):
// GET masks secrets, admin-only (Member → 403), PUT secret semantics + validation,
// and the runtime hot-swap — a PUT that enables a previously-off kind makes the
// NEXT enrichment pass enrich it (not 'disabled') via a FAKE provider builder,
// with zero network and WITHOUT reconstructing the app.

// --- Wire shapes ------------------------------------------------------------

type providerView struct {
	Slug         string   `json:"slug"`
	Name         string   `json:"name"`
	Kinds        []string `json:"kinds"`
	Role         string   `json:"role"`
	RequiresKey  bool     `json:"requiresKey"`
	Enabled      bool     `json:"enabled"`
	HasKey       bool     `json:"hasKey"`
	BaseURL      string   `json:"baseURL"`
	ImageBaseURL string   `json:"imageBaseURL"`
	Description  string   `json:"description"`
	DocsURL      string   `json:"docsURL"`
}

type providersView struct {
	Providers        []providerView `json:"providers"`
	MetadataLanguage string         `json:"metadataLanguage"`
	Enablement       struct {
		Video bool `json:"video"`
		Music bool `json:"music"`
	} `json:"enablement"`
	AutoEnrichAfterScan    bool `json:"autoEnrichAfterScan"`
	EnrichIntervalSeconds  int  `json:"enrichIntervalSeconds"`
	MusicBrainzRateLimitMs int  `json:"musicBrainzRateLimitMs"`
}

const providersPath = "/api/v1/settings/metadata-providers"

func getProviders(t *testing.T, srv *testharness.Server, token string) providersView {
	t.Helper()
	var v providersView
	status, body := srv.AuthGET(providersPath, token, &v)
	if status != http.StatusOK {
		t.Fatalf("GET providers = %d, want 200; body: %s", status, body)
	}
	return v
}

func providerBySlug(v providersView, slug string) providerView {
	for _, p := range v.Providers {
		if p.Slug == slug {
			return p
		}
	}
	return providerView{}
}

// fakeProviderBuilder maps the settings-derived ProviderConfig to fake sub-
// providers with the real per-kind enablement (DeriveEnablement): a keyed/enabled
// kind enriches (richMeta), a disabled one is never consulted. Zero network.
func fakeProviderBuilder() enrich.BuildFunc {
	return func(cfg enrich.ProviderConfig) (enrich.MetadataProvider, enrich.Enablement) {
		p := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
		return enrich.CompositeProvider{Video: p, Music: p}, enrich.DeriveEnablement(cfg)
	}
}

// TestProvidersGetMasksAndAdminOnly: GET returns the registry+settings view, an
// enabled provider reports hasKey WITHOUT ever exposing the key, and the whole
// surface is Admin-only (a Member gets 403 on GET/PUT/test).
func TestProvidersGetMasksAndAdminOnly(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	// Seed a key via PUT, then confirm GET masks it.
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "enabled": true, "apiKey": "super-secret-key"}},
	}, http.StatusOK)

	v := getProviders(t, srv, token)
	if len(v.Providers) != 7 {
		t.Fatalf("got %d providers, want 7 (the registry)", len(v.Providers))
	}
	tmdb := providerBySlug(v, "tmdb")
	if !tmdb.Enabled || !tmdb.HasKey {
		t.Errorf("tmdb = %+v, want enabled + hasKey", tmdb)
	}
	// OMDb is registry-driven, so it appears automatically as a fill-only video
	// supplement (requires a key) — disabled until an Admin turns it on.
	omdb := providerBySlug(v, "omdb")
	if len(omdb.Kinds) != 1 || omdb.Kinds[0] != "video" || omdb.Role != "supplement" || !omdb.RequiresKey {
		t.Errorf("omdb registry facts = %+v, want video/supplement/requiresKey", omdb)
	}
	if omdb.Enabled || omdb.HasKey {
		t.Errorf("omdb = %+v, want disabled + no key (greenfield, not seeded)", omdb)
	}
	if tmdb.Role != "authoritative" || tmdb.RequiresKey != true {
		t.Errorf("tmdb registry facts = %+v, want authoritative/requiresKey", tmdb)
	}
	// The raw body must NEVER contain the stored key.
	_, body := srv.AuthGET(providersPath, token, nil)
	if strings.Contains(string(body), "super-secret-key") {
		t.Fatalf("GET body leaked the API key: %s", body)
	}

	// A Member is refused on every route (server is the authority).
	srv.CreateMember("bob", "pw12345678")
	member := srv.LoginAs("bob", "pw12345678")
	if status, _ := srv.AuthGET(providersPath, member, nil); status != http.StatusForbidden {
		t.Errorf("member GET = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPut, providersPath, member, map[string]any{}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPost, providersPath+"/tmdb/test", member, map[string]any{}, nil); status != http.StatusForbidden {
		t.Errorf("member test = %d, want 403", status)
	}
}

// TestProvidersPutSecretSemantics: apiKey omitted = unchanged, "" = clear,
// non-empty = set.
func TestProvidersPutSecretSemantics(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	// set
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "enabled": true, "apiKey": "k1"}},
	}, http.StatusOK)
	if !providerBySlug(getProviders(t, srv, token), "tmdb").HasKey {
		t.Fatalf("after set: hasKey = false, want true")
	}

	// omitted = unchanged (toggle enabled only; key stays)
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "enabled": true}},
	}, http.StatusOK)
	if p := providerBySlug(getProviders(t, srv, token), "tmdb"); !p.HasKey {
		t.Errorf("after omit: hasKey = false, want true (unchanged)")
	}

	// "" = clear (must also disable, since a key-requiring provider can't stay on)
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "enabled": false, "apiKey": ""}},
	}, http.StatusOK)
	if p := providerBySlug(getProviders(t, srv, token), "tmdb"); p.HasKey || p.Enabled {
		t.Errorf("after clear: %+v, want hasKey=false enabled=false", p)
	}
}

// TestProvidersPutValidation: unknown slug, key-required, malformed base URL, and
// empty language are each rejected with their 422 code, leaving settings unchanged.
func TestProvidersPutValidation(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	cases := []struct {
		name string
		body map[string]any
		code string
	}{
		{"unknown slug", map[string]any{
			"providers": []map[string]any{{"slug": "nope", "enabled": true}},
		}, "PROVIDER_UNKNOWN"},
		{"key required", map[string]any{
			"providers": []map[string]any{{"slug": "tmdb", "enabled": true}},
		}, "PROVIDER_KEY_REQUIRED"},
		{"bad base url", map[string]any{
			"providers": []map[string]any{{"slug": "musicbrainz", "enabled": true, "baseURL": "not a url"}},
		}, "PROVIDER_INVALID_BASE_URL"},
		{"empty language", map[string]any{
			"metadataLanguage": "",
		}, "PROVIDER_INVALID_LANGUAGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			status, body := srv.JSON(http.MethodPut, providersPath, token, tc.body, &env)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", status, body)
			}
			if env.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.code)
			}
		})
	}
	// A rejected update left nothing enabled (validation happens before persist).
	if v := getProviders(t, srv, token); v.Enablement.Video || v.Enablement.Music {
		t.Errorf("settings changed despite validation failures: %+v", v.Enablement)
	}
}

// TestProvidersImageHost covers the DB-backed TMDB image host: GET exposes it for
// the source that has one (and omits it for those that don't), PUT sets and resets
// it (""→registry default), and it is rejected for a non-image provider or a
// malformed URL. This is the source-of-truth parity fix — every base URL,
// including the image host, is DB-configurable rather than config-pinned.
func TestProvidersImageHost(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	const defaultImageHost = "https://image.tmdb.org/t/p/original"

	// GET: tmdb carries its image host at the registry default; a source with no
	// image host (musicbrainz) omits the field (empty string over the wire).
	v := getProviders(t, srv, token)
	if got := providerBySlug(v, "tmdb").ImageBaseURL; got != defaultImageHost {
		t.Errorf("tmdb imageBaseURL = %q, want registry default %q", got, defaultImageHost)
	}
	if got := providerBySlug(v, "musicbrainz").ImageBaseURL; got != "" {
		t.Errorf("musicbrainz imageBaseURL = %q, want empty (no image host)", got)
	}

	// PUT set → GET reflects the override.
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "imageBaseURL": "http://img.custom/p"}},
	}, http.StatusOK)
	if got := providerBySlug(getProviders(t, srv, token), "tmdb").ImageBaseURL; got != "http://img.custom/p" {
		t.Errorf("after set, tmdb imageBaseURL = %q, want the override", got)
	}

	// PUT reset ("") → back to the registry default.
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "imageBaseURL": ""}},
	}, http.StatusOK)
	if got := providerBySlug(getProviders(t, srv, token), "tmdb").ImageBaseURL; got != defaultImageHost {
		t.Errorf("after reset, tmdb imageBaseURL = %q, want registry default", got)
	}

	// Rejections: a malformed URL, and an image host on a provider that has none.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"malformed image url", map[string]any{
			"providers": []map[string]any{{"slug": "tmdb", "imageBaseURL": "not a url"}},
		}},
		{"image host on non-image provider", map[string]any{
			"providers": []map[string]any{{"slug": "musicbrainz", "imageBaseURL": "http://img.x/p"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			status, body := srv.JSON(http.MethodPut, providersPath, token, tc.body, &env)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", status, body)
			}
			if env.Error.Code != "PROVIDER_INVALID_BASE_URL" {
				t.Errorf("code = %q, want PROVIDER_INVALID_BASE_URL", env.Error.Code)
			}
		})
	}
}

// TestProvidersRuntimeHotSwap is the key test: a PUT that enables a previously-off
// kind makes the NEXT enrichment pass enrich it (not 'disabled'), proving the
// manager's rebuild+swap took effect at runtime WITHOUT reconstructing the app —
// all via the fake provider builder, zero network.
func TestProvidersRuntimeHotSwap(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t,
		testharness.WithProviderBuilder(fakeProviderBuilder()),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")

	// Boot state: nothing configured → video disabled. A manual pass records
	// 'disabled', makes no outbound call (ADR-0001), and never matches.
	if v := getProviders(t, srv, token); v.Enablement.Video {
		t.Fatalf("precondition: video enabled at boot, want disabled")
	}
	res := enrichLib(t, srv, token, libID, "")
	if res.Disabled == 0 || res.Matched != 0 {
		t.Fatalf("disabled-boot pass = %+v, want disabled>0 matched=0", res)
	}
	if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "disabled" {
		t.Fatalf("Dune status = %q, want disabled", s)
	}

	// Enable video at runtime via the settings API (persist → rebuild → hot-swap).
	putProviders(t, srv, token, map[string]any{
		"providers": []map[string]any{{"slug": "tmdb", "enabled": true, "apiKey": "runtime-key"}},
	}, http.StatusOK)
	if v := getProviders(t, srv, token); !v.Enablement.Video {
		t.Fatalf("after PUT: video still disabled, hot-swap did not take effect")
	}

	// The very next pass — no restart — enriches Dune through the rebuilt provider.
	res = enrichLib(t, srv, token, libID, "full")
	if res.Matched == 0 {
		t.Fatalf("post-enable pass = %+v, want matched>0", res)
	}
	d := getEnrichedDetail(t, srv, token, id)
	if d.EnrichmentStatus != "matched" || d.Overview == "" {
		t.Errorf("Dune after runtime enable = %+v, want matched with overview", d)
	}
}

// TestProviderTestConnection: the probe returns a clear ok/error result. A
// key-requiring provider with no key fails fast (ok:false) with NO outbound call.
func TestProviderTestConnection(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	var out struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	status, body := srv.JSON(http.MethodPost, providersPath+"/fanarttv/test", token, map[string]any{}, &out)
	if status != http.StatusOK {
		t.Fatalf("test = %d, want 200; body: %s", status, body)
	}
	if out.OK || out.Detail == "" {
		t.Errorf("test result = %+v, want ok:false with a detail (key required, none supplied)", out)
	}
}

// TestProvidersEnrichmentBehavior covers the three server-wide behavior knobs
// (enrichment-runtime-settings): GET returns the seeded boot values; PUT changes
// each (omit=unchanged); negatives are rejected 422 PROVIDER_INVALID_SETTING; the
// surface stays Admin-only; and toggling autoEnrichAfterScan off is reflected in the
// next GET view.
func TestProvidersEnrichmentBehavior(t *testing.T) {
	srv := testharness.New(t, testharness.WithProviderBuilder(fakeProviderBuilder()))
	token := adminToken(t, srv)

	// Boot values come from the harness config seed: auto-enrich off + 0 interval
	// (harness defaults), throttle at the 1s config default (1000ms).
	v := getProviders(t, srv, token)
	if v.AutoEnrichAfterScan || v.EnrichIntervalSeconds != 0 || v.MusicBrainzRateLimitMs != 1000 {
		t.Fatalf("boot behavior = auto %v/interval %d/rate %d, want false/0/1000",
			v.AutoEnrichAfterScan, v.EnrichIntervalSeconds, v.MusicBrainzRateLimitMs)
	}

	// PUT changes all three; the returned + re-fetched view reflect them.
	putProviders(t, srv, token, map[string]any{
		"autoEnrichAfterScan":    true,
		"enrichIntervalSeconds":  1800,
		"musicBrainzRateLimitMs": 0,
	}, http.StatusOK)
	v = getProviders(t, srv, token)
	if !v.AutoEnrichAfterScan || v.EnrichIntervalSeconds != 1800 || v.MusicBrainzRateLimitMs != 0 {
		t.Fatalf("after PUT = auto %v/interval %d/rate %d, want true/1800/0",
			v.AutoEnrichAfterScan, v.EnrichIntervalSeconds, v.MusicBrainzRateLimitMs)
	}

	// Omit=unchanged: change only the interval; auto + rate stay put.
	putProviders(t, srv, token, map[string]any{"enrichIntervalSeconds": 3600}, http.StatusOK)
	v = getProviders(t, srv, token)
	if !v.AutoEnrichAfterScan || v.EnrichIntervalSeconds != 3600 || v.MusicBrainzRateLimitMs != 0 {
		t.Errorf("after partial PUT = auto %v/interval %d/rate %d, want true/3600/0 (auto+rate unchanged)",
			v.AutoEnrichAfterScan, v.EnrichIntervalSeconds, v.MusicBrainzRateLimitMs)
	}

	// Toggling auto off is reflected in the GET view.
	putProviders(t, srv, token, map[string]any{"autoEnrichAfterScan": false}, http.StatusOK)
	if getProviders(t, srv, token).AutoEnrichAfterScan {
		t.Errorf("autoEnrichAfterScan still true after toggling off")
	}

	// Negatives are rejected 422 PROVIDER_INVALID_SETTING (settings unchanged).
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"negative interval", map[string]any{"enrichIntervalSeconds": -1}},
		{"negative rate", map[string]any{"musicBrainzRateLimitMs": -5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			status, body := srv.JSON(http.MethodPut, providersPath, token, tc.body, &env)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", status, body)
			}
			if env.Error.Code != "PROVIDER_INVALID_SETTING" {
				t.Errorf("code = %q, want PROVIDER_INVALID_SETTING", env.Error.Code)
			}
		})
	}

	// Admin-only: a Member is refused the PUT.
	srv.CreateMember("carol", "pw12345678")
	member := srv.LoginAs("carol", "pw12345678")
	if status, _ := srv.JSON(http.MethodPut, providersPath, member,
		map[string]any{"autoEnrichAfterScan": true}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403", status)
	}
}

// putProviders PUTs a settings body and asserts the expected status.
func putProviders(t *testing.T, srv *testharness.Server, token string, body map[string]any, wantStatus int) {
	t.Helper()
	status, raw := srv.JSON(http.MethodPut, providersPath, token, body, nil)
	if status != wantStatus {
		t.Fatalf("PUT providers = %d, want %d; body: %s", status, wantStatus, raw)
	}
}
