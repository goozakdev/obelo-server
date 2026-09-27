package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Lyric provider Extension point, filled by a guest.
//
// Like webref.go there is nothing clever here, and the same thing deliberately
// absent: the duration and recording-id checks. Those are the HOST's judgment and
// live beside the service that consumes this seam (internal/lyricfetch), so a
// Built-in and an Installed plugin are held to exactly the same rule by exactly
// the same code.
//
// Unlike a Web reference provider, a Lyric provider has a source to reach, so
// its call runs under the ordinary fetch policy: the manifest's allowlist, plus
// the host of the URL the Settings carry.

// exportLyricProviderLyrics is this Extension point's one contract call, as a
// guest export, with the seam in the name for the reason web_reference_links has.
const exportLyricProviderLyrics = "lyric_provider_lyrics"

// registerLyricProvider adds one Plugin's lyric-provider registration to reg. A
// slug already claimed is NOT registered and says so, for the reason every other
// seam refuses one.
func (s *Set) registerLyricProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	if _, taken := reg.LyricProvider(p.id); taken {
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
	if !d.Serves(pluginapi.KindMusic) {
		// A provider serves the kinds it declared (Descriptor.Serves), and only
		// music has lyrics, so one that declared no music is never asked. Said
		// once, at registration, so an Admin wondering why it never answers has a
		// line to find.
		p.logf("obelo: plugin %s declares no kinds with lyrics (music), so this Lyric provider serves no track", p.id)
	}
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{Descriptor: d, New: p.newLyricProvider})
}

// newLyricProvider is the pluginapi.LyricProviderFactory this Plugin registers
// with. It refuses for a Plugin that was refused at load, naming the reason.
func (p *Plugin) newLyricProvider(s pluginapi.Settings) (pluginapi.LyricProvider, error) {
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
	return &guestLyricProvider{p: p, settings: s}, nil
}

// guestLyricProvider is one Installed Lyric provider: the Plugin, and the
// Settings the host resolved for it.
type guestLyricProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.LyricProvider = (*guestLyricProvider)(nil)

// Lyrics asks the guest for one track's words.
//
// The default call budget applies and a failure counts as a strike, because
// there is no item to park it on. The Settings' URL host is reachable beside the
// manifest allowlist, as it is for a Subtitle provider.
func (g *guestLyricProvider) Lyrics(ctx context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	var resp pluginapi.LyricsResponse
	buildReq := func(callCtx context.Context) any {
		return pluginapi.LyricsCall{Request: req, Settings: g.p.withSettingValues(g.settings, callCtx)}
	}
	policy := callPolicy{budget: g.p.opts.CallTimeout}
	if err := g.p.callGuestUnder(ctx, policy, exportLyricProviderLyrics, addrsOf(g.settings), buildReq, &resp); err != nil {
		if errors.Is(err, ErrDisabled) {
			return pluginapi.LyricsResponse{}, err
		}
		return pluginapi.LyricsResponse{}, fmt.Errorf("plugin %s: lyrics for %q: %w", g.p.id, req.Title, err)
	}
	return resp, nil
}
