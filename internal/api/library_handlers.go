package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/goozakdev/obelo-server/internal/library"
	"github.com/goozakdev/obelo-server/internal/store"
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
	// Linked and Available describe a mirror of another household's Library
	// (ADR-0056 §1, §6). Both are ABSENT on an ordinary local Library — a client
	// that never links never sees either — so `linked` is omitempty and
	// `available` is a pointer: false is a statement about a friend's Server being
	// down, and it must not be confused with a local Library's silence.
	Linked    bool  `json:"linked,omitempty"`
	Available *bool `json:"available,omitempty"`
	// LinkedServer is the sharing Server's display name (ADR-0056 §6). Present
	// only on a linked Library, so any authenticated User — not just an Admin
	// reading /links — can name whose shelf this is.
	LinkedServer string `json:"linkedServer,omitempty"`
}

func toLibraryJSON(l store.Library, linked linkedState) libraryJSON {
	roots := make([]libraryRootJSON, 0, len(l.Roots))
	for _, r := range l.Roots {
		roots = append(roots, libraryRootJSON{ID: r.ID, Path: r.Path})
	}
	isLinked, available, server := linkedFromLibrary(l, linked)
	return libraryJSON{
		ID:           l.ID,
		Name:         l.Name,
		Kind:         l.Kind,
		CreatedAt:    formatTimestamp(l.CreatedAt),
		RootFolders:  roots,
		Linked:       isLinked,
		Available:    available,
		LinkedServer: server,
	}
}

// --- POST /libraries (Admin) -----------------------------------------------

type createLibraryRequest struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	RootFolders []string `json:"rootFolders"`
}

func handleCreateLibrary(deps Deps) http.HandlerFunc {
	svc := deps.Library
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
		// A newly created Library is local by construction: linking is the only way
		// a mirrored one comes into being.
		writeJSON(w, http.StatusCreated, toLibraryJSON(lib, nil))
	}
}

// --- PATCH /libraries/{id} (Admin) -----------------------------------------

// updateLibraryRequest is the partial edit body: a nil name leaves the name
// unchanged; addRootFolders (absent/empty) appends nothing. The kind is fixed at
// creation and cannot be changed here.
type updateLibraryRequest struct {
	Name           *string  `json:"name"`
	AddRootFolders []string `json:"addRootFolders"`
}

// A linked Library takes a rename and NOTHING else (ADR-0056 §1). What this
// household calls somebody else's shelf is this household's business; adding a
// root folder to it is not, because it has no folders — its contents arrive over
// the Link and the Scanner never sees it.
func handleUpdateLibrary(deps Deps) http.HandlerFunc {
	svc := deps.Library
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/libraries/")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		var req updateLibraryRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if len(req.AddRootFolders) > 0 && linkedLibrary(deps, id) {
			writeLinkedLibraryRefusal(w)
			return
		}
		lib, err := svc.Update(id, library.UpdateInput{
			Name:           req.Name,
			AddRootFolders: req.AddRootFolders,
		})
		switch {
		case errors.Is(err, library.ErrNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "library not found", nil)
			return
		case errors.Is(err, library.ErrFolderOverlap):
			writeError(w, http.StatusConflict, codeFolderOverlap, err.Error(), nil)
			return
		case errors.Is(err, library.ErrValidation):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to update library", nil)
			return
		}
		writeJSON(w, http.StatusOK, toLibraryJSON(lib, loadLinkedState(deps)))
	}
}

// --- GET /libraries (Admin) ------------------------------------------------

type librariesResponse struct {
	Libraries []libraryJSON `json:"libraries"`
}

func handleListLibraries(deps Deps) http.HandlerFunc {
	svc := deps.Library
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
		linked := loadLinkedState(deps)
		out := make([]libraryJSON, 0, len(libs))
		for _, l := range libs {
			out = append(out, toLibraryJSON(l, linked))
		}
		writeJSON(w, http.StatusOK, librariesResponse{Libraries: out})
	}
}

// --- GET /libraries/{id} and DELETE /libraries/{id} (Admin) ----------------

// handleGetLibrary serves GET /libraries/{id} for any authenticated User,
// scoped: an ungranted (or unknown) Library is 404 (hide existence). It runs
// behind requireScope, so the caller's access Scope is on the context.
func handleGetLibrary(deps Deps) http.HandlerFunc {
	svc := deps.Library
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
		writeJSON(w, http.StatusOK, toLibraryJSON(lib, loadLinkedState(deps)))
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
			requireAdmin(handleCreateLibrary(deps))(w, r)
		case http.MethodGet:
			requireScope(deps.Access, handleListLibraries(deps))(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}
