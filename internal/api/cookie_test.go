package api_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// adminPassword is the password the shared adminToken/setupAdmin helpers use.
const adminPassword = "hunter2hunter2"

// Black-box tests for the media-cookie server addition (PRD "REQUIRED SERVER
// ADDITION — media cookie", issue 02). Login sets the ms_media cookie; the two
// read-only media GET endpoints (stream + artwork) accept it with NO
// Authorization header; every other endpoint rejects it; logout clears it; the
// Secure flag tracks HTTPS.

const mediaCookieName = "ms_media"

// rawLogin posts a login and returns the raw response so the test can inspect
// Set-Cookie headers. The caller owns closing the body.
func rawLogin(t *testing.T, srv *testharness.Server, username, password, clientID string) *http.Response {
	t.Helper()
	body := map[string]any{
		"username": username,
		"password": password,
		"device": map[string]any{
			"name":     "Browser",
			"platform": "web",
			"clientId": clientID,
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshaling login body: %v", err)
	}
	resp := srv.Do(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(buf))
	return resp
}

// findCookie returns the named cookie from a response's Set-Cookie headers, or
// nil if absent.
func findCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// loginToken posts a login (raw) and returns (token, the media cookie). It
// asserts both are present.
func loginWithCookie(t *testing.T, srv *testharness.Server, username, password, clientID string) (string, *http.Cookie) {
	t.Helper()
	resp := rawLogin(t, srv, username, password, clientID)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	var out loginResp
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding login body: %v\nbody: %s", err, raw)
	}
	cookie := findCookie(resp, mediaCookieName)
	if cookie == nil {
		t.Fatalf("login did not set the %q cookie; headers: %v", mediaCookieName, resp.Header)
	}
	if out.Token == "" {
		t.Fatalf("login returned empty token")
	}
	return out.Token, cookie
}

