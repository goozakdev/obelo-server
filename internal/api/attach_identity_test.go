package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for attaching an External identity from one's own profile
// (ADR-0063 decision 3): a signed-in User completes a password-flow check or a
// redirect-flow round trip, and the (plugin id, subject) it answers is linked to
// THAT User — never a new Member, never the username-collision refusal, and never
// an identity somebody else already holds.

type attachedIdentityResp struct {
	Identity struct {
		Provider     string `json:"provider"`
		ProviderName string `json:"providerName"`
		Username     string `json:"username"`
	} `json:"identity"`
}

// asBrandon is the proof brandon's attaches carry: his Local password.
var asBrandon = map[string]any{"currentPassword": brandonPassword}

// withProof is body with the proof fields added.
func withProof(body, proof map[string]any) map[string]any {
	for k, v := range proof {
		body[k] = v
	}
	return body
}

// attachPassword is the profile's password form: POST
// /auth/external-identities/password as the holder of token, carrying proof.
func attachPassword(t *testing.T, srv *testharness.Server, token string, proof map[string]any, provider, username, password string) (int, []byte) {
	t.Helper()
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/external-identities/password", token, "", nil,
		withProof(map[string]any{"provider": provider, "username": username, "password": password}, proof), nil)
	return status, body
}

// startAttach is the profile's redirect button: POST /auth/redirect/attach/start
// as the holder of token, carrying proof.
func startAttach(t *testing.T, srv *testharness.Server, token string, proof map[string]any, provider string) redirectRun {
	t.Helper()
	var out struct {
		URL string `json:"url"`
	}
	status, header, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/attach/start", token, "", nil,
		withProof(map[string]any{"provider": provider}, proof), &out)
	if status != http.StatusOK {
		t.Fatalf("attach start %s = %d, want 200; body: %s", provider, status, body)
	}
	return redirectRunFrom(t, provider, out.URL, header)
}

// redirectRunFrom reads a start's answer the way startRedirectFrom does.
func redirectRunFrom(t *testing.T, provider, rawURL string, header http.Header) redirectRun {
	t.Helper()
	run := redirectRun{}
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("the authorize URL %q does not parse: %v", rawURL, err)
	}
	q := u.Query()
	run.authorize, run.state, run.nonce, run.challenge, run.redirectURI =
		u, q.Get("state"), q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")
	for _, c := range (&http.Response{Header: header}).Cookies() {
		if strings.Contains(c.Name, "obelo_sign_in") {
			run.cookie = c.Name + "=" + c.Value
		}
	}
	if run.state == "" || run.challenge == "" || run.cookie == "" {
		t.Fatalf("start %s handed back no state, challenge or binding cookie: url %s", provider, rawURL)
	}
	return run
}

// finishAttach is the SPA's callback screen for an attach: POST
// /auth/redirect/attach/callback as the holder of token, with the binding cookie.
func finishAttach(t *testing.T, srv *testharness.Server, token string, run redirectRun, code string) (int, []byte, attachedIdentityResp) {
	t.Helper()
	var out attachedIdentityResp
	headers := http.Header{}
	if run.cookie != "" {
		headers.Set("Cookie", run.cookie)
	}
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/attach/callback", token, "", headers,
		map[string]any{"state": run.state, "code": code}, &out)
	return status, body, out
}

// meID is the id of the User named username, read from the Admin list.
func meID(t *testing.T, srv *testharness.Server, admin, username string) string {
	t.Helper()
	var users usersListResp
	if status, body := srv.AuthGET("/api/v1/users", admin, &users); status != http.StatusOK {
		t.Fatalf("GET /users status = %d; body: %s", status, body)
	}
	for _, u := range users.Users {
		if u.Username == username {
			return u.ID
		}
	}
	t.Fatalf("no user named %q", username)
	return ""
}

func identityKeys(srv *testharness.Server, userID string) []string {
	var out []string
	for _, x := range srv.ExternalIdentities(userID) {
		out = append(out, x.PluginID+"/"+x.Subject)
	}
	return out
}

