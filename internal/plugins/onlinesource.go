package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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

// The Extension point's four contract calls, as guest exports, with the seam in the
// name for the reason web_reference_links has.
const (
	exportOnlineSourceRows    = "online_source_rows"
	exportOnlineSourceRow     = "online_source_row"
	exportOnlineSourceSearch  = "online_source_search"
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

// Icon is the source's tile image: the icon.png the Plugin's package carried,
// stored beside its manifest at install, or nil when it had none. The file is
// checked again as it is read, because the directory is on disk and a hand-placed
// or damaged icon.png must never reach a User as an image the install check
// would have refused. The host's onlinesource service reads it through an optional
// interface, so this is not part of the pluginapi contract.
func (g *guestOnlineSourceProvider) Icon() []byte {
	f, err := os.Open(filepath.Join(g.p.dir, IconFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	icon, err := io.ReadAll(io.LimitReader(f, MaxIconBytes+1))
	if err != nil || len(icon) > MaxIconBytes || checkIcon(icon) != nil {
		return nil
	}
	return icon
}

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
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true, refusalIsAnAnswer: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceRows, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineRowsResponse{}, err
		}
		return pluginapi.OnlineRowsResponse{}, fmt.Errorf("plugin %s: rows: %w", g.p.id, err)
	}
	return resp, nil
}

// Row asks the guest for the next page of one row, under the budget and policy of
// Rows.
func (g *guestOnlineSourceProvider) Row(ctx context.Context, req pluginapi.OnlineRowRequest) (pluginapi.OnlineRowResponse, error) {
	var resp pluginapi.OnlineRowResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.OnlineRowCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true, refusalIsAnAnswer: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceRow, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineRowResponse{}, err
		}
		return pluginapi.OnlineRowResponse{}, fmt.Errorf("plugin %s: row %q: %w", g.p.id, req.RowID, err)
	}
	return resp, nil
}

// Search asks the guest for the items matching a query, under the budget and policy
// of Rows.
func (g *guestOnlineSourceProvider) Search(ctx context.Context, req pluginapi.OnlineSearchRequest) (pluginapi.OnlineSearchResponse, error) {
	var resp pluginapi.OnlineSearchResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.OnlineSearchCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true, refusalIsAnAnswer: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceSearch, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineSearchResponse{}, err
		}
		return pluginapi.OnlineSearchResponse{}, fmt.Errorf("plugin %s: search: %w", g.p.id, err)
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
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true, refusalIsAnAnswer: true}
	if err := g.p.callGuestUnder(ctx, policy, exportOnlineSourceResolve, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.OnlineResolveResponse{}, err
		}
		return pluginapi.OnlineResolveResponse{}, fmt.Errorf("plugin %s: resolve %q: %w", g.p.id, req.ItemID, err)
	}
	return resp, nil
}