// TestLoginSetsMediaCookie: login sets an HttpOnly, SameSite=Lax cookie whose
// value is the SAME opaque token returned in JSON, scoped to /api/v1, and (on a
// plain-HTTP test server) without the Secure flag.
func TestLoginSetsMediaCookie(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	token, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")

	if cookie.Value != token {
		t.Errorf("cookie value != JSON token:\ncookie: %q\ntoken:  %q", cookie.Value, token)
	}
	if !cookie.HttpOnly {
		t.Errorf("media cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/api/v1" {
		t.Errorf("cookie Path = %q, want /api/v1", cookie.Path)
	}
	// The httptest server is plain HTTP, so Secure must NOT be set (ADR-0005: the
	// server runs plain HTTP on the LAN; Secure there would drop the cookie).
	if cookie.Secure {
		t.Errorf("cookie Secure = true on plain HTTP, want false (LAN clients would break)")
	}
	// A sane expiry was set (non-zero MaxAge or future Expires).
	if cookie.MaxAge <= 0 {
		t.Errorf("cookie MaxAge = %d, want a positive lifetime", cookie.MaxAge)
	}
}

// TestMediaCookieSecureUnderHTTPS: the same login over an HTTPS (TLS) server
// sets the Secure flag, while plain HTTP does not (asserted above).
func TestMediaCookieSecureUnderHTTPS(t *testing.T) {
	// Boot the app and wrap its handler in a TLS httptest server so r.TLS is set.
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	// Re-serve the SAME handler over TLS. We reach the handler via the harness's
	// own server URL by constructing a TLS server that proxies isn't necessary;
	// instead drive the handler directly through an httptest TLS server.
	tlsSrv := httptest.NewTLSServer(srv.Handler())
	defer tlsSrv.Close()
	client := tlsSrv.Client() // trusts the test server's cert

	body := map[string]any{
		"username": "brandon",
		"password": "hunter2hunter2",
		"device":   map[string]any{"name": "Browser", "platform": "web", "clientId": "tls-client"},
	}
	buf, _ := json.Marshal(body)
	resp, err := client.Post(tlsSrv.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("HTTPS login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTPS login status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	cookie := findCookie(resp, mediaCookieName)
	if cookie == nil {
		t.Fatalf("HTTPS login did not set the media cookie")
	}
	if !cookie.Secure {
		t.Errorf("cookie Secure = false under HTTPS, want true")
	}
	// Sanity: TLS check used directly (avoids an unused import if the helper
	// changes) — the connection state confirms TLS was actually negotiated.
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("expected a TLS connection for the HTTPS login")
	}
}

// TestMediaCookieSecureViaForwardedProto: a plain-HTTP request carrying
// X-Forwarded-Proto: https (the reverse-proxy path, ADR-0005) also gets Secure.
func TestMediaCookieSecureViaForwardedProto(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	body := map[string]any{
		"username": "brandon",
		"password": "hunter2hunter2",
		"device":   map[string]any{"name": "Browser", "platform": "web", "clientId": "proxy-client"},
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL("/api/v1/auth/login"), bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	cookie := findCookie(resp, mediaCookieName)
	if cookie == nil {
		t.Fatalf("login did not set the media cookie")
	}
	if !cookie.Secure {
		t.Errorf("cookie Secure = false with X-Forwarded-Proto: https, want true")
	}
}

// TestMediaCookieAcceptedOnArtwork: the artwork GET succeeds with ONLY the media
// cookie (no Authorization header). It also asserts that NO cookie / a garbage
// cookie is rejected with 401.
func TestMediaCookieAcceptedOnArtwork(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	// Log in to mint a media cookie carrying a valid token. (The naming library
	// was scanned with an admin token already; we re-login the same admin to get a
	// cookie. scanNamingLibrary uses adminToken which logs in "brandon".)
	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-artwork")

	list := listAllTitles(t, srv, token, libID)
	id := findNamingTitle(t, list, "Extras Movie")
	d := getDetail(t, srv, token, id)

	var posterURL string
	for _, a := range d.Artwork {
		if a.Role == "poster" {
			posterURL = a.URL
		}
	}
	if posterURL == "" {
		t.Fatalf("Extras Movie has no poster artwork; artwork: %+v", d.Artwork)
	}

	// Cookie-only GET (no Authorization header) succeeds.
	resp := cookieGET(t, srv, posterURL, cookie, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("artwork via cookie = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Errorf("artwork body empty")
	}

	// No credential at all → 401.
	resp2 := cookieGET(t, srv, posterURL, nil, "")
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("artwork with no credential = %d, want 401", resp2.StatusCode)
	}

	// Garbage cookie → 401.
	resp3 := cookieGET(t, srv, posterURL, &http.Cookie{Name: mediaCookieName, Value: "not-a-real-token"}, "")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("artwork with garbage cookie = %d, want 401", resp3.StatusCode)
	}
}

// TestMediaCookieAcceptedOnStream: the stream GET succeeds with ONLY the media
// cookie (no Authorization header), and a Range request still works.
func TestMediaCookieAcceptedOnStream(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	// Negotiate a session (bearer; this is a JSON POST and stays bearer-only).
	var dec decisionResp
	if status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &dec); status != http.StatusOK {
		t.Fatalf("playback status = %d; body: %s", status, body)
	}

	// Mint a cookie for the SAME admin (same token authorizes the session).
	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-stream")
	// The cookie token differs from the bearer token, but it is the SAME user, who
	// owns the session — ownership is by user, not by token.

	// Cookie-only stream GET (no Authorization header) succeeds.
	resp := cookieGET(t, srv, dec.StreamURL, cookie, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream via cookie = %d, want 200", resp.StatusCode)
	}
	whole, _ := io.ReadAll(resp.Body)
	if len(whole) == 0 {
		t.Fatal("stream via cookie returned empty body")
	}

	// A Range request via the cookie returns 206.
	part := cookieGET(t, srv, dec.StreamURL, cookie, "bytes=0-9")
	defer part.Body.Close()
	if part.StatusCode != http.StatusPartialContent {
		t.Errorf("ranged stream via cookie = %d, want 206", part.StatusCode)
	}
}

// TestMediaCookieRejectedOnNonMediaEndpoint: the cookie is honored ONLY on the
// two media GETs. A non-media endpoint (GET /devices) ignores it → 401.
func TestMediaCookieRejectedOnNonMediaEndpoint(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	_, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")

	// GET /devices with ONLY the cookie (no bearer) must be 401: bearer-only.
	resp := cookieGET(t, srv, "/api/v1/devices", cookie, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("GET /devices with media cookie = %d, want 401 (bearer-only); body: %s",
			resp.StatusCode, raw)
	}
}

// TestLogoutClearsMediaCookie: logout returns a Set-Cookie that expires the
// media cookie (MaxAge<=0 / past Expires), and the token it carried is revoked.
func TestLogoutClearsMediaCookie(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, cookie := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")

	// Logout (bearer).
	req, _ := http.NewRequest(http.MethodPost, srv.URL("/api/v1/auth/logout"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}

	cleared := findCookie(resp, mediaCookieName)
	if cleared == nil {
		t.Fatalf("logout did not emit a Set-Cookie clearing the media cookie")
	}
	if cleared.MaxAge > 0 {
		t.Errorf("cleared cookie MaxAge = %d, want <= 0 (expired)", cleared.MaxAge)
	}

	// The token the cookie carried is revoked: the artwork/stream endpoints would
	// now reject it. Use the cookie value (== token) against /devices-equivalent
	// validation via Authenticate by hitting a media endpoint is fixture-heavy;
	// instead confirm the bearer token is dead on a protected endpoint.
	status, _ := srv.AuthGET("/api/v1/devices", token, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("post-logout token on /devices = %d, want 401 (revoked)", status)
	}
	_ = cookie
}

// cookieGET issues a GET against an API path with an optional single cookie and
// optional Range header, and NO Authorization header. It returns the raw
// response (caller closes Body).
func cookieGET(t *testing.T, srv *testharness.Server, apiPath string, cookie *http.Cookie, rng string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(apiPath), nil)
	if err != nil {
		t.Fatalf("building cookie GET: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cookie GET %s: %v", apiPath, err)
	}
	return resp
}
