package api_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// TestAnIssuerNamingALoopbackTokenEndpointOnAnotherHostDoesNotSignIn: an issuer
// whose discovery document names its token, userinfo and key endpoints on
// another host than its own — Google's shape — does NOT get them reached through
// the Bundled OpenID Connect plugin when that host is loopback over http. The
// issuer is served as localhost and every endpoint as http://127.0.0.1: the
// issuer's own host keeps the operator's exemption, but a host its SERVER named
// is admitted only at an https origin and only if it passes the private-address
// check, so the exchange is refused and nobody is signed in.
func TestAnIssuerNamingALoopbackTokenEndpointOnAnotherHostDoesNotSignIn(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	front := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	issuer := "http://localhost:" + port
	front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		writeTestJSON(w, map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/authorize",
			"token_endpoint":         idp.srv.URL + "/token",
			"userinfo_endpoint":      idp.srv.URL + "/userinfo",
			"jwks_uri":               idp.srv.URL + "/jwks",
		})
	})
	front.Start()
	t.Cleanup(front.Close)

	srv := testharness.New(t)
	admin := adminToken(t, srv)
	status, body := saveDeclaredSettings(t, srv, admin, "oidc", map[string]any{
		"issuer":        issuer,
		"client_id":     redirectClientID,
		"client_secret": "client-secret",
	})
	if status != http.StatusOK {
		t.Fatalf("configuring the OpenID Connect plugin = %d, want 200; body: %s", status, body)
	}

	run := startRedirect(t, srv, "oidc")
	claims := idp.claims(run, "subject-ada", "ada", "crew")
	claims["iss"] = issuer
	idp.grant(run, "code-1", idp.sign(claims, idp.key), userinfo("subject-ada", "ada", "crew"))
	status, body, out := finishRedirect(t, srv, run, "code-1")
	if status != http.StatusUnauthorized || out.User.Username != "" {
		t.Fatalf("callback = %d as %q, want 401 as nobody; body: %s", status, out.User.Username, body)
	}
}
