package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Web reference provider Extension point, filled by a guest.
//
// Like sink.go there is nothing clever here, and one thing deliberately absent:
// the https-only and held-id checks. Those are the HOST's judgment and live beside
// the service that consumes this seam (internal/webref), so a Built-in and an
// Installed plugin are held to exactly the same rule by exactly the same code.
//
// What this file does own is the network, or rather the lack of it. The call
// runs under an offline policy, so a guest that reaches for http_fetch while it
// answers is refused before anything is resolved or sent — there is no target
// either, so not even the operator's URL is on offer.

// exportWebReferenceLinks is this Extension point's one contract call, as a guest
// export. The seam is in the name for the reason metadata_lookup's is: one module
// may fill several seams without its exports colliding.
const exportWebReferenceLinks = "web_reference_links"

// registerWebReferenceProvider adds one Plugin's web-reference-provider
// registration to reg. A slug already claimed is NOT registered and says so, for
// the reason every other seam refuses one.
func (s *Set) registerWebReferenceProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	_, taken := reg.WebReferenceProvider(p.id)
	d, ok := s.claim(p, entry, taken)
	if !ok {
		return
	}
	if len(d.Kinds) == 0 {
		// A provider serves the kinds it declared (Descriptor.Serves), so one that
		// declared none is never asked. Said once, at registration, so an Admin
		// wondering why it never answers has a line to find.
		p.logf("obelo: plugin %s declares no kinds, so this Web reference provider serves no item", p.id)
	}
	reg.RegisterWebReferenceProvider(pluginapi.WebReferenceProviderRegistration{Descriptor: d, New: p.newWebReferenceProvider})
}

// newWebReferenceProvider is the pluginapi.WebReferenceProviderFactory this
// Plugin registers with. It refuses for a Plugin that was refused at load,
// naming the reason.
func (p *Plugin) newWebReferenceProvider(s pluginapi.Settings) (pluginapi.WebReferenceProvider, error) {
	if err := p.factoryGuard(); err != nil {
		return nil, err
	}
	return &guestWebReferenceProvider{p: p, settings: s}, nil
}

// guestWebReferenceProvider is one Installed Web reference provider: the Plugin,
// and the Settings the host resolved for it.
type guestWebReferenceProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.WebReferenceProvider = (*guestWebReferenceProvider)(nil)

// Links asks the guest for the references an item's ids point at.
//
// The call is made with NO target and under an offline policy: the request
// carries everything the answer can depend on, so there is nothing the guest may
// legitimately fetch. The default call budget applies and a failure counts as a
// strike, because there is no item to park it on.
func (g *guestWebReferenceProvider) Links(ctx context.Context, req pluginapi.WebReferencesRequest) (pluginapi.WebReferencesResponse, error) {
	var resp pluginapi.WebReferencesResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.WebReferencesCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, offline: true}
	if err := g.p.callGuestUnder(ctx, policy, exportWebReferenceLinks, nil, buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.WebReferencesResponse{}, err
		}
		return pluginapi.WebReferencesResponse{}, fmt.Errorf("plugin %s: web references for a %s: %w", g.p.id, req.Kind, err)
	}
	return resp, nil
}
