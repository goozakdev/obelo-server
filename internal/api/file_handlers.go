package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/catalog"
)

// Sessionless direct-file download (the "Open in VLC" affordance). Unlike the
// session-scoped GET /sessions/{id}/stream — which requires a negotiated
// Playback session and serves the tier the client can play — this route hands
// an external desktop player the ORIGINAL bytes addressed by the stable File id:
//
//	GET /api/v1/files/{id}/download   progressive byte-range stream of the File
//
// Auth is bearer header OR ?token= query param (requireAuthAllowQueryToken): a
// player like VLC, launched on a downloaded .xspf playlist, can set neither an
// Authorization header nor the ms_media cookie, so the token rides the URL. No
// session is created or cleaned up.
//
// The route is scoped exactly like browse (GET /titles/{id}): the caller's access
// Scope is resolved by requireScope and applied — in BOTH dimensions, Library
// grant and Rating ceiling — to the Title that owns the File. It has to be
// applied HERE, because a File is addressed by its own id and this path never
// touches GetTitle, so nothing else on it knows the caller's access. It used to
// be unscoped, and this comment used to describe that as intentional ("visible to
// any authenticated User… no per-Library ACL"); it was not a decision, it was a
// hole — any Member who learned a File id could pull the original bytes of
// anything in the catalog, past their grants and past their ceiling, using the
// weakest credential the server accepts. Do not remove the guard to "simplify"
// this route: the ?token= affordance is what makes it the worst one to leave open.
//
// An out-of-scope File is refused with the SAME 404 as an unknown id — same
// status, same body, byte for byte. That is load-bearing (api-contract.md
// "404, not 403"): a distinguishable refusal would confirm to a Member that a
// given id exists, which is the fact the Scope is hiding.

// handleFileSubtree dispatches the /files/{id}... routes. The only leaf is GET
// {id}/download (the direct-file stream); everything else is 404. {id} must be a
// single path element.
func handleFileSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/files/")
		if id, ok := strings.CutSuffix(rest, "/download"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			// requireScope layers INSIDE the auth middleware (it reads the identity
			// that requireAuthAllowQueryToken attached) and outside the leaf, so the
			// Scope is resolved once per request and the handler never talks to the
			// access service itself — the same wiring every other read route uses.
			requireMethod(http.MethodGet,
				requireAuthAllowQueryToken(deps.Auth,
					requireScope(deps.Access, handleFileDownload(deps.Catalog, id))))(w, r)
			return
		}
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	}
}

// handleFileDownload serves a File's original bytes over HTTP with byte-range
// support (http.ServeContent handles Range, 206, If-Range, and HEAD), so an
// external player can seek. Auth is enforced by the middleware, access by the
// Scope: an unknown id, a File whose owning Title is outside the caller's Scope
// (ungranted Library or above their Rating ceiling), a File orphaned from its
// Edition/Title, and a Missing File (soft-deleted / gone from disk) all collapse
// into the one indistinguishable 404. The collapse is the point — see the file
// header. Keep every refusal on the same writeError call; splitting the
// out-of-scope case out into its own message (or a 403) would turn this route
// into an existence oracle for File ids.
func handleFileDownload(cat *catalog.Service, fileID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identityFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		// mustScope fails closed with a 500 when the route was wired without
		// requireScope: that is a programming error, and serving the bytes under the
		// deny-all zero Scope would be the wrong way to survive it.
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		f, err := cat.FileByID(scope, fileID)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal, "failed to load file", nil)
			return
		}
		if !f.Present {
			// Missing File (soft-deleted, ADR-0008): the row survives but the bytes
			// are gone — hide it as a 404 rather than a confusing open error.
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		}
		sf, err := openSessionFile(f.Path)
		if err != nil {
			// Present in the catalog but unreadable on disk right now: 404.
			writeError(w, http.StatusNotFound, codeNotFound, "file unavailable", nil)
			return
		}
		defer sf.file.Close()
		http.ServeContent(w, r, sf.name, sf.modTime, sf.file)
	}
}
