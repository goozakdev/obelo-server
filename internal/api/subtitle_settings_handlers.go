package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/subfetch"
	"github.com/goozakdev/obelo-server/internal/subtitle"
)

// Admin-scope subtitle-provider settings (ADR-0021), the exact shape of the
// metadata-provider settings surface: an Admin views the static provider registry
// joined with the current DB settings, saves a partial update (which rebuilds +
// hot-swaps the running provider with no restart), and probes connectivity. The
// key is NEVER returned — only a hasKey boolean. Every route is Admin-only (wired
// behind requireAuth + requireAdmin via the /settings/ subtree in api.go).

// SubtitleProviderSettingsStore is the persistence the subtitle-settings handlers
// read and write. *store.DB satisfies it; the narrow interface keeps the HTTP
// layer testable.
type SubtitleProviderSettingsStore interface {
	SubtitleProviders() ([]store.SubtitleProviderRow, error)
	UpsertSubtitleProvider(u store.SubtitleProviderUpsert) error
	SubtitleAutoFetchLang() (string, error)
	SetSubtitleAutoFetchLang(lang string) error
}

// --- Wire shapes ------------------------------------------------------------

// subtitleProviderJSON is one provider in the GET/PUT response: the registry facts
// joined with the current settings. hasKey reports whether a key is on file WITHOUT
// exposing it; baseURL is the effective host (override or registry default).
type subtitleProviderJSON struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	RequiresKey bool   `json:"requiresKey"`
	Enabled     bool   `json:"enabled"`
	HasKey      bool   `json:"hasKey"`
	BaseURL     string `json:"baseURL"`
	Description string `json:"description"`
	DocsURL     string `json:"docsURL"`

	// The Installed-plugin fields, identical in meaning to the ones the event-sink
	// screen carries (issue 09). They are omitempty, so a Built-in's entry is the
	// document it always was and a client that has not heard of Installed plugins
	// reads this response unchanged.
	//
	// Installed reports that this provider is a module an Admin placed under
	// <dataDir>/plugins/, not code this server shipped.
	Installed bool `json:"installed,omitempty"`
	// Disabled is an Installed plugin this server will not call: refused at load,
	// or stopped after failing repeatedly. It is NOT the Admin's enabled toggle —
	// a provider can be switched on and still be disabled, which is exactly the
	// state that needs explaining.
	Disabled bool `json:"disabled,omitempty"`
	// LastError is the sentence that says why. A download refused for exceeding
	// the byte cap lands here, which is where an Admin finds out that a Plugin
	// answered with more than this server accepts.
	LastError string `json:"lastError,omitempty"`
	// Version is the author's own version of their Plugin, for an operator to read.
	Version string `json:"version,omitempty"`
}

// subtitleProvidersResponse is the GET/PUT body: the joined provider list plus the
// auto-fetch-after-scan language ("" = off).
type subtitleProvidersResponse struct {
	Providers     []subtitleProviderJSON `json:"providers"`
	AutoFetchLang string                 `json:"autoFetchLang"`
}

// subtitleProviderUpdateJSON is one provider's partial update in the PUT body.
// Every field is a pointer so omitted (nil) is distinguishable from an explicit
// value — the secret semantics depend on it: apiKey omitted = unchanged, "" =
// clear, non-empty = set. enabled/baseURL follow the same omit=unchanged rule.
type subtitleProviderUpdateJSON struct {
	Slug    string  `json:"slug"`
	Enabled *bool   `json:"enabled,omitempty"`
	APIKey  *string `json:"apiKey,omitempty"`
	BaseURL *string `json:"baseURL,omitempty"`
}

// updateSubtitleProvidersRequest is the PUT body: per-provider partial updates plus
// an optional auto-fetch language (omitted = unchanged; "" turns auto-fetch off).
type updateSubtitleProvidersRequest struct {
	Providers     []subtitleProviderUpdateJSON `json:"providers,omitempty"`
	AutoFetchLang *string                      `json:"autoFetchLang,omitempty"`
}

