package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/marioquake/juicebox/internal/auth"
)

// The Device authorization grant's HTTP surface (ADR-0036, docs/api-contract.md
// §3.1): a TV asks for a code, a phone approves it, the TV polls and collects a
// session.
//
// Two of the three endpoints are unauthenticated, and that is the design rather
// than an oversight: a TV that could authenticate would not need this flow. What
// stands in for auth is the shape of the secrets — see auth/device_code.go.

// linkPath is the SPA route the phone lands on (web/src/App.tsx). The code rides
// in the PATH, not a query string, and that is load-bearing: the SPA's login
// bounce preserves only location.pathname, so `/link?code=X` would come back
// from the login screen as `/link` with the code silently gone. A path segment
// survives the round trip.
const linkPath = "/link"

// --- POST /auth/device/code ------------------------------------------------

type deviceCodeRequest struct {
	Device struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
		ClientID string `json:"clientId"`
	} `json:"device"`
}

type deviceCodeResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

func handleDeviceCode(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req deviceCodeRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Device.ClientID == "" {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"device.clientId is required", nil)
			return
		}
		start, err := svc.StartDeviceAuth(auth.DeviceInput{
			Name:     req.Device.Name,
			Platform: req.Device.Platform,
			ClientID: req.Device.ClientID,
		})
		switch {
		case errors.Is(err, auth.ErrDeviceAuthBusy):
			writeError(w, http.StatusServiceUnavailable, codeDeviceAuthBusy,
				"too many sign-ins in progress; try again in a few minutes", nil)
			return
		case err != nil && strings.Contains(err.Error(), "required"):
			writeError(w, http.StatusBadRequest, codeBadRequest, err.Error(), nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not start device authorization", nil)
			return
		}

		base := externalBaseURL(r)
		writeJSON(w, http.StatusCreated, deviceCodeResponse{
			DeviceCode:              start.DeviceCode,
			UserCode:                start.UserCode,
			VerificationURI:         base + linkPath,
			VerificationURIComplete: base + linkPath + "/" + url.PathEscape(start.UserCode),
			ExpiresIn:               int(start.ExpiresIn.Seconds()),
			Interval:                int(start.Interval.Seconds()),
		})
	}
}

// --- POST /auth/device/token -----------------------------------------------

type deviceTokenRequest struct {
	DeviceCode string `json:"deviceCode"`
}

func handleDeviceToken(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req deviceTokenRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		res, err := svc.RedeemDeviceCode(req.DeviceCode)
		switch {
		case errors.Is(err, auth.ErrDeviceCodePending):
			// 400, not 202: RFC 8628's state machine models "keep polling" as an
			// error, and a client that treats 2xx as terminal must not see one here.
			writeError(w, http.StatusBadRequest, codeAuthorizationPending,
				"waiting for approval", nil)
			return
		case errors.Is(err, auth.ErrDeviceCodeSlowDown):
			writeError(w, http.StatusBadRequest, codeSlowDown,
				"polling too fast", nil)
			return
		case errors.Is(err, auth.ErrDeviceCodeExpired):
			writeError(w, http.StatusBadRequest, codeExpiredToken,
				"this code has expired; start again on your TV", nil)
			return
		case errors.Is(err, auth.ErrDeviceCodeUnknown):
			writeError(w, http.StatusBadRequest, codeInvalidDeviceCode,
				"unknown device code", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not complete device authorization", nil)
			return
		}

		// From here the response is byte-identical to POST /auth/login's, cookie
		// included. That is the contract: a client has ONE way to establish a
		// session from a LoginResponse, and this is just a second way to obtain one.
		setMediaCookie(w, r, res.Token)
		writeJSON(w, http.StatusOK, loginResponse{
			Token:  res.Token,
			User:   toUserJSON(res.User),
			Device: toDeviceJSON(res.Device),
		})
	}
}

// --- POST /auth/device/approve (authenticated) -----------------------------

type deviceApproveRequest struct {
	UserCode string `json:"userCode"`
}

// deviceApproveResponse names what was just authorized. There is no confirmation
// step before this (approval is immediate on code entry), so this response is
// the user's only chance to see which Device they signed in — worth returning
// even though nothing gates on it.
type deviceApproveResponse struct {
	Device struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
	} `json:"device"`
}

func handleDeviceApprove(svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		var req deviceApproveRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		approved, err := svc.ApproveDeviceCode(req.UserCode, id.User.ID)
		switch {
		case errors.Is(err, auth.ErrTooManyAttempts):
			writeError(w, http.StatusTooManyRequests, codeTooManyAttempts,
				"too many incorrect codes; wait a few minutes and try again", nil)
			return
		case errors.Is(err, auth.ErrUserCodeUnknown):
			// One answer for unknown, expired, and already-used. Telling them apart
			// would let a caller map which codes are live.
			writeError(w, http.StatusNotFound, codeInvalidUserCode,
				"that code is not valid; check the code on your TV", nil)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not approve the code", nil)
			return
		}

		var out deviceApproveResponse
		out.Device.Name = approved.DeviceName
		out.Device.Platform = approved.DevicePlatform
		writeJSON(w, http.StatusOK, out)
	}
}

// --- verification URL ------------------------------------------------------

// externalBaseURL reconstructs the origin the CALLER used to reach us, which is
// the only origin worth putting in a QR code: the phone has to reach the same
// server, and it is standing next to the TV that just made this request.
//
// Deriving it from the request rather than from config is what makes the QR
// correct in all three deployments this server actually sees — a LAN IP, a
// hostname, or a reverse proxy on another origin (ADR-0005) — none of which the
// server can know about itself. r.Host is the inbound Host header, so it is
// already whatever the TV typed or discovered.
//
// The known-bad case is loopback: a TV pointed at 127.0.0.1 (the simulator)
// produces a QR that resolves to the PHONE when scanned. That is not fixable
// here — the server cannot know a better address than the one it was reached on
// — and it does not arise on real hardware, where the TV is a different host and
// must have used a routable address to get here at all.
func externalBaseURL(r *http.Request) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	// A reverse proxy rewrites Host to the upstream; X-Forwarded-Host carries the
	// original. Same first-hop rule as requestIsHTTPS.
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		host = strings.TrimSpace(fwd)
	}
	return scheme + "://" + host
}
