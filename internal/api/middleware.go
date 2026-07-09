package api

import (
	"net/http"
	"strings"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/auth"
)

// requireAuth wraps h with bearer-token authentication. It extracts the token
// from the Authorization header, validates it against the auth service, and on
// success attaches the resolved identity (User, Device, raw token) to the
// request context before calling h. Missing, malformed, or revoked tokens get
// the standard 401 envelope — handlers behind this never see an unauthenticated
// request.
func requireAuth(svc *auth.Service, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"missing or malformed Authorization header", nil)
			return
		}
		id, err := svc.Authenticate(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"invalid or revoked token", nil)
			return
		}
		ctx := withIdentity(r.Context(), identity{
			User:   id.User,
			Device: id.Device,
			Token:  raw,
		})
		h(w, r.WithContext(ctx))
	}
}

// requireAuthAllowCookie is the media-auth middleware. It authenticates EITHER
// the bearer header OR the ms_media cookie, validating whichever it finds the
// SAME way as requireAuth (auth.Service.Authenticate), and attaches the resolved
// identity to the context. It exists ONLY for the read-only media GET endpoints
// a browser reaches via <img src>/<video src>/hls.js (which cannot send an
// Authorization header): GET /sessions/{id}/stream, the HLS playlist + segments
// at GET /sessions/{id}/hls/* (ADR-0004), and GET /titles/{id}/artwork/{role}.
// Every other endpoint keeps requireAuth (bearer-only) and must NOT honor the
// cookie. The posture stays SameSite=Lax, read-only GET only — CSRF exposure
// stays negligible.
//
// The bearer header wins when both are present (native clients and the API
// client always send it). Because the token is validated against the DB on each
// request, cookie-borne auth revokes immediately on logout/device-delete exactly
// like the bearer path (ADR-0015).
func requireAuthAllowCookie(svc *auth.Service, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			raw, ok = mediaCookieToken(r)
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"missing bearer token or media cookie", nil)
			return
		}
		id, err := svc.Authenticate(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"invalid or revoked token", nil)
			return
		}
		ctx := withIdentity(r.Context(), identity{
			User:   id.User,
			Device: id.Device,
			Token:  raw,
		})
		h(w, r.WithContext(ctx))
	}
}

// requireAuthAllowQueryToken authenticates EITHER the bearer header OR a
// ?token= query parameter, validating whichever it finds the SAME way as
// requireAuth. It exists ONLY for the sessionless direct-file download
// (GET /files/{id}/download): an EXTERNAL desktop player (VLC) opened on a
// downloaded .xspf playlist can neither set an Authorization header nor carry
// the HttpOnly ms_media cookie, so the token must travel in the URL.
//
// Tradeoff (accepted for the self-hosted LAN posture, ADR-0005): a URL-borne
// token can land in server access logs, browser history, and the .xspf file on
// disk. It is scoped here to a single read-only GET, the token still validates
// against the DB on every request (revokes immediately on logout/device-delete,
// ADR-0015), and NO other endpoint honors a query token. The bearer header wins
// when both are present.
func requireAuthAllowQueryToken(svc *auth.Service, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			raw, ok = queryToken(r)
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"missing bearer token or token query parameter", nil)
			return
		}
		id, err := svc.Authenticate(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"invalid or revoked token", nil)
			return
		}
		ctx := withIdentity(r.Context(), identity{
			User:   id.User,
			Device: id.Device,
			Token:  raw,
		})
		h(w, r.WithContext(ctx))
	}
}

// requireAdmin wraps h so only an authenticated Admin reaches it. It layers on
// top of requireAuth (which has already attached the identity), reading the
// User's role from context. A non-Admin authenticated User is refused with a
// 403 FORBIDDEN envelope; an unauthenticated request never gets here because
// requireAuth has already returned 401. Built as a real role check so Member
// support in a later slice works without revisiting this guard.
func requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		if id.User.Role != "admin" {
			writeError(w, http.StatusForbidden, codeForbidden,
				"admin role required", nil)
			return
		}
		h(w, r)
	}
}

// requireScope resolves the authenticated caller's access Scope once and stashes
// it on the request context for the read/play handlers (which read it via
// scopeFrom and thread it into the catalog/playback calls). It layers inside the
// auth middleware (identity already attached), so it is wrapped around a leaf
// after its requireAuth/requireAuthAllowCookie. Resolving here keeps it "once per
// request" and keeps the leaf handlers free of the access service.
func requireScope(acc *access.Service, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "not authenticated", nil)
			return
		}
		scope, err := acc.Resolve(id.User.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"failed to resolve access", nil)
			return
		}
		h(w, r.WithContext(withScope(r.Context(), scope)))
	}
}

// mustScope returns the access Scope a read handler runs behind. A missing scope
// means the handler was wired without requireScope — a programming error — so it
// fails closed with a 500 rather than serving with the deny-all zero value.
func mustScope(w http.ResponseWriter, r *http.Request) (access.Scope, bool) {
	scope, ok := scopeFrom(r.Context())
	if !ok {
		writeError(w, http.StatusInternalServerError, codeInternal,
			"access scope not resolved", nil)
		return access.Scope{}, false
	}
	return scope, true
}

// bearerToken pulls the token out of an "Authorization: Bearer <token>" header.
// It returns ("", false) when the header is absent or not a Bearer credential.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// queryToken pulls the opaque session token out of the ?token= query parameter.
// It returns ("", false) when absent or empty. Only requireAuthAllowQueryToken
// (the direct-file download) consults it; see that middleware for the posture.
func queryToken(r *http.Request) (string, bool) {
	tok := strings.TrimSpace(r.URL.Query().Get("token"))
	if tok == "" {
		return "", false
	}
	return tok, true
}