// subtitleTestResponse is the POST .../{slug}/test body: a best-effort probe result.
type subtitleTestResponse struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// subtitleTestRequest optionally carries edited credentials to probe with before
// they are saved. Omitted fields fall back to what is on file.
type subtitleTestRequest struct {
	APIKey  *string `json:"apiKey,omitempty"`
	BaseURL *string `json:"baseURL,omitempty"`
}

// --- Routing ----------------------------------------------------------------

// handleSubtitleSettingsSubtree dispatches the subtitle-providers routes off the
// /settings/ subtree (already behind requireAuth + requireAdmin):
//
//	GET  /settings/subtitle-providers             → registry + settings view
//	PUT  /settings/subtitle-providers             → partial update, rebuild+swap
//	POST /settings/subtitle-providers/{slug}/test → connectivity/credential probe
func handleSubtitleSettingsSubtree(deps Deps, rest string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if slug, ok := strings.CutSuffix(rest, "/test"); ok {
			slug = strings.TrimPrefix(slug, "subtitle-providers/")
			if slug == "" || strings.Contains(slug, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost, handleTestSubtitleProvider(deps, slug))(w, r)
			return
		}
		if rest != "subtitle-providers" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleGetSubtitleProviders(deps)(w, r)
		case http.MethodPut:
			handleUpdateSubtitleProviders(deps)(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "method not allowed", nil)
		}
	}
}

// --- GET --------------------------------------------------------------------

func handleGetSubtitleProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := buildSubtitleProvidersResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read subtitle provider settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- PUT --------------------------------------------------------------------

func handleUpdateSubtitleProviders(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req updateSubtitleProvidersRequest
		if !decodeJSON(w, r, &req) {
			return
		}

		// Resolve each partial update against the current row, validating before any
		// write (all-or-nothing on validation, mirroring the metadata settings PUT).
		current, err := currentSubtitleRows(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read subtitle provider settings", nil)
			return
		}
		var upserts []store.SubtitleProviderUpsert
		for _, u := range req.Providers {
			registration, ok := deps.Plugins.SubtitleProvider(u.Slug)
			if !ok {
				writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown, "unknown subtitle provider: "+u.Slug, nil)
				return
			}
			row := current[u.Slug] // zero value = never configured
			desired := store.SubtitleProviderUpsert{
				Slug:    u.Slug,
				Enabled: row.Enabled,
				APIKey:  row.APIKey,
				BaseURL: row.BaseURL,
			}
			if u.Enabled != nil {
				desired.Enabled = *u.Enabled
			}
			if u.APIKey != nil {
				desired.APIKey = strings.TrimSpace(*u.APIKey)
			}
			if u.BaseURL != nil {
				desired.BaseURL = strings.TrimSpace(*u.BaseURL)
			}
			// A key-requiring provider can't be enabled with no key on file.
			if desired.Enabled && registration.Descriptor.RequiresKey && desired.APIKey == "" {
				writeError(w, http.StatusUnprocessableEntity, codeProviderKeyRequired,
					"an API key is required to enable "+registration.Descriptor.Name, nil)
				return
			}
			upserts = append(upserts, desired)
		}

		var autoLang *string
		if req.AutoFetchLang != nil {
			lang := strings.TrimSpace(*req.AutoFetchLang)
			if lang != "" {
				if norm := subtitle.NormalizeLang(lang); norm == "" {
					writeError(w, http.StatusUnprocessableEntity, codeProviderInvalidLanguage,
						"unrecognized auto-fetch language: "+lang, nil)
					return
				} else {
					lang = norm
				}
			}
			autoLang = &lang
		}

		for _, u := range upserts {
			if err := deps.SubtitleProviders.UpsertSubtitleProvider(u); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save subtitle provider settings", nil)
				return
			}
		}
		if autoLang != nil {
			if err := deps.SubtitleProviders.SetSubtitleAutoFetchLang(*autoLang); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save subtitle provider settings", nil)
				return
			}
		}

		// Hot-swap the running provider so an enable/key change takes effect with no
		// restart (mirrors the metadata PUT calling ProviderManager.Reload).
		if deps.SubtitleProviderManager != nil {
			if err := deps.SubtitleProviderManager.Reload(r.Context()); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to apply subtitle provider settings", nil)
				return
			}
		}

		resp, err := buildSubtitleProvidersResponse(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read subtitle provider settings", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- POST test --------------------------------------------------------------

func handleTestSubtitleProvider(deps Deps, slug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		registration, ok := deps.Plugins.SubtitleProvider(slug)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, codeProviderUnknown, "unknown subtitle provider: "+slug, nil)
			return
		}
		var req subtitleTestRequest
		if r.ContentLength != 0 {
			if !decodeJSON(w, r, &req) {
				return
			}
		}

		current, err := currentSubtitleRows(deps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read subtitle provider settings", nil)
			return
		}
		row := current[slug]
		apiKey, baseURL := row.APIKey, row.BaseURL
		if req.APIKey != nil {
			apiKey = strings.TrimSpace(*req.APIKey)
		}
		if req.BaseURL != nil {
			baseURL = strings.TrimSpace(*req.BaseURL)
		}
		if baseURL == "" {
			baseURL = registration.Descriptor.DefaultURL
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		okProbe, detail := subfetch.TestConnection(ctx, deps.Plugins, slug, apiKey, baseURL)
		writeJSON(w, http.StatusOK, subtitleTestResponse{OK: okProbe, Detail: detail})
	}
}

