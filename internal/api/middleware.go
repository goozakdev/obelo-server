package api

import (
	"net/http"
	"strings"

	"github.com/marioquake/obelo-server/internal/access"
	"github.com/marioquake/obelo-server/internal/auth"
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
// identity to the context. It exists ONLY for the read-only GET endpoints a
// browser reaches via <img src>/<video src>/hls.js/EventSource, none of which can
// set an Authorization header:
//
//   - GET /sessions/{id}/stream and the HLS playlist + segments at
//     GET /sessions/{id}/hls/* (ADR-0004)
//   - GET /titles|shows|seasons|artists|albums/{id}/artwork/… and the cast
//     headshots at GET /people/{ref}/artwork/{role}
//   - GET /events, the SSE stream (ADR-0016)
//   - GET /providerImage?ref=…, the metadata-provider thumbnail proxy behind the
//     admin Edit-item pickers (provider_image.go), which is additionally wrapped
//     in requireAdmin — it is an Admin surface, and the cookie authenticates the
//     caller without widening WHO may reach it.
//
// Every other endpoint keeps requireAuth (bearer-only) and must NOT honor the
// cookie. The list above grew for the proxy, so state the posture rather than
// assume it still holds: the cookie is HttpOnly, SameSite=Lax, and scoped to the
// /api/v1 path, and every route here is a read-only GET that changes no state, so
// the worst a cross-site request can do is cause a fetch whose bytes it cannot
// read. CSRF exposure stays negligible. Anything that WRITES must not join this
// list, whatever it would cost the browser.
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

// clientIP returns the address the per-source controls key on: the login failure
// limiter (auth/login_limit.go) and the device-code start quota
// (auth/device_auth.go).
//
// By default it is the host portion of the connection's remote address — the
// address this process actually received the bytes from — and X-Forwarded-For,
// X-Real-IP, and every other client-supplied header are ignored outright. That
// default is the security property, not an oversight: a counter keyed on a value
// the caller writes is decorative, because every guess can arrive from a fresh
// "address".
//
// A header is read only where the operator has said which upstream may assert an
// origin, via OBELO_TRUSTED_PROXIES, and then only right-to-left for the
// left-most hop OUTSIDE that allowlist. All of that happens once per request in
// forwarded.go — read the comment on forwardedFor before touching any of it —
// and this function is only the lookup of the answer.
//
// The reverse-proxy deployment (ADR-0005) is what changes hands here. Configured,
// it now gets genuine per-client counters. UNCONFIGURED, it still collapses to a
// single global counter, because every request really does arrive from the proxy;
// that remains the safe direction to fail — one shared budget is stricter than
// per-attacker budgets, never looser — and the per-username counter still
// discriminates. So the degradation is now an operator's choice rather than a
// property of the server, and the fix is a line of configuration rather than a
// change here. Do not "improve" this by reading the header unconditionally.
func clientIP(r *http.Request) string {
	if o, ok := requestOriginFrom(r.Context()); ok {
		return o.ClientIP
	}
	// No middleware ran (a handler exercised directly). Resolve with an empty
	// allowlist, which is RemoteAddr and nothing else.
	return resolveOrigin(r, nil).ClientIP
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
