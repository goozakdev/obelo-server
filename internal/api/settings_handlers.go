package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/store"
)

// Admin-scope metadata-provider settings (metadata-providers 02). An Admin views
// the static provider registry joined with the current DB settings, saves a
// partial update (which rebuilds + hot-swaps the running provider with no
// restart), and probes a provider's connectivity. Secrets are NEVER returned —
// only a hasKey boolean. Every route is Admin-only (wired behind requireAuth +
// requireAdmin in api.go).

// ProviderSettingsStore is the persistence the settings handlers read and write.
// *store.DB satisfies it; the narrow interface keeps the HTTP layer testable.
type ProviderSettingsStore interface {
	MetadataProviders() ([]store.MetadataProviderRow, error)
	UpsertMetadataProvider(u store.MetadataProviderUpsert) error
	MetadataLanguage() (string, error)
	SetMetadataLanguage(language string) error
	// Behavior knobs (enrichment-runtime-settings): the three server-wide Enrichment
	// behavior settings, read for the GET/PUT view + the MusicBrainz throttle that
	// feeds the provider rebuild, and written by the PUT after resolving partial
	// updates against the current values.
	EnrichmentBehavior() (store.EnrichmentBehavior, error)
	SetEnrichmentBehavior(autoEnrichAfterScan bool, enrichIntervalSeconds, musicBrainzRateLimitMs int) error
}

// --- Wire shapes ------------------------------------------------------------

// providerJSON is one provider in the GET/PUT response: the registry facts joined
// with the current settings. hasKey reports whether a key is on file WITHOUT ever
// exposing it; baseURL is the effective host (override or registry default).
type providerJSON struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Kinds       []string `json:"kinds"`
	Role        string   `json:"role"`
	RequiresKey bool     `json:"requiresKey"`
	Enabled     bool     `json:"enabled"`
	HasKey      bool     `json:"hasKey"`
	BaseURL     string   `json:"baseURL"`
	// ImageBaseURL is the effective artwork host for the few sources that serve
	// images from a host distinct from their API (today only TMDB). Omitted for
	// providers with no image host, so the UI shows the extra override only where
	// it applies.
	ImageBaseURL string `json:"imageBaseURL,omitempty"`
	Description  string `json:"description"`
	DocsURL      string `json:"docsURL"`
}

// enablementJSON is the derived per-kind enablement summary — what the running
// server will actually enrich given the current settings.
type enablementJSON struct {
	Video bool `json:"video"`
	Music bool `json:"music"`
}

// providersResponse is the GET/PUT body: the joined provider list, the global
// language, and the derived per-kind enablement summary.
type providersResponse struct {
	Providers        []providerJSON `json:"providers"`
	MetadataLanguage string         `json:"metadataLanguage"`
	Enablement       enablementJSON `json:"enablement"`
	// Server-wide Enrichment behavior knobs (enrichment-runtime-settings), flat
	// siblings of metadataLanguage. autoEnrichAfterScan toggles the post-scan
	// background pass; enrichIntervalSeconds is the scheduled-sweep cadence (0 =
	// disabled); musicBrainzRateLimitMs is the MusicBrainz throttle (0 = no throttle).
	AutoEnrichAfterScan    bool `json:"autoEnrichAfterScan"`
	EnrichIntervalSeconds  int  `json:"enrichIntervalSeconds"`
	MusicBrainzRateLimitMs int  `json:"musicBrainzRateLimitMs"`
}

// providerUpdateJSON is one provider's partial update in the PUT body. Every
// field is a pointer so "omitted" (nil) is distinguishable from an explicit
// value — the secret semantics depend on it: APIKey omitted = unchanged, "" =
// clear, non-empty = set. Enabled/BaseURL follow the same omit=unchanged rule
// (BaseURL "" resets to the registry default).
type providerUpdateJSON struct {
	Slug         string  `json:"slug"`
	Enabled      *bool   `json:"enabled,omitempty"`
	APIKey       *string `json:"apiKey,omitempty"`
	BaseURL      *string `json:"baseURL,omitempty"`
	ImageBaseURL *string `json:"imageBaseURL,omitempty"`
}

