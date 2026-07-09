package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/auth"
	"github.com/marioquake/juicebox/internal/store"
)

// Wire shapes for the authentication endpoints (docs/api-contract.md). All
// fields are camelCase to match the contract; these types are the single source
// of truth for what crosses the HTTP boundary.

type deviceJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	ClientID   string `json:"clientId"`
	CreatedAt  string `json:"createdAt,omitempty"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

type userJSON struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func toUserJSON(u store.User) userJSON {
	return userJSON{ID: u.ID, Username: u.Username, Role: u.Role}
}

func toDeviceJSON(d store.Device) deviceJSON {
	return deviceJSON{
		ID:         d.ID,
		Name:       d.Name,
		Platform:   d.Platform,
		ClientID:   d.ClientID,
		CreatedAt:  formatTimestamp(d.CreatedAt),
		LastSeenAt: formatTimestamp(d.LastSeenAt),
	}
}

// --- POST /setup -----------------------------------------------------------

type setupRequest struct {
	ClaimToken string `json:"claimToken"`
	Username   string `json:"username"`
	Password   string `json:"password"`
}

type setupResponse struct {
	User userJSON `json:"user"`
}

func handleSetup(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req setupRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		user, err := svc.Setup(req.ClaimToken, req.Username, req.Password)
		switch {
		case errors.Is(err, auth.ErrSetupClosed):
			writeError(w, http.StatusForbidden, codeSetupClosed,
				"setup is already complete", nil)
			return
		case errors.Is(err, auth.ErrInvalidClaimToken):
			writeError(w, http.StatusForbidden, codeInvalidClaim,
				"invalid claim token", nil)
			return
		case err != nil && (strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "UNIQUE")):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to create admin", nil)
			return
		}
		writeJSON(w, http.StatusCreated, setupResponse{User: toUserJSON(user)})
	}
}

// --- POST /auth/login ------------------------------------------------------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Device   struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		ClientID string `json:"clientId"`
	} `json:"device"`
}

type loginResponse struct {
	Token  string     `json:"token"`
	User   userJSON   `json:"user"`
	Device deviceJSON `json:"device"`
}

func handleLogin(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Device.ClientID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"device.clientId is required", nil)
			return
		}
		res, err := svc.Login(req.Username, req.Password, auth.DeviceInput{
			Name:     req.Device.Name,
			Platform: req.Device.Platform,
			ClientID: req.Device.ClientID,
		})
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, codeInvalidLogin,
				"invalid username or password", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"login failed", nil)
			return
		}
		// Also set the media cookie carrying the SAME opaque token, so a browser
		// <video>/<img> can authenticate the read-only media GET endpoints (which
		// cannot send an Authorization header). Bearer remains the primary credential
		// for every JSON/mutation endpoint. Must run before writeJSON, which writes
		// the status line.
		setMediaCookie(w, r, res.Token)
		writeJSON(w, http.StatusOK, loginResponse{
			Token:  res.Token,
			User:   toUserJSON(res.User),
			Device: toDeviceJSON(res.Device),
		})
	}
}

// --- POST /auth/logout (authenticated) -------------------------------------

func handleLogout(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if err := svc.Logout(id.Token); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal, "logout failed", nil)
			return
		}
		// Clear the media cookie so the browser drops it immediately; the token is
		// already revoked server-side above. Must run before WriteHeader.
		clearMediaCookie(w, r)
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- GET /devices (authenticated) ------------------------------------------

type devicesResponse struct {
	Devices []deviceJSON `json:"devices"`
}

func handleListDevices(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		devices, err := svc.Devices(id.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to list devices", nil)
			return
		}
		out := make([]deviceJSON, 0, len(devices))
		for _, d := range devices {
			out = append(out, toDeviceJSON(d))
		}
		writeJSON(w, http.StatusOK, devicesResponse{Devices: out})
	}
}

// --- DELETE /devices/{id} (authenticated; self or Admin) -------------------

func handleDeleteDevice(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		// Path is "/devices/{id}" after the version prefix is stripped.
		deviceID := strings.TrimPrefix(r.URL.Path, "/devices/")
		if deviceID == "" || strings.Contains(deviceID, "/") {
			writeError(w, http.StatusNotFound, codeNotFound, "resource not found", nil)
			return
		}
		err := svc.DeleteDevice(id.User, deviceID)
		switch {
		case errors.Is(err, auth.ErrDeviceNotFound):
			// Hide existence from non-owners: a 404 either way (api-contract.md
			// "404, not 403" posture for resources outside the caller's scope).
			writeError(w, http.StatusNotFound, codeNotFound, "device not found", nil)
			return
		case errors.Is(err, auth.ErrForbidden):
			writeError(w, http.StatusNotFound, codeNotFound, "device not found", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to delete device", nil)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
