package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Marker provider Extension point, filled by a guest.
//
// Like lyrics.go there is nothing clever here, and the same thing deliberately
// absent: the recording-length check and the precedence of the File's own and
// Detected Markers. Those are the HOST's judgment and live beside the service
// that consumes this seam (internal/markerfetch) and the read that serves
// Markers (internal/store), so a Built-in and an Installed plugin are held to
// exactly the same rule by exactly the same code.
//
// A Marker provider has a remote database to reach, so its call runs under the
// ordinary fetch policy: the manifest's allowlist, plus the host of the URL the
// Settings carry.

// exportMarkerProviderMarkers is this Extension point's one contract call, as a
// guest export, with the seam in the name for the reason web_reference_links has.
const exportMarkerProviderMarkers = "marker_provider_markers"

// registerMarkerProvider adds one Plugin's marker-provider registration to reg.
// A slug already claimed is NOT registered and says so, for the reason every
// other seam refuses one.
func (s *Set) registerMarkerProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	if _, taken := reg.MarkerProvider(p.id); taken {
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
	reg.RegisterMarkerProvider(pluginapi.MarkerProviderRegistration{Descriptor: d, New: p.newMarkerProvider})
}

// newMarkerProvider is the pluginapi.MarkerProviderFactory this Plugin registers
// with. It refuses for a Plugin that was refused at load, naming the reason.
func (p *Plugin) newMarkerProvider(s pluginapi.Settings) (pluginapi.MarkerProvider, error) {
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
	return &guestMarkerProvider{p: p, settings: s}, nil
}

// guestMarkerProvider is one Installed Marker provider: the Plugin, and the
// Settings the host resolved for it.
type guestMarkerProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.MarkerProvider = (*guestMarkerProvider)(nil)

// Markers asks the guest for one File's Markers.
//
// The default call budget applies, and it covers the wait behind another call as
// well as the call itself: a read of a File's Markers waits on this call, and
// reads behind a guest that hangs must not each wait out every call ahead of
// them in turn. A failure counts as a strike, because there is no item to park it
// on. The Settings' URL host is reachable beside the manifest allowlist, as it is
// for a Lyric provider.
func (g *guestMarkerProvider) Markers(ctx context.Context, req pluginapi.MarkersRequest) (pluginapi.MarkersResponse, error) {
	var resp pluginapi.MarkersResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.MarkersCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout, queueInBudget: true}
	if err := g.p.callGuestUnder(ctx, policy, exportMarkerProviderMarkers, hostOf(g.settings.URL), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.MarkersResponse{}, err
		}
		return pluginapi.MarkersResponse{}, fmt.Errorf("plugin %s: markers for %q: %w", g.p.id, req.Title, err)
	}
	return resp, nil
}
