package enrich

import (
	"time"

	"github.com/marioquake/juicebox/internal/store"
)

// The provider registry is the STATIC code catalog of the external metadata
// sources this server knows how to use (metadata-providers 02). It is the single
// place a provider is registered: it drives both how the builder composes a
// source (SettingsToProviderConfig below) and what the settings API renders. It
// holds NO secrets and NO mutable state — enablement, API keys, and base-URL
// overrides live in the DB (store.MetadataProviderRow); a provider's identity,
// the kinds it serves, its role, its key requirement, and its default base URL
// are code.

// Provider slugs — the stable keys shared by the registry, the DB rows, and the
// settings API. Only the sources that already exist are registered here; the
// video-supplement sources (OMDb, TheTVDB, fanart.tv-video) arrive in later
// slices.
const (
	SlugTMDB        = "tmdb"
	SlugOMDb        = "omdb"
	SlugTheTVDB     = "thetvdb"
	SlugMusicBrainz = "musicbrainz"
	SlugCoverArt    = "coverart"
	SlugFanartTV    = "fanarttv"
	SlugTheAudioDB  = "theaudiodb"
)

// Media-kind groups a provider serves. These are the coarse Enrichment kinds
// (Video vs. Music), matching the per-kind Enablement snapshot — not the finer
// CONTEXT.md kinds (movie/show/…). The settings UI groups providers by these.
const (
	KindVideo = "video"
	KindMusic = "music"
)

// ProviderRole classifies a provider's contribution to a kind: the single
// authoritative source that drives identity/most fields, or a fill-only
// supplement that only adds what the authoritative source left empty.
type ProviderRole string

const (
	RoleAuthoritative ProviderRole = "authoritative"
	RoleSupplement    ProviderRole = "supplement"
)

// Registry default base URLs — the public endpoints each provider talks to when
// the operator sets no override. They mirror config's Default*BaseURL constants
// (config keeps its own for env defaulting; the registry is the runtime catalog).
const (
	registryTMDBBaseURL        = "https://api.themoviedb.org/3"
	registryTMDBImageBaseURL   = "https://image.tmdb.org/t/p/original"
	registryOMDbBaseURL        = "https://www.omdbapi.com"
	registryTheTVDBBaseURL     = "https://api4.thetvdb.com/v4"
	registryMusicBrainzBaseURL = "https://musicbrainz.org/ws/2"
	registryCoverArtBaseURL    = "https://coverartarchive.org"
	registryFanartTVBaseURL    = "https://webservice.fanart.tv/v3"
	registryTheAudioDBBaseURL  = "https://www.theaudiodb.com/api/v1/json"
)

// RegistryEntry describes one known provider statically.
type RegistryEntry struct {
	Slug string
	Name string
	// Kinds are the coarse media-kind groups the provider serves (KindVideo /
	// KindMusic). Most serve one; the UI groups by these.
	Kinds []string
	// Role is authoritative vs. fill-only supplement for the kinds it serves.
	Role ProviderRole
	// RequiresKey reports whether the source needs an API key to be enabled. A
	// key-requiring provider can't be turned on with no key on file (the API
	// validates this).
	RequiresKey bool
	// DefaultBaseURL is the public endpoint used when no base-URL override is set.
	DefaultBaseURL string
	// DefaultImageBaseURL is the public artwork host for the few sources whose
	// images come from a host distinct from their API (today only TMDB). Empty for
	// every other provider — a provider with no image host has none to configure,
	// so the settings API omits the field for it.
	DefaultImageBaseURL string
	// Description + DocsURL are human-facing copy for the settings screen.
	Description string
	DocsURL     string
}

// registry is the ordered catalog. Order is stable so the API/UI list is
// deterministic (video authoritative first, then the music sources).
var registry = []RegistryEntry{
	{
		Slug:                SlugTMDB,
		Name:                "The Movie Database (TMDB)",
		Kinds:               []string{KindVideo},
		Role:                RoleAuthoritative,
		RequiresKey:         true,
		DefaultBaseURL:      registryTMDBBaseURL,
		DefaultImageBaseURL: registryTMDBImageBaseURL,
		Description:         "Authoritative source for movies and TV: titles, overviews, cast, genres, and artwork.",
		DocsURL:             "https://www.themoviedb.org/settings/api",
	},
	{
		Slug:           SlugOMDb,
		Name:           "OMDb API",
		Kinds:          []string{KindVideo},
		Role:           RoleSupplement,
		RequiresKey:    true,
		DefaultBaseURL: registryOMDbBaseURL,
		Description:    "Fills a movie's plot, content rating, and genres from the Open Movie Database. Fill-only supplement; requires an API key.",
		DocsURL:        "https://www.omdbapi.com/apikey.aspx",
	},
	{
		Slug:           SlugTheTVDB,
		Name:           "TheTVDB",
		Kinds:          []string{KindVideo},
		Role:           RoleSupplement,
		RequiresKey:    true,
		DefaultBaseURL: registryTheTVDBBaseURL,
		Description:    "Fills TV show/episode titles, overviews, and stills TMDB missed. Fill-only supplement; requires an API key.",
		DocsURL:        "https://thetvdb.com/api-information",
	},
	{
		Slug:           SlugMusicBrainz,
		Name:           "MusicBrainz",
		Kinds:          []string{KindMusic},
		Role:           RoleAuthoritative,
		RequiresKey:    false,
		DefaultBaseURL: registryMusicBrainzBaseURL,
		Description:    "Authoritative open music encyclopedia: artists, albums, and tracks. No API key required.",
		DocsURL:        "https://musicbrainz.org/doc/MusicBrainz_API",
	},
	{
		Slug:           SlugCoverArt,
		Name:           "Cover Art Archive",
		Kinds:          []string{KindMusic},
		Role:           RoleSupplement,
		RequiresKey:    false,
		DefaultBaseURL: registryCoverArtBaseURL,
		Description:    "Album cover artwork keyed to MusicBrainz releases. No API key required; used alongside MusicBrainz.",
		DocsURL:        "https://coverartarchive.org/",
	},
	{
		Slug:           SlugFanartTV,
		Name:           "fanart.tv",
		Kinds:          []string{KindVideo, KindMusic},
		Role:           RoleSupplement,
		RequiresKey:    true,
		DefaultBaseURL: registryFanartTVBaseURL,
		Description:    "High-quality artwork to fill what the authoritative sources lack: artist images for music, plus movie/show posters and backgrounds for video. Fill-only supplement; requires an API key.",
		DocsURL:        "https://fanart.tv/get-an-api-key/",
	},
	{
		Slug:           SlugTheAudioDB,
		Name:           "TheAudioDB",
		Kinds:          []string{KindMusic},
		Role:           RoleSupplement,
		RequiresKey:    true,
		DefaultBaseURL: registryTheAudioDBBaseURL,
		Description:    "Artist images (name-matched) and biographies. Fill-only supplement; requires an API key.",
		DocsURL:        "https://www.theaudiodb.com/api_guide.php",
	},
}

