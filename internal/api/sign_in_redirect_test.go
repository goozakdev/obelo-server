package api_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for the Sign-in provider Extension point's REDIRECT flow
// (ADR-0063 decisions 2 and 8), through the Bundled OpenID Connect plugin and a
// plain OAuth2 test plugin.
//
// The identity provider is a real HTTP server in this process: a discovery
// document, a JWKS, a token endpoint that checks the PKCE verifier against the
// challenge it was sent, and a userinfo endpoint. Each test says what ID token
// the provider issues and, separately, what its userinfo reports — which is what
// the Bundled plugin answers as the identity beside the token — so a test can
// make the two disagree and see which one the server believed.

const (
	redirectClientID = "obelo-client"
	redirectKeyID    = "key-1"
)

// fakeIdP is an OpenID Connect provider serving one client.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	grants map[string]idpGrant // by code
}

// idpGrant is what one code redeems for.
type idpGrant struct {
	idToken   string
	userinfo  map[string]any
	challenge string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the provider's key: %v", err)
	}
	idp := &fakeIdP{t: t, key: key, grants: map[string]idpGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := idp.srv.URL
		writeTestJSON(w, map[string]any{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"userinfo_endpoint":      base + "/userinfo",
			"jwks_uri":               base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := idp.key.PublicKey
		writeTestJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": redirectKeyID, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		g, ok := idp.grants[r.PostForm.Get("code")]
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		switch {
		case !ok, r.PostForm.Get("grant_type") != "authorization_code",
			r.PostForm.Get("client_id") != redirectClientID,
			base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
			w.WriteHeader(http.StatusBadRequest)
			writeTestJSON(w, map[string]any{"error": "invalid_grant"})
			return
		}
		writeTestJSON(w, map[string]any{
			"access_token": "access-" + r.PostForm.Get("code"),
			"token_type":   "Bearer",
			"id_token":     g.idToken,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer access-")
		idp.mu.Lock()
		g, ok := idp.grants[code]
		idp.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, g.userinfo)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// claims is an ID token's payload that verifies for run: this provider's
// issuer, the client id, run's nonce, an hour to live. A test overrides what it
// means to break.
func (idp *fakeIdP) claims(run redirectRun, subject, username string, groups ...string) map[string]any {
	c := map[string]any{
		"iss":                idp.srv.URL,
		"aud":                redirectClientID,
		"sub":                subject,
		"nonce":              run.nonce,
		"iat":                time.Now().Unix(),
		"exp":                time.Now().Add(time.Hour).Unix(),
		"preferred_username": username,
	}
	if groups != nil {
		c["groups"] = groups
	}
	return c
}

// sign is claims as a compact RS256 JWS under key, with this provider's key id.
func (idp *fakeIdP) sign(claims map[string]any, key *rsa.PrivateKey) string {
	idp.t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": redirectKeyID})
	payload, err := json.Marshal(claims)
	if err != nil {
		idp.t.Fatalf("encoding claims: %v", err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		idp.t.Fatalf("signing: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// grant makes code redeem, for the run's PKCE challenge, to idToken, and makes
// the userinfo endpoint report userinfo for it.
func (idp *fakeIdP) grant(run redirectRun, code, idToken string, userinfo map[string]any) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.grants[code] = idpGrant{idToken: idToken, userinfo: userinfo, challenge: run.challenge}
}

// userinfo is what the provider's userinfo endpoint reports: the identity the
// Bundled plugin answers beside the token.
func userinfo(subject, username string, groups ...string) map[string]any {
	u := map[string]any{"sub": subject, "preferred_username": username}
	if groups != nil {
		u["groups"] = groups
	}
	return u
}

// redirectRun is one started sign-in, as the browser holds it.
type redirectRun struct {
	authorize   *url.URL
	state       string
	nonce       string
	challenge   string
	redirectURI string
	cookie      string
}

// startRedirect is the login screen's button: POST /auth/redirect/start.
func startRedirect(t *testing.T, srv *testharness.Server, provider string) redirectRun {
	t.Helper()
	return startRedirectFrom(t, srv, provider, "", nil)
}

// startRedirectFrom is startRedirect made from remoteAddr with headers.
func startRedirectFrom(t *testing.T, srv *testharness.Server, provider, remoteAddr string, headers http.Header) redirectRun {
	t.Helper()
	var out struct {
		URL string `json:"url"`
	}
	status, header, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", remoteAddr, headers,
		map[string]any{"provider": provider}, &out)
	if status != http.StatusOK {
		t.Fatalf("start %s = %d, want 200; body: %s", provider, status, body)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatalf("the authorize URL %q does not parse: %v", out.URL, err)
	}
	q := u.Query()
	run := redirectRun{
		authorize:   u,
		state:       q.Get("state"),
		nonce:       q.Get("nonce"),
		challenge:   q.Get("code_challenge"),
		redirectURI: q.Get("redirect_uri"),
	}
	for _, c := range (&http.Response{Header: header}).Cookies() {
		if strings.Contains(c.Name, "obelo_sign_in") {
			run.cookie = c.Name + "=" + c.Value
		}
	}
	if run.state == "" || run.challenge == "" || run.cookie == "" {
		t.Fatalf("start %s handed back no state, challenge or binding cookie: url %s", provider, out.URL)
	}
	return run
}

// finishRedirect is the SPA's callback screen: POST /auth/redirect/callback
// with what the provider sent the browser back with, and the binding cookie.
func finishRedirect(t *testing.T, srv *testharness.Server, run redirectRun, code string) (int, []byte, loginResp) {
	t.Helper()
	var out loginResp
	headers := http.Header{}
	if run.cookie != "" {
		headers.Set("Cookie", run.cookie)
	}
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/callback", "", "", headers,
		map[string]any{
			"state": run.state,
			"code":  code,
			"device": map[string]any{
				"name": "Browser", "platform": "web", "clientId": "redirect-client",
			},
		}, &out)
	return status, body, out
}

// oidcServer boots a server whose Bundled OpenID Connect plugin is configured
// against a fresh provider, with brandon as its first Admin.
func oidcServer(t *testing.T, opts ...testharness.Option) (*testharness.Server, string, *fakeIdP) {
	t.Helper()
	srv := testharness.New(t, opts...)
	admin := adminToken(t, srv)
	idp := newFakeIdP(t)
	status, body := saveDeclaredSettings(t, srv, admin, "oidc", map[string]any{
		"issuer":        idp.srv.URL,
		"client_id":     redirectClientID,
		"client_secret": "client-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("configuring the OpenID Connect plugin = %d, want 200; body: %s", status, body)
	}
	return srv, admin, idp
}

// wantRefused asserts a callback was refused and minted nothing.
func wantRefused(t *testing.T, srv *testharness.Server, admin string, status int, body []byte, out loginResp, usersBefore []string) {
	t.Helper()
	if status != http.StatusUnauthorized || !strings.Contains(string(body), "SIGN_IN_REFUSED") {
		t.Fatalf("callback = %d %s, want 401 SIGN_IN_REFUSED", status, body)
	}
	if out.Token != "" {
		t.Fatalf("a refused callback handed back a token")
	}
	if after := listUsernames(t, srv, admin); len(after) != len(usersBefore) {
		t.Fatalf("users after a refused callback = %v, want still %v", after, usersBefore)
	}
}

// TestTheIDTokensSubjectWinsOverThePluginsIdentity: the provider's ID token
// verifies and names subject-ada; the identity the plugin answers beside it —
// read from userinfo — names somebody else entirely. The server signs in the
// TOKEN's subject: the External identity is recorded under it, a second sign-in
// of the same token subject reaches the same User whatever the plugin claims, and
// the plugin's subject reaches nobody.
func TestTheIDTokensSubjectWinsOverThePluginsIdentity(t *testing.T) {
	t.Parallel()
	srv, _, idp := oidcServer(t)

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key),
		userinfo("subject-mallory", "mallory"))
	status, body, first := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusOK {
		t.Fatalf("callback = %d, want 200; body: %s", status, body)
	}
	if first.User.Username != "ada" {
		t.Fatalf("signed in as %q, want the token's ada rather than the plugin's mallory", first.User.Username)
	}
	ids := srv.ExternalIdentities(first.User.ID)
	if len(ids) != 1 || ids[0].PluginID != "oidc" || ids[0].Subject != "subject-ada" {
		t.Fatalf("external identities = %+v, want one: (oidc, subject-ada)", ids)
	}

	// The same token subject, with the plugin now claiming a different person.
	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-2", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key),
		userinfo("subject-eve", "eve"))
	status, body, second := finishRedirect(t, srv, run, "code-2")
	if status != http.StatusOK || second.User.ID != first.User.ID {
		t.Fatalf("second sign-in of subject-ada = %d (user %q), want 200 as the same user %q; body: %s",
			status, second.User.ID, first.User.ID, body)
	}

	// The plugin claims ada's subject; the token names somebody new.
	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-3", idp.sign(idp.claims(run, "subject-bob", "bob"), idp.key),
		userinfo("subject-ada", "ada"))
	status, body, third := finishRedirect(t, srv, run, "code-3")
	if status != http.StatusOK || third.User.ID == first.User.ID || third.User.Username != "bob" {
		t.Fatalf("a token naming subject-bob = %d as %q (%q), want a new user bob, not ada %q; body: %s",
			status, third.User.Username, third.User.ID, first.User.ID, body)
	}
}

// TestAnIDTokenThatDoesNotVerifyIsRefused: each callback's ID token is wrong in
// exactly one way — signed by a key the issuer never published, issued by
// another issuer, for another client, carrying another nonce, or expired — and
// the plugin's own identity beside it is perfectly plausible. Every one is
// refused with the one refusal, and none creates a User or a session.
func TestAnIDTokenThatDoesNotVerifyIsRefused(t *testing.T) {
	t.Parallel()
	srv, admin, idp := oidcServer(t)
	stranger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		token func(run redirectRun) string
	}{
		{"bad signature", func(run redirectRun) string {
			return idp.sign(idp.claims(run, "subject-ada", "ada"), stranger)
		}},
		{"wrong issuer", func(run redirectRun) string {
			c := idp.claims(run, "subject-ada", "ada")
			c["iss"] = "https://someone-else.example"
			return idp.sign(c, idp.key)
		}},
		{"wrong audience", func(run redirectRun) string {
			c := idp.claims(run, "subject-ada", "ada")
			c["aud"] = "another-client"
			return idp.sign(c, idp.key)
		}},
		{"wrong nonce", func(run redirectRun) string {
			c := idp.claims(run, "subject-ada", "ada")
			c["nonce"] = "a-nonce-this-server-never-minted"
			return idp.sign(c, idp.key)
		}},
		{"expired", func(run redirectRun) string {
			c := idp.claims(run, "subject-ada", "ada")
			c["iat"] = time.Now().Add(-2 * time.Hour).Unix()
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return idp.sign(c, idp.key)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := listUsernames(t, srv, admin)
			run := startRedirect(t, srv, "oidc")
			code := "code-" + strings.ReplaceAll(tc.name, " ", "-")
			idp.grant(run, code, tc.token(run), userinfo("subject-ada", "ada"))
			status, body, out := finishRedirect(t, srv, run, code)
			wantRefused(t, srv, admin, status, body, out, before)
		})
	}
}

// TestTheIDTokensGroupsWinOverThePluginsGroups: the token carries family and
// media; the plugin's identity says admins. The sign-in is accepted and the
// External identity records the token's groups.
func TestTheIDTokensGroupsWinOverThePluginsGroups(t *testing.T) {
	t.Parallel()
	srv, _, idp := oidcServer(t)

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada", "family", "media"), idp.key),
		userinfo("subject-ada", "ada", "admins"))
	status, body, out := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusOK {
		t.Fatalf("callback = %d, want 200; body: %s", status, body)
	}
	ids := srv.ExternalIdentities(out.User.ID)
	if len(ids) != 1 || strings.Join(ids[0].Groups, ",") != "family,media" {
		t.Fatalf("external identities = %+v, want the token's groups family,media", ids)
	}
}

