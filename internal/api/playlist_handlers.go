package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/organize"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes + handlers for the Playlists surface (collections-playlists 03):
// User-owned, ordered, single-media-kind queues. Every route is PUBLIC scope with
// the caller as the owner — there is no requireAdmin and no Admin override. A
// Playlist is PRIVATE: every read/write resolves caller ownership, and a non-owner
// (Member OR Admin) is hidden a 404 (not a 403), exactly like another User's
// playback session (handleSessionSubtree). Members decorate to the EXACT same
// titleSummary a browse grid shows (decorateMembers, reused from the Collection
// path) PLUS an itemId so duplicates are distinguishable.
//
// Ordering (reorder, remove-by-item-id) and the explicit access/Missing-survival
// tests are issue 04; this slice is create/list/get/rename/delete + append +
// single-kind + ownership.

// --- wire shapes ------------------------------------------------------------

type playlistJSON struct {
	ID string `json:"id"`
	// Kind is "movie" | "tv" | "music", or omitted while the Playlist is still
	// untyped (empty until its first item fixes it).
	Kind string `json:"kind,omitempty"`
	// System is the system-Playlist slug ("watchlist") when this is a system
	// Playlist, omitted for an ordinary one. The client reads it to mark the
	// Watchlist as non-renamable/non-deletable.
	System    string `json:"system,omitempty"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

func toPlaylistJSON(p store.Playlist) playlistJSON {
	return playlistJSON{
		ID:        p.ID,
		Kind:      p.Kind,
		System:    p.System,
		Name:      p.Name,
		CreatedAt: formatTimestamp(p.CreatedAt),
		UpdatedAt: formatTimestamp(p.UpdatedAt),
	}
}

// playlistCardJSON is one card in GET /playlists: the Playlist metadata plus a raw
// itemCount (a nice-to-have, the unfiltered item-row count).
type playlistCardJSON struct {
	playlistJSON
	ItemCount int `json:"itemCount"`
}

// playlistMemberJSON is one ordered, decorated member of a Playlist: a browse-grid
// titleSummary PLUS the playlist_items id (so duplicates are distinguishable and
// issue 04 can remove-by-item-id).
type playlistMemberJSON struct {
	titleSummaryJSON
	ItemID string `json:"itemId"`
}

// playlistDetailJSON is GET /playlists/{id}: the Playlist plus its resolved member
// Titles in position order, each decorated identically to a browse-list summary.
type playlistDetailJSON struct {
	playlistJSON
	MemberCount int                  `json:"memberCount"`
	Members     []playlistMemberJSON `json:"members"`
}

type playlistsListJSON struct {
	Playlists []playlistCardJSON `json:"playlists"`
}

// --- collection-level dispatch (/playlists) ---------------------------------

// handlePlaylistsCollection dispatches the playlist-level methods on "/playlists":
// POST creates (caller becomes owner), GET lists the caller's own. Both run behind
// requireAuth (identity attached); neither needs an access Scope (the list is
// metadata-only and the caller always owns what it returns).
func handlePlaylistsCollection(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleCreatePlaylist(deps.Organize)(w, r)
		case http.MethodGet:
			handleListPlaylists(deps.Organize)(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// handlePlaylistSubtree dispatches every route under "/playlists/{id}...", behind
// requireAuth. It routes by sub-resource:
//
//   - {id}/items/{itemId}  DELETE        → remove a member by item id (owner)
//   - {id}/items           POST / PUT    → append a Title / reorder (owner)
//   - {id}                 GET           → detail + decorated members (owner; Scope)
//   - {id}                 PUT / DELETE  → rename / delete (owner)
//
// The {id}/items/{itemId} sub-resource is matched BEFORE {id}/items so the longer
// suffix isn't shadowed (mirrors the Collection subtree's /items/ vs /items order).
// Every leaf resolves ownership inside its handler and hides a foreign Playlist as
// 404. The GET detail and PUT reorder leaves are additionally wrapped with
// requireScope: both are judged against what the caller can SEE, so both need the
// caller's (== owner's) access Scope — detail to resolve members, reorder to
// validate the payload against that same set.
func handlePlaylistSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/playlists/")
		if rest == "" {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}

		// {id}/items/{itemId} — remove one member by item id (owner).
		if id, itemID, found := strings.Cut(rest, "/items/"); found {
			if id == "" || itemID == "" || strings.Contains(id, "/") || strings.Contains(itemID, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodDelete, handleRemovePlaylistItem(deps.Organize, id, itemID))(w, r)
			return
		}

		// {id}/items — append (POST) or reorder (PUT) a member (owner).
		if id, ok := strings.CutSuffix(rest, "/items"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			switch r.Method {
			case http.MethodPost:
				handleAppendPlaylistItem(deps.Organize, id)(w, r)
			case http.MethodPut:
				requireScope(deps.Access, handleReorderPlaylistItems(deps, id))(w, r)
			default:
				w.Header().Set("Allow", "POST, PUT")
				writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
					"method not allowed", nil)
			}
			return
		}

		// {id} — single-Playlist ops.
		if strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			// GET detail resolves the caller's (== owner's) access Scope so member
			// resolution reuses the shared access filter.
			requireScope(deps.Access, handleGetPlaylist(deps, rest))(w, r)
		case http.MethodPut:
			handleRenamePlaylist(deps.Organize, rest)(w, r)
		case http.MethodDelete:
			handleDeletePlaylist(deps.Organize, rest)(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// --- handlers (owner == caller) ---------------------------------------------

type playlistWriteRequest struct {
	Name string `json:"name"`
}

func handleCreatePlaylist(svc *organize.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req playlistWriteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		p, err := svc.CreatePlaylist(id.User.ID, req.Name)
		switch {
		case errors.Is(err, organize.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, "playlist name is required", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to create playlist", nil)
			return
		}
		writeJSON(w, http.StatusCreated, toPlaylistJSON(p))
	}
}

func handleListPlaylists(svc *organize.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		ps, err := svc.ListPlaylists(id.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to list playlists", nil)
			return
		}
		out := playlistsListJSON{Playlists: make([]playlistCardJSON, 0, len(ps))}
		for _, p := range ps {
			out.Playlists = append(out.Playlists, playlistCardJSON{
				playlistJSON: toPlaylistJSON(p),
				ItemCount:    p.ItemCount,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleGetPlaylist(deps Deps, id string) http.HandlerFunc {
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
		p, err := deps.Organize.GetPlaylist(ident.User.ID, id)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get playlist", nil)
			return
		}
		members, err := deps.Organize.PlaylistMembers(scope, ident.User.ID, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get playlist", nil)
			return
		}
		decorated, err := decoratePlaylistMembers(deps, ident.User.ID, members)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get playlist", nil)
			return
		}
		writeJSON(w, http.StatusOK, playlistDetailJSON{
			playlistJSON: toPlaylistJSON(p),
			MemberCount:  len(decorated),
			Members:      decorated,
		})
	}
}

func handleRenamePlaylist(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req playlistWriteRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		p, err := svc.RenamePlaylist(ident.User.ID, id, req.Name)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist not found", nil)
			return
		case errors.Is(err, organize.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, "playlist name is required", nil)
			return
		case errors.Is(err, organize.ErrSystemPlaylist):
			writeError(w, http.StatusUnprocessableEntity, codeSystemPlaylist,
				"this playlist is managed by the system and can't be renamed", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to update playlist", nil)
			return
		}
		writeJSON(w, http.StatusOK, toPlaylistJSON(p))
	}
}

func handleDeletePlaylist(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		switch err := svc.DeletePlaylist(ident.User.ID, id); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist not found", nil)
		case errors.Is(err, organize.ErrSystemPlaylist):
			writeError(w, http.StatusUnprocessableEntity, codeSystemPlaylist,
				"this playlist is managed by the system and can't be deleted", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to delete playlist", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

type playlistItemRequest struct {
	TitleID string `json:"titleId"`
}

func handleAppendPlaylistItem(svc *organize.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req playlistItemRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		_, err := svc.AppendPlaylistItem(ident.User.ID, id, req.TitleID)
		switch {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist not found", nil)
			return
		case errors.Is(err, organize.ErrUnknownTitle):
			writeError(w, http.StatusUnprocessableEntity, codeUnknownTitle,
				"titleId names a title that does not exist", nil)
			return
		case errors.Is(err, organize.ErrKindMismatch):
			writeError(w, http.StatusUnprocessableEntity, codeKindMismatch,
				"title kind does not match the playlist's media kind", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to append playlist item", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type playlistReorderRequest struct {
	ItemIDs []string `json:"itemIds"`
}

// handleReorderPlaylistItems handles PUT /playlists/{id}/items: the body is the
// FULL desired order of the item ids this caller can SEE — the set GET
// /playlists/{id} just returned them. Positions are rewritten to match, with
// members hidden from that view keeping their place. Owner only (getOwned → 404 for
// a non-owner). A payload that doesn't EXACTLY match the visible item set is a 422
// ITEM_SET_MISMATCH no-op. Returns 204.
//
// Wrapped in requireScope for the same reason the GET detail leaf is: the payload is
// judged against what the caller can see, so it needs the caller's (== owner's)
// access Scope to know what that is.
func handleReorderPlaylistItems(deps Deps, id string) http.HandlerFunc {
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
		var req playlistReorderRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		switch err := deps.Organize.ReorderPlaylistItems(scope, ident.User.ID, id, req.ItemIDs); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist not found", nil)
		case errors.Is(err, organize.ErrItemSetMismatch):
			writeError(w, http.StatusUnprocessableEntity, codeItemSetMismatch,
				"itemIds must be exactly the playlist's current item ids", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to reorder playlist", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// handleRemovePlaylistItem handles DELETE /playlists/{id}/items/{itemId}: removes
// exactly the one entry by its item id. Owner only (getOwned → 404 for a non-owner).
// An itemId that is not a row of this Playlist → 404. Returns 204.
func handleRemovePlaylistItem(svc *organize.Service, id, itemID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		switch err := svc.RemovePlaylistItem(ident.User.ID, id, itemID); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "playlist item not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to remove playlist item", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// decoratePlaylistMembers turns resolved Playlist members into the browse-grid
// titleSummary PLUS each entry's itemId. It reuses decorateMembers (the exact
// Collection/browse decoration path: catalog bulk readers + toTitleSummary) over
// the member Titles, then re-pairs each summary with its item id by index —
// decorateMembers preserves order, so the indexes line up — keeping duplicates
// distinct via their item ids.
func decoratePlaylistMembers(deps Deps, userID string, members []organize.PlaylistMember) ([]playlistMemberJSON, error) {
	titles := make([]store.Title, 0, len(members))
	for _, m := range members {
		titles = append(titles, m.Title)
	}
	summaries, err := decorateMembers(deps, userID, titles)
	if err != nil {
		return nil, err
	}
	out := make([]playlistMemberJSON, 0, len(members))
	for i, m := range members {
		out = append(out, playlistMemberJSON{
			titleSummaryJSON: summaries[i],
			ItemID:           m.ItemID,
		})
	}
	return out, nil
}
