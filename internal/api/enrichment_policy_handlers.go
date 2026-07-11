package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/store"
)

// Admin-scope per-Library Enrichment policy (ADR-0027):
//
//	GET /libraries/{id}/enrichment-policy → the stored sparse overrides + the
//	    effective/inherited enablement for display
//	PUT /libraries/{id}/enrichment-policy → a partial update: set a key, or clear
//	    it back to inherit
//
// The policy is SPARSE (Model A): a key left unset inherits the global config
// live. This first slice carries the enrich-on/off key. The response reports both
// the stored override (so the UI shows inherited-vs-overridden) and the derived
// effective enablement (what the Library will actually enrich). Saving re-enriches
// the Library immediately (ReEnrichLibrary). Admin-only, enforced in the router.

// EnrichmentPolicyStore is the persistence the policy handlers read and write.
// *store.DB satisfies it; the narrow interface keeps the HTTP layer testable.
type EnrichmentPolicyStore interface {
	LibraryByID(id string) (store.Library, error)
	LibraryEnrichmentPolicy(libraryID string) (store.LibraryEnrichmentPolicy, error)
	SetLibraryEnrichEnabled(libraryID string, enabled *bool) error
	SetLibraryMetadataLanguage(libraryID string, language *string) error
	SetLibraryAuthoritativeProvider(libraryID string, slug *string) error
	SetLibraryProviderOverride(libraryID, provider string, enabled *bool) error
}

// EnrichmentPolicyResolver derives the per-Library enablement the policy view
// reports for display — the effective result under the Library's policy and the
// inherited (global) baseline the unset keys track. *enrich.Manager satisfies it;
// the interface keeps the handler unit-testable without a live provider Manager.
type EnrichmentPolicyResolver interface {
	EffectiveEnablement(ctx context.Context, libraryID string) (enrich.Enablement, error)
	GlobalEnablement() enrich.Enablement
	// GlobalMetadataLanguage is the server-wide preferred metadata language the
	// language key inherits when unset — reported so the UI can label the inherit
	// option with what "inherit" currently resolves to.
	GlobalMetadataLanguage() string
	// UsableFullProviders lists the Full providers of a coarse media kind currently
	// selectable as an Authoritative provider (keyed) — the dropdown candidate set.
	UsableFullProviders(kind string) []enrich.ProviderRef
	// EffectiveAuthoritative reports the slug currently leading a Library's
	// Enrichment for its kind, plus any fallback (a chosen-but-unreachable slug).
	EffectiveAuthoritative(ctx context.Context, libraryID, kind string) (slug, fallbackFrom string, err error)
	// SupplementProviders lists the togglable Supplements of a Library's kind (the
	// current Authoritative provider excluded), each with the global enabled state
	// its per-Library tri-state inherits when unset.
	SupplementProviders(ctx context.Context, libraryID, kind string) ([]enrich.SupplementRef, error)
}

// --- Wire shapes ------------------------------------------------------------

// enrichmentPolicyResponse is the GET/PUT body. enrichEnabled is the STORED
// override (null = inherit; the UI reads null-vs-value as inherited-vs-overridden).
// inheritedEnrichEnabled is what "inherit" currently resolves to (any kind enabled
// globally), so the UI can label the inherit option "(currently On/Off)". effective
// is the derived per-kind enablement the Library will actually enrich under this
// policy.
type enrichmentPolicyResponse struct {
	EnrichEnabled          *bool          `json:"enrichEnabled"`
	InheritedEnrichEnabled bool           `json:"inheritedEnrichEnabled"`
	Effective              enablementJSON `json:"effective"`
	// MetadataLanguage is the STORED language override (null = inherit; the UI reads
	// null-vs-value as inherited-vs-overridden). InheritedMetadataLanguage is the
	// global language the unset key tracks live, so the UI can label "Inherit
	// (currently en-US)" and prefill the field.
	MetadataLanguage          *string `json:"metadataLanguage"`
	InheritedMetadataLanguage string  `json:"inheritedMetadataLanguage"`

	// Authoritative-provider pointer (issue 03). AuthoritativeProvider is the STORED
	// override slug (null = inherit the kind default). InheritedAuthoritative is the
	// kind's global default (what "inherit" resolves to). EffectiveAuthoritative is
	// the provider actually leading under this policy. AuthoritativeUnreachable is set
	// (to the chosen-but-unreachable slug) when the pointer fell back to the default —
	// the Admin-facing attention signal (never silently dropped). Candidates is the
	// dropdown: the usable Full providers of the Library's kind.
	AuthoritativeProvider    *string           `json:"authoritativeProvider"`
	InheritedAuthoritative   providerRefJSON   `json:"inheritedAuthoritative"`
	EffectiveAuthoritative   providerRefJSON   `json:"effectiveAuthoritative"`
	AuthoritativeUnreachable *string           `json:"authoritativeUnreachable"`
	AuthoritativeCandidates  []providerRefJSON `json:"authoritativeCandidates"`

	// Supplements is the per-provider Supplement tri-state (issue 05): one entry per
	// togglable Supplement of the Library's kind (the current Authoritative provider
	// excluded), each carrying its STORED override (null = inherit) and the global
	// enabled state the unset key tracks live.
	Supplements []supplementJSON `json:"supplements"`
}