// TestARedirectSignInResolvesLikeThePasswordFlow: a full round trip — state,
// PKCE and nonce minted by the server (the provider's token endpoint refuses a
// verifier that does not match the challenge it was sent) — makes a new Member
// granted nothing, keyed by (oidc, subject). The same subject renamed at the
// provider signs in as the same User under the name it was created with, and a
// new subject whose name is already taken here is the password flow's 409, with
// nothing created.
func TestARedirectSignInResolvesLikeThePasswordFlow(t *testing.T) {
	t.Parallel()
	srv, admin, idp := oidcServer(t)

	run := startRedirect(t, srv, "oidc")
	if got := run.authorize.Query().Get("client_id"); got != redirectClientID {
		t.Fatalf("authorize URL client_id = %q, want %q", got, redirectClientID)
	}
	if got := run.authorize.Query().Get("code_challenge_method"); got != "S256" {
		t.Fatalf("authorize URL code_challenge_method = %q, want S256", got)
	}
	if !strings.HasSuffix(run.redirectURI, "/sign-in/callback") || run.nonce == "" {
		t.Fatalf("authorize URL redirect_uri %q / nonce %q, want the server's callback and a nonce",
			run.redirectURI, run.nonce)
	}
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key), userinfo("subject-ada", "ada"))
	status, body, first := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusOK || first.Token == "" || first.User.Role != "member" {
		t.Fatalf("callback = %d %+v, want 200 with a token for a new member; body: %s", status, first, body)
	}
	var detail signInUserDetail
	if status, body := srv.AuthGET("/api/v1/users/"+first.User.ID, admin, &detail); status != http.StatusOK {
		t.Fatalf("GET /users/{id} status = %d; body: %s", status, body)
	}
	if detail.Role != "member" || len(detail.LibraryIDs) != 0 {
		t.Fatalf("the new User = %+v, want a member granted no library", detail)
	}
	if status, body := srv.AuthGET("/api/v1/devices", first.Token, nil); status != http.StatusOK {
		t.Fatalf("GET /devices with the redirect session = %d; body: %s", status, body)
	}

	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-2", idp.sign(idp.claims(run, "subject-ada", "ada-lovelace"), idp.key),
		userinfo("subject-ada", "ada-lovelace"))
	status, body, renamed := finishRedirect(t, srv, run, "code-2")
	if status != http.StatusOK || renamed.User.ID != first.User.ID || renamed.User.Username != "ada" {
		t.Fatalf("renamed sign-in = %d as %q (%q), want the same user ada %q; body: %s",
			status, renamed.User.Username, renamed.User.ID, first.User.ID, body)
	}

	before := listUsernames(t, srv, admin)
	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-3", idp.sign(idp.claims(run, "subject-other-brandon", "brandon"), idp.key),
		userinfo("subject-other-brandon", "brandon"))
	status, body, _ = finishRedirect(t, srv, run, "code-3")
	if status != http.StatusConflict || !strings.Contains(string(body), "SIGN_IN_USERNAME_TAKEN") {
		t.Fatalf("a new identity named brandon = %d %s, want 409 SIGN_IN_USERNAME_TAKEN", status, body)
	}
	if after := listUsernames(t, srv, admin); len(after) != len(before) {
		t.Fatalf("users after a collision = %v, want still %v", after, before)
	}
}

