package api

import (
	"net/http"
	"strings"
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
// Secure flag: the server runs plain HTTP behind a TLS-terminating reverse proxy
// (ADR-0005). Setting Secure unconditionally would make the cookie vanish on the
// plain-HTTP LAN path, breaking browser media there. So Secure is set ONLY when
// the request actually arrived over TLS — detected via r.TLS or the proxy's
// X-Forwarded-Proto header.
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

// requestIsHTTPS reports whether the request reached us over TLS, either
// directly (r.TLS set) or via a TLS-terminating reverse proxy that forwards the
// original scheme (ADR-0005). Used to decide the cookie's Secure flag: Secure on
// HTTPS, off on the plain-HTTP LAN path so the cookie is not silently dropped.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// A reverse proxy that terminated TLS forwards the original scheme. Accept the
	// standard header (and the comma-separated first hop if a chain set it).
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		return false
	}
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
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
