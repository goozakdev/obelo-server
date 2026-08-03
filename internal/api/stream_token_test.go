package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for session stream tokens (.scratch/session-stream-tokens,
// issue 01): the Decision carries one, a live session can re-mint one, ending or
// reaping the session revokes them, and the credential is refused everywhere a
// bearer is expected.
//
// The token's own media routes (/stream/{token}/…) are issue 02, so what is
// asserted here is minting, revocation, and — the security claim — every REFUSAL
// that must hold before those routes exist.

// streamTokenResp is the body of POST /sessions/{id}/stream-token.
type streamTokenResp struct {
	StreamToken          string `json:"streamToken"`
	StreamTokenExpiresAt string `json:"streamTokenExpiresAt"`
	ExpiresIn            int    `json:"expiresIn"`
}

// negotiateDune sits in playback_test.go; this is the same call with the response
// kept, for the tests below that need both the session id and the token.
func streamTokenSession(t *testing.T, srv *testharness.Server, token, titleID string) decisionResp {
	t.Helper()
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	return dec
}

// duneSession scans the fixture library and negotiates one Dune session, the
// arrange step every test here shares.
func duneSession(t *testing.T, srv *testharness.Server) (token, titleID string, dec decisionResp) {
	t.Helper()
	token = adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	titleID = findTitle(t, list, "Dune")
	return token, titleID, streamTokenSession(t, srv, token, titleID)
}

// TestDecisionCarriesStreamToken: the Decision mints the token with the session,
// so a client that will hand the URL to a receiver needs one request, not two.
func TestDecisionCarriesStreamToken(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)

	if dec.StreamToken == "" {
		t.Fatal("decision carried no streamToken")
	}
	if dec.StreamToken == dec.SessionID {
		t.Fatal("streamToken is the session id, not a secret")
	}
	if len(dec.StreamToken) != 43 {
		t.Errorf("streamToken is %d chars, want 43 (256 bits of base64url)", len(dec.StreamToken))
	}
	if escaped := url.PathEscape(dec.StreamToken); escaped != dec.StreamToken {
		t.Errorf("streamToken %q escapes to %q — it must ride a URL path unaltered", dec.StreamToken, escaped)
	}
	expires, err := time.Parse(time.RFC3339, dec.StreamTokenExpiresAt)
	if err != nil {
		t.Fatalf("streamTokenExpiresAt %q is not RFC3339: %v", dec.StreamTokenExpiresAt, err)
	}
	if until := time.Until(expires); until <= time.Hour || until > 5*time.Hour {
		t.Errorf("streamTokenExpiresAt is %v away; want the ~4h session-scoped TTL", until)
	}
}

// TestDecisionStreamTokensAreDistinctPerSession: two negotiations are two
// sessions and two credentials — a token is never shared between streams.
func TestDecisionStreamTokensAreDistinctPerSession(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	first := streamTokenSession(t, srv, token, duneID)
	second := streamTokenSession(t, srv, token, duneID)
	if first.SessionID == second.SessionID {
		t.Fatal("two negotiations reused one session id")
	}
	if first.StreamToken == second.StreamToken {
		t.Fatal("two sessions were handed the same stream token")
	}
}

// TestMintStreamTokenEndpoint: POST /sessions/{id}/stream-token re-mints for a
// live session, because a session outlives a short-TTL token and a client may
// decide to AirPlay long after it negotiated.
func TestMintStreamTokenEndpoint(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token, _, dec := duneSession(t, srv)

	var out streamTokenResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/stream-token", token, nil, &out)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d, want 201; body: %s", status, body)
	}
	if out.StreamToken == "" {
		t.Fatal("mint returned no streamToken")
	}
	if out.StreamToken == dec.StreamToken {
		t.Fatal("re-mint returned the Decision's token rather than a fresh one")
	}
	if _, err := time.Parse(time.RFC3339, out.StreamTokenExpiresAt); err != nil {
		t.Errorf("streamTokenExpiresAt %q is not RFC3339: %v", out.StreamTokenExpiresAt, err)
	}
	if out.ExpiresIn <= 0 {
		t.Errorf("expiresIn = %d, want a positive number of seconds", out.ExpiresIn)
	}
	// Both credentials are live: re-minting adds one, it does not rotate the old
	// one out from under a receiver that is already playing.
	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 2 {
		t.Errorf("tokens for the session = %d, want 2", n)
	}
}

