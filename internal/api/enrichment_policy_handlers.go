package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

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
}

// EnrichmentPolicyResolver derives the per-Library enablement the policy view
// reports for display — the effective result under the Library's policy and the
// inherited (global) baseline the unset keys track. *enrich.Manager satisfies it;
// the interface keeps the handler unit-testable without a live provider Manager.
type EnrichmentPolicyResolver interface {
	EffectiveEnablement(ctx context.Context, libraryID string) (enrich.Enablement, error)
	GlobalEnablement() enrich.Enablement
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

// updateEnrichmentPolicyRequest is the PUT body. Each key is a patchBool so a
// client can set it, clear it to inherit (null), or omit it (unchanged) — the
// sparse-override contract. Later slices add the language / authoritative keys.
type updateEnrichmentPolicyRequest struct {
	EnrichEnabled patchBool `json:"enrichEnabled"`
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
		if !libraryExists(w, deps, id) {
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
		if !libraryExists(w, deps, id) {
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

// libraryExists validates the Library exists, writing a 404 (hide-existence) and
// returning false otherwise. A read error is a 500. Shared by the GET/PUT handlers.
func libraryExists(w http.ResponseWriter, deps Deps, id string) bool {
	_, err := deps.EnrichmentPolicy.LibraryByID(id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return false
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to read library", nil)
		return false
	}
	return true
}

// buildEnrichmentPolicyResponse assembles the view: the stored override (read
// fresh so a just-applied change is reflected) plus the derived effective and
// inherited enablement from the resolver. The resolver may be nil in a unit test
// that only exercises persistence — the derived fields then stay zero.
func buildEnrichmentPolicyResponse(ctx context.Context, deps Deps, id string) (enrichmentPolicyResponse, error) {
	policy, err := deps.EnrichmentPolicy.LibraryEnrichmentPolicy(id)
	if err != nil {
		return enrichmentPolicyResponse{}, err
	}
	resp := enrichmentPolicyResponse{EnrichEnabled: policy.EnrichEnabled}
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
	}
	return resp, nil
}
