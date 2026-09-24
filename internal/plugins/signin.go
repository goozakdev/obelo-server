package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Sign-in provider Extension point, filled by a guest — the password flow.
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
	reg.RegisterSignInProvider(pluginapi.SignInProviderRegistration{Descriptor: d, New: p.newSignInProvider})
}

// newSignInProvider is the pluginapi.SignInProviderFactory this Plugin registers
// with. It refuses for a Plugin that was refused at load, naming the reason.
func (p *Plugin) newSignInProvider(s pluginapi.Settings) (pluginapi.SignInProvider, error) {
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
}

var _ pluginapi.SignInProvider = (*guestSignInProvider)(nil)

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
	}
	if err := g.p.callGuestUnder(ctx, policy, exportSignInPassword, "", buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.SignInPasswordResponse{}, err
		}
		return pluginapi.SignInPasswordResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}
