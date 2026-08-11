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
//
// # One cookie NAME per scheme, and why a single name cannot work
//
// The Secure flag above is necessary and was not sufficient, because a cookie
// jar is not partitioned by scheme or by port: http://obelo.local:8080 and
// https://obelo.local write the same host-only entry. While both listeners wrote
// the name "ms_media" at Path=/api/v1, they fought over one jar slot, and the
// browser resolves that fight in exactly one direction — RFC 6265bis §5.7's
// "leave secure cookies alone" rule (Chrome since M52, Firefox, WebKit): a
// Set-Cookie from a NON-secure origin is discarded outright when an existing
// cookie of the same name/domain/path has secure-only set.
//
// So one HTTPS login permanently broke browser media on the plain-HTTP origin:
// the plain login kept sending a correct, Secure-less cookie, the browser kept
// throwing it away to protect the HTTPS one, and the HTTPS one was never sent
// back over http. The failure is silent and it does not self-repair, because
// logging in again over HTTP is the very write being refused. What the user sees
// is a page that works — the SPA's JSON calls carry the bearer from an origin-
// scoped localStorage — with every <img> and <video> 401ing, since those are the
// only requests that depend on the cookie.
//
// The fix is to stop sharing the slot: each scheme writes a name the other never
// writes, so neither can shadow the other and a browser can hold a live session
// on both at once. The HTTPS name carries the __Secure- prefix, which makes the
// separation browser-enforced rather than a convention — a non-secure origin
// cannot set a __Secure- cookie at all, whatever this server sends.
//
// Do not "tidy" these back into one constant, and do not have either scheme
// clear the other's cookie: that reintroduces the same interference at login
// time, one origin logging the other out of byte-serving.
const (
	// mediaCookieName carries the session token for the media GET endpoints on
	// the PLAIN-HTTP listener. It is deliberately NOT the pre-split "ms_media":
	// a browser poisoned before the split still holds a Secure "ms_media", which
	// would go on refusing this write under the rule described above. A fresh name
	// is what makes the fix take effect on the browsers that actually hit the bug,
	// rather than 30 days later when the poisoned entry finally expires.
	mediaCookieName = "ms_media_plain"

	// secureMediaCookieName carries the same token on the HTTPS listener. The
	// __Secure- prefix is a browser-enforced contract (Secure required, non-secure
	// origins refused), so the plain listener cannot collide with it by accident.
	secureMediaCookieName = "__Secure-ms_media"

	// legacyMediaCookieName is the single name BOTH listeners wrote before the
	// split. It is READ on the way in, so a session established before the upgrade
	// keeps serving bytes instead of silently 401ing, and NEVER written. It expires
	// on its own; nothing needs to migrate it.
	legacyMediaCookieName = "ms_media"
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

// mediaCookieNameFor returns the cookie name for the scheme the request arrived
// on — the __Secure- name over TLS, the plain name otherwise. Every write goes
// through it, so a response can only ever set the name belonging to its own
// listener (see the "one cookie NAME per scheme" note above).
func mediaCookieNameFor(r *http.Request) string {
	if requestIsHTTPS(r) {
		return secureMediaCookieName
	}
	return mediaCookieName
}

// setMediaCookie writes the media cookie carrying the opaque session token. It
// is HttpOnly + SameSite=Lax, scoped to the API path, named for the request's
// scheme (see mediaCookieNameFor), with Secure set only when the request is
// HTTPS (see requestIsHTTPS). Called by the login handler.
func setMediaCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     mediaCookieNameFor(r),
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
//
// It clears THIS scheme's cookie and the legacy shared name, so a logout leaves
// no media credential behind on the origin it was issued to. It does NOT clear
// the other scheme's cookie: that is a separate session, on a separate origin,
// holding a separate token, and reaching across would be the cross-scheme
// interference this split exists to end.
func clearMediaCookie(w http.ResponseWriter, r *http.Request) {
	https := requestIsHTTPS(r)
	for _, name := range []string{mediaCookieNameFor(r), legacyMediaCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     mediaCookiePath,
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
			Secure:   https,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// mediaCookieToken extracts the opaque token from the media cookie, or
// ("", false) when no media cookie is present with a value.
//
// The order is the freshest-first order, and it is scheme-independent by
// construction rather than by accident: a __Secure- cookie never reaches a
// plain-HTTP request (the browser will not send it), so on that listener the
// first candidate is simply absent and the plain name wins. Where several DO
// arrive together — over HTTPS, in a browser that also has a live plain-HTTP
// session, or an unexpired pre-split cookie, or both — the one this listener
// most recently wrote is the one that matches the caller's current session, so
// it must be preferred. Getting this backwards would resurrect the stale-cookie
// identity leak that POST /auth/media-cookie exists to close: a re-issue writes
// the scheme's own name, and a reader that preferred some other name would go on
// authenticating as the previous user.
func mediaCookieToken(r *http.Request) (string, bool) {
	for _, name := range []string{secureMediaCookieName, mediaCookieName, legacyMediaCookieName} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value, true
		}
	}
	return "", false
}
