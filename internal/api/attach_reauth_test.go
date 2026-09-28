package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the re-authentication an attach needs. A session alone
// never attaches an External identity: a User with a Local password supplies it
// in the attach request, and a User without one first signs in again through an
// identity they already hold, which answers a short-lived, single-use re-auth
// grant for that User and session to put in the attach.

const brandonPassword = "hunter2hunter2"

type reauthGrantResp struct {
	Grant     string `json:"grant"`
	ExpiresIn int    `json:"expiresIn"`
}

// attachPasswordWith is the profile's password form with proof in it.
func attachPasswordWith(t *testing.T, srv *testharness.Server, token string, body map[string]any) (int, []byte) {
	t.Helper()
	status, _, raw := srv.JSONFrom(http.MethodPost, "/api/v1/auth/external-identities/password", token, "", nil, body, nil)
	return status, raw
}

// startAttachWith is the profile's redirect button with proof in it; a 200 is
// read into the round trip.
func startAttachWith(t *testing.T, srv *testharness.Server, token string, body map[string]any) (int, []byte, redirectRun) {
	t.Helper()
	var out struct {
		URL string `json:"url"`
	}
	status, header, raw := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/attach/start", token, "", nil, body, &out)
	if status != http.StatusOK {
		return status, raw, redirectRun{}
	}
	return status, raw, redirectRunFrom(t, body["provider"].(string), out.URL, header)
}

// reauthPassword is the profile's "confirm it is you" form for a password-flow
// identity the caller holds.
func reauthPassword(t *testing.T, srv *testharness.Server, token, provider, username, password string) (int, []byte, reauthGrantResp) {
	t.Helper()
	var out reauthGrantResp
	status, _, raw := srv.JSONFrom(http.MethodPost, "/api/v1/auth/reauth/password", token, "", nil,
		map[string]any{"provider": provider, "username": username, "password": password}, &out)
	return status, raw, out
}

func wantReauthRequired(t *testing.T, what string, status int, body []byte) {
	t.Helper()
	if status != http.StatusForbidden || !strings.Contains(string(body), "REAUTH_REQUIRED") {
		t.Fatalf("%s = %d %s, want 403 REAUTH_REQUIRED", what, status, body)
	}
}

// TestAnAttachWithASessionButNoLocalPasswordIsRefused: brandon is signed in and
// has a Local password. An attach by either flow that leaves it out, or gets it
// wrong, is refused and attaches nothing; his session survives it. With it, both
// flows attach.
func TestAnAttachWithASessionButNoLocalPasswordIsRefused(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "bj:dir-pw:subject-bj::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	brandon := meID(t, srv, admin, "brandon")

	for _, proof := range []map[string]any{{}, {"currentPassword": "wrong"}, {"reauthGrant": "made-up"}} {
		body := map[string]any{"provider": "directory", "username": "bj", "password": "dir-pw"}
		for k, v := range proof {
			body[k] = v
		}
		status, raw := attachPasswordWith(t, srv, admin, body)
		wantReauthRequired(t, "password attach with "+fmt.Sprint(proof), status, raw)

		body = map[string]any{"provider": "oauth"}
		for k, v := range proof {
			body[k] = v
		}
		status, raw, _ = startAttachWith(t, srv, admin, body)
		wantReauthRequired(t, "redirect attach start with "+fmt.Sprint(proof), status, raw)
	}
	if got := identityKeys(srv, brandon); len(got) != 0 {
		t.Fatalf("brandon's identities after refused attaches = %v, want none", got)
	}
	if status, body := srv.AuthGET("/api/v1/devices", admin, nil); status != http.StatusOK {
		t.Fatalf("a refused attach ended the session: %d %s", status, body)
	}

	status, raw := attachPasswordWith(t, srv, admin, map[string]any{
		"provider": "directory", "username": "bj", "password": "dir-pw", "currentPassword": brandonPassword})
	if status != http.StatusOK {
		t.Fatalf("password attach with the Local password = %d %s, want 200", status, raw)
	}
	status, raw, run := startAttachWith(t, srv, admin, map[string]any{"provider": "oauth", "currentPassword": brandonPassword})
	if status != http.StatusOK {
		t.Fatalf("redirect attach start with the Local password = %d %s, want 200", status, raw)
	}
	if status, body, _ := finishAttach(t, srv, admin, run, "subject-bj-oauth|bj|"); status != http.StatusOK {
		t.Fatalf("redirect attach callback = %d %s, want 200", status, body)
	}
	if got := identityKeys(srv, brandon); len(got) != 2 {
		t.Fatalf("brandon's identities = %v, want both", got)
	}
}