// TestARedirectCallbackIsBoundToItsStartAndUsedOnce: the server's state and
// binding are what a callback rests on. A callback whose browser does not carry
// the binding cookie, one naming a state the server never minted, and a replay
// of a state already spent are all refused.
func TestARedirectCallbackIsBoundToItsStartAndUsedOnce(t *testing.T) {
	t.Parallel()
	srv, admin, idp := oidcServer(t)

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key), userinfo("subject-ada", "ada"))
	before := listUsernames(t, srv, admin)
	unbound := run
	unbound.cookie = ""
	status, body, out := finishRedirect(t, srv, unbound, "code-1")
	wantRefused(t, srv, admin, status, body, out, before)

	run = startRedirect(t, srv, "oidc")
	idp.grant(run, "code-2", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key), userinfo("subject-ada", "ada"))
	forged := run
	forged.state = "a-state-this-server-never-minted"
	status, body, out = finishRedirect(t, srv, forged, "code-2")
	wantRefused(t, srv, admin, status, body, out, before)

	if status, body, _ := finishRedirect(t, srv, run, "code-2"); status != http.StatusOK {
		t.Fatalf("the genuine callback = %d, want 200; body: %s", status, body)
	}
	before = listUsernames(t, srv, admin)
	status, body, out = finishRedirect(t, srv, run, "code-2")
	wantRefused(t, srv, admin, status, body, out, before)
}

