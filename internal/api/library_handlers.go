package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/library"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes for the Library endpoints (docs/api-contract.md): camelCase, the
// single source of truth for what crosses the HTTP boundary.

type libraryRootJSON struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type libraryJSON struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	CreatedAt   string            `json:"createdAt,omitempty"`
	RootFolders []libraryRootJSON `json:"rootFolders"`
}

func toLibraryJSON(l store.Library) libraryJSON {
	roots := make([]libraryRootJSON, 0, len(l.Roots))
	for _, r := range l.Roots {
		roots = append(roots, libraryRootJSON{ID: r.ID, Path: r.Path})
	}
	return libraryJSON{
		ID:          l.ID,
		Name:        l.Name,
		Kind:        l.Kind,
		CreatedAt:   formatTimestamp(l.CreatedAt),
		RootFolders: roots,
	}
}

// --- POST /libraries (Admin) -----------------------------------------------

type createLibraryRequest struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	RootFolders []string `json:"rootFolders"`
}

func handleCreateLibrary(svc *library.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createLibraryRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		lib, err := svc.Create(library.CreateInput{
			Name:        req.Name,
			Kind:        req.Kind,
			RootFolders: req.RootFolders,
		})
		switch {
		case errors.Is(err, library.ErrFolderOverlap):
			writeError(w, http.StatusConflict, codeFolderOverlap, err.Error(), nil)
			return
		case errors.Is(err, library.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to create library", nil)
			return
		}
		writeJSON(w, http.StatusCreated, toLibraryJSON(lib))
	}
}

// --- GET /libraries (Admin) ------------------------------------------------

type librariesResponse struct {
	Libraries []libraryJSON `json:"libraries"`
}

func handleListLibraries(svc *library.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		libs, err := svc.List(scope)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to list libraries", nil)
			return
		}
		out := make([]libraryJSON, 0, len(libs))
		for _, l := range libs {
			out = append(out, toLibraryJSON(l))
		}
		writeJSON(w, http.StatusOK, librariesResponse{Libraries: out})
	}
}

// --- GET /libraries/{id} and DELETE /libraries/{id} (Admin) ----------------

// handleGetLibrary serves GET /libraries/{id} for any authenticated User,
// scoped: an ungranted (or unknown) Library is 404 (hide existence). It runs
// behind requireScope, so the caller's access Scope is on the context.
func handleGetLibrary(svc *library.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := mustScope(w, r)
		if !ok {
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/libraries/")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		lib, err := svc.Get(scope, id)
		switch {
		case errors.Is(err, library.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to get library", nil)
			return
		}
		writeJSON(w, http.StatusOK, toLibraryJSON(lib))
	}
}

// handleDeleteLibrary serves DELETE /libraries/{id} (Admin scope).
func handleDeleteLibrary(svc *library.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/libraries/")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		err := svc.Delete(id)
		switch {
		case errors.Is(err, library.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to delete library", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleLibrariesCollection dispatches the collection-level methods on
// "/libraries": POST creates (Admin scope), GET lists (any authenticated User,
// filtered to their access Scope). It runs behind requireAuth; the per-method
// gate (requireAdmin / requireScope) is applied here rather than at the route so
// the two scopes can diverge on one path.
func handleLibrariesCollection(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			requireAdmin(handleCreateLibrary(deps.Library))(w, r)
		case http.MethodGet:
			requireScope(deps.Access, handleListLibraries(deps.Library))(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}