// TestAnOutsideOnlyUserAttachesOnlyAfterAFreshReauth: ada exists only through
// the directory. Her session is not enough, and no password stands in for the
// Local password she does not have. A re-auth through her directory identity
// answers a grant, and the grant attaches — once.
func TestAnOutsideOnlyUserAttachesOnlyAfterAFreshReauth(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	adminToken(t, srv)
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	for _, proof := range []map[string]any{{}, {"currentPassword": "ada-pw"}, {"reauthGrant": "made-up"}} {
		body := map[string]any{"provider": "oauth"}
		for k, v := range proof {
			body[k] = v
		}
		status, raw, _ := startAttachWith(t, srv, ada.Token, body)
		wantReauthRequired(t, "ada's attach start with "+fmt.Sprint(proof), status, raw)
	}

	status, raw, grant := reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")
	if status != http.StatusOK || grant.Grant == "" || grant.ExpiresIn != 300 {
		t.Fatalf("re-auth = %d %+v, want 200 with a grant good for 300s; body: %s", status, grant, raw)
	}
	status, raw, run := startAttachWith(t, srv, ada.Token, map[string]any{"provider": "oauth", "reauthGrant": grant.Grant})
	if status != http.StatusOK {
		t.Fatalf("attach start with a fresh re-auth = %d %s, want 200", status, raw)
	}
	if status, body, _ := finishAttach(t, srv, ada.Token, run, "subject-ada-oauth|ada|"); status != http.StatusOK {
		t.Fatalf("attach callback = %d %s, want 200", status, body)
	}
	if got := identityKeys(srv, ada.User.ID); len(got) != 2 {
		t.Fatalf("ada's identities = %v, want two", got)
	}

	status, raw, _ = startAttachWith(t, srv, ada.Token, map[string]any{"provider": "oauth", "reauthGrant": grant.Grant})
	wantReauthRequired(t, "the same grant again", status, raw)
}

// TestAReauthGrantFromAnotherSessionIsRefused: a grant minted on ada's laptop is
// no good from her phone, nor to bj; a re-auth through an identity she does not hold, or
// with a password the directory rejects, answers no grant at all.
func TestAReauthGrantFromAnotherSessionIsRefused(t *testing.T) {
	t.Parallel()
	srv, _ := signInServer(t, map[string]string{
		"directory": "ada:ada-pw:subject-ada::;bj:bj-pw:subject-bj::"})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	phone := login(t, srv, "ada", "ada-pw", "Phone", "test", "ada-phone")
	bj := login(t, srv, "bj", "bj-pw", "Laptop", "test", "bj-client")

	_, _, grant := reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")
	if grant.Grant == "" {
		t.Fatal("no grant")
	}
	status, raw := attachPasswordWith(t, srv, phone.Token, map[string]any{
		"provider": "directory", "username": "bj", "password": "bj-pw", "reauthGrant": grant.Grant})
	wantReauthRequired(t, "a laptop grant from the phone", status, raw)
	_, _, grant = reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")
	status, raw = attachPasswordWith(t, srv, bj.Token, map[string]any{
		"provider": "directory", "username": "ada", "password": "ada-pw", "reauthGrant": grant.Grant})
	wantReauthRequired(t, "ada's grant presented by bj", status, raw)

	status, raw, out := reauthPassword(t, srv, ada.Token, "directory", "bj", "bj-pw")
	wantReauthRequired(t, "a re-auth through bj's identity", status, raw)
	if out.Grant != "" {
		t.Fatal("a refused re-auth answered a grant")
	}
	status, raw, _ = reauthPassword(t, srv, ada.Token, "directory", "ada", "wrong")
	if status != http.StatusUnauthorized || !strings.Contains(string(raw), "SIGN_IN_REFUSED") {
		t.Fatalf("a re-auth the directory rejects = %d %s, want 401 SIGN_IN_REFUSED", status, raw)
	}
	if status, _, _ := reauthPassword(t, srv, "", "directory", "ada", "ada-pw"); status != http.StatusUnauthorized {
		t.Fatalf("a re-auth with no session = %d, want 401", status)
	}
}

