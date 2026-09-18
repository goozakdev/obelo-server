package enrich

import (
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ProviderConfig carries everything BuildProvider needs to compose the per-kind
// metadata sources: the API keys, the base-URL overrides, the preferred metadata
// language, and the MusicBrainz opt-in + throttle policy. It is deliberately
// decoupled from config.Config (ADR-0006 modular monolith — the domain never
// imports config; app.New maps one to the other, exactly as it does for
// playback.Governance). Keeping the inputs in a plain struct also lets a future
// settings-driven rebuild construct it from the DB instead of env without
// touching the composition logic here.
type ProviderConfig struct {
	// Video (TMDB) — the authoritative video source. A non-empty key turns the
	// video kinds on; base-URL overrides point tests/e2e at a local stub.
	TMDBAPIKey       string
	TMDBBaseURL      string
	TMDBImageBaseURL string

	// OMDb — the optional fill-only movie supplement. A key wraps TMDB in the
	// fill-only video chain; no key keeps plain TMDB with zero calls to OMDb
	// (ADR-0001). A supplement never turns the video kinds on by itself — that
	// stays TMDB's job (see videoEnabled).
	OMDbAPIKey  string
	OMDbBaseURL string

	// TheTVDB — the optional fill-only TV supplement (show/season/episode). A key
	// wraps TMDB in the fill-only video chain; no key keeps plain TMDB with zero
	// calls to TheTVDB (ADR-0001). Like OMDb it never turns the video kinds on by
	// itself — that stays TMDB's job (see videoEnabled).
	TheTVDBAPIKey  string
	TheTVDBBaseURL string

	// AniDB — the anime-specialist Full video provider (ADR-0027). Ships globally
	// disabled, so its key is present here ONLY when a Library leads with it (the
	// resolver injects it — always-active-if-keyed) or an Admin globally enabled it.
	// When it leads, it is the video authoritative (AuthoritativeVideo == "anidb");
	// otherwise a keyed AniDB runs as a fill-only supplement like any other.
	AniDBAPIKey  string
	AniDBBaseURL string

	// MetadataLanguage is the preferred language/region for every source.
	MetadataLanguage string

	// AuthoritativeVideo is the registry slug of the Full video provider that LEADS
	// the video chain (ADR-0027). Empty means the global default (TMDB). A Library's
	// Enrichment policy can repoint it at another keyed Full video provider (OMDb,
	// TheTVDB, AniDB), which then leads while the remaining keyed video providers run
	// as fill-only Supplements in registry order. The pointed-at provider's key must
	// be present in this config (the resolver injects it — always-active-if-keyed —
	// even when the provider is globally disabled).
	AuthoritativeVideo string

	// Music (MusicBrainz + Cover Art Archive) — these hosts need no key, so Music
	// enrichment turns on via the explicit MusicBrainzEnabled opt-in (or alongside
	// a TMDB key, which enables every kind). MusicBrainzRateLimit throttles the
	// MusicBrainz host (0 disables throttling for a mirror with no rate policy).
	MusicBrainzEnabled   bool
	MusicBrainzBaseURL   string
	CoverArtBaseURL      string
	MusicBrainzRateLimit time.Duration

	// AuthoritativeMusic is the slug of the Full provider that LEADS the music
	// chain, the twin of AuthoritativeVideo (ADR-0027). Empty means the global
	// default (MusicBrainz), which is what every Library inherits and what the
	// eight Built-ins alone can express — MusicBrainz is the only Full music source
	// this binary ships. It exists so that ADR-0057 decision 4 is true of music as
	// well: a Library may be repointed at a Plugin-provided Full music source, and
	// nothing about that is a special case (the anime case for video is the
	// classical/jazz case for music).
	AuthoritativeMusic string

	// FanartTV / TheAudioDB — the optional artist-image (and, for TheAudioDB, bio)
	// sources for the Music kind. A key wraps MusicBrainz in the fill-only chain;
	// no key keeps plain MusicBrainz with zero calls to either host (ADR-0001).
	FanartTVAPIKey    string
	FanartTVBaseURL   string
	TheAudioDBAPIKey  string
	TheAudioDBBaseURL string

	// ProviderKeys holds the API key of any OTHER registered Plugin — one this
	// binary has no named field for, which from Phase 2 is every Installed plugin
	// and today is any Plugin a test registers. The named fields above stay because
	// they are what the Built-ins' own construction reads and what every existing
	// caller writes; this is the open half of the same map, so that "which provider
	// is keyed" has an answer for a source the builder was not written around.
	//
	// Without it ADR-0057 decision 4 is not actually true: a registered Full
	// provider could be pointed at by an Enrichment policy and then be built with
	// no key, because the resolver had nowhere to inject one. Nil is empty.
	ProviderKeys map[string]string

	// ProviderEndpoints is the URL twin of ProviderKeys: the effective base URLs of
	// any OTHER registered Plugin — from Phase 2, every Installed one.
	//
	// It is separate from the named fields for the same reason, and it closes the
	// same kind of hole one level down. Without it an Installed provider is built
	// with an EMPTY url however carefully its manifest declared a default and
	// however plainly the operator typed an override, because providerSettings'
	// switch has a case per Built-in slug and no default. A source with no endpoint
	// makes no requests, silently — the worst of the three ways this could fail.
	//
	// Unlike ProviderKeys it is filled for every such Plugin whether or not it is
	// active, because a URL is not a credential: knowing where a source lives costs
	// nothing and a Plugin that is built at all must know it. It is READ-ONLY to the
	// per-Library resolver (which copies the config and shares this map), so unlike
	// ProviderKeys it needs no copy-on-write. Nil is empty.
	ProviderEndpoints map[string]ProviderEndpoint

	// ProviderActive is the EXPLICIT per-provider "this source is switched on"
	// fact, for every Plugin this binary has no named field for — from Phase 2,
	// every Installed one (.scratch/plugin-system issue 13).
	//
	// It exists because until it did, "active" was inferred from KEY PRESENCE, and
	// a source that honestly declares it needs no credential then had no way to be
	// on at all: `requiresSecret: false` made a Plugin registered, configurable,
	// offered in the Authoritative-provider dropdown — and never composed, because
	// every gate asked whether its key was non-empty and a keyless source has no
	// key to put there. Issue 04 named the hole and issue 11 hit it hard enough
	// that every test in it declared `requiresSecret: true` to get round it.
	//
	// The rule it replaces the inference with is the one an Admin would state:
	// a provider is composed when it is ENABLED and either holds a secret or needs
	// none. An entry is present for every such Plugin — true or false — and the
	// absence of an entry means "ask the old question", which is what keeps the
	// eight Built-ins byte-identical: each of them has a named key field (or
	// MusicBrainz's own opt-in) that IS its active fact, so none of them ever gets
	// an entry here and none of their gates moved.
	//
	// Written copy-on-write by the per-Library resolver, beside the key, for the
	// reason ProviderKeys is: the resolver works on a struct copy that shares this
	// map with the global config.
	ProviderActive map[string]bool
}

// ProviderEndpoint is one Plugin's effective hosts: its base URL, and the second
// one for the rare source whose images come from elsewhere. It is the pair
// pluginapi.Settings carries as URL and URL2, resolved from the row's overrides or
// the Descriptor's defaults.
type ProviderEndpoint struct {
	URL  string
	URL2 string
}

// videoAuthoritativeSlug is the slug of the Full provider that leads the video
// chain: the configured AuthoritativeVideo, or the registry default (TMDB) when
// unset. It is the single place the "which video source leads" decision reads.
func (c ProviderConfig) videoAuthoritativeSlug() string {
	if c.AuthoritativeVideo != "" {
		return c.AuthoritativeVideo
	}
	return SlugTMDB
}

// musicAuthoritativeSlug is the slug of the Full provider that leads the music
// chain: the configured AuthoritativeMusic, or the registry default (MusicBrainz)
// when unset. Twin of videoAuthoritativeSlug, and the single place the "which
// music source leads" decision reads.
func (c ProviderConfig) musicAuthoritativeSlug() string {
	if c.AuthoritativeMusic != "" {
		return c.AuthoritativeMusic
	}
	return SlugMusicBrainz
}

// providerKey returns the API key configured for a key-bearing provider in this
// config (empty ⇒ not active, and empty for a KEYLESS source, which has no key to
// configure — providerReachable is where that difference is honored). It is how the
// builder decides which sources to compose: fanart.tv rides its single key across
// both the video and music chains, and any provider this binary has no named field
// for is answered from ProviderKeys.
func (c ProviderConfig) providerKey(slug string) string {
	switch slug {
	case SlugTMDB:
		return c.TMDBAPIKey
	case SlugOMDb:
		return c.OMDbAPIKey
	case SlugTheTVDB:
		return c.TheTVDBAPIKey
	case SlugAniDB:
		return c.AniDBAPIKey
	case SlugFanartTV:
		return c.FanartTVAPIKey
	case SlugTheAudioDB:
		return c.TheAudioDBAPIKey
	default:
		return c.ProviderKeys[slug]
	}
}

// providerSettings is the fixed Settings shape (ADR-0057) the host resolves for one
// Plugin out of this config: the key it holds, the effective base URL, a second
// host for the two sources that have one, the server-wide metadata language and —
// for the throttled host — the operator's rate policy. Enabled is true because the
// host only builds a Plugin it means to use.
//
// The two second-URL cases differ in where the value comes from and that is worth
// seeing side by side: TMDB's image host is TMDB's own setting, while MusicBrainz's
// is the Cover Art Archive's — a separate registration with its own row, resolved
// into this Plugin's URL2 because Cover Art Archive has no client of its own.
func (c ProviderConfig) providerSettings(slug string) pluginapi.Settings {
	s := pluginapi.Settings{
		Enabled:  true,
		Secret:   c.providerKey(slug),
		Language: c.MetadataLanguage,
	}
	switch slug {
	case SlugTMDB:
		s.URL, s.URL2 = c.TMDBBaseURL, c.TMDBImageBaseURL
	case SlugOMDb:
		s.URL = c.OMDbBaseURL
	case SlugTheTVDB:
		s.URL = c.TheTVDBBaseURL
	case SlugAniDB:
		s.URL = c.AniDBBaseURL
	case SlugFanartTV:
		s.URL = c.FanartTVBaseURL
	case SlugMusicBrainz:
		s.URL, s.URL2 = c.MusicBrainzBaseURL, c.CoverArtBaseURL
		// Always stated for this one source, including the 0 that means "do not
		// throttle": the operator's rate policy is DB-authoritative and this config
		// carries it, so leaving it absent would silently substitute the Plugin's own
		// default for a saved setting (ADR-0049).
		ms := int(c.MusicBrainzRateLimit / time.Millisecond)
		s.RateLimitMillis = &ms
	case SlugTheAudioDB:
		s.URL = c.TheAudioDBBaseURL
	default:
		// Every Plugin this binary was not written around — from Phase 2, every
		// Installed one. Its manifest's defaultUrl and the operator's override were
		// resolved into ProviderEndpoints by SettingsToProviderConfig, exactly as the
		// named fields above were; without this case the source would be built with
		// no endpoint and would quietly make no requests at all.
		//
		// RateLimitMillis is deliberately left ABSENT here and not set to zero: absent
		// is "use your own default pacing", which is the only honest thing to tell a
		// source this server holds no rate policy for, where 0 would be the operator
		// explicitly saying "do not throttle" (ADR-0049). A per-Plugin rate setting is
		// the generic settings schema's (issue 13).
		e := c.ProviderEndpoints[slug]
		s.URL, s.URL2 = e.URL, e.URL2
	}
	return s
}

// newProvider builds the Plugin registered under a slug from this config and adapts
// it back to a MetadataProvider, or returns nil when no Plugin claims the slug,
// when the one that does does not serve the wanted coarse kind, or when it carries
// no factory. It is the one place a source is built, so the authoritative lead and
// the fill-only supplements of BOTH kinds go through the contract — the difference
// between them is only their POSITION in the chain (ADR-0027).
//
// The kind is passed rather than inferred because the same slug can mean two
// positions: fanart.tv serves video and music, and asking for it "as a music
// source" is what keeps a Catalog holding only video Plugins from composing one
// into the music chain.
func (cat Catalog) newProvider(cfg ProviderConfig, slug, kind string) MetadataProvider {
	e, ok := cat.Entry(slug)
	if !ok || !e.Serves(kind) {
		return nil
	}
	return cat.buildPlugin(slug, cfg.providerSettings(slug))
}

// authoritativeSlugFor returns the slug of the provider LEADING a given media kind
// in this effective config. Used by the per-item override precedence (issue 06) to
// decide whether a pinned Title's record provider differs from the Library's leader.
func (c ProviderConfig) authoritativeSlugFor(kind string) string {
	switch kind {
	case "artist", "album", "track":
		return c.musicAuthoritativeSlug()
	default:
		return c.videoAuthoritativeSlug()
	}
}

// kindGroupFor maps a fine entity kind onto the coarse Enrichment media-kind group
// a Plugin declares (KindVideo / KindMusic) — the translation between the vocabulary
// the Titles are filed in and the one a registration speaks.
func kindGroupFor(kind string) string {
	switch kind {
	case "artist", "album", "track":
		return KindMusic
	default:
		return KindVideo
	}
}

// providerActive reports whether a source is switched ON in this effective config
// — the ONE question every composition gate asks, and the one place the answer is
// decided.
//
// There are two ways a provider can answer it and the difference is which half of
// the server registered it:
//
//   - An EXPLICIT fact, in ProviderActive. Every Plugin this binary has no named
//     key field for gets one, true or false, derived as "enabled, and holding a
//     secret or needing none". It is what lets a keyless Installed provider be on.
//   - KEY PRESENCE, for everything else. The eight Built-ins each have a named
//     field that is their active fact, so none of them reaches the map and none of
//     their behaviour moved. This is the rule that was the ONLY rule before issue
//     13, kept exactly as it was for exactly the sources it was written for.
//
// The explicit fact wins wherever there is one, which is what makes a per-Library
// force-off of an Installed provider mean "off" even though a URL is still in the
// config beside it.
func (c ProviderConfig) providerActive(slug string) bool {
	if on, stated := c.ProviderActive[slug]; stated {
		return on
	}
	return c.providerKey(slug) != ""
}

// providerReachable reports whether a provider is usable in this effective config —
// it is switched on, or (for the keyless MusicBrainz, whose activation rides its
// own opt-in) the music kind is on. It is how the pass decides a pinned Title's
// record provider is still reachable (issue 06): a policy change that cleared or
// muted the provider makes it inactive here.
func (c ProviderConfig) providerReachable(slug string) bool {
	if slug == SlugMusicBrainz {
		return c.musicEnabled()
	}
	return c.providerActive(slug)
}

// videoEnabled reports whether the Movie/TV kinds enrich: video is on exactly when
// the Library's AUTHORITATIVE video provider is ACTIVE (mirrors the old "TMDB has a
// key" rule when the authoritative is the default TMDB, because key presence is
// still what answers that for TMDB). A repointed authoritative that is active turns
// video on even if TMDB itself is unkeyed; a supplement never turns video on by
// itself.
//
// "Active" rather than "keyed" is issue 13's change, and it is the whole of what a
// keyless Installed Full provider needed: a source that declares it requires no
// secret is on when the Admin switched it on, where before it could not be on at
// all (see ProviderConfig.ProviderActive).
func (c ProviderConfig) videoEnabled() bool {
	return c.providerActive(c.videoAuthoritativeSlug())
}

// musicEnabled reports whether the Music kind enriches. With MusicBrainz leading —
// the default, and every Library that has not been repointed — it is MusicBrainz's
// own opt-in, because MusicBrainz and Cover Art Archive need no key: Music turns on
// via MusicBrainzEnabled, or alongside a TMDB key, which enables every kind
// (mirrors config.MusicEnrichmentEnabled).
//
// A Library led by some OTHER Full music provider gates on that provider being
// ACTIVE instead, exactly as videoEnabled gates on the video lead's — a repointed
// lead that is on turns music on even where MusicBrainz's opt-in is off, and a
// supplement still never turns a kind on by itself.
func (c ProviderConfig) musicEnabled() bool {
	if slug := c.musicAuthoritativeSlug(); slug != SlugMusicBrainz {
		return c.providerActive(slug)
	}
	return c.MusicBrainzEnabled || c.TMDBAPIKey != ""
}

// musicImageEnabled reports whether an artist-image source is configured (at
// least one of fanart.tv / TheAudioDB has a key). Mirrors config.MusicImageEnabled.
func (c ProviderConfig) musicImageEnabled() bool {
	return c.FanartTVAPIKey != "" || c.TheAudioDBAPIKey != ""
}

// videoSupplements returns the fill-only video supplements to compose behind the
// authoritative lead: every OTHER keyed video-serving provider, in registry order
// (ADR-0027 keeps the global order — there is no per-Library reordering). The
// authoritative slug is excluded (it leads, it doesn't also fill), so repointing
// the authoritative at OMDb makes TMDB a supplement and vice versa. A supplement
// never turns the video kinds on by itself — that stays the authoritative's job.
func (cat Catalog) videoSupplements(cfg ProviderConfig, authoritative string) []MetadataProvider {
	var out []MetadataProvider
	for _, e := range cat.entries {
		if e.Slug == authoritative || !e.Serves(KindVideo) {
			continue
		}
		if !cfg.providerActive(e.Slug) {
			continue // switched off → zero calls to it (ADR-0001)
		}
		if p := cat.newProvider(cfg, e.Slug, KindVideo); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Enablement is the derived per-kind on/off snapshot BuildProvider produces
// alongside the composed provider. A disabled kind makes no outbound calls and
// its candidates are recorded 'disabled' (ADR-0001 offline-first). It is a value
// type so it can be swapped atomically into the running Service together with the
// provider (see Service.SetProvider).
type Enablement struct {
	// Video gates the Movie/TV kinds (movie/show/season/episode).
	Video bool
	// Music gates the Music kind (artist/album/track).
	Music bool
}

// enabledFor reports whether the given media kind is on in this snapshot. Music
// kinds gate on Music; the video kinds (and any default) gate on Video.
func (e Enablement) enabledFor(kind string) bool {
	switch kind {
	case "artist", "album", "track":
		return e.Music
	default:
		return e.Video
	}
}

// DeriveEnablement returns the per-kind Enablement snapshot for a ProviderConfig
// WITHOUT composing the providers — the same derivation BuildProvider applies,
// exposed so the settings API can report what a saved configuration will enrich
// without constructing (and discarding) the real sources.
func DeriveEnablement(cfg ProviderConfig) Enablement {
	return Enablement{Video: cfg.videoEnabled(), Music: cfg.musicEnabled()}
}

// BuilderFor returns the BuildFunc the Manager rebuilds from on every settings
// save, closed over the Catalog the composition root derived from the Plugin
// registry. It is what app.New hands to NewManager in production; a test
// substitutes its own BuildFunc through app.WithProviderBuilder exactly as before.
func BuilderFor(cat Catalog) BuildFunc { return cat.BuildProvider }

// BuildProvider composes the per-kind sources behind the single MetadataProvider
// seam and returns them together with the derived per-kind Enablement snapshot.
// It is the ONE place the enrichment composition lives: app.New calls it at boot,
// and a settings-driven rebuild calls the same function to hot-swap the running
// Service (see Service.SetProvider).
//
//   - Video composes the Library's Authoritative provider plus the keyed fill-only
//     Supplements, each built THROUGH THE CONTRACT: the Catalog asks the registered
//     Plugin's factory for a source and wraps what comes back in the host-side
//     adapter, so a Built-in and (from Phase 2) an Installed plugin reach the chain
//     by exactly the same path (ADR-0057 decision 5).
//   - Music composes the Library's Authoritative music provider (MusicBrainz by
//     default, its Cover Art Archive host arriving as the Plugin's second URL),
//     wrapped in the fill-only MusicChainProvider only when an image source is
//     configured AND Music is enabled — so an enriched artist also gets a poster
//     (fanart.tv, preferred, MBID-keyed) and a real bio (TheAudioDB, name-capable).
//     With no image key (or Music off) it stays the plain lead, making zero calls
//     to either host (ADR-0001 explicit opt-in). Every one of those sources is now
//     built THROUGH THE CONTRACT, so no provider in this server is registered or
//     composed outside it.
func (cat Catalog) BuildProvider(cfg ProviderConfig) (MetadataProvider, Enablement) {
	// Music leads with the Library's Authoritative music provider — MusicBrainz
	// unless an Enrichment policy repointed it (ADR-0027) — built from its
	// registration exactly as the video lead is. The operator's throttle policy and
	// the Cover Art Archive host travel in that Plugin's Settings.
	musicSlug := cfg.musicAuthoritativeSlug()
	music := cat.newProvider(cfg, musicSlug, KindMusic)
	if music == nil {
		// A pointer at a slug no music Plugin claims can't lead; fall back to the
		// default MusicBrainz lead so the composite is always well-formed (the resolver
		// never sets such a pointer, but BuildProvider stays total). On a Catalog
		// holding no music Plugin at all this stays nil, and a nil sub-provider is the
		// all-off posture CompositeProvider already documents (ADR-0001) — the same
		// answer the video half gives for the same reason.
		music = cat.newProvider(cfg, SlugMusicBrainz, KindMusic)
		musicSlug = SlugMusicBrainz
	}
	if music != nil && cfg.musicImageEnabled() && cfg.musicEnabled() {
		// An image source is configured: wrap the lead in the fill-only chain,
		// composing whichever sources have a key — fanart.tv (image) and/or
		// TheAudioDB (image + biography). Both are artwork-only, so neither can BE the
		// lead; the guard is what keeps a source from supplementing itself.
		var fanart, audioDB MetadataProvider
		if cfg.FanartTVAPIKey != "" && musicSlug != SlugFanartTV {
			fanart = cat.newProvider(cfg, SlugFanartTV, KindMusic)
		}
		if cfg.TheAudioDBAPIKey != "" && musicSlug != SlugTheAudioDB {
			audioDB = cat.newProvider(cfg, SlugTheAudioDB, KindMusic)
		}
		music = NewMusicChainProvider(music, fanart, audioDB)
	}

	// Video composes the Library's Authoritative provider (TMDB by default, or a
	// repointed Full provider — ADR-0027) as the lead, wrapping it in the fill-only
	// chain when at least one other keyed video source is active. The lead is always
	// built (an unconfigured lead simply makes no calls when video is off); the chain
	// wrap is added only when video is on AND a supplement is active, so an
	// all-supplements-off Library is plain lead with zero calls to the others.
	authSlug := cfg.videoAuthoritativeSlug()
	video := cat.newProvider(cfg, authSlug, KindVideo)
	if video == nil {
		// A pointer at a slug no video Plugin claims can't lead the video chain; fall
		// back to the default TMDB lead so the composite is always well-formed (the
		// resolver never sets such a pointer, but BuildProvider stays total). On a
		// Catalog holding no video Plugin at all this stays nil, and a nil
		// sub-provider is the all-off posture CompositeProvider already documents
		// (ADR-0001).
		video = cat.newProvider(cfg, SlugTMDB, KindVideo)
		authSlug = SlugTMDB
	}
	if supplements := cat.videoSupplements(cfg, authSlug); video != nil && cfg.videoEnabled() && len(supplements) > 0 {
		video = NewVideoChainProvider(video, supplements...)
	}

	provider := CompositeProvider{
		Video: video,
		Music: music,
	}
	return provider, Enablement{Video: cfg.videoEnabled(), Music: cfg.musicEnabled()}
}