// TestAPlainOAuth2ProviderIsAcceptedOnStateAndPKCEAndMarkedUnverified: a
// redirect provider that declares no ID token has nothing to verify. Its identity
// is accepted as given, and the Admin screen lists it as not independently
// verified, beside the Bundled OpenID Connect plugin, which is. One that answers
// an ID token anyway — a token nobody can verify — is refused.
func TestAPlainOAuth2ProviderIsAcceptedOnStateAndPKCEAndMarkedUnverified(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)

	run := startRedirect(t, srv, "oauth")
	if run.authorize.Host != "oauth.example.test" {
		t.Fatalf("authorize URL = %s, want the plugin's", run.authorize)
	}
	status, body, out := finishRedirect(t, srv, run, "subject-octavia|octavia|crew")
	if status != http.StatusOK || out.User.Username != "octavia" {
		t.Fatalf("callback = %d as %q, want 200 as octavia; body: %s", status, out.User.Username, body)
	}
	ids := srv.ExternalIdentities(out.User.ID)
	if len(ids) != 1 || ids[0].PluginID != "oauth" || ids[0].Subject != "subject-octavia" ||
		strings.Join(ids[0].Groups, ",") != "crew" {
		t.Fatalf("external identities = %+v, want (oauth, subject-octavia) in crew", ids)
	}

	before := listUsernames(t, srv, admin)
	run = startRedirect(t, srv, "oauth")
	status, body, out = finishRedirect(t, srv, run, "token|subject-ursula|ursula|")
	wantRefused(t, srv, admin, status, body, out, before)

	var view struct {
		Redirect []struct {
			ID       string `json:"id"`
			Verified bool   `json:"verified"`
		} `json:"redirect"`
	}
	if status, body := srv.AuthGET("/api/v1/settings/sign-in-providers", admin, &view); status != http.StatusOK {
		t.Fatalf("GET sign-in providers = %d; body: %s", status, body)
	}
	verified := map[string]bool{}
	for _, p := range view.Redirect {
		verified[p.ID] = p.Verified
	}
	if v, ok := verified["oauth"]; !ok || v {
		t.Fatalf("redirect providers = %+v, want oauth listed as not verified", view.Redirect)
	}
	if v, ok := verified["oidc"]; !ok || !v {
		t.Fatalf("redirect providers = %+v, want oidc listed as verified", view.Redirect)
	}
}

