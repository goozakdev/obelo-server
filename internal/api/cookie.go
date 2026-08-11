package api

import (
	"net/http"
	"time"
)

// The media cookie (PRD "REQUIRED SERVER ADDITION — media cookie").
//
// A browser <video src>/<img src> cannot set an Authorization header, so the
// read-only media GET endpoints need ambient auth. At login the server sets an
// HttpOnly, SameSite=Lax cookie carrying the SAME opaque session token the JSON
// response returns; logout clears it. Only the two read-only media GETs honor
// the cookie (requireAuthAllowCookie); every other endpoint stays bearer-only
// (requireAuth) and must NOT honor the cookie. Because the cookie authorizes
// only non-state-changing GETs and is SameSite=Lax, CSRF exposure is negligible
// (PRD), so no CSRF token is added.
//
// Secure flag: the server serves plain HTTP on the LAN whether or not it also
// terminates TLS (ADR-0041 — the HTTPS listener is an addition, never a
// replacement). Setting Secure unconditionally would make the cookie vanish on
// the plain-HTTP path, breaking browser media there. So Secure is set ONLY when
// the request actually arrived over TLS — r.TLS, or a trusted proxy's
// X-Forwarded-Proto (see requestIsHTTPS).
const (
	// mediaCookieName carries the session token for the media GET endpoints.
	mediaCookieName = "ms_media"
	// mediaCookiePath scopes the cookie to the API so it is never sent to the
	// SPA's static asset routes — and, with SameSite=Lax, only on top-level
	// GET navigations to /api/v1/* (i.e. the <img>/<video> media URLs).
	mediaCookiePath = APIPrefix
	// mediaCookieMaxAge is the cookie lifetime. It is a convenience lifetime for
	// the browser; the token itself is validated against the DB on every request
	// (ADR-0015), so revocation (logout / device delete) takes effect immediately
	// regardless of this expiry.
	mediaCookieMaxAge = 30 * 24 * time.Hour
)

// requestIsHTTPS reports whether the request reached us over TLS: directly
// (r.TLS is set, which since ADR-0041 is a real signal because this server can
// terminate HTTPS itself), or via a TLS-terminating reverse proxy that forwarded
// the original scheme. Used to decide the cookie's Secure flag — Secure on HTTPS,
// off on the plain-HTTP LAN path so the cookie is not silently dropped.
//
// X-Forwarded-Proto is honoured ONLY from a peer inside OBELO_TRUSTED_PROXIES,
// and this function no longer reads it directly at all: the decision is made once
// per request in forwarded.go and looked up here. Reading it unconditionally was
// defensible while a proxy was the only thing that could reach the socket
// (ADR-0005); it stopped being so the moment clients started connecting directly,
// at which point a stranger's `X-Forwarded-Proto: https` on a plain-HTTP request
// marks the cookie Secure, the browser then refuses to send it back over http,
// and the user's media quietly stops working. Self-inflicted denial rather than
// disclosure, but a client-supplied header steering a security attribute all the
// same.
//
// The consequence worth naming: an existing reverse-proxy deployment that sets
// no OBELO_TRUSTED_PROXIES now gets a cookie WITHOUT Secure, where it previously
// got one with it. That is the intended trade — the header is unbelievable
// without the setting, and the cookie still travels the proxy's TLS — and it is
// one line of configuration to restore.
func requestIsHTTPS(r *http.Request) bool {
	if o, ok := requestOriginFrom(r.Context()); ok {
		return o.HTTPS
	}
	// No middleware ran (a handler exercised directly). Resolve with an empty
	// allowlist, which is r.TLS and nothing else.
	return resolveOrigin(r, nil).HTTPS
}

// setMediaCookie writes the media cookie carrying the opaque session token. It
// is HttpOnly + SameSite=Lax, scoped to the API path, with Secure set only when
// the request is HTTPS (see requestIsHTTPS). Called by the login handler.
func setMediaCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     mediaCookieName,
		Value:    token,
		Path:     mediaCookiePath,
		MaxAge:   int(mediaCookieMaxAge / time.Second),
		Expires:  time.Now().Add(mediaCookieMaxAge),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearMediaCookie expires the media cookie. Called by the logout handler so the
// browser drops it immediately (the token is also revoked server-side). The
// Path/SameSite/Secure attributes mirror setMediaCookie so the browser matches
// and overwrites the existing cookie.
func clearMediaCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     mediaCookieName,
		Value:    "",
		Path:     mediaCookiePath,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// mediaCookieToken extracts the opaque token from the media cookie, or
// ("", false) when the cookie is absent or empty.
func mediaCookieToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(mediaCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}
