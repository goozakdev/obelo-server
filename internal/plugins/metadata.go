package plugins

import (
	"context"
	"errors"
	"fmt"
	"sync"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Metadata provider Extension point, filled by a guest (ADR-0057 decisions 3
// and 4, ADR-0058; .scratch/plugin-system issue 11).
//
// Like sink.go there is deliberately nothing clever here. An Installed metadata
// source is a pluginapi.MetadataProvider exactly as TMDB and MusicBrainz are, so
// the enrichment Catalog lists it, the settings screen renders it, the builder
// composes it into the per-kind chain by its declared Role, and an Enrichment
// policy may point a Library's Authoritative provider at it when its Class is
// full. Everything that makes a guest different — the sandbox, the instance
// lifecycle, the allowlist — is BELOW this line.
//
// # The host keeps every judgment
//
// Nothing here decides anything. The title acceptance test runs in the service on
// what the guest returned (ADR-0050, issue 01); an undeclared capability answers
// "unavailable" in internal/enrich's host-side adapter WITHOUT a call; and there
// is no second Outcome→sentinel mapping in this file, because enrich.
// ProviderFromPlugin already owns the only one. A guest reaches the chain by the
// same path a Built-in does, and that is the claim this file exists to make true.
//
// # Artwork
//
// A guest returns artwork URLs and never bytes. The HOST's artwork fetcher
// downloads them, under safefetch's redirect policy, into the identity-keyed
// artwork cache — so the cache, the per-host throttle and the raster-only rule are
// unchanged and a Plugin can never put bytes into the cache the host did not
// fetch itself.
//
// The manifest's network allowlist does NOT apply to that download, and that is
// deliberate rather than an oversight: the allowlist bounds what the GUEST may
// reach through http_fetch, and the artwork fetch is not the guest reaching
// anywhere — it is the host fetching a URL a third party returned, which is
// exactly what it already does for every Built-in's poster, and it runs under the
// same policy that protects those (a hop onto 127.0.0.1 or 169.254.169.254 is
// refused by safefetch). Making an author allowlist their own image CDN would add
// a second list that only breaks artwork when it is wrong.

// The guest exports, one per contract call. They are unprefixed in the way the
// Event sink's `deliver` is: obelo_alloc/obelo_free/last_error are ABI plumbing
// and carry the prefix, while a contract call is named after the call. The
// `metadata_` part names the Extension point, so one module can fill two seams
// without its exports colliding.
const (
	exportMetadataLookup            = "metadata_lookup"
	exportMetadataSearch            = "metadata_search"
	exportMetadataArtworkCandidates = "metadata_artwork_candidates"
	exportMetadataSeriesSeasons     = "metadata_series_seasons"
	exportMetadataSeasonEpisodes    = "metadata_season_episodes"
	exportMetadataAlbumTracklist    = "metadata_album_tracklist"
	exportMetadataReleaseEditions   = "metadata_release_editions"
	exportMetadataExternalRef       = "metadata_external_ref"
)

// metaState is the per-call state settings_get reads.
//
// TWO things, and they are not the same lock.
//
//   - mu ORDERS two metadata calls into one Plugin. It is taken before callGuest
//     takes callMu and is never taken by a host function, so there is no path on
//     which a host function running re-entrantly inside a guest call can block on
//     it. It exists because a Plugin can be built twice from one registration —
//     fanart.tv is composed into both chains today — and the second build's
//     Settings must not overwrite the first's while the first call is queued.
//   - settings is read by settings_get under the PLUGIN's mu (the one host
//     functions already use), because that is the lock a host function may take.
//
// A nil settings is "no call is in flight", and settings_get answers a zero
// Settings for it rather than the last call's secret.
type metaState struct {
	mu       sync.Mutex
	settings *pluginapi.Settings
}

// registerMetadataProvider registers one `metadata-provider` entry of a manifest.
// It is Set.registerOne's metadata branch, kept here beside the adapter.
//
// A refused Plugin is registered too, with a factory that refuses — the same rule
// the Event sink follows, and for the same reason: a Plugin an operator placed
// belongs on the settings screen with the sentence that says why it is not
// working, not in a second list nothing else reads.
func (s *Set) registerMetadataProvider(reg *pluginapi.Registry, p *Plugin, entry pluginapi.ManifestProvides) {
	if _, taken := reg.MetadataProvider(p.id); taken {
		// Shadowing a Built-in would move an Admin's API key onto code the
		// maintainer did not write, so the id is refused rather than resolved.
		err := fmt.Errorf("the id %q is already claimed by another Plugin on this server", p.id)
		p.mu.Lock()
		p.refuse(err)
		p.mu.Unlock()
		p.logf("obelo: plugin %s was not registered: %v", p.id, err)
		return
	}
	d := descriptorFor(p.manifest, entry)
	// The DIRECTORY is the identity, always — see registerOne.
	d.Slug = p.id
	if d.Name == "" {
		d.Name = p.id
	}
	reg.RegisterMetadataProvider(pluginapi.MetadataProviderRegistration{
		Descriptor: d,
		New:        p.newMetadataProvider,
	})
}

// newMetadataProvider is the pluginapi.MetadataProviderFactory this Plugin
// registers with. It refuses for a Plugin that was refused at load, naming the
// reason, so a chain is composed WITHOUT a broken source rather than with one that
// fails every call (ADR-0001: the builder skips a factory that refuses).
func (p *Plugin) newMetadataProvider(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
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
	return &guestProvider{p: p, settings: s}, nil
}

// guestProvider is one configured Installed Metadata provider: the Plugin, and the
// Settings the host resolved for it.
//
// The settings are held HERE and reach the guest only through settings_get, for
// the duration of one call. That is the same "secrets at call time only" rule the
// Event sink gets by carrying its Settings in the request: a guest rebuilt after a
// trap starts holding nobody's credential.
type guestProvider struct {
	p        *Plugin
	settings pluginapi.Settings
}

// It implements every optional interface unconditionally, exactly as
// enrich.builtinPlugin does. That is not a claim that the guest answers them: the
// host consults the DESCRIPTOR before it calls, so an undeclared capability costs
// no call, and a declared one whose export is missing answers "unavailable" below.
var (
	_ pluginapi.MetadataProvider  = (*guestProvider)(nil)
	_ pluginapi.EpisodeLister     = (*guestProvider)(nil)
	_ pluginapi.AlbumTracklister  = (*guestProvider)(nil)
	_ pluginapi.ExternalRefParser = (*guestProvider)(nil)
)

// --- the three mandatory calls -----------------------------------------------

// Lookup resolves one ref to one record. It is what a Metadata provider IS, so a
// module that does not export it is broken rather than merely limited, and the
// error says so instead of being dressed up as "no match".
func (g *guestProvider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	var resp pluginapi.LookupResponse
	if err := g.call(ctx, exportMetadataLookup, req, &resp); err != nil {
		return pluginapi.LookupResponse{}, err
	}
	return resp, nil
}

// Search offers candidates for the Edit-item box. The host caps and judges them;
// a Plugin that did not declare CapabilitySearch is never called at all, and the
// box shows the same "unavailable" an unconfigured kind shows.
func (g *guestProvider) Search(ctx context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	var resp pluginapi.SearchResponse
	if err := g.call(ctx, exportMetadataSearch, req, &resp); err != nil {
		return pluginapi.SearchResponse{}, err
	}
	return resp, nil
}

// ArtworkCandidates lists the images the source offers for one role — URLs, which
// the host downloads (see the file comment).
func (g *guestProvider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	var resp pluginapi.ArtworkCandidatesResponse
	if err := g.call(ctx, exportMetadataArtworkCandidates, req, &resp); err != nil {
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	return resp, nil
}

// --- the optional capabilities ------------------------------------------------
//
// Each answers OutcomeUnavailable for a module that does not export the call, and
// that is not a silent failure: the host already refuses to make the call at all
// unless the manifest DECLARED the capability, so reaching one of these means the
// manifest claimed something the module does not do. "Unavailable" is what the
// declaration would have produced had it been honest, which is the graceful
// posture the rest of enrichment takes (ADR-0001), and the missing export is not
// counted as a failure because nothing ran.

func (g *guestProvider) SeriesSeasons(ctx context.Context, req pluginapi.SeriesSeasonsRequest) (pluginapi.SeriesSeasonsResponse, error) {
	var resp pluginapi.SeriesSeasonsResponse
	if err := g.call(ctx, exportMetadataSeriesSeasons, req, &resp); err != nil {
		if unavailable(err) {
			return pluginapi.SeriesSeasonsResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
		}
		return pluginapi.SeriesSeasonsResponse{}, err
	}
	return resp, nil
}

func (g *guestProvider) SeasonEpisodes(ctx context.Context, req pluginapi.SeasonEpisodesRequest) (pluginapi.SeasonEpisodesResponse, error) {
	var resp pluginapi.SeasonEpisodesResponse
	if err := g.call(ctx, exportMetadataSeasonEpisodes, req, &resp); err != nil {
		if unavailable(err) {
			return pluginapi.SeasonEpisodesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
		}
		return pluginapi.SeasonEpisodesResponse{}, err
	}
	return resp, nil
}

// AlbumTracklist answers OutcomeNoMatch — "this album has no tracklist" — for a
// module that does not export it, NOT OutcomeUnavailable. That is the call-scoped
// reading issue 04 pinned: within this one call no-match can only mean the album
// holds nothing, and it is what the pass already knows how to record so the Tracks
// below it fall through to the tiers ADR-0050 puts under them.
func (g *guestProvider) AlbumTracklist(ctx context.Context, req pluginapi.TracklistRequest) (pluginapi.TracklistResponse, error) {
	var resp pluginapi.TracklistResponse
	if err := g.call(ctx, exportMetadataAlbumTracklist, req, &resp); err != nil {
		if unavailable(err) {
			return pluginapi.TracklistResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
		}
		return pluginapi.TracklistResponse{}, err
	}
	return resp, nil
}

// ReleaseGroupEditions answers OutcomeUnavailable — the picker's "not now", which
// degrades to the pasted-URL escape hatch (ADR-0052) — and deliberately NOT the
// no-match its sibling answers: an album with no editions to choose from is a
// real, matched answer, so the absent one has to be distinguishable from it.
func (g *guestProvider) ReleaseGroupEditions(ctx context.Context, req pluginapi.ReleaseEditionsRequest) (pluginapi.ReleaseEditionsResponse, error) {
	var resp pluginapi.ReleaseEditionsResponse
	if err := g.call(ctx, exportMetadataReleaseEditions, req, &resp); err != nil {
		if unavailable(err) {
			return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
		}
		return pluginapi.ReleaseEditionsResponse{}, err
	}
	return resp, nil
}

// ParseExternalRef reads a string an Admin pasted. A Plugin that declares the
// capability owns its source's id shapes, and its three refusals are what the host
// renders — the same two distinct 400 messages the MusicBrainz Built-in produces,
// because the got/want kinds travel in the response rather than in a Go error's
// type (issue 04).
func (g *guestProvider) ParseExternalRef(ctx context.Context, req pluginapi.ExternalRefRequest) (pluginapi.ExternalRefResponse, error) {
	var resp pluginapi.ExternalRefResponse
	if err := g.call(ctx, exportMetadataExternalRef, req, &resp); err != nil {
		if unavailable(err) {
			return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
		}
		return pluginapi.ExternalRefResponse{}, err
	}
	return resp, nil
}

// --- the call ------------------------------------------------------------------

// call makes one call into the guest with this provider's Settings visible to
// settings_get for exactly its duration.
//
// The lock ordering is metaState.mu → callMu, always, and nothing ever takes them
// the other way round: a host function takes neither (it takes Plugin.mu), so the
// re-entrant call a guest makes from inside http_fetch or settings_get cannot
// deadlock against either.
func (g *guestProvider) call(ctx context.Context, export string, req, out any) error {
	g.p.meta.mu.Lock()
	defer g.p.meta.mu.Unlock()

	g.p.setCallSettings(&g.settings)
	defer g.p.setCallSettings(nil)

	// hostOf is the Event sink's: the operator's own configured host is reachable
	// beside the manifest allowlist, because an author cannot know which mirror an
	// operator points their base-URL override at (ADR-0058 decision 5, as amended
	// by issue 09). For a provider that URL is the source's base URL.
	//
	// The budget is the METADATA one — thirty seconds by default, or what this
	// manifest asked for up to the host's cap — and not the ten-second default a
	// sink gets (ADR-0059 decision 6). A lookup makes several fetches and, since
	// pacing became the guest's, waits between them; every one of those fetches is
	// bounded by what is left of this budget, so the guest is always back with an
	// answer before the deadline that would kill it.
	//
	// And the policy is the METADATA one: a guest that runs to completion and
	// cleanly answers an error has ANSWERED — the item is parked, the instance is
	// kept, and no strike is counted (see callPolicy).
	return g.p.callGuestUnder(ctx, callPolicy{
		budget:            g.p.metaCallBudget,
		refusalIsAnAnswer: true,
	}, export, hostOf(g.settings.URL), func(context.Context) any { return req }, out)
}

// setCallSettings publishes (or withdraws) the Settings settings_get answers with.
// It takes the PLUGIN's mu, which is the lock a host function may take.
func (p *Plugin) setCallSettings(s *pluginapi.Settings) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.meta.settings = s
}

// currentSettings is what settings_get answers. A COPY, so a guest's answer can
// never alias the value the host is about to reuse; and a ZERO Settings when no
// call is in flight, so a secret is never readable outside the call it belongs to.
//
// It answers BOTH halves of the settings field: the fixed shape the host resolved
// for this call, and — since issue 13 — the manifest-declared values in Values,
// read fresh so a save takes effect on the next call rather than on the next
// rebuild. Outside a call neither half is answered, declared secrets included.
//
// ctx is the wazero call ctx settings_get was invoked with — the SAME bounded
// callCtx callGuestUnder built this call's deadline from — so
// callRemainingMillis(ctx) reads that deadline straight back rather than
// restating the nominal budget (ADR-0059 decision 6).
func (p *Plugin) currentSettings(ctx context.Context) pluginapi.Settings {
	p.mu.Lock()
	inFlight := p.meta.settings != nil
	var s pluginapi.Settings
	if inFlight {
		s = *p.meta.settings
	}
	p.mu.Unlock()
	if !inFlight {
		return pluginapi.Settings{}
	}
	return p.withSettingValues(s, ctx)
}

// unavailable reports whether an error means "this module does not answer that
// call" rather than "the call failed". Only a missing export qualifies: a trap, a
// deadline kill or a disabled Plugin are real failures and must not be laundered
// into a domain answer, because the host's whole retry policy (ADR-0048) turns on
// telling a transport failure apart from a settled nothing.
func unavailable(err error) bool { return errors.Is(err, errNoExport) }
