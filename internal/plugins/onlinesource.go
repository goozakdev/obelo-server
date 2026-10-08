package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Online source provider Extension point, filled by a guest.
//
// Like markers.go there is nothing clever here, and the same thing deliberately
// absent: the judgment of what a Plugin answers (https-only URLs, URL-safe ids,
// which variant plays). That is the HOST's and lives beside the service that
// consumes this seam (internal/onlinesource), so a Built-in and an Installed
// plugin are held to exactly the same rule by exactly the same code.
//
// A source is a remote service, so its calls run under the ordinary fetch policy:
// the manifest's allowlist, plus the host of the URL the Settings carry.

// The Extension point's two contract calls, as guest exports, with the seam in the
// name for the reason web_reference_links has.
const (
	exportOnlineSourceRows    = "online_source_rows"
	exportOnlineSourceResolve = "online_source_resolve"
)

// registerOnlineSourceProvider adds one Plugin's online-source-provider
// registration to reg. A slug already claimed is NOT registered and says so, for
// the reason every other seam refuses one.
func (s *Set) registerOnlineSourceProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	_, taken := reg.OnlineSourceProvider(p.id)
	d, ok := s.claim(p, entry, taken)
	if !ok {
		return
	}
	reg.RegisterOnlineSourceProvider(pluginapi.OnlineSourceProviderRegistration{Descriptor: d, New: p.newOnlineSourceProvider})
}

// newOnlineSourceProvider is the pluginapi.OnlineSourceProviderFactory this Plugin
// registers with. It refuses for a Plugin that was refused at load, naming the
// reason.
func (p *Plugin) newOnlineSourceProvider(s pluginapi.Settings) (pluginapi.OnlineSourceProvider, error) {
	if err := p.factoryGuard(); err != nil {
		return nil, err
	}
	return &guestOnlineSourceProvider{p: p, settings: s}, nil
}

// guestOnlineSourceProvider is one Installed Online source provider: the Plugin,
// and the Settings the host resolved for it.
type guestOnlineSourceProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.OnlineSourceProvider = (*guestOnlineSourceProvider)(nil)

// Rows asks the guest for the source's rows.
//
// The default call budget applies, and it covers the wait behind another call as
// well as the call itself, because a User is waiting on the page. A failure counts
// as a strike, because there is no item to park it on. The Settings' URL host is
// reachable beside the manifest allowlist, as it is for a Marker provider.
func (g *guestOnlineSourceProvider) Rows(ctx context.Context, req pluginapi.OnlineRowsRequest) (pluginapi.OnlineRowsResponse, error) {
	var resp pluginapi.OnlineRowsResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.OnlineRowsCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceRows, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineRowsResponse{}, err
		}
		return pluginapi.OnlineRowsResponse{}, fmt.Errorf("plugin %s: rows: %w", g.p.id, err)
	}
	return resp, nil
}

// Resolve asks the guest how to play one item, under the same budget and policy as
// Rows.
func (g *guestOnlineSourceProvider) Resolve(ctx context.Context, req pluginapi.OnlineResolveRequest) (pluginapi.OnlineResolveResponse, error) {
	var resp pluginapi.OnlineResolveResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.OnlineResolveCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceResolve, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineResolveResponse{}, err
		}
		return pluginapi.OnlineResolveResponse{}, fmt.Errorf("plugin %s: resolve %q: %w", g.p.id, req.ItemID, err)
	}
	return resp, nil
}
