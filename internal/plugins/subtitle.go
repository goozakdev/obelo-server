package plugins

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Subtitle provider Extension point, filled by a guest (ADR-0021 behind
// ADR-0057, .scratch/plugin-system issue 12).
//
// Like sink.go there is deliberately nothing clever here. An Installed Subtitle
// provider is a pluginapi.SubtitleProvider the way the OpenSubtitles Built-in is,
// so subfetch.BuildProvider composes it from the same settings row, the same
// adapter in internal/subfetch/plugin.go maps its Outcomes back to the domain's
// sentinels, the settings screen lists it beside OpenSubtitles, and the player's
// "search online" reaches it through the handlers unchanged. There is ONE
// outcome mapping in this server and it is not here.
//
// The one thing this file owns that sink.go does not is the byte cap, because
// this is the seam where a guest hands the host a file rather than a verdict.

// The two contract calls of this Extension point, as guest exports. The seam is
// in the name — one module may legitimately be a sink AND a provider, and the
// loader looks up only the exports the manifest's provides list says to expect,
// so two seams in one module never collide (ADR-0058 decision 3: one function
// per contract call).
const (
	exportSubtitleSearch   = "obelo_subtitle_search"
	exportSubtitleDownload = "obelo_subtitle_download"
)

// registerSubtitleProvider adds one Plugin's subtitle-provider registration to
// reg, refused ones included — their factory refuses with the reason, so a Plugin
// an operator placed is on the subtitle-providers screen either way.
//
// A slug already claimed — by the OpenSubtitles Built-in, or by another directory
// — is NOT registered and says so, for the reason a sink is not: an Admin's API
// key must never move onto code the maintainer did not write.
func (s *Set) registerSubtitleProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	if _, taken := reg.SubtitleProvider(p.id); taken {
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
	reg.RegisterSubtitleProvider(pluginapi.SubtitleProviderRegistration{Descriptor: d, New: p.newSubtitleProvider})
}

// newSubtitleProvider is the pluginapi.SubtitleProviderFactory this Plugin
// registers with. It refuses for a Plugin that was refused at load, naming the
// reason, so an Admin who enables a broken Plugin is told why by the settings
// save rather than by a search that quietly finds nothing.
func (p *Plugin) newSubtitleProvider(s pluginapi.Settings) (pluginapi.SubtitleProvider, error) {
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
	return &guestSubtitleProvider{p: p, settings: s}, nil
}

// guestSubtitleProvider is one configured Installed Subtitle provider: the
// Plugin, and the Settings the Admin saved. The settings are held HERE and handed
// to the guest with each call rather than installed into it, which is what keeps
// an API key out of an instance that outlives the search (ADR-0058 decision 5).
type guestSubtitleProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

var _ pluginapi.SubtitleProvider = (*guestSubtitleProvider)(nil)

// SearchSubtitles asks the guest for the candidates it offers for one Title in
// one language.
//
// Everything the guest may key the search by is in the request, INCLUDING the
// content hash: the host computed it from the media file, because a Plugin has no
// filesystem and will never read a frame of anybody's library. A guest that
// answers nothing at all, traps or spins past its deadline is an error here, and
// enough of those in a row disable the Plugin — the subtitle Service degrades to
// an empty candidate list, so a viewer sees "nothing found" rather than a failure
// (ADR-0001).
func (g *guestSubtitleProvider) SearchSubtitles(ctx context.Context, req pluginapi.SubtitleSearchRequest) (pluginapi.SubtitleSearchResponse, error) {
	call := pluginapi.SubtitleSearchCall{Request: req, Settings: g.p.withSettingValues(g.settings)}
	var resp pluginapi.SubtitleSearchResponse

	if err := g.p.callGuest(ctx, exportSubtitleSearch, hostOf(g.settings.URL), call, &resp); err != nil {
		return pluginapi.SubtitleSearchResponse{}, g.wrap("searching for "+req.Language, err)
	}
	return resp, nil
}

// DownloadSubtitle asks the guest for one candidate's bytes, whole, and refuses
// an answer that is bigger than this server will take.
//
// The cap is enforced on BOTH sides of the call and neither is decorative. Going
// in, MaxBytes is narrowed to what the host will accept and restated to the
// guest, so a well-behaved Plugin can refuse before it spends the bandwidth.
// Coming back, the length of what actually arrived is checked against the same
// number — because the contract says the CALLER caps, and a cap a guest enforces
// is not one.
//
// An oversize answer is DISCARDED, whole. It is not truncated to the cap and
// stored, because a truncated subtitle file is one the host would cache, record
// as fetched and serve, and a viewer would watch a subtitle that simply stops. It
// is recorded as a failure of the Plugin — which is what it is — so it reaches
// the Plugin's last error and, repeated, disables it.
func (g *guestSubtitleProvider) DownloadSubtitle(ctx context.Context, req pluginapi.SubtitleDownloadRequest) (pluginapi.SubtitleDownloadResponse, error) {
	limit := g.p.downloadLimit(req.MaxBytes)
	req.MaxBytes = limit

	call := pluginapi.SubtitleDownloadCall{Request: req, Settings: g.p.withSettingValues(g.settings)}
	var resp pluginapi.SubtitleDownloadResponse

	if err := g.p.callGuest(ctx, exportSubtitleDownload, hostOf(g.settings.URL), call, &resp); err != nil {
		return pluginapi.SubtitleDownloadResponse{}, g.wrap("downloading "+req.Candidate.ID, err)
	}
	if int64(len(resp.Data)) > limit {
		err := fmt.Errorf("answered %d bytes for candidate %s, more than the %d this server accepts from a plugin",
			len(resp.Data), req.Candidate.ID, limit)
		g.p.recordOversize(err.Error())
		return pluginapi.SubtitleDownloadResponse{}, fmt.Errorf("plugin %s: %w", g.p.id, err)
	}
	return resp, nil
}

// recordOversize counts an answer bigger than the cap the host stated in the
// request it was answering.
//
// It is counted as a VIOLATION and not as a call failure, for the reason issue
// 09 counts an allowlist breach that way: the call itself worked — the guest was
// asked, it answered, the ABI did its job — and recordFailure's counter is
// cleared by exactly such a call, so a Plugin that answered oversize every single
// time would clear its own record on the way out and never be stopped. A
// violation never resets. Three of them and the Plugin is disabled, whatever else
// it got right in between, because a guest that was TOLD the cap and exceeded it
// anyway is misbehaving rather than unlucky.
func (p *Plugin) recordOversize(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.violations++
	p.lastError = detail
	p.logf("obelo: plugin %s: the download was refused: %s", p.id, detail)
	if p.violations >= p.opts.FailureThreshold && !p.disabled {
		p.disabled = true
		p.logf("obelo: plugin %s is disabled after %d refused answers: %s", p.id, p.violations, detail)
	}
}

// wrap names the Plugin and the call in an error the settings screen and the
// server log can both be read from. A disabled Plugin's refusal passes through
// with its sentinel intact, because "it is switched off" is not the same fact as
// "it failed" and the caller may want to tell them apart.
func (g *guestSubtitleProvider) wrap(what string, err error) error {
	if errors.Is(err, ErrDisabled) {
		return err
	}
	return fmt.Errorf("plugin %s: %s: %w", g.p.id, what, err)
}

// downloadLimit is the number of bytes this server will accept back from a guest
// for one subtitle: the smaller of what the caller asked for and what the loader
// allows.
//
// The fetch limit is the right number and not an approximation of one. It is
// already the cap on a body the host hands a guest through http_fetch, so a
// subtitle larger than it could not have reached the guest through the only way
// out it has; a bigger answer than that is a guest synthesizing bytes, which is
// precisely the case the host-side check exists for. It also bounds what lands in
// the guest's linear memory, which only ever grows (ADR-0058 decision 7).
//
// It is the RESOLVED limit — this Plugin's own, which its manifest may have
// raised up to the host's cap (ADR-0059 decision 6) — rather than the Options
// default, because otherwise that first sentence stops being true the moment a
// manifest raises it.
func (p *Plugin) downloadLimit(requested int64) int64 {
	limit := p.fetchLimit
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}