// TestAttachingAPasswordFlowIdentityLinksItToTheCallersOwnUser: brandon, signed
// in with his Local password, attaches the directory's subject-bj from his
// profile. No User is created, brandon holds the identity, his session still
// works, and signing in later through the directory — even under the username
// "brandon", which before the attach was the collision refusal — is brandon.
func TestAttachingAPasswordFlowIdentityLinksItToTheCallersOwnUser(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "brandon:dir-pw:subject-bj::family"})
	brandon := meID(t, srv, admin, "brandon")

	if status, _, body := signInAttempt(t, srv, "brandon", "dir-pw"); status != http.StatusConflict ||
		!strings.Contains(string(body), "SIGN_IN_USERNAME_TAKEN") {
		t.Fatalf("before the attach, the directory's brandon = %d %s, want 409 SIGN_IN_USERNAME_TAKEN", status, body)
	}

	before := listUsernames(t, srv, admin)
	var out attachedIdentityResp
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/external-identities/password", admin, "", nil,
		map[string]any{"provider": "directory", "username": "brandon", "password": "dir-pw", "currentPassword": brandonPassword}, &out)
	if status != http.StatusOK || out.Identity.Provider != "directory" || out.Identity.Username != "brandon" {
		t.Fatalf("attach = %d %+v, want 200 naming the directory identity; body: %s", status, out, body)
	}
	if after := listUsernames(t, srv, admin); len(after) != len(before) {
		t.Fatalf("users after an attach = %v, want still %v", after, before)
	}
	if got := identityKeys(srv, brandon); len(got) != 1 || got[0] != "directory/subject-bj" {
		t.Fatalf("brandon's identities = %v, want [directory/subject-bj]", got)
	}
	if status, body := srv.AuthGET("/api/v1/devices", admin, nil); status != http.StatusOK {
		t.Fatalf("brandon's session after the attach = %d, want still good; body: %s", status, body)
	}

	later := login(t, srv, "brandon", "dir-pw", "Phone", "test", "phone-client")
	if later.User.ID != brandon {
		t.Fatalf("a later directory sign-in signed in as %q, want brandon %q", later.User.ID, brandon)
	}
}

// TestAttachingARedirectFlowIdentityLinksItToTheCallersOwnUser: the same through
// the Bundled OpenID Connect plugin. The ID token is verified exactly as a
// sign-in's is, and the verified subject is what brandon gains.
func TestAttachingARedirectFlowIdentityLinksItToTheCallersOwnUser(t *testing.T) {
	srv, admin, idp := oidcServer(t)
	brandon := meID(t, srv, admin, "brandon")

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-0", idp.sign(idp.claims(run, "subject-bj", "brandon"), idp.key), userinfo("subject-bj", "brandon"))
	if status, body, _ := finishRedirect(t, srv, run, "code-0"); status != http.StatusConflict {
		t.Fatalf("before the attach, the provider's brandon = %d %s, want 409", status, body)
	}

	before := listUsernames(t, srv, admin)
	run = startAttach(t, srv, admin, asBrandon, "oidc")
	if !strings.HasSuffix(run.redirectURI, "/sign-in/callback") {
		t.Fatalf("attach redirect_uri = %q, want the one callback a sign-in uses", run.redirectURI)
	}
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-bj", "brandon"), idp.key), userinfo("subject-bj", "brandon"))
	status, body, out := finishAttach(t, srv, admin, run, "code-1")
	if status != http.StatusOK || out.Identity.Provider != "oidc" {
		t.Fatalf("attach callback = %d %+v, want 200; body: %s", status, out, body)
	}
	if after := listUsernames(t, srv, admin); len(after) != len(before) {
		t.Fatalf("users after an attach = %v, want still %v", after, before)
	}
	if got := identityKeys(srv, brandon); len(got) != 1 || got[0] != "oidc/subject-bj" {
		t.Fatalf("brandon's identities = %v, want [oidc/subject-bj]", got)
	}
	if status, body := srv.AuthGET("/api/v1/devices", admin, nil); status != http.StatusOK {
		t.Fatalf("brandon's session after the attach = %d, want still good; body: %s", status, body)
	}

	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-2", idp.sign(idp.claims(run, "subject-bj", "brandon"), idp.key), userinfo("subject-bj", "brandon"))
	status, body, later := finishRedirect(t, srv, run, "code-2")
	if status != http.StatusOK || later.User.ID != brandon {
		t.Fatalf("a later redirect sign-in = %d as %q, want 200 as brandon %q; body: %s", status, later.User.ID, brandon, body)
	}
}

