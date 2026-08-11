package api_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/marioquake/obelo-server/internal/testharness"
)

// adminPassword is the password the shared adminToken/setupAdmin helpers use.
const adminPassword = "hunter2hunter2"

// Black-box tests for the media-cookie server addition (PRD "REQUIRED SERVER
// ADDITION — media cookie", issue 02). Login sets the ms_media cookie; the two
// read-only media GET endpoints (stream + artwork) accept it with NO
// Authorization header; every other endpoint rejects it; logout clears it; the
// Secure flag tracks HTTPS.

// The two media-cookie names, one per listener scheme. They are spelled out here
// rather than imported because these are black-box tests, and because the exact
// strings ARE the contract: the whole point of the split is that the plain-HTTP
// name is one no HTTPS response ever writes, and vice versa (internal/api/
// cookie.go, "One cookie NAME per scheme"). See also
// TestMediaCookieNamesDoNotCollideAcrossListeners in cmd/obelo/tls_test.go.
const (
	mediaCookieName       = "ms_media_plain"
	secureMediaCookieName = "__Secure-ms_media"
	legacyMediaCookieName = "ms_media"
)

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
//
// The request also carries a contradicting `X-Forwarded-Proto: http`, and no
// proxy is trusted. r.TLS is the one scheme signal that is not hearsay — this
// process negotiated the handshake — so it wins outright and no header can turn
// it off. Without that the header would be a downgrade switch for anybody.
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
	req, err := http.NewRequest(http.MethodPost, tlsSrv.URL+"/api/v1/auth/login", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "http")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("HTTPS login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTPS login status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	cookie := findCookie(resp, secureMediaCookieName)
	if cookie == nil {
		t.Fatalf("HTTPS login did not set the %q cookie; headers: %v",
			secureMediaCookieName, resp.Header.Values("Set-Cookie"))
	}
	if !cookie.Secure {
		t.Errorf("cookie Secure = false under HTTPS, want true")
	}
	// And it did NOT also write the plain-HTTP name, which is what would let one
	// origin shadow the other's cookie in the browser's jar.
	if c := findCookie(resp, mediaCookieName); c != nil {
		t.Errorf("HTTPS login also set %q; the plain-HTTP name must be written by the plain listener alone", mediaCookieName)
	}
	// Sanity: TLS check used directly (avoids an unused import if the helper
	// changes) — the connection state confirms TLS was actually negotiated.
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Errorf("expected a TLS connection for the HTTPS login")
	}
}

// TestMediaCookieSecureViaForwardedProto: a plain-HTTP request carrying
// X-Forwarded-Proto: https FROM A TRUSTED PROXY (the reverse-proxy path,
// ADR-0005) also gets Secure. The listener is loopback, so trusting 127.0.0.1
// makes the test client the "proxy" — which is the whole point: the header does
// nothing until an operator says who may send it. The untrusted half of the pair
// is TestMediaCookieIgnoresForwardedProtoFromUntrustedPeer below.
func TestMediaCookieSecureViaForwardedProto(t *testing.T) {
	srv := testharness.New(t, testharness.WithTrustedProxies("127.0.0.1/32", "::1/128"))
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
	// A trusted proxy's "this was HTTPS" makes the request an HTTPS request for
	// every purpose, the cookie's NAME included — otherwise a proxy deployment
	// would get a Secure cookie under the plain-HTTP name, which is the shadowing
	// hazard again with an extra step.
	cookie := findCookie(resp, secureMediaCookieName)
	if cookie == nil {
		t.Fatalf("login behind a trusted proxy did not set the %q cookie; headers: %v",
			secureMediaCookieName, resp.Header.Values("Set-Cookie"))
	}
	if !cookie.Secure {
		t.Errorf("cookie Secure = false with X-Forwarded-Proto: https from a trusted proxy, want true")
	}
}

// TestMediaCookieIgnoresForwardedProtoFromUntrustedPeer is the half of the pair
// that ADR-0041 made necessary. Once the server terminates TLS itself, clients
// reach it directly and X-Forwarded-Proto is written by whoever connected — so on
// a server with no trusted-proxy configuration (the default, and every existing
// deployment) a stranger's `X-Forwarded-Proto: https` on a plain-HTTP request
// would mark the session cookie Secure, the browser would then decline to send it
// back over http, and the user's media would quietly stop working.
//
// Two servers, because "the header is ignored" and "the header is ignored FROM
// THIS PEER" are different claims and only the second one is a trust check: the
// first has no allowlist at all, the second has one that does not contain the
// caller.
func TestMediaCookieIgnoresForwardedProtoFromUntrustedPeer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted []string
	}{
		{name: "no allowlist configured"},
		{name: "peer outside the allowlist", trusted: []string{"10.0.0.0/8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t, testharness.WithTrustedProxies(tc.trusted...))
			setupAdmin(t, srv, "brandon", "hunter2hunter2")

			spoofed := http.Header{"X-Forwarded-Proto": []string{"https"}}
			var out struct {
				Token string `json:"token"`
			}
			status, header, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/login", "",
				"203.0.113.44:51000", spoofed, map[string]any{
					"username": "brandon",
					"password": "hunter2hunter2",
					"device":   map[string]any{"name": "Browser", "platform": "web", "clientId": "spoof-client"},
				}, &out)
			if status != http.StatusOK {
				t.Fatalf("login status = %d, want 200; body: %s", status, body)
			}
			cookie := findSetCookie(t, header, mediaCookieName)
			if cookie.Secure {
				t.Errorf("cookie Secure = true from an untrusted peer claiming X-Forwarded-Proto: https — " +
					"the header is attacker-supplied on the direct path, and marking the cookie " +
					"Secure over plain HTTP makes the browser stop sending it")
			}
		})
	}
}

