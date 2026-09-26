// Package oidc is the Obelo OpenID Connect Sign-in provider, as a plugin
// (ADR-0063 decisions 1 and 2).
//
// It does the two things the redirect flow asks of a Plugin and nothing else. It
// builds the authorize URL from the issuer's discovery document and the state,
// PKCE challenge and nonce the host hands it, and it exchanges the code the
// browser comes back with at the token endpoint, answering with the raw ID token
// and the identity the userinfo endpoint reports.
//
// Between sign-ins it redeems the refresh token its exchange handed back, so
// the host can re-check the identity with nobody present (ADR-0063 decision 4).
// A token endpoint that refuses the grant as invalid_grant is answered with an
// error, which the host treats as the provider being unreachable: an expired
// refresh token says nothing about whether the person may still sign in.
//
// What it deliberately does NOT do is verify the ID token. The host does that —
// signature against the issuer's JWKS, iss, aud, nonce and exp, against the
// issuer and client id the operator typed into this plugin's settings — and takes
// the subject and groups from the verified token. The identity answered beside it
// is this plugin's own reading, which the host keeps only as a label.
package oidc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The keys of this plugin's declared settings (manifest.json).
const (
	settingIssuer       = "issuer"
	settingClientID     = "client_id"
	settingClientSecret = "client_secret"
	settingScopes       = "scopes"
)

// Provider is the OpenID Connect redirect provider.
type Provider struct {
	host pluginsdk.Host
}

// New returns a Provider that reaches the network, and reads its settings,
// through h.
func New(h pluginsdk.Host) *Provider { return &Provider{host: h} }

var (
	_ pluginapi.SignInRedirectProvider = (*Provider)(nil)
	_ pluginapi.SignInRefreshProvider  = (*Provider)(nil)
)

type config struct {
	issuer, clientID, clientSecret, scopes string
}

func (p *Provider) config() (config, error) {
	values := p.host.Settings().Values
	str := func(key string) string {
		s, _ := values[key].(string)
		return strings.TrimSpace(s)
	}
	c := config{
		issuer:       str(settingIssuer),
		clientID:     str(settingClientID),
		clientSecret: str(settingClientSecret),
		scopes:       str(settingScopes),
	}
	if c.issuer == "" || c.clientID == "" {
		return config{}, errors.New("the issuer and the client id are not configured")
	}
	return c, nil
}

// discovery is the part of the issuer's discovery document this plugin reads.
type discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

func (p *Provider) discover(ctx context.Context, issuer string) (discovery, error) {
	var d discovery
	target := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	if err := pluginsdk.GetJSON(ctx, p.host, target, nil, &d,
		pluginsdk.Header("Accept", "application/json")); err != nil {
		return discovery{}, err
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return discovery{}, errors.New("the discovery document names no authorization or token endpoint")
	}
	return d, nil
}

// scopesFor always asks for openid, whatever the operator typed.
func scopesFor(typed string) string {
	fields := strings.Fields(typed)
	for _, f := range fields {
		if f == "openid" {
			return strings.Join(fields, " ")
		}
	}
	return strings.Join(append([]string{"openid"}, fields...), " ")
}