// TestTheBundledOpenIDConnectPluginIsInstalledAndConfigurable: a fresh server
// carries it on the Plugins screen with its declared settings. Until an Admin
// types an issuer and a client id it offers nobody a button and refuses to
// start; once they have, the login screen lists it.
func TestTheBundledOpenIDConnectPluginIsInstalledAndConfigurable(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	p := readDeclaredSettings(t, srv, admin, "oidc")
	keys := map[string]bool{}
	for _, f := range p.SettingsSchema {
		keys[f.Key] = true
	}
	for _, want := range []string{"issuer", "client_id", "client_secret", "scopes"} {
		if !keys[want] {
			t.Fatalf("the OpenID Connect plugin declares %v, want %s among them", keys, want)
		}
	}

	var listed struct {
		Providers []struct {
			ID string `json:"id"`
		} `json:"providers"`
	}
	if status, body := srv.GET("/api/v1/auth/sign-in-providers", &listed); status != http.StatusOK || len(listed.Providers) != 0 {
		t.Fatalf("login screen providers before configuring = %d %s, want none", status, body)
	}
	status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", "", nil,
		map[string]any{"provider": "oidc"}, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("start before configuring = %d, want 503; body: %s", status, body)
	}

	idp := newFakeIdP(t)
	if status, body := saveDeclaredSettings(t, srv, admin, "oidc", map[string]any{
		"issuer": idp.srv.URL, "client_id": redirectClientID,
	}); status != http.StatusOK {
		t.Fatalf("configuring = %d; body: %s", status, body)
	}
	if status, body := srv.GET("/api/v1/auth/sign-in-providers", &listed); status != http.StatusOK ||
		len(listed.Providers) != 1 || listed.Providers[0].ID != "oidc" {
		t.Fatalf("login screen providers after configuring = %d %s, want oidc", status, body)
	}
}

