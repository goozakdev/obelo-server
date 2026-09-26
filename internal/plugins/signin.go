package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Sign-in provider Extension point, filled by a guest — the password flow
// and the redirect flow.
//
// Like webref.go there is nothing clever here, and the same thing deliberately
// absent: every judgment on the answer. Whether an accepted answer is complete
// enough to sign anyone in, and what it resolves to, is the HOST's call and lives
// in internal/signin and internal/auth, so a Built-in and an Installed plugin are
// held to exactly the same rule by exactly the same code.
//
// The call carries a password. Nothing here logs the request, and while the call
// runs the Plugin records and logs only fixed sentences of the host's and the
// kind of thing that happened — never what the guest said, fetched or stored
// (callPolicy.describe and callPolicy.secret).

// exportSignInPassword is the password flow's one contract call, as a guest
// export. The seam is in the name for the reason metadata_lookup's is.
const exportSignInPassword = "sign_in_password"

// The redirect flow's two contract calls, as guest exports.
const (
	exportSignInAuthorizeURL = "sign_in_authorize_url"
	exportSignInExchange     = "sign_in_exchange"
)

// The re-check's two contract calls, as guest exports: lookup(subject) for a
// provider that declares sign-in-lookup, and a refresh for one that declares
// sign-in-refresh.
const (
	exportSignInLookup  = "sign_in_lookup"
	exportSignInRefresh = "sign_in_refresh"
)

// registerSignInProvider adds one Plugin's sign-in-provider registration to reg.
// A slug already claimed is NOT registered and says so, for the reason every
// other seam refuses one.
func (s *Set) registerSignInProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	if _, taken := reg.SignInProvider(p.id); taken {
		err := fmt.Errorf("the id %q is already claimed by another Plugin on this server", p.id)
		p.mu.Lock()
		p.refuse(err)
		p.mu.Unlock()
		p.logf("obelo: plugin %s was not registered: %v", p.id, err)
		return
	}
	d := descriptorFor(p.manifest, entry)
	// The DIRECTORY is the identity, always — the same rule the sink path states.
	d.Slug = p.id
	if d.Name == "" {
		d.Name = p.id
	}
	idToken := entry.IDToken
	reg.RegisterSignInProvider(pluginapi.SignInProviderRegistration{Descriptor: d,
		New: func(s pluginapi.Settings) (pluginapi.SignInProvider, error) {
			g, err := p.newSignInProvider(s)
			if err != nil {
				return nil, err
			}
			g.idToken = idToken
			return g, nil
		}})
}

// newSignInProvider builds the adapter this Plugin's registration hands out. It
// refuses for a Plugin that was refused at load, naming the reason.
func (p *Plugin) newSignInProvider(s pluginapi.Settings) (*guestSignInProvider, error) {
	p.mu.Lock()
	disabled, lastErr := p.disabled, p.lastError
	p.mu.Unlock()
	if disabled {
		if lastErr == "" {
			lastErr = "it is disabled"
		}
		return nil, fmt.Errorf("plugin %s: %s", p.id, lastErr)
	}
	if p.compiled == nil {
		return nil, fmt.Errorf("plugin %s: no module is loaded", p.id)
	}
	return &guestSignInProvider{p: p, settings: s}, nil
}

// guestSignInProvider is one Installed Sign-in provider: the Plugin, and the
// Settings the host resolved for it.
type guestSignInProvider struct {
	p        *Plugin
	settings pluginapi.Settings
	// idToken is the provides entry's declaration of which settings hold the
	// issuer and client id its ID tokens are verified against; nil for a plain
	// OAuth2 provider, and for a password-only one.
	idToken *pluginapi.ManifestIDToken
}

var (
	_ pluginapi.SignInProvider         = (*guestSignInProvider)(nil)
	_ pluginapi.SignInRedirectProvider = (*guestSignInProvider)(nil)
	_ pluginapi.SignInLookupProvider   = (*guestSignInProvider)(nil)
	_ pluginapi.SignInRefreshProvider  = (*guestSignInProvider)(nil)
)

