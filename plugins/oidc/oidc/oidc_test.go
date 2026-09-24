package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

const issuer = "https://auth.example.test/application/o/obelo/"

// provider answers the discovery document, a token endpoint that records the
// form it was posted, and — when withUserinfo — a userinfo endpoint.
func provider(t *testing.T, withUserinfo bool, idToken string, posted *url.Values) *sdktest.Host {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/application/o/obelo/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{
			"issuer":                 issuer,
			"authorization_endpoint": "https://auth.example.test/authorize/",
			"token_endpoint":         "https://auth.example.test/token/",
		}
		if withUserinfo {
			doc["userinfo_endpoint"] = "https://auth.example.test/userinfo/"
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/token/", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		*posted = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1", "id_token": idToken})
	})
	mux.HandleFunc("/userinfo/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "userinfo-sub", "preferred_username": "ada", "groups": []string{"media"}})
	})
	return sdktest.New(sdktest.WithHandler(mux), sdktest.WithSettings(pluginapi.Settings{Values: map[string]any{
		"issuer": issuer, "client_id": "obelo", "client_secret": "shh", "scopes": "profile groups",
	}}))
}

func unsignedToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
}

func TestTheAuthorizeURLCarriesTheHostsValues(t *testing.T) {
	var posted url.Values
	p := New(provider(t, true, "", &posted))
	resp, err := p.AuthorizeURL(context.Background(), pluginapi.SignInAuthorizeRequest{
		State: "st", CodeChallenge: "ch", CodeChallengeMethod: "S256", Nonce: "n",
		RedirectURI: "https://obelo.example/sign-in/callback",
	})
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}
	u, err := url.Parse(resp.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"response_type": "code", "client_id": "obelo", "state": "st", "nonce": "n",
		"code_challenge": "ch", "code_challenge_method": "S256",
		"redirect_uri": "https://obelo.example/sign-in/callback", "scope": "openid profile groups",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if !strings.HasPrefix(resp.URL, "https://auth.example.test/authorize/?") {
		t.Errorf("url = %q, want the discovered authorization endpoint", resp.URL)
	}
}

func TestTheExchangeAnswersTheTokenAndTheUserinfoIdentity(t *testing.T) {
	var posted url.Values
	token := unsignedToken(t, map[string]any{"sub": "token-sub"})
	p := New(provider(t, true, token, &posted))
	resp, err := p.Exchange(context.Background(), pluginapi.SignInExchangeRequest{
		Code: "code-1", CodeVerifier: "verifier-1", RedirectURI: "https://obelo.example/sign-in/callback",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if posted.Get("code_verifier") != "verifier-1" || posted.Get("code") != "code-1" ||
		posted.Get("client_secret") != "shh" || posted.Get("grant_type") != "authorization_code" {
		t.Fatalf("token request = %v, want the code, the verifier and the client's credentials", posted)
	}
	if !resp.Accepted || resp.IDToken != token || resp.Identity == nil || resp.Identity.Subject != "userinfo-sub" ||
		resp.Identity.Username != "ada" || strings.Join(resp.Identity.Groups, ",") != "media" {
		t.Fatalf("exchange = %+v (identity %+v), want the raw token and userinfo's identity", resp, resp.Identity)
	}
}

func TestWithoutUserinfoTheIdentityIsReadFromTheToken(t *testing.T) {
	var posted url.Values
	token := unsignedToken(t, map[string]any{"sub": "token-sub", "email": "ada@example.test"})
	p := New(provider(t, false, token, &posted))
	resp, err := p.Exchange(context.Background(), pluginapi.SignInExchangeRequest{Code: "code-1", CodeVerifier: "v"})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if !resp.Accepted || resp.Identity.Subject != "token-sub" || resp.Identity.Username != "ada@example.test" {
		t.Fatalf("exchange identity = %+v, want token-sub named by its email", resp.Identity)
	}
}

func TestAnUnconfiguredPluginRefuses(t *testing.T) {
	p := New(sdktest.New())
	if _, err := p.AuthorizeURL(context.Background(), pluginapi.SignInAuthorizeRequest{}); err == nil {
		t.Fatal("AuthorizeURL with no issuer answered no error")
	}
}
