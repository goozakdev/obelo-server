package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/organize"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes + handlers for the Collections surface (collections-playlists
// 01/02): Admin-curated, shared groupings of Titles. Writes are Admin scope; reads
// are any authenticated User and are access-filtered per viewer (issue 02): a
// member in an ungranted Library or above the viewer's Rating ceiling is absent,
// and a Collection with zero visible members is hidden from a non-Admin (absent
// from the list, 404 on detail). An Admin sees full membership and every
// Collection. Members decorate to the EXACT same
// titleSummary a browse grid shows: the organize service resolves the visible,
// ordered member Title rows and this layer runs them through the existing catalog
// bulk readers (watch state / genres / artwork version) + toTitleSummary.

// --- wire shapes ------------------------------------------------------------

type collectionJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
}

func toCollectionJSON(c store.Collection) collectionJSON {
	return collectionJSON{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		CreatedAt:   formatTimestamp(c.CreatedAt),
		UpdatedAt:   formatTimestamp(c.UpdatedAt),
	}
}

// collectionSummaryJSON is one card in GET /collections: the Collection plus the
// computed list metadata — a member count and a representative poster (the first
// visible member's poster, in sort_title order). Both are computed over the
// resolved (Missing-omitted) membership; in this slice that is the full membership
// (issue 02 makes the count/poster per-viewer).
type collectionSummaryJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	MemberCount int    `json:"memberCount"`
	// PosterURL points at the representative member's poster-artwork endpoint
	// (the same convention a Title summary uses). Omitted when the Collection has
	// no visible members. The client renders it as the card image; a member with no
	// poster simply 404s the image, exactly like a Title without artwork.
	PosterURL string `json:"posterUrl,omitempty"`
}

type collectionsListJSON struct {
	Collections []collectionSummaryJSON `json:"collections"`
}

// collectionDetailJSON is GET /collections/{id}: the Collection plus its resolved
// member Titles, each decorated identically to a browse-list summary.
type collectionDetailJSON struct {
	collectionJSON
	MemberCount int                `json:"memberCount"`
	Members     []titleSummaryJSON `json:"members"`
}

// --- collection-level dispatch (/collections) -------------------------------