// TestAttachingAnIdentityAnotherUserHoldsIsRefused: ada signed in through the
// directory first, so she holds subject-ada. brandon, who knows her directory
// password, tries to attach it — by either flow. Refused, and neither User's
// identities change.
func TestAttachingAnIdentityAnotherUserHoldsIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::"))
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	brandon := meID(t, srv, admin, "brandon")

	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	_, _, grant := reauthPassword(t, srv, ada.Token, "directory", "ada", "ada-pw")
	run := startAttach(t, srv, ada.Token, map[string]any{"reauthGrant": grant.Grant}, "oauth")
	if status, body, _ := finishAttach(t, srv, ada.Token, run, "subject-ada-oauth|ada|"); status != http.StatusOK {
		t.Fatalf("ada attaching her oauth identity = %d; body: %s", status, body)
	}
	adaBefore := identityKeys(srv, ada.User.ID)

	status, body := attachPassword(t, srv, admin, asBrandon, "directory", "ada", "ada-pw")
	if status != http.StatusConflict || !strings.Contains(string(body), "EXTERNAL_IDENTITY_HELD") {
		t.Fatalf("brandon attaching ada's directory identity = %d %s, want 409 EXTERNAL_IDENTITY_HELD", status, body)
	}
	run = startAttach(t, srv, admin, asBrandon, "oauth")
	status, body, _ = finishAttach(t, srv, admin, run, "subject-ada-oauth|ada|")
	if status != http.StatusConflict || !strings.Contains(string(body), "EXTERNAL_IDENTITY_HELD") {
		t.Fatalf("brandon attaching ada's oauth identity = %d %s, want 409 EXTERNAL_IDENTITY_HELD", status, body)
	}
	if strings.Contains(string(body), "ada") {
		t.Fatalf("the refusal names the holder: %s", body)
	}

	if got := identityKeys(srv, brandon); len(got) != 0 {
		t.Fatalf("brandon's identities = %v, want none", got)
	}
	if got := identityKeys(srv, ada.User.ID); strings.Join(got, ",") != strings.Join(adaBefore, ",") {
		t.Fatalf("ada's identities = %v, want unchanged %v", got, adaBefore)
	}
	if again := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client"); again.User.ID != ada.User.ID {
		t.Fatalf("ada's directory sign-in after the refused attach = %q, want ada %q", again.User.ID, ada.User.ID)
	}
}

// TestAnAttachIsOnlyForTheSignedInCaller: attaching needs a session; a password
// the directory rejects links nothing; and re-attaching an identity the caller
// already holds is not a refusal.
func TestAnAttachIsOnlyForTheSignedInCaller(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "bj:bj-pw:subject-bj::"})
	brandon := meID(t, srv, admin, "brandon")

	if status, body := attachPassword(t, srv, "", asBrandon, "directory", "bj", "bj-pw"); status != http.StatusUnauthorized {
		t.Fatalf("an attach with no session = %d %s, want 401", status, body)
	}
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/attach/start", "", "", nil,
		map[string]any{"provider": "oidc"}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("an attach start with no session = %d %s, want 401", status, body)
	}
	if status, body := attachPassword(t, srv, admin, asBrandon, "directory", "bj", "wrong"); status != http.StatusUnauthorized ||
		!strings.Contains(string(body), "SIGN_IN_REFUSED") {
		t.Fatalf("an attach the directory rejects = %d %s, want 401 SIGN_IN_REFUSED", status, body)
	}
	if status, body := attachPassword(t, srv, admin, asBrandon, "nobody", "bj", "bj-pw"); status != http.StatusNotFound {
		t.Fatalf("an attach naming no provider = %d %s, want 404", status, body)
	}
	if status, body := srv.AuthGET("/api/v1/devices", admin, nil); status != http.StatusOK {
		t.Fatalf("a refused attach ended the session: %d %s", status, body)
	}
	if got := identityKeys(srv, brandon); len(got) != 0 {
		t.Fatalf("identities after refusals = %v, want none", got)
	}
	for range 2 {
		if status, body := attachPassword(t, srv, admin, asBrandon, "directory", "bj", "bj-pw"); status != http.StatusOK {
			t.Fatalf("attaching (again) = %d %s, want 200", status, body)
		}
	}
	if got := identityKeys(srv, brandon); len(got) != 1 {
		t.Fatalf("identities after attaching twice = %v, want one", got)
	}
}

