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
	SlugAniDB       = "anidb"
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

// ProviderClass is the capability distinction the per-Library Authoritative-
// provider pointer constrains against (ADR-0027): a Full provider supplies
// complete descriptive records and is therefore eligible to LEAD a Library's
// Enrichment (be its Authoritative provider), whereas an Artwork-only provider
// only fills images/fields the authoritative left empty and can only ever be a
// Supplement. It is orthogonal to Role: Role is the provider's DEFAULT position in
// the global chain; Class is what a per-Library pointer may repoint to.
type ProviderClass string

const (
	ClassFull        ProviderClass = "full"
	ClassArtworkOnly ProviderClass = "artwork"
)

// Registry default base URLs — the public endpoints each provider talks to when
// the operator sets no override. They mirror config's Default*BaseURL constants
// (config keeps its own for env defaulting; the registry is the runtime catalog).
const (
	registryTMDBBaseURL        = "https://api.themoviedb.org/3"
	registryTMDBImageBaseURL   = "https://image.tmdb.org/t/p/original"
	registryOMDbBaseURL        = "https://www.omdbapi.com"
	registryTheTVDBBaseURL     = "https://api4.thetvdb.com/v4"
	registryAniDBBaseURL       = "http://api.anidb.net:9001/httpapi"
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
	// Class is the Full vs. Artwork-only capability (ADR-0027): only a Full provider
	// may be pointed at as a Library's Authoritative provider. Artwork-only providers
	// (fanart.tv, Cover Art Archive, TheAudioDB) can only ever be Supplements.
	Class ProviderClass
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
		Class:               ClassFull,
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
		Class:          ClassFull,
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
		Class:          ClassFull,
		RequiresKey:    true,
		DefaultBaseURL: registryTheTVDBBaseURL,
		Description:    "Fills TV show/episode titles, overviews, and stills TMDB missed. Fill-only supplement; requires an API key.",
		DocsURL:        "https://thetvdb.com/api-information",
	},
	{
		Slug:  SlugAniDB,
		Name:  "AniDB",
		Kinds: []string{KindVideo},
		// A Full, authoritative-capable anime source. It is NOT the global default
		// authoritative (TMDB, registered first, is) — AniDB ships globally DISABLED
		// (no seed row) so it touches no Library until one explicitly points its
		// Authoritative provider at it (ADR-0027). RequiresKey: the AniDB HTTP API
		// needs a registered client name, so it is selectable only once configured.
		Role:           RoleAuthoritative,
		Class:          ClassFull,
		RequiresKey:    true,
		DefaultBaseURL: registryAniDBBaseURL,
		Description:    "Anime-specialist source for anime movies and series: titles, synopses, and cover art. A Full provider you can lead an anime Library with (its Authoritative provider); ships disabled and requires a registered AniDB HTTP-API client.",
		DocsURL:        "https://wiki.anidb.net/HTTP_API_Definition",
	},
	{
		Slug:           SlugMusicBrainz,
		Name:           "MusicBrainz",
		Kinds:          []string{KindMusic},
		Role:           RoleAuthoritative,
		Class:          ClassFull,
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
		Class:          ClassArtworkOnly,
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
		Class:          ClassArtworkOnly,
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
		Class:          ClassArtworkOnly,
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

// serves reports whether a registry entry serves the given coarse media kind.
func (e RegistryEntry) serves(kind string) bool {
	for _, k := range e.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// FullProvidersForKind returns the Full providers that serve the given coarse
// media kind (KindVideo / KindMusic), in registry order — the STATIC candidate set
// for a Library's Authoritative-provider pointer (ADR-0027). It is capability-only:
// the caller intersects it with runtime reachability (a Full provider is
// SELECTABLE only once it is keyed) to get the usable candidates. Artwork-only
// providers are never returned; they can only ever be Supplements.
func FullProvidersForKind(kind string) []RegistryEntry {
	var out []RegistryEntry
	for _, e := range registry {
		if e.Class == ClassFull && e.serves(kind) {
			out = append(out, e)
		}
	}
	return out
}

// SupplementProvidersForKind returns the providers of a coarse media kind that a
// Library can force on/off via its per-provider Supplement tri-state (ADR-0027),
// in registry order: the key-bearing providers (RequiresKey) — the ones the
// resolver activates/mutes by injecting or clearing a key. Keyless providers
// (MusicBrainz, Cover Art Archive) have no independent per-Library toggle (their
// activation rides their authoritative), so they are excluded. The caller removes
// the current Authoritative provider (its off-switch is enrich_enabled, not a
// per-provider toggle) before presenting the list.
func SupplementProvidersForKind(kind string) []RegistryEntry {
	var out []RegistryEntry
	for _, e := range registry {
		if e.RequiresKey && e.serves(kind) {
			out = append(out, e)
		}
	}
	return out
}

// DefaultAuthoritativeForKind returns the slug of the global default Authoritative
// provider for a coarse media kind — the Full provider a Library inherits when its
// authoritative pointer is unset, and the fallback target when a chosen
// authoritative becomes unreachable (ADR-0027): TMDB for video, MusicBrainz for
// music. It is the FIRST authoritative-role Full provider registered for the kind,
// so the registry order is the single source of truth.
func DefaultAuthoritativeForKind(kind string) string {
	for _, e := range registry {
		if e.Class == ClassFull && e.Role == RoleAuthoritative && e.serves(kind) {
			return e.Slug
		}
	}
	return ""
}

// ProviderState is the server-global mutable state of one provider that the
// per-Library resolver needs BEYOND the composed global ProviderConfig (ADR-0027):
// whether it is globally enabled, whether it is keyed (a key on file, or none
// required), and the key itself — so the resolver can honor the always-active-if-
// keyed Authoritative provider (which runs even when globally disabled) and the
// per-provider Supplement tri-state (which can force a globally-disabled-but-keyed
// source on). The composed ProviderConfig carries a key only for ENABLED providers,
// so it alone can't answer "keyed but globally disabled"; this fills that gap.
type ProviderState struct {
	Enabled bool
	Keyed   bool
	APIKey  string
}

// ProviderStatesFromRows derives the per-slug ProviderState map the resolver reads,
// for every registered provider (a provider with no row is disabled + unkeyed,
// unless it requires no key). Sibling to SettingsToProviderConfig — both are pure
// derivations over the same rows, read together on each Manager Reload.
func ProviderStatesFromRows(rows []store.MetadataProviderRow) map[string]ProviderState {
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	out := make(map[string]ProviderState, len(registry))
	for _, e := range registry {
		r, ok := byslug[e.Slug]
		out[e.Slug] = ProviderState{
			Enabled: ok && r.Enabled,
			// A key-requiring provider is keyed only with a key on file; a keyless one
			// (MusicBrainz, Cover Art Archive) is always keyed (nothing to configure).
			Keyed:  !e.RequiresKey || (ok && r.APIKey != ""),
			APIKey: r.APIKey,
		}
	}
	return out
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
		AniDBBaseURL:         baseURL(SlugAniDB),
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
	if active(SlugAniDB) {
		cfg.AniDBAPIKey = byslug[SlugAniDB].APIKey
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