// Registry returns the static provider catalog (a copy of the ordering; the
// entries are immutable value types).
func Registry() []RegistryEntry {
	out := make([]RegistryEntry, len(registry))
	copy(out, registry)
	return out
}

// RegistryEntryFor returns the catalog entry for a slug, or ok=false for an
// unknown slug (the API rejects an unknown slug as PROVIDER_UNKNOWN).
func RegistryEntryFor(slug string) (RegistryEntry, bool) {
	for _, e := range registry {
		if e.Slug == slug {
			return e, true
		}
	}
	return RegistryEntry{}, false
}

// FixedProviderInputs carries the non-per-provider Enrichment inputs threaded into
// every rebuild: the MusicBrainz throttle policy. As of enrichment-runtime-settings
// this is DB-authoritative like the rest of the settings surface — the Manager reads
// it from store.EnrichmentBehavior on each Reload and the settings API reads it the
// same way, so a saved rate-limit change hot-swaps into the rebuilt provider with no
// restart. (The TMDB image host is DB-backed via each provider's own image_base_url
// override — see SettingsToProviderConfig.)
type FixedProviderInputs struct {
	MusicBrainzRateLimit time.Duration
}

// SettingsToProviderConfig maps the persisted provider rows + language into the
// decoupled ProviderConfig the builder consumes, applying registry default base
// URLs where a row has no override. Only rows that are BOTH enabled and (for a
// key-requiring source) hold a key contribute an active source; anything else
// leaves that source off, so the derived Enablement reports it disabled
// (ADR-0001). Cover Art Archive has no independent switch — it is part of the
// MusicBrainz provider — so its row contributes only its base-URL override and
// rides MusicBrainz's enablement.
func SettingsToProviderConfig(rows []store.MetadataProviderRow, language string, fixed FixedProviderInputs) ProviderConfig {
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	// baseURL returns the row's override or the registry default for a slug.
	baseURL := func(slug string) string {
		e, _ := RegistryEntryFor(slug)
		if r, ok := byslug[slug]; ok && r.BaseURL != "" {
			return r.BaseURL
		}
		return e.DefaultBaseURL
	}
	// imageBaseURL returns the row's image-host override or the registry default,
	// for the sources that serve artwork from a distinct host (today only TMDB).
	imageBaseURL := func(slug string) string {
		e, _ := RegistryEntryFor(slug)
		if r, ok := byslug[slug]; ok && r.ImageBaseURL != "" {
			return r.ImageBaseURL
		}
		return e.DefaultImageBaseURL
	}
	// active reports whether a source contributes: its row is enabled and, when the
	// source requires a key, a key is on file.
	active := func(slug string) bool {
		r, ok := byslug[slug]
		if !ok || !r.Enabled {
			return false
		}
		e, _ := RegistryEntryFor(slug)
		if e.RequiresKey && r.APIKey == "" {
			return false
		}
		return true
	}

	cfg := ProviderConfig{
		MetadataLanguage:     language,
		MusicBrainzRateLimit: fixed.MusicBrainzRateLimit,
		TMDBBaseURL:          baseURL(SlugTMDB),
		TMDBImageBaseURL:     imageBaseURL(SlugTMDB),
		OMDbBaseURL:          baseURL(SlugOMDb),
		TheTVDBBaseURL:       baseURL(SlugTheTVDB),
		MusicBrainzBaseURL:   baseURL(SlugMusicBrainz),
		CoverArtBaseURL:      baseURL(SlugCoverArt),
		FanartTVBaseURL:      baseURL(SlugFanartTV),
		TheAudioDBBaseURL:    baseURL(SlugTheAudioDB),
	}
	if active(SlugTMDB) {
		cfg.TMDBAPIKey = byslug[SlugTMDB].APIKey
	}
	if active(SlugOMDb) {
		cfg.OMDbAPIKey = byslug[SlugOMDb].APIKey
	}
	if active(SlugTheTVDB) {
		cfg.TheTVDBAPIKey = byslug[SlugTheTVDB].APIKey
	}
	if active(SlugMusicBrainz) {
		cfg.MusicBrainzEnabled = true
	}
	if active(SlugFanartTV) {
		cfg.FanartTVAPIKey = byslug[SlugFanartTV].APIKey
	}
	if active(SlugTheAudioDB) {
		cfg.TheAudioDBAPIKey = byslug[SlugTheAudioDB].APIKey
	}
	return cfg
}