// updateProvidersRequest is the PUT body: a set of per-provider partial updates
// plus an optional global metadata language (omitted = unchanged; "" is rejected).
type updateProvidersRequest struct {
	Providers        []providerUpdateJSON `json:"providers,omitempty"`
	MetadataLanguage *string              `json:"metadataLanguage,omitempty"`
	// Behavior knobs (enrichment-runtime-settings): each an OPTIONAL pointer so
	// omitted = unchanged. enrichIntervalSeconds / musicBrainzRateLimitMs must be
	// >= 0 (0 is the meaningful "disabled" value); a negative is rejected 422.
	AutoEnrichAfterScan    *bool `json:"autoEnrichAfterScan,omitempty"`
	EnrichIntervalSeconds  *int  `json:"enrichIntervalSeconds,omitempty"`
	MusicBrainzRateLimitMs *int  `json:"musicBrainzRateLimitMs,omitempty"`
}

// testProviderResponse is the POST .../{slug}/test body: a best-effort probe result.
type testProviderResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// testProviderRequest optionally carries edited credentials to probe with before
// they are saved (so an Admin can validate a key they just typed). Omitted fields
// fall back to what is on file.
type testProviderRequest struct {
	APIKey  *string `json:"apiKey,omitempty"`
	BaseURL *string `json:"baseURL,omitempty"`
}

// --- Routing ----------------------------------------------------------------