// TestRepeatedWrongReauthPasswordsAreThrottled: a password re-auth puts the
// credential to the directory exactly as a login does, so it is limited as one:
// wrong passwords, each for a different directory username so only the per-IP
// counter fills, end in 429 TOO_MANY_ATTEMPTS with a Retry-After — and then even
// ada's right one is refused.
func TestRepeatedWrongReauthPasswordsAreThrottled(t *testing.T) {
	t.Parallel()
	srv, _ := signInServer(t, map[string]string{"directory": "ada:ada-pw:subject-ada::"})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")

	const from = "203.0.113.9:51000"
	reauth := func(username, password string) (int, http.Header, []byte) {
		t.Helper()
		return srv.JSONFrom(http.MethodPost, "/api/v1/auth/reauth/password", ada.Token, from, nil,
			map[string]any{"provider": "directory", "username": username, "password": password}, nil)
	}
	var status int
	var header http.Header
	var raw []byte
	for i := 0; i < 64 && status != http.StatusTooManyRequests; i++ {
		status, header, raw = reauth(fmt.Sprintf("guess-%d", i), "wrong")
		if status != http.StatusUnauthorized && status != http.StatusTooManyRequests {
			t.Fatalf("attempt %d = %d %s, want 401 or 429", i, status, raw)
		}
	}
	if status != http.StatusTooManyRequests || !strings.Contains(string(raw), "TOO_MANY_ATTEMPTS") {
		t.Fatalf("64 wrong re-auth passwords were never throttled; last = %d %s", status, raw)
	}
	if header.Get("Retry-After") == "" {
		t.Fatal("a throttled re-auth has no Retry-After")
	}
	if status, _, raw := reauth("ada", "ada-pw"); status != http.StatusTooManyRequests {
		t.Fatalf("ada's right password once throttled = %d %s, want 429", status, raw)
	}
}

// TestARedirectReauthAnswersAGrant: newcomer exists only through the oauth
// provider. A redirect re-auth round trip through his own identity answers a
// grant that attaches the directory; one that comes back as somebody else
// answers none, and a re-auth's state is neither a sign-in nor an attach.
func TestARedirectReauthAnswersAGrant(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "nc:nc-pw:subject-nc-dir::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	run := startRedirect(t, srv, "oauth")
	status, body, newcomer := finishRedirect(t, srv, run, "subject-nc|newcomer|")
	if status != http.StatusOK {
		t.Fatalf("newcomer's first sign-in = %d %s", status, body)
	}

	startReauth := func() redirectRun {
		t.Helper()
		var out struct {
			URL string `json:"url"`
		}
		status, header, raw := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/reauth/start", newcomer.Token, "", nil,
			map[string]any{"provider": "oauth"}, &out)
		if status != http.StatusOK {
			t.Fatalf("re-auth start = %d %s, want 200", status, raw)
		}
		return redirectRunFrom(t, "oauth", out.URL, header)
	}
	finishReauth := func(run redirectRun, code string) (int, []byte, reauthGrantResp) {
		t.Helper()
		var out reauthGrantResp
		headers := http.Header{}
		headers.Set("Cookie", run.cookie)
		status, _, raw := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/reauth/callback", newcomer.Token, "", headers,
			map[string]any{"state": run.state, "code": code}, &out)
		return status, raw, out
	}

	status, raw, out := finishReauth(startReauth(), "subject-someone-else|someone|")
	wantReauthRequired(t, "a re-auth that came back as somebody else", status, raw)
	if out.Grant != "" {
		t.Fatal("a refused re-auth answered a grant")
	}

	before := listUsernames(t, srv, admin)
	status, raw, login := finishRedirect(t, srv, startReauth(), "subject-nc|newcomer|")
	wantRefused(t, srv, admin, status, raw, login, before)
	status, raw, _ = finishAttach(t, srv, newcomer.Token, startReauth(), "subject-nc|newcomer|")
	if status != http.StatusUnauthorized || !strings.Contains(string(raw), "SIGN_IN_REFUSED") {
		t.Fatalf("a re-auth state at the attach callback = %d %s, want 401 SIGN_IN_REFUSED", status, raw)
	}

	status, raw, out = finishReauth(startReauth(), "subject-nc|newcomer|")
	if status != http.StatusOK || out.Grant == "" {
		t.Fatalf("a re-auth through newcomer's own identity = %d %s, want 200 with a grant", status, raw)
	}
	status, raw = attachPasswordWith(t, srv, newcomer.Token, map[string]any{
		"provider": "directory", "username": "nc", "password": "nc-pw", "reauthGrant": out.Grant})
	if status != http.StatusOK {
		t.Fatalf("attach with the redirect re-auth's grant = %d %s, want 200", status, raw)
	}
	if got := identityKeys(srv, newcomer.User.ID); len(got) != 2 {
		t.Fatalf("newcomer's identities = %v, want two", got)
	}
}
