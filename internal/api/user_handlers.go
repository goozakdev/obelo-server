package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/auth"
)

// Admin-scope user management (docs/api-contract.md). These handlers let an
// Admin create and manage Users beyond the first-Admin bootstrap. The wire
// User shape is the shared userJSON from auth_handlers.go (id/username/role);
// granted libraries and the Rating ceiling join it in a later slice.

// --- POST /users, GET /users -----------------------------------------------

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type usersResponse struct {
	Users []userJSON `json:"users"`
}

// handleUsersCollection dispatches the collection-level methods on "/users":
// POST creates a User, GET lists them.
func handleUsersCollection(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleCreateUser(svc)(w, r)
		case http.MethodGet:
			handleListUsers(svc)(w, r)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

func handleCreateUser(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createUserRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		user, err := svc.CreateUser(req.Username, req.Password, req.Role)
		switch {
		case errors.Is(err, auth.ErrInvalidUser):
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"username and password are required; role must be admin or member", nil)
			return
		case errors.Is(err, auth.ErrUsernameTaken):
			writeError(w, http.StatusConflict, codeUsernameTaken, "username already taken", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to create user", nil)
			return
		}
		writeJSON(w, http.StatusCreated, toUserJSON(user))
	}
}

func handleListUsers(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := svc.Users()
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to list users", nil)
			return
		}
		out := make([]userJSON, 0, len(users))
		for _, u := range users {
			out = append(out, toUserJSON(u))
		}
		writeJSON(w, http.StatusOK, usersResponse{Users: out})
	}
}

// --- GET/DELETE /users/{id}, PUT /users/{id}/password ----------------------

type setPasswordRequest struct {
	Password string `json:"password"`
}

// setLibraryAccessRequest is the body of PUT /users/{id}/libraryAccess: the full
// desired set of granted Library ids (replace-set, not a delta).
type setLibraryAccessRequest struct {
	LibraryIDs []string `json:"libraryIds"`
}

// setRatingCeilingRequest is the body of PUT /users/{id}/ratingCeiling: the
// canonical ceiling label, or null/"" to clear it (uncapped).
type setRatingCeilingRequest struct {
	Rating string `json:"rating"`
}

// userDetailJSON is the GET /users/{id} shape: the User plus their granted
// Library ids and Rating ceiling. libraryIds is empty for an Admin (all-access
// by role); ratingCeiling is "" when uncapped — the client reads role to
// interpret an Admin's empty fields as "all Libraries, no cap".
type userDetailJSON struct {
	ID            string   `json:"id"`
	Username      string   `json:"username"`
	Role          string   `json:"role"`
	LibraryIDs    []string `json:"libraryIds"`
	RatingCeiling string   `json:"ratingCeiling"`
}

// handleUserSubtree dispatches the single-User endpoints under "/users/":
//
//	GET    /users/{id}                → one User (with granted libraryIds)
//	DELETE /users/{id}                → delete a User (last-Admin guarded)
//	PUT    /users/{id}/password       → reset a User's password
//	PUT    /users/{id}/libraryAccess  → replace a Member's granted Libraries
//
// Method is gated here (not via requireMethod) because the {id} subtree serves
// more than one method, mirroring the /libraries single-resource dispatcher.
func handleUserSubtree(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/users/")

		if id, ok := strings.CutSuffix(rest, "/password"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut, handleSetUserPassword(deps.Auth, id))(w, r)
			return
		}
		if id, ok := strings.CutSuffix(rest, "/libraryAccess"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut, handleSetLibraryAccess(deps.Access, id))(w, r)
			return
		}
		if id, ok := strings.CutSuffix(rest, "/ratingCeiling"); ok {
			if id == "" || strings.Contains(id, "/") {
				writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
				return
			}
			requireMethod(http.MethodPut, handleSetRatingCeiling(deps.Access, id))(w, r)
			return
		}

		if rest == "" || strings.Contains(rest, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			handleGetUser(deps, rest)(w, r)
		case http.MethodDelete:
			handleDeleteUser(deps.Auth, rest)(w, r)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
				"method not allowed", nil)
		}
	}
}

func handleGetUser(deps Deps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := deps.Auth.User(id)
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to get user", nil)
			return
		}
		grants, err := deps.Access.LibraryAccess(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to get user", nil)
			return
		}
		if grants == nil {
			grants = []string{}
		}
		ceiling, err := deps.Access.RatingCeiling(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to get user", nil)
			return
		}
		writeJSON(w, http.StatusOK, userDetailJSON{
			ID:            user.ID,
			Username:      user.Username,
			Role:          user.Role,
			LibraryIDs:    grants,
			RatingCeiling: ceiling,
		})
	}
}

// handleSetRatingCeiling sets or clears a Member's Rating ceiling (Admin scope).
// rating "" (or null) clears it; an unknown label or an Admin target is rejected.
func handleSetRatingCeiling(svc *access.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setRatingCeilingRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		err := svc.SetRatingCeiling(id, req.Rating)
		switch {
		case errors.Is(err, access.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case errors.Is(err, access.ErrAdminCeiling):
			writeError(w, http.StatusUnprocessableEntity, codeAdminCeiling,
				"cannot set a rating ceiling on an admin (admins see all)", nil)
			return
		case errors.Is(err, access.ErrUnknownRating):
			writeError(w, http.StatusUnprocessableEntity, codeUnknownRating,
				"unknown rating label", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to set rating ceiling", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSetLibraryAccess replaces a Member's granted Library set (Admin scope).
// The body carries the FULL desired set; granting to an Admin or naming an
// unknown Library is rejected, leaving any prior set unchanged.
func handleSetLibraryAccess(svc *access.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setLibraryAccessRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		err := svc.SetLibraryAccess(id, req.LibraryIDs)
		switch {
		case errors.Is(err, access.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case errors.Is(err, access.ErrAdminGrant):
			writeError(w, http.StatusUnprocessableEntity, codeAdminGrant,
				"cannot grant libraries to an admin (admins see all)", nil)
			return
		case errors.Is(err, access.ErrUnknownLibrary):
			writeError(w, http.StatusUnprocessableEntity, codeUnknownLibrary,
				"grant set names a library that does not exist", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to set library access", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleDeleteUser(svc *auth.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := svc.DeleteUser(id)
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case errors.Is(err, auth.ErrLastAdmin):
			writeError(w, http.StatusConflict, codeLastAdmin,
				"cannot delete the last admin", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to delete user", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleSetUserPassword(svc *auth.Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setPasswordRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		err := svc.SetPassword(id, req.Password)
		switch {
		case errors.Is(err, auth.ErrInvalidUser):
			writeError(w, http.StatusBadRequest, codeBadRequest, "password is required", nil)
			return
		case errors.Is(err, auth.ErrUserNotFound):
			writeError(w, http.StatusNotFound, codeNotFound, "user not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to set password", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