// TestADeviceGrantApprovedFromARedirectSessionSignsTheDeviceInAsThatIdentity:
// a TV starts the device authorization grant; ada signs in on the web through
// the OpenID Connect plugin and approves its code; the TV collects a session for
// ada — the identity the redirect resolved.
func TestADeviceGrantApprovedFromARedirectSessionSignsTheDeviceInAsThatIdentity(t *testing.T) {
	t.Parallel()
	srv, _, idp := oidcServer(t)
	tv := startFlow(t, srv)

	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-ada", "ada"), idp.key), userinfo("subject-ada", "ada"))
	status, body, web := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusOK {
		t.Fatalf("callback = %d, want 200; body: %s", status, body)
	}

	if status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/approve", web.Token,
		map[string]any{"userCode": tv.UserCode}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body: %s", status, raw)
	}
	var session loginResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": tv.DeviceCode}, &session)
	if status != http.StatusOK || session.Token == "" {
		t.Fatalf("redeem = %d, want 200 with a token; body: %s", status, raw)
	}
	if session.User.ID != web.User.ID || session.User.Username != "ada" {
		t.Fatalf("the TV signed in as %q (%q), want ada %q", session.User.Username, session.User.ID, web.User.ID)
	}
	if status, raw := srv.AuthGET("/api/v1/devices", session.Token, nil); status != http.StatusOK {
		t.Fatalf("GET /devices with the TV's session = %d; body: %s", status, raw)
	}
}

// TestAnOpenIDConnectProviderThatAnswersNoIDTokenIsRefused: an Installed
// provider whose manifest declares an idToken, configured with an issuer and a
// client id, exchanges the code for a perfectly plausible identity and no ID
// token. A provider that declares ID tokens is believed on its token or not at
// all, so the callback is refused and nobody is signed in.
func TestAnOpenIDConnectProviderThatAnswersNoIDTokenIsRefused(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	m := plugintest.RedirectSignInManifest("idp", "https://idp.example.test/authorize")
	m.Settings.Fields = append(m.Settings.Fields,
		pluginapi.SettingsField{Key: "issuer", Type: pluginapi.FieldURL, Label: "Issuer"},
		pluginapi.SettingsField{Key: "client_id", Type: pluginapi.FieldString, Label: "Client id"})
	m.Provides[0].IDToken = &pluginapi.ManifestIDToken{IssuerSetting: "issuer", ClientIDSetting: "client_id"}
	plugintest.Install(t, dataDir, m)
	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	admin := adminToken(t, srv)
	if status, body := saveDeclaredSettings(t, srv, admin, "idp", map[string]any{
		"issuer": "https://idp.example.test", "client_id": redirectClientID,
	}); status != http.StatusOK {
		t.Fatalf("configuring the provider = %d; body: %s", status, body)
	}

	before := listUsernames(t, srv, admin)
	run := startRedirect(t, srv, "idp")
	status, body, out := finishRedirect(t, srv, run, "subject-mallory|mallory|admins")
	wantRefused(t, srv, admin, status, body, out, before)
}

// TestARedirectSignInCallShowsTheAdminNoTextOfItsOwn: a provider that logs what
// each redirect call handed it — the state, nonce and challenge, or the code and
// PKCE verifier, and its client secret — and fails the call with the same. After
// the start and the callback reach it, neither the Admin plugins list nor any
// log line carries any of it; the last error is the host's own sentence.
func TestARedirectSignInCallShowsTheAdminNoTextOfItsOwn(t *testing.T) {
	// Not parallel: it captures the process-wide log output.
	const secret = "client-s3cret-horse"
	for _, tc := range []struct {
		name      string
		authorize string
		forbidden []string
	}{
		{"authorize", "https://oauth.example.test/" + plugintest.SignInRedirectFailsWithTheSecrets,
			[]string{"state=", "nonce=", "challenge="}},
		{"exchange", "https://oauth.example.test/authorize",
			[]string{plugintest.SignInRedirectFailsWithTheSecrets, "verifier="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := &lockedBuffer{}
			prev := log.Writer()
			log.SetOutput(logged)
			t.Cleanup(func() { log.SetOutput(prev) })

			dataDir := t.TempDir()
			plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
			srv := testharness.New(t, testharness.WithDataDir(dataDir))
			admin := adminToken(t, srv)
			if status, body := saveDeclaredSettings(t, srv, admin, "oauth", map[string]any{
				"authorize": tc.authorize, "client_secret": secret,
			}); status != http.StatusOK {
				t.Fatalf("configuring the provider = %d; body: %s", status, body)
			}

			if tc.name == "authorize" {
				status, _, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", "", nil,
					map[string]any{"provider": "oauth"}, nil)
				if status != http.StatusBadGateway {
					t.Fatalf("start = %d, want 502; body: %s", status, body)
				}
			} else {
				run := startRedirect(t, srv, "oauth")
				if status, body, _ := finishRedirect(t, srv, run, plugintest.SignInRedirectFailsWithTheSecrets); status != http.StatusUnauthorized {
					t.Fatalf("callback = %d, want 401; body: %s", status, body)
				}
			}

			var list struct {
				Plugins []struct {
					ID        string `json:"id"`
					LastError string `json:"lastError"`
				} `json:"plugins"`
			}
			status, body := srv.AuthGET("/api/v1/settings/plugins", admin, &list)
			if status != http.StatusOK {
				t.Fatalf("GET /settings/plugins status = %d; body: %s", status, body)
			}
			for _, p := range list.Plugins {
				if p.ID == "oauth" && !strings.HasPrefix(p.LastError, "redirect sign-in") {
					t.Errorf("lastError = %q, want the host's own sentence", p.LastError)
				}
			}
			for _, text := range append([]string{secret, "the guest was handed", "secret="}, tc.forbidden...) {
				if strings.Contains(string(body), text) {
					t.Errorf("the Admin plugins list carries %q: %s", text, body)
				}
				if strings.Contains(logged.String(), text) {
					t.Errorf("a log line carries %q:\n%s", text, logged.String())
				}
			}
		})
	}
}