// CheckPassword asks the guest about one credential.
//
// The call is made with NO operator target — a Sign-in provider reaches only the
// hosts its manifest lists — under the default budget, which covers the wait
// behind another call as well as the call itself. A failure is recorded,
// because a provider that cannot answer is one an Admin should hear about, but it
// is NOT a strike: anyone can make a login, so a login flood must not be a way to
// disable the provider.
func (g *guestSignInProvider) CheckPassword(ctx context.Context, req pluginapi.SignInPasswordRequest) (pluginapi.SignInPasswordResponse, error) {
	var resp pluginapi.SignInPasswordResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SignInPasswordCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{
		budget:        g.p.opts.CallTimeout,
		secret:        true,
		describe:      "password sign-in",
		noStrike:      true,
		queueInBudget: true,
		socket:        true,
	}
	if err := g.p.callGuestUnder(ctx, policy, exportSignInPassword, "", buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInPasswordResponse{}, err
		}
		return pluginapi.SignInPasswordResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}

// IDTokenAudience is what the host verifies this provider's ID tokens against:
// the issuer and client id the operator typed into the settings the manifest's
// idToken declaration names. declared is false for a provider that declared no
// ID token — a plain OAuth2 provider — and then the other two are empty.
func (g *guestSignInProvider) IDTokenAudience() (issuer, clientID string, declared bool) {
	if g.idToken == nil {
		return "", "", false
	}
	values := g.p.settingValues()
	issuer, _ = values[g.idToken.IssuerSetting].(string)
	clientID, _ = values[g.idToken.ClientIDSetting].(string)
	return strings.TrimSpace(issuer), strings.TrimSpace(clientID), true
}

// redirectTarget is the host a redirect call may reach beyond the manifest's
// list: the issuer the operator typed, for the reason an Event sink may reach
// the receiver they typed. A provider with no issuer reaches its manifest hosts
// and nothing else.
func (g *guestSignInProvider) redirectTarget() string {
	issuer, _, _ := g.IDTokenAudience()
	if issuer == "" {
		return ""
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return ""
	}
	return normalizeHost(u.Hostname())
}

// redirectPolicy is the redirect flow's call policy: the password flow's, with
// its own description. The exchange carries a code and the Plugin's settings carry
// a client secret, so nothing the guest says is recorded, and anyone can start a
// sign-in, so a failure is no strike.
func (g *guestSignInProvider) redirectPolicy(describe string) callPolicy {
	return callPolicy{
		budget:        g.p.opts.CallTimeout,
		secret:        true,
		describe:      describe,
		noStrike:      true,
		queueInBudget: true,
		socket:        true,
		discovery:     g.discoveryURL(),
	}
}

// discoveryURL is the issuer's discovery document, where an issuer whose token
// and userinfo endpoints live on other hosts (Google's do) names them. Empty for
// a provider with no issuer.
func (g *guestSignInProvider) discoveryURL() string {
	issuer, _, _ := g.IDTokenAudience()
	if issuer == "" {
		return ""
	}
	return strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
}

// beginDiscovery records the issuer's discovery document for the call in flight,
// and answers the function that forgets it and every host it named. An empty
// discoveryURL admits nothing.
func (p *Plugin) beginDiscovery(discoveryURL string) func() {
	p.mu.Lock()
	p.discovery, p.discovered = discoveryURL, nil
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.discovery, p.discovered = "", nil
		p.mu.Unlock()
	}
}