// findSetCookie parses one named cookie out of a response's Set-Cookie headers.
// The JSONFrom helper hands back headers rather than an *http.Response, so the
// findCookie above (which takes a response) cannot be reused.
func findSetCookie(t *testing.T, header http.Header, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q cookie in Set-Cookie: %v", name, header.Values("Set-Cookie"))
	return nil
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

// --- the cookie-name split (internal/api/cookie.go) ------------------------
//
// The read half. The write half — that each listener writes only its own name —
// is asserted above for HTTPS and in TestMediaCookieNamesDoNotCollideAcrossListeners
// (cmd/obelo/tls_test.go), which is the one place both real listeners exist.

// mediaAuthProbe reports whether a set of cookies authenticates a media GET, with
// no Authorization header, and without needing a scanned library: an unknown
// session id is 404 once the CALLER is authenticated (handleSessionStream hides
// existence) and 401 when it is not, so the two statuses separate "the cookie was
// read and accepted" from "no credential was found" with nothing else in play.
func mediaAuthProbe(t *testing.T, srv *testharness.Server, cookies ...*http.Cookie) bool {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL("/api/v1/sessions/no-such-session/stream"), nil)
	if err != nil {
		t.Fatalf("building probe request: %v", err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe GET: %v", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return true
	case http.StatusUnauthorized:
		return false
	default:
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("probe status = %d, want 404 (authenticated) or 401 (not); body: %s", resp.StatusCode, raw)
		return false
	}
}

// TestLegacyMediaCookieStillAuthenticates: a session established BEFORE the
// name split still serves bytes. The upgrade renamed the plain-HTTP cookie, and
// a browser holding the old name would otherwise have every <img>/<video> start
// 401ing at deploy time with no way for the user to recover but a logout — the
// same silent breakage the split exists to fix, arriving from the other side.
func TestLegacyMediaCookieStillAuthenticates(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, _ := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")

	legacy := &http.Cookie{Name: legacyMediaCookieName, Value: token}
	if !mediaAuthProbe(t, srv, legacy) {
		t.Errorf("a %q cookie carrying a valid token did not authenticate; a pre-split session must survive the upgrade", legacyMediaCookieName)
	}
	if mediaAuthProbe(t, srv, &http.Cookie{Name: legacyMediaCookieName, Value: "not-a-real-token"}) {
		t.Errorf("a %q cookie carrying a garbage token authenticated; the legacy name is read, not trusted", legacyMediaCookieName)
	}
}

// TestMediaCookiePreferenceOrder: when several media cookies arrive at once, the
// name THIS listener writes wins over the legacy shared name. A browser can hold
// both — an unexpired pre-split cookie alongside a current one — and preferring
// the stale entry would authenticate as whoever held it, which is exactly the
// identity leak POST /auth/media-cookie exists to close (a re-issue writes the
// current name, so the reader must prefer it).
func TestMediaCookiePreferenceOrder(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, current := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")
	if current.Name != mediaCookieName {
		t.Fatalf("plain-HTTP login set cookie %q, want %q", current.Name, mediaCookieName)
	}

	stale := &http.Cookie{Name: legacyMediaCookieName, Value: "not-a-real-token"}
	live := &http.Cookie{Name: mediaCookieName, Value: token}
	if !mediaAuthProbe(t, srv, stale, live) {
		t.Errorf("a live %q cookie was not preferred over a stale %q one", mediaCookieName, legacyMediaCookieName)
	}
	// Order in the Cookie header must not decide it.
	if !mediaAuthProbe(t, srv, live, stale) {
		t.Errorf("preference changed with the order of the Cookie header; it must depend on the name alone")
	}
}

// TestLogoutClearsLegacyMediaCookieToo: logout expires this scheme's cookie AND
// the legacy shared name, so no media credential is left behind on the origin it
// was issued to. It must NOT clear the other scheme's name — that is a separate
// session on a separate origin, and reaching across is the cross-scheme
// interference the split removes.
func TestLogoutClearsLegacyMediaCookieToo(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	token, _ := loginWithCookie(t, srv, "brandon", "hunter2hunter2", "web-client")

	req, _ := http.NewRequest(http.MethodPost, srv.URL("/api/v1/auth/logout"), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	defer resp.Body.Close()

	for _, name := range []string{mediaCookieName, legacyMediaCookieName} {
		c := findCookie(resp, name)
		if c == nil {
			t.Errorf("logout emitted no Set-Cookie clearing %q", name)
			continue
		}
		if c.MaxAge > 0 {
			t.Errorf("cleared %q MaxAge = %d, want <= 0 (expired)", name, c.MaxAge)
		}
	}
	if c := findCookie(resp, secureMediaCookieName); c != nil {
		t.Errorf("a plain-HTTP logout cleared %q; that is the HTTPS origin's separate session", secureMediaCookieName)
	}
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