// handleSettingsSubtree dispatches the /settings/ subtree:
//
//	GET  /settings/metadata-providers            → registry + settings view
//	PUT  /settings/metadata-providers            → partial update, rebuild+swap
//	POST /settings/metadata-providers/{slug}/test → connectivity/credential probe
//
// Method + sub-resource are dispatched here (mirroring the /users subtree) because
// the subtree serves more than one method and a nested {slug}/test leaf.
func handleSettingsSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/settings/")

		// Subtitle-provider settings (ADR-0021), the exact shape of the
		// metadata-provider subtree below. Branched first so its /test and collection
		// routes aren't swallowed by the metadata handling.
		if rest == "subtitle-providers" || strings.HasPrefix(rest, "subtitle-providers/") {
			handleSubtitleSettingsSubtree(deps, rest)(w, r)
			return
		}

		// POST /settings/metadata-providers/{slug}/test
		if slug, ok := strings.CutSuffix(rest, "/test"); ok {
			slug = strings.TrimPrefix(slug, "metadata-providers/")
			if slug == "" || strings.Contains(slug, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost, handleTestProvider(deps, slug))(w, r)
			return
		}

		if rest != "metadata-providers" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleGetProviders(deps)(w, r)
		case http.MethodPut:
			handleUpdateProviders(deps)(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// --- GET --------------------------------------------------------------------

func handleGetProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := buildProvidersResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read provider settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- PUT --------------------------------------------------------------------

func handleUpdateProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req updateProvidersRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// Load the current state so partial updates (omit=unchanged) resolve against
		// what is on file.
		rows, err := deps.Providers.MetadataProviders()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read provider settings", nil)
			return
		}
		current := make(map[string]store.MetadataProviderRow, len(rows))
		for _, row := range rows {
			current[row.Slug] = row
		}

		// Resolve + validate every provider update into a full desired state BEFORE
		// persisting anything, so a rejected update leaves the settings unchanged.
		var upserts []store.MetadataProviderUpsert
		for _, u := range req.Providers {
			entry, ok := enrich.RegistryEntryFor(u.Slug)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown,
					"unknown provider: "+u.Slug, nil)
				return
			}
			cur := current[u.Slug] // zero value (disabled, no key) when absent

			desired := store.MetadataProviderUpsert{
				Slug:         u.Slug,
				Enabled:      cur.Enabled,
				APIKey:       cur.APIKey,
				BaseURL:      cur.BaseURL,
				ImageBaseURL: cur.ImageBaseURL,
			}
			if u.Enabled != nil {
				desired.Enabled = *u.Enabled
			}
			if u.APIKey != nil { // omitted = unchanged; "" = clear; value = set
				desired.APIKey = *u.APIKey
			}
			if u.BaseURL != nil { // omitted = unchanged; "" = reset to default; value = set
				desired.BaseURL = strings.TrimSpace(*u.BaseURL)
				if desired.BaseURL != "" && !validBaseURL(desired.BaseURL) {
					writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidBaseURL,
						"base URL must be an absolute http(s) URL", nil)
					return
				}
			}
			if u.ImageBaseURL != nil { // omitted = unchanged; "" = reset to default; value = set
				// Only providers that actually serve artwork from a distinct host have an
				// image host to override; reject the field for any other provider.
				if entry.DefaultImageBaseURL == "" && strings.TrimSpace(*u.ImageBaseURL) != "" {
					writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidBaseURL,
						entry.Name+" has no configurable image host", nil)
					return
				}
				desired.ImageBaseURL = strings.TrimSpace(*u.ImageBaseURL)
				if desired.ImageBaseURL != "" && !validBaseURL(desired.ImageBaseURL) {
					writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidBaseURL,
						"image base URL must be an absolute http(s) URL", nil)
					return
				}
			}
			// A key-requiring provider cannot be enabled with no key on file and none
			// supplied (ADR-0001: enabling is explicit and must be usable).
			if desired.Enabled && entry.RequiresKey && desired.APIKey == "" {
				writeError(w, http.StatusUnprocessableEntity, codeProviderKeyRequired,
					entry.Name+" requires an API key to be enabled", nil)
				return
			}
			upserts = append(upserts, desired)
		}

		if req.MetadataLanguage != nil && strings.TrimSpace(*req.MetadataLanguage) == "" {
			writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidLanguage,
				"metadataLanguage must not be empty", nil)
			return
		}

		// Behavior knobs (enrichment-runtime-settings): validate negatives, then
		// resolve each optional field against the current DB value (omit=unchanged)
		// into the full concrete triple the store writer persists.
		if req.EnrichIntervalSeconds != nil && *req.EnrichIntervalSeconds < 0 {
			writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidSetting,
				"enrichIntervalSeconds must be >= 0", nil)
			return
		}
		if req.MusicBrainzRateLimitMs != nil && *req.MusicBrainzRateLimitMs < 0 {
			writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidSetting,
				"musicBrainzRateLimitMs must be >= 0", nil)
			return
		}
		behaviorChanged := req.AutoEnrichAfterScan != nil || req.EnrichIntervalSeconds != nil || req.MusicBrainzRateLimitMs != nil
		var desiredBehavior store.EnrichmentBehavior
		if behaviorChanged {
			cur, err := deps.Providers.EnrichmentBehavior()
			if err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to read provider settings", nil)
				return
			}
			auto := cur.Auto()
			interval := cur.IntervalSeconds()
			rate := cur.RateLimitMs()
			if req.AutoEnrichAfterScan != nil {
				auto = *req.AutoEnrichAfterScan
			}
			if req.EnrichIntervalSeconds != nil {
				interval = *req.EnrichIntervalSeconds
			}
			if req.MusicBrainzRateLimitMs != nil {
				rate = *req.MusicBrainzRateLimitMs
			}
			desiredBehavior = store.EnrichmentBehavior{
				AutoEnrichAfterScan: &auto, EnrichIntervalSeconds: &interval, MusicBrainzRateLimitMs: &rate,
			}
		}

		// Persist the validated changes, then rebuild + hot-swap the running provider.
		for _, u := range upserts {
			if err := deps.Providers.UpsertMetadataProvider(u); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to save provider settings", nil)
				return
			}
		}
		if req.MetadataLanguage != nil {
			if err := deps.Providers.SetMetadataLanguage(strings.TrimSpace(*req.MetadataLanguage)); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to save provider settings", nil)
				return
			}
		}
		if behaviorChanged {
			if err := deps.Providers.SetEnrichmentBehavior(
				*desiredBehavior.AutoEnrichAfterScan,
				*desiredBehavior.EnrichIntervalSeconds,
				*desiredBehavior.MusicBrainzRateLimitMs,
			); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to save provider settings", nil)
				return
			}
		}
		if deps.ProviderManager != nil {
			// Reload rebuilds the provider, picking up a changed MusicBrainz throttle.
			if err := deps.ProviderManager.Reload(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal,
					"failed to apply provider settings", nil)
				return
			}
		}
		// Wake the scheduled-enrich goroutine so a changed EnrichInterval applies
		// promptly (nil-safe in unit tests without the app wiring).
		if deps.SettingsChanged != nil {
			deps.SettingsChanged()
		}

		resp, err := buildProvidersResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read provider settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- POST {slug}/test -------------------------------------------------------

