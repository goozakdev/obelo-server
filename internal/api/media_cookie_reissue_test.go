package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for POST /auth/media-cookie (appletv-parity/12): a
// bearer-authenticated re-issue of the ms_media cookie carrying the REQUESTING
// bearer's session token. It closes the identity-leak the web instant user
// switch opens — a switch swaps the bearer from JS but cannot touch the HttpOnly
// media cookie, so byte-serving keeps authenticating as the previous user until
// the server rewrites the cookie. These mirror cookie_test.go: the re-issued
// cookie's attributes match login's (same writer), it is bearer-only (a lone
// cookie can't authorize it), unauth is rejected, and Secure tracks HTTPS.

// reissueMediaCookie POSTs to /auth/media-cookie with an optional bearer token
// and an optional presented ms_media cookie, and NO other credential. It returns
// the raw response (caller closes Body).
func reissueMediaCookie(t *testing.T, srv *testharness.Server, bearer string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL("/api/v1/auth/media-cookie"), nil)
	if err != nil {
		t.Fatalf("building re-issue request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("re-issue POST: %v", err)
	}
	return resp
}

// TestReissueMediaCookieCarriesBearerToken: an authenticated re-issue sets
// ms_media to the CALLER's bearer token — NOT the token in any presented cookie.
// The scenario is the identity leak itself: the caller presents bearer T2 but an
// old ms_media cookie carrying T1 (the previous identity). The re-issued cookie
// must carry T2, and its attributes must match the login cookie (same writer).
func TestReissueMediaCookieCarriesBearerToken(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	// Two logins for the SAME user mint two distinct tokens + cookies.
	t1, c1 := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-old")
	t2, _ := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-new")
	if t1 == t2 {
		t.Fatalf("expected two distinct login tokens, got the same: %q", t1)
	}

	// Present bearer T2 but the STALE cookie carrying T1 (the previous identity).
	resp := reissueMediaCookie(t, srv, t2, c1)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("re-issue status = %d, want 204; body: %s", resp.StatusCode, raw)
	}

	reissued := findCookie(resp, mediaCookieName)
	if reissued == nil {
		t.Fatalf("re-issue did not set the %q cookie; headers: %v", mediaCookieName, resp.Header)
	}
	// The core guarantee: the cookie carries the BEARER's token, not the presented
	// cookie's — so byte-serving flips to the active identity, closing the leak.
	if reissued.Value != t2 {
		t.Errorf("re-issued cookie value = %q, want the bearer token %q (not the stale %q)",
			reissued.Value, t2, t1)
	}
	// Attributes are byte-for-byte the login cookie's (same setMediaCookie writer).
	if !reissued.HttpOnly {
		t.Errorf("re-issued cookie is not HttpOnly")
	}
	if reissued.SameSite != http.SameSiteLaxMode {
		t.Errorf("re-issued cookie SameSite = %v, want Lax", reissued.SameSite)
	}
	if reissued.Path != "/api/v1" {
		t.Errorf("re-issued cookie Path = %q, want /api/v1", reissued.Path)
	}
	// Plain-HTTP test server → Secure must NOT be set (LAN path, ADR-0005).
	if reissued.Secure {
		t.Errorf("re-issued cookie Secure = true on plain HTTP, want false")
	}
	if reissued.MaxAge <= 0 {
		t.Errorf("re-issued cookie MaxAge = %d, want a positive lifetime", reissued.MaxAge)
	}
}

// TestReissueMediaCookieRejectsUnauthenticated: no credential → 401, no cookie.
func TestReissueMediaCookieRejectsUnauthenticated(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	resp := reissueMediaCookie(t, srv, "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("re-issue with no credential = %d, want 401", resp.StatusCode)
	}
	if c := findCookie(resp, mediaCookieName); c != nil {
		t.Errorf("re-issue set a media cookie on an unauthenticated request: %+v", c)
	}
}

// TestReissueMediaCookieRejectsInvalidBearer: a garbage bearer → 401, no cookie.
func TestReissueMediaCookieRejectsInvalidBearer(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	resp := reissueMediaCookie(t, srv, "not-a-real-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("re-issue with garbage bearer = %d, want 401", resp.StatusCode)
	}
	if c := findCookie(resp, mediaCookieName); c != nil {
		t.Errorf("re-issue set a media cookie for an invalid bearer: %+v", c)
	}
}

// TestReissueMediaCookieIgnoresLoneCookie: a VALID ms_media cookie with NO bearer
// does NOT authorize a re-issue — the endpoint is bearer-only (requireAuth, not
// requireAuthAllowCookie), so a stale cookie can never perpetuate its own leak.
func TestReissueMediaCookieIgnoresLoneCookie(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	_, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-lone")

	resp := reissueMediaCookie(t, srv, "", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("re-issue with ONLY a media cookie = %d, want 401 (bearer-only); body: %s",
			resp.StatusCode, raw)
	}
	if c := findCookie(resp, mediaCookieName); c != nil {
		t.Errorf("a lone cookie authorized a re-issue (set a new cookie): %+v", c)
	}
}

// TestReissueMediaCookieSecureUnderHTTPS: the same re-issue over TLS sets Secure.
func TestReissueMediaCookieSecureUnderHTTPS(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, _ := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-tls")

	// Re-serve the SAME app handler over TLS so r.TLS is set on the request.
	tlsSrv := httptest.NewTLSServer(srv.Handler())
	defer tlsSrv.Close()
	client := tlsSrv.Client() // trusts the test server's cert

	req, err := http.NewRequest(http.MethodPost, tlsSrv.URL+"/api/v1/auth/media-cookie", nil)
	if err != nil {
		t.Fatalf("building HTTPS re-issue request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS re-issue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTPS re-issue status = %d, want 204", resp.StatusCode)
	}
	cookie := findCookie(resp, mediaCookieName)
	if cookie == nil {
		t.Fatalf("HTTPS re-issue did not set the media cookie")
	}
	if !cookie.Secure {
		t.Errorf("re-issued cookie Secure = false under HTTPS, want true")
	}
}

// TestReissueMediaCookieSecureViaForwardedProto: a plain-HTTP re-issue carrying
// X-Forwarded-Proto: https (the reverse-proxy path, ADR-0005) also gets Secure.
func TestReissueMediaCookieSecureViaForwardedProto(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, _ := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-proxy")

	req, err := http.NewRequest(http.MethodPost, srv.URL("/api/v1/auth/media-cookie"), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("re-issue status = %d, want 204", resp.StatusCode)
	}
	cookie := findCookie(resp, mediaCookieName)
	if cookie == nil {
		t.Fatalf("re-issue did not set the media cookie")
	}
	if !cookie.Secure {
		t.Errorf("re-issued cookie Secure = false with X-Forwarded-Proto: https, want true")
	}
}
