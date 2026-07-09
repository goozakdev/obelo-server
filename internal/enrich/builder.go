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

	// MetadataLanguage is the preferred language/region for every source.
	MetadataLanguage string

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

// videoEnabled reports whether the Movie/TV kinds enrich: TMDB is the video
// provider and requires an API key, so video is on exactly when a key is set
// (mirrors config.VideoEnrichmentEnabled).
func (c ProviderConfig) videoEnabled() bool { return c.TMDBAPIKey != "" }

// musicEnabled reports whether the Music kind enriches: MusicBrainz + Cover Art
// Archive need no key, so Music turns on via its own opt-in — or alongside a TMDB
// key, which enables every kind (mirrors config.MusicEnrichmentEnabled).
func (c ProviderConfig) musicEnabled() bool { return c.MusicBrainzEnabled || c.TMDBAPIKey != "" }

// musicImageEnabled reports whether an artist-image source is configured (at
// least one of fanart.tv / TheAudioDB has a key). Mirrors config.MusicImageEnabled.
func (c ProviderConfig) musicImageEnabled() bool {
	return c.FanartTVAPIKey != "" || c.TheAudioDBAPIKey != ""
}

// videoSupplementEnabled reports whether any fill-only video supplement is
// configured (OMDb for movies, TheTVDB for TV, fanart.tv for movie/show artwork).
// Like musicImageEnabled it only wraps the chain — a supplement never turns the
// video kinds on by itself (that stays TMDB's job). fanart.tv's single key feeds
// both the music and video chains.
func (c ProviderConfig) videoSupplementEnabled() bool {
	return c.OMDbAPIKey != "" || c.TheTVDBAPIKey != "" || c.FanartTVAPIKey != ""
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

	var video MetadataProvider = NewTMDBProvider(cfg.TMDBAPIKey, cfg.MetadataLanguage, cfg.TMDBBaseURL, cfg.TMDBImageBaseURL)
	if cfg.videoEnabled() && cfg.videoSupplementEnabled() {
		// A video supplement is configured: wrap TMDB in the fill-only chain,
		// composing whichever supplements have a key — OMDb (movies) and/or TheTVDB
		// (TV). A supplement never turns video on by itself, so this only wraps when
		// TMDB is already the active authoritative source (with all supplements off
		// there are zero calls to them).
		var supplements []MetadataProvider
		if cfg.OMDbAPIKey != "" {
			supplements = append(supplements, NewOMDbProvider(cfg.OMDbAPIKey, cfg.OMDbBaseURL))
		}
		if cfg.TheTVDBAPIKey != "" {
			supplements = append(supplements, NewTheTVDBProvider(cfg.TheTVDBAPIKey, cfg.TheTVDBBaseURL))
		}
		if cfg.FanartTVAPIKey != "" {
			// The same fanart.tv key/base feeds both chains: the music chain uses it for
			// artist images, and here it supplies movie/show posters + backgrounds.
			supplements = append(supplements, NewFanartTVProvider(cfg.FanartTVAPIKey, cfg.FanartTVBaseURL))
		}
		video = NewVideoChainProvider(video, supplements...)
	}

	provider := CompositeProvider{
		Video: video,
		Music: music,
	}
	return provider, Enablement{Video: cfg.videoEnabled(), Music: cfg.musicEnabled()}
}