func handleTestProvider(deps Deps, slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, ok := enrich.RegistryEntryFor(slug)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown,
				"unknown provider: "+slug, nil)
			return
		}

		// The test may carry edited creds (not yet saved); fall back to what's on file.
		var req testProviderRequest
		if r.ContentLength != 0 {
			if !decodeJSON(w, r, &req) {
				return
			}
		}
		rows, err := deps.Providers.MetadataProviders()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read provider settings", nil)
			return
		}
		lang, err := deps.Providers.MetadataLanguage()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to read provider settings", nil)
			return
		}
		var cur store.MetadataProviderRow
		for _, row := range rows {
			if row.Slug == slug {
				cur = row
				break
			}
		}
		apiKey, baseURL := cur.APIKey, cur.BaseURL
		if req.APIKey != nil {
			apiKey = *req.APIKey
		}
		if req.BaseURL != nil {
			baseURL = strings.TrimSpace(*req.BaseURL)
		}
		_ = entry // registry entry validated the slug; TestConnection re-reads it

		// Bound the probe so a hung host can't stall the request; failure is a clean
		// {ok:false}, never a 500.
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		ok2, detail := enrich.TestConnection(ctx, slug, apiKey, baseURL, lang)
		writeJSON(w, http.StatusOK, testProviderResponse{OK: ok2, Detail: detail})
	}
}

// --- Shared view builder ----------------------------------------------------

// buildProvidersResponse joins the static registry with the current DB settings
// into the masked GET/PUT response, and derives the per-kind enablement summary
// the same way the running Service does (via the builder), so the screen reflects
// what enrichment will actually do.
func buildProvidersResponse(deps Deps) (providersResponse, error) {
	rows, err := deps.Providers.MetadataProviders()
	if err != nil {
		return providersResponse{}, err
	}
	lang, err := deps.Providers.MetadataLanguage()
	if err != nil {
		return providersResponse{}, err
	}
	behavior, err := deps.Providers.EnrichmentBehavior()
	if err != nil {
		return providersResponse{}, err
	}
	bySlug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, row := range rows {
		bySlug[row.Slug] = row
	}

	out := providersResponse{
		MetadataLanguage:       lang,
		AutoEnrichAfterScan:    behavior.Auto(),
		EnrichIntervalSeconds:  behavior.IntervalSeconds(),
		MusicBrainzRateLimitMs: behavior.RateLimitMs(),
	}
	for _, e := range enrich.Registry() {
		row, has := bySlug[e.Slug]
		baseURL := e.DefaultBaseURL
		if has && row.BaseURL != "" {
			baseURL = row.BaseURL
		}
		// The image host is emitted only for providers that have one (via omitempty),
		// so the UI renders the extra override exactly where it applies.
		imageBaseURL := e.DefaultImageBaseURL
		if has && row.ImageBaseURL != "" {
			imageBaseURL = row.ImageBaseURL
		}
		out.Providers = append(out.Providers, providerJSON{
			Slug:         e.Slug,
			Name:         e.Name,
			Kinds:        e.Kinds,
			Role:         string(e.Role),
			RequiresKey:  e.RequiresKey,
			Enabled:      has && row.Enabled,
			HasKey:       has && row.APIKey != "",
			BaseURL:      baseURL,
			ImageBaseURL: imageBaseURL,
			Description:  e.Description,
			DocsURL:      e.DocsURL,
		})
	}

	// Derive the enablement summary through the same mapping the manager uses, so
	// the API view matches the running server exactly — including the DB-sourced
	// MusicBrainz throttle (read above), so the composed ProviderConfig is identical
	// to what Manager.Reload builds.
	fixed := enrich.FixedProviderInputs{
		MusicBrainzRateLimit: time.Duration(behavior.RateLimitMs()) * time.Millisecond,
	}
	cfg := enrich.SettingsToProviderConfig(rows, lang, fixed)
	enablement := enrich.DeriveEnablement(cfg)
	out.Enablement = enablementJSON{Video: enablement.Video, Music: enablement.Music}
	return out, nil
}

// validBaseURL reports whether s is a well-formed absolute http(s) URL — the
// shape a base-URL override must take (a mirror or a local test stub).
func validBaseURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