// --- helpers ----------------------------------------------------------------

// currentSubtitleRows reads the persisted provider rows into a slug-keyed map (a
// missing provider = the zero row = never configured).
func currentSubtitleRows(deps Deps) (map[string]store.SubtitleProviderRow, error) {
	rows, err := deps.SubtitleProviders.SubtitleProviders()
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.SubtitleProviderRow, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out, nil
}

// buildSubtitleProvidersResponse joins the registered Subtitle provider Plugins
// with the DB rows, masking the key to a hasKey boolean and resolving the effective
// base URL. The list is in registration order, which is what keeps the screen
// deterministic now that the catalog is a value the composition root builds rather
// than a package-level slice (ADR-0057 decision 5).
func buildSubtitleProvidersResponse(deps Deps) (subtitleProvidersResponse, error) {
	rows, err := currentSubtitleRows(deps)
	if err != nil {
		return subtitleProvidersResponse{}, err
	}
	autoLang, err := deps.SubtitleProviders.SubtitleAutoFetchLang()
	if err != nil {
		return subtitleProvidersResponse{}, err
	}

	var providers []subtitleProviderJSON
	for _, registration := range deps.Plugins.SubtitleProviders() {
		d := registration.Descriptor
		row := rows[d.Slug]
		base := row.BaseURL
		if base == "" {
			base = d.DefaultURL
		}
		// An Installed plugin's runtime state, joined on by slug exactly as the
		// event-sink screen joins it. A slug the loader has never heard of is a
		// Built-in and reads as all-zero, which is the document this response
		// always was.
		var (
			installed plugins.Status
			known     bool
		)
		if deps.InstalledPlugins != nil {
			installed, known = deps.InstalledPlugins.Status(d.Slug)
		}
		providers = append(providers, subtitleProviderJSON{
			Slug:        d.Slug,
			Name:        d.Name,
			RequiresKey: d.RequiresKey,
			Enabled:     row.Enabled,
			HasKey:      row.APIKey != "",
			BaseURL:     base,
			Description: d.Description,
			DocsURL:     d.DocsURL,
			Installed:   known,
			Disabled:    installed.Disabled,
			LastError:   installed.LastError,
			Version:     installed.Version,
		})
	}
	return subtitleProvidersResponse{Providers: providers, AutoFetchLang: autoLang}, nil
}