// providerRefJSON is a provider's stable slug + display name, for the authoritative
// candidate list and the inherited/effective pointers.
type providerRefJSON struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// supplementJSON is one per-Supplement tri-state control's data: the provider, its
// STORED override (null = inherit; true/false = forced on/off), and the global
// enabled state inheriting resolves to (for the "Inherit (currently On/Off)" label).
type supplementJSON struct {
	Slug             string `json:"slug"`
	Name             string `json:"name"`
	Override         *bool  `json:"override"`
	InheritedEnabled bool   `json:"inheritedEnabled"`
}

// patchBool is a JSON tri-state that distinguishes an OMITTED key (Present=false,
// leave unchanged) from an explicit null (Present=true, Value=nil, clear back to
// inherit) from a set value (Present=true, Value=&x). A plain *bool cannot: both an
// omitted key and a null decode to nil. UnmarshalJSON fires only when the key is
// present, so the zero value cleanly means "omitted".
type patchBool struct {
	Present bool
	Value   *bool
}

func (p *patchBool) UnmarshalJSON(data []byte) error {
	p.Present = true
	if string(data) == "null" {
		p.Value = nil
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	p.Value = &b
	return nil
}

// patchString is the string analogue of patchBool: it distinguishes an OMITTED
// key (Present=false, leave unchanged) from an explicit null (clear back to
// inherit) from a set value. Used for the metadata_language override.
type patchString struct {
	Present bool
	Value   *string
}

func (p *patchString) UnmarshalJSON(data []byte) error {
	p.Present = true
	if string(data) == "null" {
		p.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	p.Value = &s
	return nil
}

// updateEnrichmentPolicyRequest is the PUT body. Each key is a patch* tri-state so
// a client can set it, clear it to inherit (null), or omit it (unchanged) — the
// sparse-override contract. Later slices add the authoritative key.
type updateEnrichmentPolicyRequest struct {
	EnrichEnabled         patchBool   `json:"enrichEnabled"`
	MetadataLanguage      patchString `json:"metadataLanguage"`
	AuthoritativeProvider patchString `json:"authoritativeProvider"`
	// ProviderOverrides is the per-Supplement tri-state partial update: a slug mapped
	// to true/false forces it on/off; a slug mapped to null clears it back to inherit;
	// a slug ABSENT from the object is unchanged. (A JSON object distinguishes
	// present-null from absent-key, which a plain map value cannot — so the presence
	// of the key in the decoded map is the "touch this provider" signal.)
	ProviderOverrides map[string]*bool `json:"providerOverrides"`
}

// --- Handlers ---------------------------------------------------------------

// handleGetEnrichmentPolicy returns a Library's Enrichment policy + derived
// enablement. An unknown Library is 404 (hide-existence).
func handleGetEnrichmentPolicy(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrichment-policy")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		if _, ok := loadLibrary(w, deps, id); !ok {
			return
		}
		resp, err := buildEnrichmentPolicyResponse(r.Context(), deps, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read enrichment policy", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleUpdateEnrichmentPolicy applies a partial update to a Library's Enrichment
// policy (set a key or clear it to inherit), then re-enriches the Library
// immediately (ADR-0027) so the change is visible without waiting for a scan. It
// returns the fresh view (stored override + derived enablement).
func handleUpdateEnrichmentPolicy(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := pathParam(r.URL.Path, "/libraries/", "/enrichment-policy")
		if id == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		lib, ok := loadLibrary(w, deps, id)
		if !ok {
			return
		}
		var req updateEnrichmentPolicyRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.EnrichEnabled.Present {
			// Value nil ⇒ clear to inherit (stored NULL); non-nil ⇒ deliberate override.
			if err := deps.EnrichmentPolicy.SetLibraryEnrichEnabled(id, req.EnrichEnabled.Value); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save enrichment policy", nil)
				return
			}
		}
		if req.MetadataLanguage.Present {
			// A blank language is meaningless as an override (it can't localize anything)
			// and would blur "inherit" vs. "deliberately none" — normalize it to a clear.
			lang := req.MetadataLanguage.Value
			if lang != nil && strings.TrimSpace(*lang) == "" {
				lang = nil
			}
			if err := deps.EnrichmentPolicy.SetLibraryMetadataLanguage(id, lang); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save enrichment policy", nil)
				return
			}
		}
		if req.AuthoritativeProvider.Present {
			// A blank slug clears the pointer back to inherit the kind default.
			slug := req.AuthoritativeProvider.Value
			if slug != nil && strings.TrimSpace(*slug) == "" {
				slug = nil
			}
			// Constrain the pointer's domain to USABLE Full providers of the Library's
			// kind (ADR-0027): reject an unknown, artwork-only, wrong-kind, or unkeyed
			// slug so a Library is never pointed at a source that can't lead.
			if slug != nil && !authoritativeSelectable(deps, lib.Kind, *slug) {
				writeError(w, http.StatusUnprocessableEntity, codeProviderNotAuthoritative,
					"provider is not a usable authoritative for this library", nil)
				return
			}
			if err := deps.EnrichmentPolicy.SetLibraryAuthoritativeProvider(id, slug); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save enrichment policy", nil)
				return
			}
		}
		// Validate every override slug BEFORE writing any, so one bad slug never leaves
		// a partially-applied update (map iteration order is unspecified).
		for slug := range req.ProviderOverrides {
			if !supplementSelectable(lib.Kind, slug) {
				writeError(w, http.StatusUnprocessableEntity, codeProviderNotAuthoritative,
					"provider is not a supplement for this library", nil)
				return
			}
		}
		for slug, enabled := range req.ProviderOverrides {
			// enabled nil ⇒ clear to inherit (row deleted); non-nil ⇒ forced on/off.
			if err := deps.EnrichmentPolicy.SetLibraryProviderOverride(id, slug, enabled); err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to save enrichment policy", nil)
				return
			}
		}
		// Re-enrich the Library immediately: the app trigger invalidates the Library's
		// cached effective provider and kicks a background full pass (emitting the usual
		// enrichProgress SSE). Nil-safe for unit tests without the app wiring.
		if deps.ReEnrichLibrary != nil {
			deps.ReEnrichLibrary(id)
		}
		resp, err := buildEnrichmentPolicyResponse(r.Context(), deps, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to read enrichment policy", nil)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// loadLibrary fetches the Library, writing a 404 (hide-existence) and returning
// ok=false when it does not exist (a read error is a 500). Shared by the GET/PUT
// handlers, which need the Library's kind to scope the authoritative candidates.
func loadLibrary(w http.ResponseWriter, deps Deps, id string) (store.Library, bool) {
	lib, err := deps.EnrichmentPolicy.LibraryByID(id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return store.Library{}, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read library", nil)
		return store.Library{}, false
	}
	return lib, true
}

// coarseKind maps a Library's fine kind (movie/tv/music) onto the coarse
// Enrichment kind the registry groups by (KindVideo / KindMusic).
func coarseKind(libraryKind string) string {
	if libraryKind == "music" {
		return enrich.KindMusic
	}
	return enrich.KindVideo
}

// authoritativeSelectable reports whether a slug is a USABLE Authoritative provider
// for a Library of the given kind: it must be one of the resolver's usable Full
// providers (keyed) for the coarse kind. This is the write-side guard mirroring the
// dropdown's read-side candidate list (ADR-0027).
func authoritativeSelectable(deps Deps, libraryKind, slug string) bool {
	if deps.PolicyResolver == nil {
		return false
	}
	for _, c := range deps.PolicyResolver.UsableFullProviders(coarseKind(libraryKind)) {
		if c.Slug == slug {
			return true
		}
	}
	return false
}

// supplementSelectable reports whether a slug is a togglable Supplement of a
// Library of the given kind — a key-bearing provider of the coarse kind (the
// write-side guard mirroring the per-Supplement control list, ADR-0027).
func supplementSelectable(libraryKind, slug string) bool {
	for _, e := range enrich.SupplementProvidersForKind(coarseKind(libraryKind)) {
		if e.Slug == slug {
			return true
		}
	}
	return false
}

// providerRef resolves a slug to its display-name pair for the view (empty name for
// an unknown slug — defensive; the resolver only ever emits registered slugs).
func providerRef(slug string) providerRefJSON {
	if slug == "" {
		return providerRefJSON{}
	}
	e, _ := enrich.RegistryEntryFor(slug)
	return providerRefJSON{Slug: slug, Name: e.Name}
}

// buildEnrichmentPolicyResponse assembles the view: the stored overrides (read
// fresh so a just-applied change is reflected) plus the derived effective/inherited
// enablement, the effective + inherited Authoritative provider, any fallback, and
// the authoritative candidate list. The resolver may be nil in a unit test that
// only exercises persistence — the derived fields then stay zero.
func buildEnrichmentPolicyResponse(ctx context.Context, deps Deps, id string) (enrichmentPolicyResponse, error) {
	policy, err := deps.EnrichmentPolicy.LibraryEnrichmentPolicy(id)
	if err != nil {
		return enrichmentPolicyResponse{}, err
	}
	lib, err := deps.EnrichmentPolicy.LibraryByID(id)
	if err != nil {
		return enrichmentPolicyResponse{}, err
	}
	kind := coarseKind(lib.Kind)
	resp := enrichmentPolicyResponse{
		EnrichEnabled:          policy.EnrichEnabled,
		MetadataLanguage:       policy.MetadataLanguage,
		AuthoritativeProvider:  policy.AuthoritativeProvider,
		InheritedAuthoritative: providerRef(enrich.DefaultAuthoritativeForKind(kind)),
	}
	if deps.PolicyResolver != nil {
		eff, err := deps.PolicyResolver.EffectiveEnablement(ctx, id)
		if err != nil {
			return enrichmentPolicyResponse{}, err
		}
		global := deps.PolicyResolver.GlobalEnablement()
		resp.Effective = enablementJSON{Video: eff.Video, Music: eff.Music}
		// "Enrich this library" resolves on/off; inheriting it means "enrich iff the
		// server enriches any kind" (the global baseline the unset key tracks live).
		resp.InheritedEnrichEnabled = global.Video || global.Music
		resp.InheritedMetadataLanguage = deps.PolicyResolver.GlobalMetadataLanguage()

		effSlug, fallbackFrom, err := deps.PolicyResolver.EffectiveAuthoritative(ctx, id, kind)
		if err != nil {
			return enrichmentPolicyResponse{}, err
		}
		resp.EffectiveAuthoritative = providerRef(effSlug)
		if fallbackFrom != "" {
			resp.AuthoritativeUnreachable = &fallbackFrom
		}
		// Always a JSON array (never null) so the UI can .map it unconditionally.
		resp.AuthoritativeCandidates = []providerRefJSON{}
		for _, c := range deps.PolicyResolver.UsableFullProviders(kind) {
			resp.AuthoritativeCandidates = append(resp.AuthoritativeCandidates, providerRefJSON{Slug: c.Slug, Name: c.Name})
		}

		// Per-Supplement tri-state: one entry per togglable Supplement of the kind,
		// merging the resolver's global-enabled baseline with the stored override.
		supplements, err := deps.PolicyResolver.SupplementProviders(ctx, id, kind)
		if err != nil {
			return enrichmentPolicyResponse{}, err
		}
		resp.Supplements = []supplementJSON{}
		for _, s := range supplements {
			var override *bool
			if v, ok := policy.SupplementOverrides[s.Slug]; ok {
				override = &v
			}
			resp.Supplements = append(resp.Supplements, supplementJSON{
				Slug: s.Slug, Name: s.Name, Override: override, InheritedEnabled: s.InheritedEnabled,
			})
		}
	}
	return resp, nil
}