// AuthorizeURL is the issuer's authorization endpoint with the host's state,
// PKCE challenge, nonce and callback on it.
func (p *Provider) AuthorizeURL(ctx context.Context, req pluginapi.SignInAuthorizeRequest) (pluginapi.SignInAuthorizeResponse, error) {
	c, err := p.config()
	if err != nil {
		return pluginapi.SignInAuthorizeResponse{}, err
	}
	d, err := p.discover(ctx, c.issuer)
	if err != nil {
		return pluginapi.SignInAuthorizeResponse{}, err
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", req.RedirectURI)
	q.Set("scope", scopesFor(c.scopes))
	q.Set("state", req.State)
	q.Set("nonce", req.Nonce)
	q.Set("code_challenge", req.CodeChallenge)
	q.Set("code_challenge_method", req.CodeChallengeMethod)
	return pluginapi.SignInAuthorizeResponse{URL: pluginsdk.URLWithQuery(d.AuthorizationEndpoint, q)}, nil
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// claims is what this plugin reads of a person, from userinfo or, failing that,
// from the ID token's own payload.
type claims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Groups            []string `json:"groups"`
}

// Exchange redeems the code at the token endpoint and answers with the raw ID
// token and this plugin's reading of who it names.
func (p *Provider) Exchange(ctx context.Context, req pluginapi.SignInExchangeRequest) (pluginapi.SignInExchangeResponse, error) {
	c, err := p.config()
	if err != nil {
		return pluginapi.SignInExchangeResponse{}, err
	}
	d, err := p.discover(ctx, c.issuer)
	if err != nil {
		return pluginapi.SignInExchangeResponse{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", req.Code)
	form.Set("redirect_uri", req.RedirectURI)
	form.Set("code_verifier", req.CodeVerifier)
	form.Set("client_id", c.clientID)
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	var tok tokenResponse
	if err := pluginsdk.DoJSON(ctx, p.host, pluginapi.FetchRequest{
		Method: "POST",
		URL:    d.TokenEndpoint,
		Headers: []pluginapi.FetchHeader{
			pluginsdk.Header("Content-Type", "application/x-www-form-urlencoded"),
			pluginsdk.Header("Accept", "application/json"),
		},
		Body: []byte(form.Encode()),
	}, &tok); err != nil {
		return pluginapi.SignInExchangeResponse{}, err
	}
	if tok.IDToken == "" {
		return pluginapi.SignInExchangeResponse{}, errors.New("the token endpoint issued no ID token")
	}

	who, err := p.identify(ctx, d, tok)
	if err != nil {
		return pluginapi.SignInExchangeResponse{}, err
	}
	if who.Subject == "" {
		return pluginapi.SignInExchangeResponse{IDToken: tok.IDToken}, nil
	}
	username := who.PreferredUsername
	if username == "" {
		username = who.Email
	}
	return pluginapi.SignInExchangeResponse{
		Accepted:     true,
		Identity:     &pluginapi.SignInIdentity{Subject: who.Subject, Username: username, Groups: who.Groups},
		IDToken:      tok.IDToken,
		RefreshToken: tok.RefreshToken,
	}, nil
}

// bundled-sample:begin refresh

// Refresh redeems a refresh token at the token endpoint and answers the fresh
// ID token, and the rotated refresh token when the issuer rotated it. Every
// failure is an error, which the host treats as unreachable — a grant the
// issuer refuses as invalid_grant included: an expired or revoked refresh token
// says nothing about whether the person is still there, and answering gone
// would revoke every session they hold.
func (p *Provider) Refresh(ctx context.Context, req pluginapi.SignInRefreshRequest) (pluginapi.SignInRefreshResponse, error) {
	c, err := p.config()
	if err != nil {
		return pluginapi.SignInRefreshResponse{}, err
	}
	d, err := p.discover(ctx, c.issuer)
	if err != nil {
		return pluginapi.SignInRefreshResponse{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", req.RefreshToken)
	form.Set("client_id", c.clientID)
	if c.clientSecret != "" {
		form.Set("client_secret", c.clientSecret)
	}
	resp, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{
		Method: "POST",
		URL:    d.TokenEndpoint,
		Headers: []pluginapi.FetchHeader{
			pluginsdk.Header("Content-Type", "application/x-www-form-urlencoded"),
			pluginsdk.Header("Accept", "application/json"),
		},
		Body: []byte(form.Encode()),
	})
	if err != nil {
		var fe *pluginsdk.FetchError
		if errors.As(err, &fe) && (fe.Status == 400 || fe.Status == 401) {
			var refusal struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(resp.Body, &refusal) == nil && refusal.Error == "invalid_grant" {
				return pluginapi.SignInRefreshResponse{}, errors.New("the issuer refused the refresh token (invalid_grant)")
			}
		}
		return pluginapi.SignInRefreshResponse{}, err
	}
	var tok tokenResponse
	if err := json.Unmarshal(resp.Body, &tok); err != nil {
		return pluginapi.SignInRefreshResponse{}, fmt.Errorf("the token endpoint's answer is not JSON: %w", err)
	}
	if tok.IDToken == "" {
		return pluginapi.SignInRefreshResponse{}, errors.New("the token endpoint issued no ID token on refresh")
	}
	return pluginapi.SignInRefreshResponse{
		Status:       pluginapi.SignInActive,
		IDToken:      tok.IDToken,
		RefreshToken: tok.RefreshToken,
	}, nil
}

// bundled-sample:end refresh

// identify reads the person from the userinfo endpoint when the issuer has one,
// and from the ID token's payload when it does not. Neither is verified here.
func (p *Provider) identify(ctx context.Context, d discovery, tok tokenResponse) (claims, error) {
	var who claims
	if d.UserinfoEndpoint != "" && tok.AccessToken != "" {
		err := pluginsdk.GetJSON(ctx, p.host, d.UserinfoEndpoint, nil, &who,
			pluginsdk.Header("Authorization", "Bearer "+tok.AccessToken),
			pluginsdk.Header("Accept", "application/json"))
		return who, err
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return claims{}, fmt.Errorf("the ID token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims{}, fmt.Errorf("the ID token's payload is not base64url: %w", err)
	}
	if err := json.Unmarshal(payload, &who); err != nil {
		return claims{}, fmt.Errorf("the ID token's payload is not JSON: %w", err)
	}
	return who, nil
}
