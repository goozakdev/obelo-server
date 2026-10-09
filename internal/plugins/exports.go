package plugins

import (
	"context"
	"fmt"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// What a module must export, and the install-time probe that checks it (ADR-0069 Q6:
// "compile, instantiate and export every claimed call before anything is swapped").
//
// requiredExports is the ONE place that says which guest exports a manifest's provides
// list implies. It is built from the same constants the per-kind adapters call, so a
// call added to an adapter and forgotten here would be a compile-time-visible omission
// in one file next to the other, not a second list that drifts.

// requiredExport is one export a manifest's claims need, and the claim that needs it.
type requiredExport struct {
	name, because string
}

// requiredExports lists every export the manifest's provides entries imply: the
// calls an Extension point IS, plus one per declared capability.
func requiredExports(m pluginapi.Manifest) []requiredExport {
	var out []requiredExport
	seen := map[string]bool{}
	add := func(name, because string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, requiredExport{name, because})
		}
	}
	for _, e := range m.Provides {
		point := string(e.Kind)
		switch e.Kind {
		case pluginapi.ExtensionEventSink:
			add(exportDeliver, point)
		case pluginapi.ExtensionSubtitleProvider:
			add(exportSubtitleSearch, point)
			add(exportSubtitleDownload, point)
		case pluginapi.ExtensionWebReferenceProvider:
			add(exportWebReferenceLinks, point)
		case pluginapi.ExtensionLyricProvider:
			add(exportLyricProviderLyrics, point)
		case pluginapi.ExtensionMarkerProvider:
			add(exportMarkerProviderMarkers, point)
		case pluginapi.ExtensionOnlineSourceProvider:
			add(exportOnlineSourceRows, point)
			add(exportOnlineSourceRow, point)
			add(exportOnlineSourceSearch, point)
			add(exportOnlineSourceResolve, point)
		case pluginapi.ExtensionMetadataProvider:
			add(exportMetadataLookup, point)
		}
		for _, c := range e.Capabilities {
			because := point + " capability " + string(c)
			switch c {
			case pluginapi.CapabilitySearch:
				if e.Kind == pluginapi.ExtensionMetadataProvider {
					add(exportMetadataSearch, because)
				}
			case pluginapi.CapabilityArtworkCandidates:
				add(exportMetadataArtworkCandidates, because)
			case pluginapi.CapabilityEpisodeList:
				add(exportMetadataSeriesSeasons, because)
				add(exportMetadataSeasonEpisodes, because)
			case pluginapi.CapabilityAlbumTracklist:
				add(exportMetadataAlbumTracklist, because)
				add(exportMetadataReleaseEditions, because)
			case pluginapi.CapabilityExternalRef:
				add(exportMetadataExternalRef, because)
			case pluginapi.CapabilityPasswordSignIn:
				add(exportSignInPassword, because)
			case pluginapi.CapabilityRedirectSignIn:
				add(exportSignInAuthorizeURL, because)
				add(exportSignInExchange, because)
			case pluginapi.CapabilitySignInLookup:
				add(exportSignInLookup, because)
			case pluginapi.CapabilitySignInRefresh:
				add(exportSignInRefresh, because)
			}
		}
	}
	return out
}

// probeInstance is the install-time proof a compiled module will actually run: it is
// instantiated ONCE in a throwaway instance (so _initialize runs, under the ordinary
// call timeout, and a start function that traps or spins is found here and not at the
// first call), every export the manifest's claims need is looked up, and the instance
// is closed. p is the probe Plugin loadOne built; nothing else can reach it.
func (p *Plugin) probeInstance(ctx context.Context) error {
	if p.compiled == nil {
		return fmt.Errorf("the module was never loaded")
	}
	// THE PROBE GRANTS NOTHING. _initialize is the author's code running before the
	// Admin has confirmed anything, and possibly in a package that is refused: it gets
	// no key-value store (kv_* answer that this server has none), and it is marked as a
	// call with no network, so http_fetch and socket refuse before a name is resolved.
	// It also has no settings (settings_get answers a zero Settings outside a call).
	p.opts.KV = nil
	p.mu.Lock()
	p.offline = true
	p.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, p.probeBudget())
	defer cancel()
	p.callDeadline, _ = callCtx.Deadline()
	inst, err := p.compiled.instantiate(callCtx, p)
	if err != nil {
		return fmt.Errorf("the module could not be started: %w", err)
	}
	defer inst.close(context.WithoutCancel(ctx))
	for _, need := range requiredExports(p.manifest) {
		if inst.call(need.name) == nil {
			return fmt.Errorf("the module does not export %q, which its %s claim needs", need.name, need.because)
		}
	}
	return nil
}

// probeBudget is the longest a start-up may take: the largest call budget this
// manifest could be given at runtime, so an _initialize that is slow but legitimate is
// accepted here exactly when it would be accepted at the first call.
func (p *Plugin) probeBudget() time.Duration {
	budget := p.opts.CallTimeout
	if budget <= 0 {
		budget = DefaultCallTimeout
	}
	for _, e := range p.manifest.Provides {
		b := time.Duration(0)
		switch e.Kind {
		case pluginapi.ExtensionMetadataProvider:
			b = p.metaCallBudget
		case pluginapi.ExtensionSubtitleProvider:
			b = p.subtitleCallBudget
		}
		if b > budget {
			budget = b
		}
	}
	return budget
}