// TestMintStreamTokenForAnotherUsersSessionIs404: 404, never 403 — the same
// existence-hiding posture the stream handlers take. A caller must not learn that
// somebody else's session id is real from the shape of the refusal.
func TestMintStreamTokenForAnotherUsersSessionIs404(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)

	srv.CreateMember("member", "memberpass123")
	other := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	status, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/stream-token", other, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("foreign-session mint = %d, want 404; body: %s", status, body)
	}
	if strings.Contains(string(body), "FORBIDDEN") {
		t.Errorf("refusal leaked a FORBIDDEN code: %s", body)
	}
	// An id that never existed must produce the SAME answer, or the difference
	// between the two is itself the disclosure.
	unknownStatus, unknownBody := srv.JSON(http.MethodPost,
		"/api/v1/sessions/00000000-0000-4000-8000-000000000000/stream-token", other, nil, nil)
	if unknownStatus != status || !bytes.Equal(unknownBody, body) {
		t.Errorf("unknown-session mint (%d, %s) differs from foreign-session mint (%d, %s)",
			unknownStatus, unknownBody, status, body)
	}
	// And nothing was minted for the session the Member does not own.
	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 1 {
		t.Errorf("tokens after the refused mint = %d, want the owner's 1", n)
	}
}

// TestMintStreamTokenIsBearerOnly: minting a media credential is an account-level
// act. A lone ms_media cookie must not do it, and neither must an absent
// credential — otherwise a token handed to a television could extend its own life.
func TestMintStreamTokenIsBearerOnly(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	_, _, dec := duneSession(t, srv)
	path := "/api/v1/sessions/" + dec.SessionID + "/stream-token"

	if status, _ := srv.JSON(http.MethodPost, path, "", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated mint = %d, want 401", status)
	}

	// A cookie-only request: the same credential the browser's <video> uses.
	srv.CreateMember("cookieuser", "cookiepass1234")
	_, cookie := loginWithCookie(t, srv, "cookieuser", "cookiepass1234", "browser-client")
	req, err := http.NewRequest(http.MethodPost, srv.URL(path), nil)
	if err != nil {
		t.Fatalf("building cookie mint request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cookie mint request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("cookie-only mint = %d, want 401; body: %s", resp.StatusCode, raw)
	}
}

// TestMintStreamTokenRejectsOtherMethods: the route is POST. A GET must not
// quietly become a way to obtain a credential from a URL alone.
func TestMintStreamTokenRejectsOtherMethods(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token, _, dec := duneSession(t, srv)
	path := "/api/v1/sessions/" + dec.SessionID + "/stream-token"

	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		status, body := srv.JSON(method, path, token, nil, nil)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405; body: %s", method, path, status, body)
		}
	}
}

// TestStreamTokenIsRefusedAsABearer is half of "the two namespaces do not
// overlap": a stream token authorises media for one session and NOTHING else —
// not a JSON route, not a progress report, not a session delete.
func TestStreamTokenIsRefusedAsABearer(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")
	dec := streamTokenSession(t, srv, token, duneID)
	if dec.StreamToken == "" {
		t.Fatal("decision carried no streamToken")
	}

	refusals := []struct {
		name, method, path string
		body               any
	}{
		{"title JSON", http.MethodGet, "/api/v1/titles/" + duneID, nil},
		{"home JSON", http.MethodGet, "/api/v1/home", nil},
		{"devices JSON", http.MethodGet, "/api/v1/devices", nil},
		{"negotiate", http.MethodPost, "/api/v1/titles/" + duneID + "/playback", mp4Profile()},
		{"progress", http.MethodPost, "/api/v1/sessions/" + dec.SessionID + "/progress",
			map[string]any{"positionMs": 1000, "state": "playing"}},
		{"re-mint", http.MethodPost, "/api/v1/sessions/" + dec.SessionID + "/stream-token", nil},
		{"end session", http.MethodDelete, "/api/v1/sessions/" + dec.SessionID, nil},
	}
	for _, c := range refusals {
		status, body := srv.JSON(c.method, c.path, dec.StreamToken, c.body, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s with a stream token as bearer = %d, want 401; body: %s", c.name, status, body)
		}
	}
	// The session is untouched by all of that: still endable by its real owner.
	if status, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil); status != http.StatusNoContent {
		t.Errorf("owner delete after the refusals = %d, want 204", status)
	}
}