// noteDiscovery reads a fetched document as the issuer's discovery document when
// it is exactly that — the URL the call's policy named answered 200 itself, with
// no redirect on the way, naming the same issuer — and admits the https origins
// of its token and userinfo endpoints for the rest of the call. Only those two:
// the authorization endpoint is the browser's to reach and the key set the
// host's own.
func (p *Plugin) noteDiscovery(answered *http.Request, status int, body []byte) {
	p.mu.Lock()
	discovery := p.discovery
	p.mu.Unlock()
	if discovery == "" || status != 200 || answered == nil || answered.Response != nil ||
		answered.URL.String() != discovery {
		return
	}
	var doc struct {
		Issuer   string `json:"issuer"`
		Token    string `json:"token_endpoint"`
		Userinfo string `json:"userinfo_endpoint"`
	}
	if json.Unmarshal(body, &doc) != nil ||
		strings.TrimSuffix(doc.Issuer, "/")+"/.well-known/openid-configuration" != discovery {
		return
	}
	named := map[string]bool{}
	for _, endpoint := range []string{doc.Token, doc.Userinfo} {
		if u, err := url.Parse(endpoint); err == nil {
			if origin := httpsOrigin(u); origin != "" {
				named[origin] = true
			}
		}
	}
	p.mu.Lock()
	if p.discovery == discovery {
		p.discovered = named
	}
	p.mu.Unlock()
}

// discoveryNamed reports whether the issuer's discovery document, read during
// the call in flight, named target's https origin as its token or userinfo
// endpoint's.
func (p *Plugin) discoveryNamed(target *url.URL) bool {
	origin := httpsOrigin(target)
	if origin == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.discovered[origin]
}

// httpsOrigin is u's scheme, host and port with the default port spelled out,
// or empty for anything that is not an https URL with a host.
func httpsOrigin(u *url.URL) string {
	host := normalizeHost(u.Hostname())
	if u.Scheme != "https" || host == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return "https://" + net.JoinHostPort(host, port)
}

// AuthorizeURL asks the guest where to send the browser.
func (g *guestSignInProvider) AuthorizeURL(ctx context.Context, req pluginapi.SignInAuthorizeRequest) (pluginapi.SignInAuthorizeResponse, error) {
	var resp pluginapi.SignInAuthorizeResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SignInAuthorizeCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	if err := g.p.callGuestUnder(ctx, g.redirectPolicy("redirect sign-in"), exportSignInAuthorizeURL,
		g.redirectTarget(), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInAuthorizeResponse{}, err
		}
		return pluginapi.SignInAuthorizeResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}

// Exchange asks the guest to turn the code the browser came back with into an
// identity. What it answers is judged by the host, never here.
func (g *guestSignInProvider) Exchange(ctx context.Context, req pluginapi.SignInExchangeRequest) (pluginapi.SignInExchangeResponse, error) {
	var resp pluginapi.SignInExchangeResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SignInExchangeCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	if err := g.p.callGuestUnder(ctx, g.redirectPolicy("redirect sign-in exchange"), exportSignInExchange,
		g.redirectTarget(), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInExchangeResponse{}, err
		}
		return pluginapi.SignInExchangeResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}

// Lookup asks the guest whether an identity it vouched for still exists, and
// its groups now. The host asks only a provider that declared sign-in-lookup.
// Nobody is present and nobody can make one happen but the host's schedule or
// an Admin, and a failure is still no strike: a directory that is down for a
// night must not disable the provider every person signs in with.
func (g *guestSignInProvider) Lookup(ctx context.Context, req pluginapi.SignInLookupRequest) (pluginapi.SignInLookupResponse, error) {
	var resp pluginapi.SignInLookupResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SignInLookupCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	if err := g.p.callGuestUnder(ctx, g.redirectPolicy("sign-in re-check"), exportSignInLookup,
		g.redirectTarget(), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInLookupResponse{}, err
		}
		return pluginapi.SignInLookupResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}

// Refresh asks the guest to redeem a refresh token. The call carries the token,
// so nothing the guest says is recorded. What it answers — the ID token above
// all — is judged by the host, never here.
func (g *guestSignInProvider) Refresh(ctx context.Context, req pluginapi.SignInRefreshRequest) (pluginapi.SignInRefreshResponse, error) {
	var resp pluginapi.SignInRefreshResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.SignInRefreshCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	if err := g.p.callGuestUnder(ctx, g.redirectPolicy("sign-in refresh"), exportSignInRefresh,
		g.redirectTarget(), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInRefreshResponse{}, err
		}
		return pluginapi.SignInRefreshResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}
