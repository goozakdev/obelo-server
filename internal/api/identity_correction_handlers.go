package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/match"
	"github.com/marioquake/juicebox/internal/store"
)

// Wrong item — the destructive, identity-changing correction (item-editing/04,
// ADR-0019/ADR-0014). Offered on Movies and Shows ONLY. When a file is genuinely a
// different work than its name parsed to, an Admin searches (reusing the Fix-info
// picker), picks the correct work, and applies it here. In one coherent operation
// the server:
//
//  1. writes/updates the folder-keyed Match override (match.FixMatch) so the
//     corrected identity survives the next rescan (ADR-0014), AND re-keys the live
//     row NOW so browse reflects the new work immediately;
//  2. because identity actually changed, resets that item's watch state and clears
//     its Locked fields (a genuinely different work is a clean slate);
//  3. pins the picked record as an Enrichment override and re-enriches clean
//     (reusing the Fix-info apply primitive).
//
// The action's CHOICE is the sole signal identity changed — never inferred from a
// diff (ADR-0019). Absent on Artist/Album/Track and individual Episodes: music
// identity is tag-anchored (no folder key) and Episodes have no per-episode anchor;
// the endpoint rejects those kinds (WRONG_KIND). Admin-only; emits libraryUpdated.

// identityCorrectionRequest is the body of PUT /titles|shows/{id}/identityCorrection:
// the picked candidate the search picker returned. externalId (the authoritative
// TMDB id) is required; title/year come from the candidate so the corrected identity
// reads right immediately (the server resolves them from the id when omitted).
type identityCorrectionRequest struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	// Cascade requests the "also apply to children" cascade after the identity
	// correction (item-editing/05). Honored only on a Show (→episodes); a Movie is a
	// childless leaf, so it is ignored there.
	Cascade bool `json:"cascade"`
}

// handleTitleIdentityCorrection performs the Wrong-item correction on a Movie leaf
// (PUT /titles/{id}/identityCorrection, Admin-only). Rejects a non-Movie leaf
// (Episode / Track) with 422 WRONG_KIND — those kinds have no folder-keyed identity
// anchor. Unknown Title → 404. On success it re-keys identity, resets watch state,
// clears Locked fields, pins + re-enriches the picked record, emits a libraryUpdated
// SSE nudge, and returns the updated Title detail (its identityKey now the picked
// work's, watch state cleared, lockedFields empty).
func handleTitleIdentityCorrection(deps Deps, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req identityCorrectionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		externalID := strings.TrimSpace(req.ExternalID)
		if externalID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "externalId is required", nil)
			return
		}

		kind, libraryID, anchor, err := deps.Catalog.FolderAnchorForTitle(titleID)
		switch {
		case errors.Is(err, catalog.ErrNotFound), errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "title not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to correct identity", nil)
			return
		}
		if kind != "movie" {
			writeError(w, http.StatusUnprocessableEntity, codeWrongKind,
				"Wrong item is only available on Movies and Shows — this is a "+kind, nil)
			return
		}

		title, year, ok := resolveCorrectedIdentity(r, deps.Enrich, "movie", externalID, req.Title, req.Year)
		if !ok {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"a corrected title is required (none supplied and the provider could not resolve one)", nil)
			return
		}

		ov, err := deps.Match.FixMatch(match.FixMatchInput{
			LibraryID: libraryID, FolderPath: anchor, Title: title, Year: year, TMDBID: externalID,
		})
		if !writeMatchError(w, err) {
			return
		}
		if err := deps.Catalog.CorrectTitleIdentity(titleID, title, year, externalID, ov.IdentityKey); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to correct identity", nil)
			return
		}
		// Pin the picked record + re-enrich clean (locks were just cleared, so every
		// field refreshes from the new work). Best-effort: a re-enrich failure does not
		// undo the identity change (the override + re-key already stuck).
		if err := deps.Enrich.ApplyOverride(r.Context(), titleID, externalID); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to re-enrich corrected item", nil)
			return
		}
		writeReEnrichedDetail(w, deps.Catalog, deps.Events, ident.User.ID, titleID)
	}
}

// handleShowIdentityCorrection is the Wrong-item correction on a Show
// (PUT /shows/{id}/identityCorrection, Admin-only) — the parent analogue of the
// Movie path: re-key the Show, reset every Episode's watch state, clear the Show's
// Locked fields, pin + re-enrich the picked show record, and return the updated
// parent detail. Unknown Show → 404.
func handleShowIdentityCorrection(deps Deps, showID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req identityCorrectionRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		externalID := strings.TrimSpace(req.ExternalID)
		if externalID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest, "externalId is required", nil)
			return
		}

		libraryID, anchor, err := deps.Catalog.FolderAnchorForShow(showID)
		switch {
		case errors.Is(err, catalog.ErrNotFound), errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "show not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to correct identity", nil)
			return
		}

		title, year, ok := resolveCorrectedIdentity(r, deps.Enrich, "show", externalID, req.Title, req.Year)
		if !ok {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"a corrected title is required (none supplied and the provider could not resolve one)", nil)
			return
		}

		ov, err := deps.Match.FixMatch(match.FixMatchInput{
			LibraryID: libraryID, FolderPath: anchor, Title: title, Year: year, TMDBID: externalID,
		})
		if !writeMatchError(w, err) {
			return
		}
		if err := deps.Catalog.CorrectShowIdentity(showID, title, year, externalID, ov.IdentityKey); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to correct identity", nil)
			return
		}
		if err := deps.Enrich.ApplyEntityOverride(r.Context(), store.EntityShow, showID, externalID); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to re-enrich corrected item", nil)
			return
		}
		// "Also apply to children" (item-editing/05): after the Show pin applied (lock
		// released), best-effort re-resolve its Episodes positionally under the corrected
		// show. Best-effort — a partial cascade never undoes the identity correction.
		sum := runEntityCascade(r, deps.Enrich, req.Cascade, store.EntityShow, showID, externalID)
		writeEntityDetailAfterCascade(w, deps.Catalog, deps.Events, store.EntityShow, showID, sum)
	}
}

// resolveCorrectedIdentity settles the corrected work's display title + year. A
// caller-supplied title (from the picked candidate) wins; when it is blank the
// authoritative provider is asked to resolve them from the external id (mirroring
// handleFixMatch's by-id fill). ok is false only when no title could be determined
// at all — the correction cannot key an identity without one.
func resolveCorrectedIdentity(r *http.Request, enr *enrich.Service, kind, externalID, title string, year int) (string, int, bool) {
	title = strings.TrimSpace(title)
	if title == "" && enr != nil {
		if t, y, matched, err := enr.ResolveIdentity(r.Context(), enrich.TitleRef{Kind: kind, TMDBID: externalID}); err == nil && matched {
			title = t
			if year == 0 {
				year = y
			}
		}
	}
	if title == "" {
		return "", 0, false
	}
	return title, year, true
}

// writeMatchError maps a match.FixMatch error to its HTTP response, returning true
// when there was no error (the caller proceeds). Mirrors handleFixMatch's mapping.
func writeMatchError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, match.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
		return false
	case errors.Is(err, match.ErrValidation):
		writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
		return false
	case err != nil:
		writeError(w, http.StatusInternalServerError, codeInternal, "failed to record identity correction", nil)
		return false
	}
	return true
}