// TestStreamTokenIsRefusedAsAQueryToken: ?token= is scoped to exactly one route
// (GET /files/{id}/download) and this ticket does not widen it. A stream token
// must not sneak in there either — it is not an account credential, and that
// route is sessionless.
func TestStreamTokenIsRefusedAsAQueryToken(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")
	dec := streamTokenSession(t, srv, token, duneID)

	// The stream URL is session-scoped; a stream token in ?token= must not open it.
	resp := srv.Do(http.MethodGet, dec.StreamURL+"?token="+url.QueryEscape(dec.StreamToken), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("stream with ?token=<streamToken> = %d, want 401; body: %s", resp.StatusCode, raw)
	}
}

// TestDeleteSessionRevokesStreamTokens: session end IS revocation, and it takes
// EVERY token the session minted — a client that re-minted twice keeps nothing.
func TestDeleteSessionRevokesStreamTokens(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token, _, dec := duneSession(t, srv)

	var extra streamTokenResp
	if status, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+dec.SessionID+"/stream-token", token, nil, &extra); status != http.StatusCreated {
		t.Fatalf("re-mint status = %d, want 201; body: %s", status, body)
	}
	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 2 {
		t.Fatalf("tokens before delete = %d, want 2", n)
	}

	if status, body := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+dec.SessionID, token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, body)
	}
	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 0 {
		t.Errorf("tokens after delete = %d, want 0 — DELETE /sessions/{id} must revoke", n)
	}
}

// TestReapedSessionRevokesStreamTokens: the idle reaper ends a session exactly as
// a clean DELETE does, so it must revoke the same way. An abandoned session that
// left a live credential behind would be the quiet failure this cascade exists to
// prevent — nobody is watching when the reaper runs.
func TestReapedSessionRevokesStreamTokens(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithSessionIdleTimeout(150*time.Millisecond))
	_, _, dec := duneSession(t, srv)

	if n := srv.CountStreamTokensForSession(dec.SessionID); n != 1 {
		t.Fatalf("tokens before the reap = %d, want 1", n)
	}
	// Stay silent — no progress reports — and let the reaper sweep.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.CountStreamTokensForSession(dec.SessionID) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("tokens after reaping = %d, want 0 — the reaper must revoke like a DELETE does",
		srv.CountStreamTokensForSession(dec.SessionID))
}

// TestStreamTokenIsNotEchoedByOtherEndpoints: the secret is returned exactly
// twice — by the Decision that minted it and by the re-mint endpoint — and by
// nothing else. Anything that echoed it back is another place it can be logged.
func TestStreamTokenIsNotEchoedByOtherEndpoints(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token, titleID, dec := duneSession(t, srv)

	checks := []struct {
		name, method, path string
		body               any
	}{
		{"progress", http.MethodPost, "/api/v1/sessions/" + dec.SessionID + "/progress",
			map[string]any{"positionMs": 1000, "state": "playing"}},
		{"title detail", http.MethodGet, "/api/v1/titles/" + titleID, nil},
		{"transcoding", http.MethodGet, "/api/v1/transcoding", nil},
	}
	for _, c := range checks {
		_, body := srv.JSON(c.method, c.path, token, c.body, nil)
		if bytes.Contains(body, []byte(dec.StreamToken)) {
			t.Errorf("%s echoed the stream token: %s", c.name, body)
		}
	}
}
