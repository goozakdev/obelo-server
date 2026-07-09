package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/catalog"
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
// session is created or cleaned up; the File is visible to any authenticated
// User, exactly like browse (GET /titles/{id}) — there is no per-Library ACL.

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
			requireMethod(http.MethodGet,
				requireAuthAllowQueryToken(deps.Auth, handleFileDownload(deps.Catalog, id)))(w, r)
			return
		}
		writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
	}
}

// handleFileDownload serves a File's original bytes over HTTP with byte-range
// support (http.ServeContent handles Range, 206, If-Range, and HEAD), so an
// external player can seek. An unknown id, or a Missing File (soft-deleted /
// gone from disk), is hidden as a 404. Auth is enforced by the middleware; any
// authenticated User may fetch any File (browse visibility, no per-User ACL).
func handleFileDownload(cat *catalog.Service, fileID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := identityFrom(r.Context()); !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		f, err := cat.FileByID(fileID)
		if errors.Is(err, catalog.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeNotFound, "file not found", nil)
			return
		}
		if err != nil {
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
