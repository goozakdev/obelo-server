package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/organize"
)

// The Watchlist surface: a thin, dedicated façade over the per-User system
// Watchlist Playlist (migration 0021). Every route is owner == caller (requireAuth,
// no Admin override), exactly like the ordinary /playlists surface. Its whole reason
// to exist as a NAMED endpoint — rather than making callers discover the Watchlist's
// id first — is that the Watchlist is a fixed, always-present entity: the client
// (and future features) say "the Watchlist" and the server resolves/creates it. Each
// handler ENSURES the Watchlist exists before acting, so a User who never had one
// (created after the back-fill) gets it seeded on first touch.
//
//   - GET    /watchlist              → the Watchlist + its decorated members (Scope)
//   - POST   /watchlist/items        → append a Title { titleId } (→ 204)
//   - DELETE /watchlist/items/{itemId} → remove one entry by item id (→ 204)

// handleWatchlist dispatches the collection-level Watchlist routes on "/watchlist".
// GET returns the Watchlist detail; member resolution needs the caller's (== owner's)
// access Scope, so the GET leaf is wrapped with requireScope.
func handleWatchlist(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			requireScope(deps.Access, handleGetWatchlist(deps))(w, r)
		default:
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

// handleWatchlistSubtree dispatches the item routes under "/watchlist/...":
// POST /watchlist/items (append) and DELETE /watchlist/items/{itemId} (remove).
func handleWatchlistSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/watchlist/")

		// /items/{itemId} — remove one entry by item id.
		if itemID, found := strings.CutPrefix(rest, "items/"); found {
			if itemID == "" || strings.Contains(itemID, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodDelete, handleRemoveWatchlistItem(deps.Organize, itemID))(w, r)
			return
		}

		// /items — append a Title.
		if rest == "items" {
			requireMethod(http.MethodPost, handleAppendWatchlistItem(deps.Organize))(w, r)
			return
		}

		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	}
}

func handleGetWatchlist(deps Deps) http.HandlerFunc {
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
		wl, err := deps.Organize.Watchlist(ident.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get watchlist", nil)
			return
		}
		members, err := deps.Organize.PlaylistMembers(scope, ident.User.ID, wl.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get watchlist", nil)
			return
		}
		decorated, err := decoratePlaylistMembers(deps, ident.User.ID, members)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to get watchlist", nil)
			return
		}
		writeJSON(w, http.StatusOK, playlistDetailJSON{
			playlistJSON: toPlaylistJSON(wl),
			MemberCount:  len(decorated),
			Members:      decorated,
		})
	}
}

func handleAppendWatchlistItem(svc *organize.Service) http.HandlerFunc {
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
		_, err := svc.AppendToWatchlist(ident.User.ID, req.TitleID)
		switch {
		case errors.Is(err, organize.ErrUnknownTitle):
			writeError(w, http.StatusUnprocessableEntity, codeUnknownTitle,
				"titleId names a title that does not exist", nil)
			return
		case errors.Is(err, organize.ErrKindMismatch):
			writeError(w, http.StatusUnprocessableEntity, codeKindMismatch,
				"title kind does not match the watchlist's media kind", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to add to watchlist", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleRemoveWatchlistItem(svc *organize.Service, itemID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		wl, err := svc.Watchlist(ident.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to remove watchlist item", nil)
			return
		}
		switch err := svc.RemovePlaylistItem(ident.User.ID, wl.ID, itemID); {
		case errors.Is(err, organize.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "watchlist item not found", nil)
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to remove watchlist item", nil)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}
