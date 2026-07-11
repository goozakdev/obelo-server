package enrich

import "time"

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

	// FanartTV / TheAudioDB — the optional artist-image (and, for TheAudioDB, bio)
	// sources for the Music kind. A key wraps MusicBrainz in the fill-only chain;
	// no key keeps plain MusicBrainz with zero calls to either host (ADR-0001).
	FanartTVAPIKey    string
	FanartTVBaseURL   string
	TheAudioDBAPIKey  string
	TheAudioDBBaseURL string
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

// videoProviderKey returns the API key configured for a video-serving provider in
// this config (empty ⇒ not active). It is how BuildProvider decides which video
// sources to compose: fanart.tv rides its single key across both the video and
// music chains.
func (c ProviderConfig) videoProviderKey(slug string) string {
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
	default:
		return ""
	}
}

// newVideoProvider constructs the video-serving provider for a slug from this
// config, or nil for a slug that serves no video kind. It is the one place a video
// source is built, so both the authoritative lead and the fill-only supplements go
// through it (the difference is only their POSITION in the chain, ADR-0027).
func (c ProviderConfig) newVideoProvider(slug string) MetadataProvider {
	switch slug {
	case SlugTMDB:
		return NewTMDBProvider(c.TMDBAPIKey, c.MetadataLanguage, c.TMDBBaseURL, c.TMDBImageBaseURL)
	case SlugOMDb:
		return NewOMDbProvider(c.OMDbAPIKey, c.OMDbBaseURL)
	case SlugTheTVDB:
		return NewTheTVDBProvider(c.TheTVDBAPIKey, c.TheTVDBBaseURL)
	case SlugAniDB:
		return NewAniDBProvider(c.AniDBAPIKey, c.AniDBBaseURL, c.MetadataLanguage)
	case SlugFanartTV:
		return NewFanartTVProvider(c.FanartTVAPIKey, c.FanartTVBaseURL)
	default:
		return nil
	}
}

// authoritativeSlugFor returns the slug of the provider LEADING a given media kind
// in this effective config: the video authoritative for the video kinds, MusicBrainz
// for the music kinds. Used by the per-item override precedence (issue 06) to decide
// whether a pinned Title's record provider differs from the Library's leader.
func (c ProviderConfig) authoritativeSlugFor(kind string) string {
	switch kind {
	case "artist", "album", "track":
		return SlugMusicBrainz
	default:
		return c.videoAuthoritativeSlug()
	}
}

// providerReachable reports whether a provider is usable in this effective config —
// its key is present (video), or (MusicBrainz) the music kind is on. It is how the
// pass decides a pinned Title's record provider is still reachable (issue 06): a
// policy change that cleared/muted the provider makes its key absent here.
func (c ProviderConfig) providerReachable(slug string) bool {
	if slug == SlugMusicBrainz {
		return c.musicEnabled()
	}
	return c.videoProviderKey(slug) != ""
}

// videoEnabled reports whether the Movie/TV kinds enrich: video is on exactly when
// the Library's AUTHORITATIVE video provider is keyed (mirrors the old "TMDB has a
// key" rule when the authoritative is the default TMDB). A repointed authoritative
// that is keyed turns video on even if TMDB itself is unkeyed; a supplement never
// turns video on by itself.
func (c ProviderConfig) videoEnabled() bool {
	return c.videoProviderKey(c.videoAuthoritativeSlug()) != ""
}

// musicEnabled reports whether the Music kind enriches: MusicBrainz + Cover Art
// Archive need no key, so Music turns on via its own opt-in — or alongside a TMDB
// key, which enables every kind (mirrors config.MusicEnrichmentEnabled).
func (c ProviderConfig) musicEnabled() bool { return c.MusicBrainzEnabled || c.TMDBAPIKey != "" }

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
func (c ProviderConfig) videoSupplements(authoritative string) []MetadataProvider {
	var out []MetadataProvider
	for _, e := range registry {
		if e.Slug == authoritative || !e.serves(KindVideo) {
			continue
		}
		if c.videoProviderKey(e.Slug) == "" {
			continue // not keyed → inactive (zero calls to it, ADR-0001)
		}
		if p := c.newVideoProvider(e.Slug); p != nil {
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

// BuildProvider composes the per-kind sources behind the single MetadataProvider
// seam and returns them together with the derived per-kind Enablement snapshot.
// It is the ONE place the enrichment composition lives: app.New calls it at boot,
// and a future settings-driven rebuild calls the same function to hot-swap the
// running Service (see Service.SetProvider). The composition is byte-for-byte the
// block that previously lived inline in app.New:
//
//   - Video is TMDB (the authoritative video source for movie/show/season/episode).
//   - Music is MusicBrainz + Cover Art Archive, wrapped in the fill-only
//     MusicChainProvider only when an image source is configured AND Music is
//     enabled — so an enriched artist also gets a poster (fanart.tv, preferred,
//     MBID-keyed) and a real bio (TheAudioDB, name-capable). With no image key
//     (or Music off) it stays plain MusicBrainz, making zero calls to either host
//     (ADR-0001 explicit opt-in).
func BuildProvider(cfg ProviderConfig) (MetadataProvider, Enablement) {
	// Honor the operator's throttle policy for the configured MusicBrainz host (a
	// mirror may allow more than the public ~1 req/sec; 0 disables throttling).
	mb := NewMusicBrainzProvider(cfg.MusicBrainzBaseURL, cfg.CoverArtBaseURL, cfg.MetadataLanguage)
	mb.MinInterval = cfg.MusicBrainzRateLimit
	var music MetadataProvider = mb
	if cfg.musicImageEnabled() && cfg.musicEnabled() {
		// An image source is configured: wrap MusicBrainz in the fill-only chain,
		// composing whichever sources have a key — fanart.tv (image) and/or
		// TheAudioDB (image + biography).
		var fanart, audioDB MetadataProvider
		if cfg.FanartTVAPIKey != "" {
			fanart = NewFanartTVProvider(cfg.FanartTVAPIKey, cfg.FanartTVBaseURL)
		}
		if cfg.TheAudioDBAPIKey != "" {
			audioDB = NewTheAudioDBProvider(cfg.TheAudioDBAPIKey, cfg.TheAudioDBBaseURL, cfg.MetadataLanguage)
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
	var video MetadataProvider = cfg.newVideoProvider(authSlug)
	if video == nil {
		// A pointer at a non-video slug can't lead the video chain; fall back to the
		// default TMDB lead so the composite is always well-formed (the resolver
		// never sets such a pointer, but BuildProvider stays total).
		video = NewTMDBProvider(cfg.TMDBAPIKey, cfg.MetadataLanguage, cfg.TMDBBaseURL, cfg.TMDBImageBaseURL)
		authSlug = SlugTMDB
	}
	if supplements := cfg.videoSupplements(authSlug); cfg.videoEnabled() && len(supplements) > 0 {
		video = NewVideoChainProvider(video, supplements...)
	}

	provider := CompositeProvider{
		Video: video,
		Music: music,
	}
	return provider, Enablement{Video: cfg.videoEnabled(), Music: cfg.musicEnabled()}
}