// startCookie is the binding cookie a start answers, made from remoteAddr with
// headers.
func startCookie(t *testing.T, srv *testharness.Server, remoteAddr string, headers http.Header) *http.Cookie {
	t.Helper()
	status, header, body := srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", remoteAddr, headers,
		map[string]any{"provider": "oauth"}, nil)
	if status != http.StatusOK {
		t.Fatalf("start = %d, want 200; body: %s", status, body)
	}
	for _, c := range (&http.Response{Header: header}).Cookies() {
		if strings.Contains(c.Name, "obelo_sign_in") {
			return c
		}
	}
	t.Fatalf("the start set no binding cookie: %v", header)
	return nil
}

// TestTheSignInBindingCookieIsHttpOnlyAndLax: the binding cookie is out of the
// page's reach and travels back on the provider's top-level redirect only; it
// is Secure, under the __Secure- name, when the request was HTTPS (here, by a
// trusted proxy's say-so), and not over plain HTTP, where Secure would drop it.
func TestTheSignInBindingCookieIsHttpOnlyAndLax(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithTrustedProxies("127.0.0.1/32"))

	plain := startCookie(t, srv, "", nil)
	if plain.Name != "obelo_sign_in" || !plain.HttpOnly || plain.SameSite != http.SameSiteLaxMode || plain.Secure {
		t.Fatalf("plain-HTTP binding cookie = %+v, want obelo_sign_in, HttpOnly, SameSite=Lax, not Secure", plain)
	}
	secure := startCookie(t, srv, "127.0.0.1:40000", http.Header{"X-Forwarded-Proto": []string{"https"}})
	if secure.Name != "__Secure-obelo_sign_in" || !secure.HttpOnly || secure.SameSite != http.SameSiteLaxMode || !secure.Secure {
		t.Fatalf("HTTPS binding cookie = %+v, want __Secure-obelo_sign_in, HttpOnly, SameSite=Lax, Secure", secure)
	}
}