// TestAnAttachRoundTripBelongsToWhoStartedIt: a started attach is good only for
// the User who started it, at the attach callback; a started sign-in is good
// only at the sign-in callback. Either crossing is the one refusal and links or
// signs in nobody.
func TestAnAttachRoundTripBelongsToWhoStartedIt(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	brandon := meID(t, srv, admin, "brandon")
	if status, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "mallory", "password": "mallory-pw-long", "role": "member"}, nil); status != http.StatusCreated {
		t.Fatalf("creating mallory = %d; body: %s", status, body)
	}
	mallory := login(t, srv, "mallory", "mallory-pw-long", "Laptop", "test", "mallory-client")

	// mallory starts an attach and gets brandon's browser to finish it.
	run := startAttach(t, srv, mallory.Token, map[string]any{"currentPassword": "mallory-pw-long"}, "oauth")
	status, body, _ := finishAttach(t, srv, admin, run, "subject-bj|brandon|")
	if status != http.StatusUnauthorized || !strings.Contains(string(body), "SIGN_IN_REFUSED") {
		t.Fatalf("an attach finished by another User = %d %s, want 401 SIGN_IN_REFUSED", status, body)
	}

	// An attach's state is no sign-in.
	before := listUsernames(t, srv, admin)
	run = startAttach(t, srv, admin, asBrandon, "oauth")
	status, body, out := finishRedirect(t, srv, run, "subject-new|newcomer|")
	wantRefused(t, srv, admin, status, body, out, before)

	// A sign-in's state is no attach.
	run = startRedirect(t, srv, "oauth")
	status, body, _ = finishAttach(t, srv, admin, run, "subject-bj|brandon|")
	if status != http.StatusUnauthorized || !strings.Contains(string(body), "SIGN_IN_REFUSED") {
		t.Fatalf("a sign-in state at the attach callback = %d %s, want 401 SIGN_IN_REFUSED", status, body)
	}

	for _, id := range []string{brandon, mallory.User.ID} {
		if got := identityKeys(srv, id); len(got) != 0 {
			t.Fatalf("identities of %s after crossed round trips = %v, want none", id, got)
		}
	}
}

// TestTheProfileListsTheCallersIdentitiesAndWhatTheyCanAttach: GET
// /auth/external-identities answers the caller's own identities and the
// providers of each flow they can attach from.
func TestTheProfileListsTheCallersIdentitiesAndWhatTheyCanAttach(t *testing.T) {
	srv, admin := signInServer(t, map[string]string{"directory": "bj:bj-pw:subject-bj::"})
	if status, body := attachPassword(t, srv, admin, asBrandon, "directory", "bj", "bj-pw"); status != http.StatusOK {
		t.Fatalf("attach = %d %s", status, body)
	}
	var view struct {
		Identities []struct {
			Provider     string `json:"provider"`
			ProviderName string `json:"providerName"`
			Username     string `json:"username"`
		} `json:"identities"`
		Password []struct {
			ID string `json:"id"`
		} `json:"password"`
		Redirect []struct {
			ID string `json:"id"`
		} `json:"redirect"`
	}
	status, body := srv.AuthGET("/api/v1/auth/external-identities", admin, &view)
	if status != http.StatusOK {
		t.Fatalf("GET /auth/external-identities = %d; body: %s", status, body)
	}
	if len(view.Identities) != 1 || view.Identities[0].Provider != "directory" ||
		view.Identities[0].ProviderName == "" || view.Identities[0].Username != "bj" {
		t.Fatalf("identities = %+v, want the directory's bj", view.Identities)
	}
	if len(view.Password) != 1 || view.Password[0].ID != "directory" || view.Redirect == nil {
		t.Fatalf("attachable = %+v / %+v, want the directory and a redirect list", view.Password, view.Redirect)
	}
	if status, _ := srv.AuthGET("/api/v1/auth/external-identities", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("GET with no session = %d, want 401", status)
	}
}