// handleCollectionsCollection dispatches the collection-level methods on
// "/collections": POST creates (Admin scope), GET lists (any authenticated
// User). It runs behind requireAuth; the per-method gate is applied here so the
// two scopes can diverge on one path (mirrors handleLibrariesCollection).
func handleCollectionsCollection(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			requireAdmin(handleCreateCollection(deps.Organize))(w, r)
		case http.MethodGet:
			// GET lists per-viewer: requireScope resolves the caller's access Scope so
			// the cards reflect what THEY can see (and zero-visible Collections hide).
			requireScope(deps.Access, handleListCollections(deps))(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// handleCollectionSubtree dispatches every route under "/collections/{id}...".
// It runs behind requireAuth (identity attached), then routes by sub-resource and
// applies the right scope per leaf:
//
//   - {id}/items            POST          → add Titles (Admin)
//   - {id}/items/{titleId}  DELETE        → remove a Title (Admin)
//   - {id}                  GET           → detail + decorated members (any User)
//   - {id}                  PUT / DELETE  → rename / delete (Admin)
//
// Sub-resources are matched first so they aren't shadowed by the single-Collection
// handler (which rejects any path containing a "/").
func handleCollectionSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/collections/")
		if rest == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		// {id}/items/{titleId} — remove one member (Admin).
		if id, titleID, found := strings.Cut(rest, "/items/"); found {
			if id == "" || titleID == "" || strings.Contains(id, "/") || strings.Contains(titleID, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodDelete,
				requireAdmin(handleRemoveCollectionItem(deps.Organize, id, titleID)))(w, r)
			return
		}
		// {id}/items — add members (Admin).
		if id, ok := strings.CutSuffix(rest, "/items"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPost,
				requireAdmin(handleAddCollectionItems(deps.Organize, id)))(w, r)
			return
		}

		// {id} — single-Collection ops.
		if strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			// GET detail is per-viewer: requireScope resolves the caller's access Scope
			// so members are access-filtered and a zero-visible Collection 404s.
			requireScope(deps.Access, handleGetCollection(deps, rest))(w, r)
		case http.MethodPut:
			requireAdmin(handleUpdateCollection(deps.Organize, rest))(w, r)
		case http.MethodDelete:
			requireAdmin(handleDeleteCollection(deps.Organize, rest))(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// --- write handlers (Admin) -------------------------------------------------

type collectionWriteRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func handleCreateCollection(svc *organize.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req collectionWriteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		c, err := svc.Create(req.Name, req.Description)
		switch {
		case errors.Is(err, organize.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, "collection name is required", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to create collection", nil)
			return
		}
		writeJSON(w, http.StatusCreated, toCollectionJSON(c))
	}
}

func handleUpdateCollection(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req collectionWriteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		c, err := svc.Update(id, req.Name, req.Description)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
			return
		case errors.Is(err, organize.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, "collection name is required", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to update collection", nil)
			return
		}
		writeJSON(w, http.StatusOK, toCollectionJSON(c))
	}
}

func handleDeleteCollection(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch err := svc.Delete(id); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to delete collection", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

type collectionItemsRequest struct {
	TitleIDs []string `json:"titleIds"`
}

func handleAddCollectionItems(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req collectionItemsRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		err := svc.AddItems(id, req.TitleIDs)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
			return
		case errors.Is(err, organize.ErrUnknownTitle):
			writeError(w, http.StatusUnprocessableEntity, codeUnknownTitle,
				"item set names a title that does not exist", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to add collection items", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleRemoveCollectionItem(svc *organize.Service, id, titleID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch err := svc.RemoveItem(id, titleID); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to remove collection item", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// --- read handlers (any authenticated User) ---------------------------------

func handleListCollections(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		cols, err := deps.Organize.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list collections", nil)
			return
		}
		out := collectionsListJSON{Collections: make([]collectionSummaryJSON, 0, len(cols))}
		for _, c := range cols {
			// Resolve THIS viewer's visible membership to compute the card's count +
			// poster — both are per-viewer (a restricted member never contributes).
			members, err := deps.Organize.Members(scope, c.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, codeInternal, "failed to list collections", nil)
				return
			}
			// Zero-visible hiding (headline correctness property): a non-Admin viewer
			// who can see NONE of a Collection's members must not even learn it exists,
			// so it is dropped from their list (never an empty card that leaks "a
			// restricted Collection exists"). An Admin is exempt — they always see
			// every Collection, including genuinely empty ones, so management is not
			// broken (issue line 18: "Admin sees every member and every Collection").
			if len(members) == 0 && !scope.IsAdmin {
				continue
			}
			summary := collectionSummaryJSON{
				ID:          c.ID,
				Name:        c.Name,
				Description: c.Description,
				CreatedAt:   formatTimestamp(c.CreatedAt),
				UpdatedAt:   formatTimestamp(c.UpdatedAt),
				MemberCount: len(members),
			}
			if len(members) > 0 {
				summary.PosterURL = APIPrefix + "/titles/" + members[0].ID + "/artwork/poster"
			}
			out.Collections = append(out.Collections, summary)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleGetCollection(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		c, err := deps.Organize.Get(id)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get collection", nil)
			return
		}
		members, err := deps.Organize.Members(scope, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get collection", nil)
			return
		}
		// Zero-visible hiding: a non-Admin viewer who can see none of this
		// Collection's members 404s — existence is hidden (404-not-403), so neither a
		// member nor a non-zero count leaks restricted content. An Admin is exempt and
		// sees the Collection even when empty (issue line 18), so Admin management of a
		// fresh/empty Collection still works.
		if len(members) == 0 && !scope.IsAdmin {
			writeError(w, http.StatusNotFound, codeNotFound, "collection not found", nil)
			return
		}
		summaries, err := decorateMembers(deps, ident.User.ID, members)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get collection", nil)
			return
		}
		writeJSON(w, http.StatusOK, collectionDetailJSON{
			collectionJSON: toCollectionJSON(c),
			MemberCount:    len(summaries),
			Members:        summaries,
		})
	}
}

// decorateMembers turns resolved member Title rows into titleSummary JSON
// decorated with the calling User's watch state, enriched genres, and the
// per-Title artwork cache-bust version — the EXACT decoration a browse list
// applies (handleListTitles), via the same catalog bulk readers, so a Collection
// grid is byte-for-byte consistent with a browse grid.
func decorateMembers(deps Deps, userID string, members []store.Title) ([]titleSummaryJSON, error) {
	out := make([]titleSummaryJSON, 0, len(members))
	if len(members) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(members))
	for _, t := range members {
		ids = append(ids, t.ID)
	}
	states, err := deps.Catalog.WatchStatesForTitles(userID, ids)
	if err != nil {
		return nil, err
	}
	genres, err := deps.Catalog.GenresForTitles(ids)
	if err != nil {
		return nil, err
	}
	versions, err := deps.Catalog.ArtworkVersionsForTitles(ids)
	if err != nil {
		return nil, err
	}
	for _, t := range members {
		js := toTitleSummary(t, states[t.ID], genres[t.ID])
		js.ArtworkVersion = versions[t.ID]
		out = append(out, js)
	}
	return out, nil
}