// TestStartsPastTheCapEvictTheOldestRatherThanLockingOut: sixteen sign-ins in
// flight from one address is the cap, and a seventeenth start still works end
// to end; the one given up is the oldest. The address is one client's, and then
// a reverse proxy's this server was not told to trust, behind which every
// browser shares it — where refusing instead would let anybody lock everybody
// out of signing in.
func TestStartsPastTheCapEvictTheOldestRatherThanLockingOut(t *testing.T) {
	t.Parallel()
	const perClient = 16
	for _, tc := range []struct {
		name    string
		headers func(i int) http.Header
	}{
		{"one client", func(int) http.Header { return nil }},
		{"every client behind one proxy address", func(i int) http.Header {
			return http.Header{"X-Forwarded-For": []string{fmt.Sprintf("198.51.100.%d", i+1)}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
			srv := testharness.New(t, testharness.WithDataDir(dataDir))
			admin := adminToken(t, srv)

			var runs []redirectRun
			for i := 0; i <= perClient; i++ {
				runs = append(runs, startRedirectFrom(t, srv, "oauth", "203.0.113.7:40000", tc.headers(i)))
			}
			newest := runs[perClient]
			status, body, out := finishRedirect(t, srv, newest, "subject-nadia|nadia|")
			if status != http.StatusOK || out.User.Username != "nadia" {
				t.Fatalf("the newest start's callback = %d as %q, want 200 as nadia; body: %s", status, out.User.Username, body)
			}
			before := listUsernames(t, srv, admin)
			status, body, out = finishRedirect(t, srv, runs[0], "subject-olga|olga|")
			wantRefused(t, srv, admin, status, body, out, before)
			status, body, out = finishRedirect(t, srv, runs[1], "subject-petra|petra|")
			if status != http.StatusOK || out.User.Username != "petra" {
				t.Fatalf("the second-oldest start's callback = %d as %q, want 200 as petra; body: %s", status, out.User.Username, body)
			}
		})
	}
}

// TestRedirectStartsAreRateLimitedPerAddress: POST /auth/redirect/start runs the
// plugin on every call, so one address is refused past its limit — 429
// TOO_MANY_ATTEMPTS with a Retry-After, as a login is — while another address
// still starts.
func TestRedirectStartsAreRateLimitedPerAddress(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir))

	var status int
	var header http.Header
	var body []byte
	var allowed int
	for i := 0; i < 200; i++ {
		status, header, body = srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", "203.0.113.7:40000", nil,
			map[string]any{"provider": "oauth"}, nil)
		if status != http.StatusOK {
			break
		}
		allowed++
	}
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "TOO_MANY_ATTEMPTS") {
		t.Fatalf("start %d from one address = %d %s, want 429 TOO_MANY_ATTEMPTS", allowed+1, status, body)
	}
	if header.Get("Retry-After") == "" {
		t.Error("the 429 carries no Retry-After")
	}
	if allowed <= 16 {
		t.Errorf("one address was allowed %d starts, want more than the 16 sign-ins one client may have in flight", allowed)
	}
	startRedirectFrom(t, srv, "oauth", "198.51.100.4:40000", nil)
}

// TestARedirectFirstSignInNamingAnUnusableUsernameIsRefused: a new identity
// whose provider names a username breaking the rule is a failed sign-in, like
// any answer the Server cannot act on, and nothing is created.
func TestARedirectFirstSignInNamingAnUnusableUsernameIsRefused(t *testing.T) {
	t.Parallel()
	srv, admin, idp := oidcServer(t)
	long := strings.Repeat("a", 65)

	before := listUsernames(t, srv, admin)
	run := startRedirect(t, srv, "oidc")
	idp.grant(run, "code-1", idp.sign(idp.claims(run, "subject-long", long), idp.key), userinfo("subject-long", long))
	status, body, _ := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusUnauthorized || !strings.Contains(string(body), "SIGN_IN_REFUSED") {
		t.Fatalf("a new identity named with 65 characters = %d %s, want 401 SIGN_IN_REFUSED", status, body)
	}
	if after := listUsernames(t, srv, admin); len(after) != len(before) {
		t.Fatalf("users after the refusal = %v, want still %v", after, before)
	}
}

// TestRedirectStartsAreLimitedPerClientBehindATrustedProxy: behind a proxy
// named in OBELO_TRUSTED_PROXIES, the limit counts the client the proxy
// forwarded for, not the proxy — so many browsers behind it each keep their own
// budget, and one browser is still refused past its own.
func TestRedirectStartsAreLimitedPerClientBehindATrustedProxy(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.RedirectSignInManifest("oauth", "https://oauth.example.test/authorize"))
	srv := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithTrustedProxies("127.0.0.1/32"))

	for i := 1; i <= 40; i++ {
		startRedirectFrom(t, srv, "oauth", "127.0.0.1:40000",
			http.Header{"X-Forwarded-For": []string{fmt.Sprintf("198.51.100.%d", i)}})
	}

	one := http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}
	var status int
	var body []byte
	var allowed int
	for i := 0; i < 200; i++ {
		status, _, body = srv.JSONFrom(http.MethodPost, "/api/v1/auth/redirect/start", "", "127.0.0.1:40000", one,
			map[string]any{"provider": "oauth"}, nil)
		if status != http.StatusOK {
			break
		}
		allowed++
	}
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "TOO_MANY_ATTEMPTS") {
		t.Fatalf("start %d from one forwarded client = %d %s, want 429 TOO_MANY_ATTEMPTS", allowed+1, status, body)
	}
}
