package enrich

import (
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Metadata provider catalog. It used to be a package-level slice of registry
// entries that every consumer reached for by name; since ADR-0057 it is a VALUE —
// a Catalog derived from the Plugin registry the composition root built — that the
// builder, the Manager, the per-Library resolver and the settings API each receive.
// Nothing here is reachable without being handed one, so a test composes a server
// with exactly the Plugins it means and a Phase 2 loader adds Installed plugins to
// the same value the Built-ins registered into.
//
// It still holds NO secrets and NO mutable state: enablement, API keys and
// base-URL overrides live in the DB (store.MetadataProviderRow); a Plugin's
// identity, the kinds it serves, its Role, its Class, its key requirement, its
// declared capabilities and its default URLs are code, now carried by
// pluginapi.Descriptor exactly as they were by a registry entry.

// Provider slugs — the stable keys shared by the Descriptors, the DB rows, and the
// settings API. The enrichment domain keeps its own copies because a settings row
// it seeds must not depend on which Plugins happen to be registered, and a row
// whose slug no Plugin claims is simply never built.
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

// defaultVideoLeadSlug / defaultMusicLeadSlug are the providers this binary ships
// as each kind's default Authoritative lead — the value a ProviderConfig falls back
// to when no Enrichment policy repointed its kind (ADR-0027).
//
// They live HERE, beside the registrations, and nowhere else in the host
// (.scratch/bundled-plugins: issue 01). Catalog.DefaultAuthoritativeForKind derives
// the same two answers from registration order and is what every path with a
// Catalog in hand uses; these constants exist only for ProviderConfig, which is a
// plain value with no registry to ask. They are the last compiled-in mention of a
// shipped provider outside this file, and ADR-0059 decision 3 is what replaces them:
// once the seven are Bundled plugins the server's ORDERED bundled list is what puts
// TMDB first for video and MusicBrainz first for music, and the lead rule reads it
// off the catalog exactly as it does today.
const (
	defaultVideoLeadSlug = SlugTMDB
	defaultMusicLeadSlug = SlugMusicBrainz
)

// videoIDColumnProvider is the source the `titles.tmdb_id` COLUMN is named after.
// It is not a statement about which provider leads anything: it is the reason a
// video Title carrying an external id is pinned to that source however the Library
// was repointed (see pinnedProviderFor). It sits here with the other shipped names
// so the host proper carries none (.scratch/bundled-plugins: issue 01), and it is
// the one of them a Bundled plugin does NOT retire — the schema gap it names is
// follow-up issue 10, a source-namespaced external-id map on the Title.
const videoIDColumnProvider = SlugTMDB

// The coarse Enrichment media-kind groups (Video vs. Music), the Plugin's default
// chain Role and the ADR-0027 Class, named here in the enrichment domain's own
// vocabulary while BEING the contract's values — the Authoritative-provider
// pointer constrains against a registration fact, so these must not be a second
// set of constants that could drift from the ones a Plugin registers with.
const (
	KindVideo = pluginapi.KindVideo
	KindMusic = pluginapi.KindMusic

	RoleAuthoritative = pluginapi.RoleAuthoritative
	RoleSupplement    = pluginapi.RoleSupplement

	ClassFull        = pluginapi.ClassFull
	ClassArtworkOnly = pluginapi.ClassArtworkOnly
)

// Default base URLs — the public endpoints each source talks to when the operator
// sets no override. They mirror config's Default*BaseURL constants (config keeps
// its own for env defaulting; the Descriptors are the runtime catalog).
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

// MetadataPlugins is the ordered set of Metadata provider Built-ins this binary
// ships: what each one IS (its Descriptor) and how to build it from an Admin's
// Settings. internal/builtins hands the
// whole list to the Registry from the composition root — explicitly, with no
// init() side effect anywhere — and app.New then derives the Catalog from that
// Registry.
//
// The registrations live beside the implementations they describe, because these
// sources are still in this package: a Built-in is code plus its own
// self-description, and splitting the two would make "what does fanart.tv
// require" answerable in one place and "what does fanart.tv do" in another.
// OpenSubtitles differs only because its client moved to internal/builtins, so
// its Descriptor moved with it.
//
// ORDER IS THE CATALOG ORDER and three things read it: the settings screen lists
// sources in it, the fill-only Supplements are composed behind the Authoritative
// provider in it (ADR-0027 keeps one global order), and the first
// authoritative-role Full provider of a kind is that kind's default lead.
//
// EVERY source here is now reached through its factory — no provider is composed
// outside the contract — with exactly one permanent exception: Cover Art Archive
// has no factory and never will. It is not a client; it is the artwork HOST of the
// MusicBrainz Plugin, registered so an Admin can see it, read what it is, and
// override its base URL. The host resolves that override into MusicBrainz's second
// URL (see SettingsToProviderConfig and providerSettings), which is the same thing
// TMDB's image host is, arriving from a neighbouring registration instead of its
// own. Inventing a client for it so that every registration could carry a factory
// would have added a source nothing calls.
func MetadataPlugins() []pluginapi.MetadataProviderRegistration {
	return []pluginapi.MetadataProviderRegistration{
		{
			Descriptor: pluginapi.Descriptor{
				Slug:        SlugTMDB,
				Name:        "The Movie Database (TMDB)",
				Kinds:       []string{KindVideo},
				Role:        RoleAuthoritative,
				Class:       ClassFull,
				RequiresKey: true,
				// TMDB is the only video source that owns a candidate list, a listable
				// image set AND an episode list — which is why the assertions the chains
				// used to make on the concrete type are now three declared capabilities.
				Capabilities: []pluginapi.Capability{
					pluginapi.CapabilitySearch,
					pluginapi.CapabilityArtworkCandidates,
					pluginapi.CapabilityEpisodeList,
				},
				DefaultURL: registryTMDBBaseURL,
				// TMDB serves its images from a host distinct from its API — the one
				// source with a second URL, and the reason the contract's Settings has one.
				DefaultURL2: registryTMDBImageBaseURL,
				Description: "Authoritative source for movies and TV: titles, overviews, cast, genres, and artwork.",
				DocsURL:     "https://www.themoviedb.org/settings/api",
				// The connection probe (ADR-0059 decision 8), declared here beside every
				// other static fact about this source instead of in a switch in
				// connectivity.go. The image host is irrelevant to a probe (no artwork
				// bytes are fetched), so a title lookup is the whole test.
				Probe: &pluginapi.MediaRef{Kind: "movie", Title: "Inception", Year: 2010},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewTMDBProvider(s.Secret, s.Language, s.URL, s.URL2)), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:        SlugOMDb,
				Name:        "OMDb API",
				Kinds:       []string{KindVideo},
				Role:        RoleSupplement,
				Class:       ClassFull,
				RequiresKey: true,
				// A fill-only supplement owns no candidate list and no listable image set
				// (ADR-0019), so it declares neither capability and is never asked for one.
				DefaultURL:  registryOMDbBaseURL,
				Description: "Fills a movie's plot, content rating, and genres from the Open Movie Database. Fill-only supplement; requires an API key.",
				DocsURL:     "https://www.omdbapi.com/apikey.aspx",
				Probe:       &pluginapi.MediaRef{Kind: "movie", Title: "Inception", Year: 2010},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewOMDbProvider(s.Secret, s.URL)), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:        SlugTheTVDB,
				Name:        "TheTVDB",
				Kinds:       []string{KindVideo},
				Role:        RoleSupplement,
				Class:       ClassFull,
				RequiresKey: true,
				DefaultURL:  registryTheTVDBBaseURL,
				Description: "Fills TV show/episode titles, overviews, and stills TMDB missed. Fill-only supplement; requires an API key.",
				DocsURL:     "https://thetvdb.com/api-information",
				Probe:       &pluginapi.MediaRef{Kind: "show", Title: "Breaking Bad"},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewTheTVDBProvider(s.Secret, s.URL)), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:  SlugAniDB,
				Name:  "AniDB",
				Kinds: []string{KindVideo},
				// A Full, authoritative-capable anime source. It is NOT the global default
				// authoritative (TMDB, registered first, is) — AniDB ships globally DISABLED
				// (no seed row) so it touches no Library until one explicitly points its
				// Authoritative provider at it (ADR-0027). RequiresKey: the AniDB HTTP API
				// needs a registered client name, so it is selectable only once configured.
				Role:        RoleAuthoritative,
				Class:       ClassFull,
				RequiresKey: true,
				// It declares search even though its HTTP API offers none: the declaration
				// says "ask me and I will answer", and AniDB's answer is an EMPTY candidate
				// list, which the Edit-item box renders as "no results" rather than as the
				// "search unavailable" an undeclared capability means. Those are different
				// sentences and an anime Library leading with AniDB sees the right one.
				Capabilities: []pluginapi.Capability{
					pluginapi.CapabilitySearch,
					pluginapi.CapabilityArtworkCandidates,
				},
				DefaultURL:  registryAniDBBaseURL,
				Description: "Anime-specialist source for anime movies and series: titles, synopses, and cover art. A Full provider you can lead an anime Library with (its Authoritative provider); ships disabled and requires a registered AniDB HTTP-API client.",
				DocsURL:     "https://wiki.anidb.net/HTTP_API_Definition",
				// AniDB resolves BY anime id and offers no name search, which is precisely
				// why a probe is the author's to declare rather than the host's to guess: a
				// well-known aid (1) answers either a record or an unknown-aid no-match, and
				// both prove the host answered and the client name was accepted.
				Probe: &pluginapi.MediaRef{Kind: "show", Title: "Cowboy Bebop", AniDBID: "1"},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewAniDBProvider(s.Secret, s.URL, s.Language)), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:        SlugMusicBrainz,
				Name:        "MusicBrainz",
				Kinds:       []string{KindMusic},
				Role:        RoleAuthoritative,
				Class:       ClassFull,
				RequiresKey: false,
				Capabilities: []pluginapi.Capability{
					pluginapi.CapabilitySearch,
					pluginapi.CapabilityArtworkCandidates,
					pluginapi.CapabilityAlbumTracklist,
					pluginapi.CapabilityExternalRef,
				},
				DefaultURL: registryMusicBrainzBaseURL,
				// NO DefaultURL2, deliberately, even though this Plugin is built with two
				// hosts: DefaultURL2 is what the settings screen renders as a source's own
				// image-host field, and MusicBrainz does not have one — the Cover Art
				// Archive is a separate registration with a separate row and a separate
				// override. The host reads that row and hands it over as URL2.
				Description: "Authoritative open music encyclopedia: artists, albums, and tracks. No API key required.",
				DocsURL:     "https://musicbrainz.org/doc/MusicBrainz_API",
				// An ARTIST probe: the artwork host is irrelevant to it, which is exactly
				// what lets the Cover Art Archive registration probe this same Plugin with
				// an ALBUM instead and reach the host under test.
				Probe: &pluginapi.MediaRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				mb := NewMusicBrainzProvider(s.URL, s.URL2, s.Language)
				// A nil rate limit keeps the constructor's own ~1 req/sec default, which is
				// the public host's policy; an explicit 0 is the operator saying their
				// mirror has none (ADR-0049). The two are different instructions and the
				// pointer is what keeps them apart.
				if s.RateLimitMillis != nil {
					mb.MinInterval = time.Duration(*s.RateLimitMillis) * time.Millisecond
				}
				return pluginFromProvider(mb), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:        SlugCoverArt,
				Name:        "Cover Art Archive",
				Kinds:       []string{KindMusic},
				Role:        RoleSupplement,
				Class:       ClassArtworkOnly,
				RequiresKey: false,
				DefaultURL:  registryCoverArtBaseURL,
				Description: "Album cover artwork keyed to MusicBrainz releases. No API key required; used alongside MusicBrainz.",
				DocsURL:     "https://coverartarchive.org/",
				// An ALBUM probe, because this host is only ever reached through a cover
				// lookup. It is run against the MUSICBRAINZ Plugin, since this registration
				// has no client of its own — see the one remaining special case in
				// TestConnection, which issue 06 deletes along with this registration.
				Probe: &pluginapi.MediaRef{Kind: "album", Title: "OK Computer", Artist: "Radiohead"},
			},
			// FACTS ONLY, permanently — the one registration with no factory. Cover Art
			// Archive has no client of its own: it is the host MusicBrainz's album cover
			// URLs point at, and the MusicBrainz Plugin is what talks to it. What this
			// registration buys is everything the facts are for and nothing more — a row
			// on the settings screen with a name, a description and a docs link; a
			// base-URL override an operator can point at a mirror, which the host hands
			// to the MusicBrainz Plugin as its second URL; and ClassArtworkOnly, so the
			// Authoritative-provider pointer can never select it (ADR-0027). It declares
			// no capabilities because it answers no calls.
			//
			// buildPlugin skips a nil factory rather than erroring, which is what makes
			// "registered, never built" a state the composition can hold.
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug: SlugFanartTV,
				Name: "fanart.tv",
				// The one source that serves BOTH kinds from one client and one key.
				Kinds:        []string{KindVideo, KindMusic},
				Role:         RoleSupplement,
				Class:        ClassArtworkOnly,
				RequiresKey:  true,
				Capabilities: []pluginapi.Capability{pluginapi.CapabilityArtworkCandidates},
				DefaultURL:   registryFanartTVBaseURL,
				Description:  "High-quality artwork to fill what the authoritative sources lack: artist images for music, plus movie/show posters and backgrounds for video. Fill-only supplement; requires an API key.",
				DocsURL:      "https://fanart.tv/get-an-api-key/",
				// fanart.tv is strictly MBID-keyed, so its probe carries one.
				Probe: &pluginapi.MediaRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead", MusicbrainzID: probeArtistMBID},
			},
			// ONE factory, both kinds. The video chain and the music chain each build
			// their fanart.tv from this registration with the same Settings, so the
			// split the video slice left behind — a Plugin on one side, a direct
			// constructor on the other — is closed. It is still two INSTANCES, exactly
			// as it was two instances before the contract existed: one client, one key,
			// one process-wide host throttle, two positions in the composition. Sharing
			// one instance between the chains would be a cache-sharing change nothing
			// asked for, where two is the composition this Plugin has always had.
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewFanartTVProvider(s.Secret, s.URL)), nil
			},
		},
		{
			Descriptor: pluginapi.Descriptor{
				Slug:         SlugTheAudioDB,
				Name:         "TheAudioDB",
				Kinds:        []string{KindMusic},
				Role:         RoleSupplement,
				Class:        ClassArtworkOnly,
				RequiresKey:  true,
				Capabilities: []pluginapi.Capability{pluginapi.CapabilityArtworkCandidates},
				DefaultURL:   registryTheAudioDBBaseURL,
				Description:  "Artist images (name-matched) and biographies. Fill-only supplement; requires an API key.",
				DocsURL:      "https://www.theaudiodb.com/api_guide.php",
				Probe:        &pluginapi.MediaRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"},
			},
			New: func(s pluginapi.Settings) (pluginapi.MetadataProvider, error) {
				return pluginFromProvider(NewTheAudioDBProvider(s.Secret, s.URL, s.Language)), nil
			},
		},
	}
}

// Catalog is the enrichment domain's view of the registered Metadata provider
// Plugins: the ordered Descriptors, read from the registry they came from. It is a
// plain value — copying it is free and two Catalogs never see each other's
// Plugins.
//
// It holds the REGISTRY AND NOTHING ELSE, and that is the whole of
// .scratch/plugin-system issue 19. It used to copy the Descriptor list into a
// slice at construction, which made every catalog read answer a question about the
// Plugins this server had AT BOOT: a Metadata provider installed from the Plugins
// screen was keyable on the settings screen (those handlers derive a fresh Catalog
// per request) and yet could not lead a Library, could not be toggled per Library
// and was never composed into a chain until a restart, because the enrichment
// Manager held one Catalog for the life of the process. Issue 10 made
// pluginapi.Registry copy-on-write behind an atomic pointer precisely so that
// every reader could hold the same pointer and still see a whole new set of
// Plugins the instant one is installed; deriving the entries on each read is how
// this reader takes that up. Registry.MetadataProviders returns an immutable copy,
// so the derivation is race-free and a reader can never observe a half-built
// catalog.
//
// The GlobalEnrichment and providerSnapshot values that EMBED a Catalog follow for
// free: they hold this same value, with this same pointer, so a swap reaches them
// without anyone having to remember to rebuild them.
//
// A zero Catalog (and one built from a nil Registry) is an empty catalog, which
// reads as "this server has no Metadata providers": every list is empty, every
// slug is unknown, and the composed chain makes no calls. That is the same
// all-off posture an unconfigured server has (ADR-0001), so a narrow test that
// wires no Plugins degrades instead of panicking.
type Catalog struct {
	registry *pluginapi.Registry
}

// NewCatalog derives the enrichment catalog from the Plugin registry the
// composition root built. Registration order is preserved, because it is the
// catalog order — and it is preserved on every read, not captured here: see the
// type's comment.
func NewCatalog(reg *pluginapi.Registry) Catalog {
	return Catalog{registry: reg}
}

// entries is every catalog read's entry point: the registered Metadata providers'
// Descriptors, in registration order, AS OF THIS CALL. It is deliberately not a
// field — see the type comment — and it is cheap: Registry.MetadataProviders reads
// one atomic pointer and copies a slice whose length is the number of Plugins this
// server has.
//
// Callers that need several derivations to agree with each other read it ONCE into
// a local and range that, rather than calling it per loop.
func (c Catalog) entries() []pluginapi.Descriptor {
	regs := c.registry.MetadataProviders()
	out := make([]pluginapi.Descriptor, 0, len(regs))
	for _, r := range regs {
		out = append(out, r.Descriptor)
	}
	return out
}

// Entries returns the registered Metadata providers' Descriptors in catalog order
// (a fresh slice; a Descriptor is an immutable value).
func (c Catalog) Entries() []pluginapi.Descriptor {
	return c.entries()
}

// Entry returns the Descriptor for a slug, or ok=false for a slug no Plugin
// claimed (the API rejects an unknown slug as PROVIDER_UNKNOWN). It asks the
// registry for the one registration rather than deriving the whole list, because
// it is called per slug inside loops.
func (c Catalog) Entry(slug string) (pluginapi.Descriptor, bool) {
	reg, ok := c.registry.MetadataProvider(slug)
	if !ok {
		return pluginapi.Descriptor{}, false
	}
	return reg.Descriptor, true
}

// FullProvidersForKind returns the Full providers that serve the given coarse
// media kind (KindVideo / KindMusic), in catalog order — the STATIC candidate set
// for a Library's Authoritative-provider pointer (ADR-0027). It is capability-only:
// the caller intersects it with runtime reachability (a Full provider is
// SELECTABLE only once it is keyed) to get the usable candidates. Artwork-only
// providers are never returned; they can only ever be Supplements.
func (c Catalog) FullProvidersForKind(kind string) []pluginapi.Descriptor {
	var out []pluginapi.Descriptor
	for _, e := range c.entries() {
		if e.Class == ClassFull && e.Serves(kind) {
			out = append(out, e)
		}
	}
	return out
}

// SupplementProvidersForKind returns the providers of a coarse media kind that a
// Library can force on/off via its per-provider Supplement tri-state (ADR-0027),
// in catalog order: the key-bearing providers (RequiresKey) — the ones the
// resolver activates/mutes by injecting or clearing a key. Keyless providers
// (MusicBrainz, Cover Art Archive) have no independent per-Library toggle (their
// activation rides their authoritative), so they are excluded. The caller removes
// the current Authoritative provider (its off-switch is enrich_enabled, not a
// per-provider toggle) before presenting the list.
func (c Catalog) SupplementProvidersForKind(kind string) []pluginapi.Descriptor {
	var out []pluginapi.Descriptor
	for _, e := range c.entries() {
		if e.RequiresKey && e.Serves(kind) {
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
// so the catalog order is the single source of truth.
func (c Catalog) DefaultAuthoritativeForKind(kind string) string {
	for _, e := range c.entries() {
		if e.Class == ClassFull && e.Role == RoleAuthoritative && e.Serves(kind) {
			return e.Slug
		}
	}
	return ""
}

// buildPlugin constructs the Plugin registered under a slug from the Settings the
// host resolved, and adapts it back to the MetadataProvider the chains call. It
// returns nil when no Plugin claims the slug, when the registration carries no
// factory (facts only — Cover Art Archive, see MetadataPlugins), or when
// the factory refuses these settings: a Plugin that cannot be built makes no calls
// at all rather than half-working (ADR-0001), and the caller composes without it.
func (c Catalog) buildPlugin(slug string, s pluginapi.Settings) MetadataProvider {
	registration, ok := c.registry.MetadataProvider(slug)
	if !ok || registration.New == nil {
		return nil
	}
	plugin, err := registration.New(s)
	if err != nil || plugin == nil {
		return nil
	}
	return ProviderFromPlugin(registration.Descriptor, plugin)
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
func (c Catalog) ProviderStatesFromRows(rows []store.MetadataProviderRow) map[string]ProviderState {
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	entries := c.entries()
	out := make(map[string]ProviderState, len(entries))
	for _, e := range entries {
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
// every rebuild: the operator's pacing policy. As of enrichment-runtime-settings
// this is DB-authoritative like the rest of the settings surface — the Manager reads
// it from store.EnrichmentBehavior on each Reload and the settings API reads it the
// same way, so a saved rate-limit change hot-swaps into the rebuilt provider with no
// restart. (Each source's own hosts are DB-backed via its row's base_url /
// image_base_url overrides — see SettingsToProviderConfig.)
//
// The field keeps the name the DB column and the environment variable have carried
// since the throttle existed for one host. What CHANGED in
// .scratch/bundled-plugins issue 01 is who receives it: this one number is now
// stated to every Metadata provider's Settings, not to MusicBrainz by name
// (ADR-0059 decision 5).
type FixedProviderInputs struct {
	MusicBrainzRateLimit time.Duration
}

// SettingsToProviderConfig maps the persisted provider rows + language into the
// decoupled ProviderConfig the builder consumes, applying each Descriptor's default
// base URL where a row has no override. Only rows that are BOTH enabled and (for a
// key-requiring source) hold a key contribute an active source; anything else
// leaves that source off, so the derived Enablement reports it disabled (ADR-0001).
//
// It treats every registered Plugin the same way (.scratch/bundled-plugins: issue
// 01). It used to fill eight named struct fields from eight named slugs and then
// loop over "everything else"; now there is only the loop, so a Plugin that
// arrives as an Installed module is keyed, pointed at its hosts and switched on by
// exactly the code that does it for one the server shipped.
func (c Catalog) SettingsToProviderConfig(rows []store.MetadataProviderRow, language string, fixed FixedProviderInputs) ProviderConfig {
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	// ONE read of the catalog for the whole derivation. The entries come from the
	// live registry (see Catalog), so a Plugin could in principle be installed or
	// uninstalled between two reads inside this function and leave two halves of the
	// result describing different sets of Plugins. Reading once removes the question
	// rather than answering it, and it is also why the closures below look a
	// Descriptor up in a map instead of asking the registry per slug.
	entries := c.entries()
	descs := make(map[string]pluginapi.Descriptor, len(entries))
	for _, e := range entries {
		descs[e.Slug] = e
	}
	// baseURL returns the row's override or the Descriptor default for a slug.
	baseURL := func(slug string) string {
		e := descs[slug]
		if r, ok := byslug[slug]; ok && r.BaseURL != "" {
			return r.BaseURL
		}
		return e.DefaultURL
	}
	// imageBaseURL returns the row's image-host override or the Descriptor default,
	// for the sources that serve artwork from a distinct host.
	imageBaseURL := func(slug string) string {
		e := descs[slug]
		if r, ok := byslug[slug]; ok && r.ImageBaseURL != "" {
			return r.ImageBaseURL
		}
		return e.DefaultURL2
	}
	// active reports whether a source contributes: its row is enabled and, when the
	// source requires a key, a key is on file. This is the rule an Admin would state
	// — enabled, and holding a secret or needing none — and it is the ONE place the
	// answer is derived for every Plugin alike.
	active := func(slug string) bool {
		r, ok := byslug[slug]
		if !ok || !r.Enabled {
			return false
		}
		e := descs[slug]
		if e.RequiresKey && r.APIKey == "" {
			return false
		}
		return true
	}

	// The operator's pacing policy, stated for every provider (ADR-0059 decision 5)
	// including the 0 that means "do not throttle": it is DB-authoritative and this
	// config carries it, so leaving it absent would silently substitute a Plugin's
	// own default for a saved setting (ADR-0049).
	rateLimitMs := int(fixed.MusicBrainzRateLimit / time.Millisecond)
	cfg := ProviderConfig{
		MetadataLanguage:  language,
		RateLimitMillis:   &rateLimitMs,
		ProviderKeys:      map[string]string{},
		ProviderEndpoints: map[string]ProviderEndpoint{},
		ProviderActive:    map[string]bool{},
	}
	for _, e := range entries {
		// ENDPOINTS for every Plugin, active or not — a URL is not a credential, and a
		// Plugin the per-Library resolver activates (by injecting its key) must already
		// know where its source lives. The row's override, else the Descriptor's
		// default, which for an Installed plugin is what its manifest declared.
		cfg.ProviderEndpoints[e.Slug] = ProviderEndpoint{
			URL:  baseURL(e.Slug),
			URL2: imageBaseURL(e.Slug),
		}
		// The EXPLICIT ACTIVE FACT, stated for every Plugin, true and false alike: the
		// absence of an entry means "infer it from the key", and a switched-off keyless
		// Plugin must say so rather than fall through to a rule that cannot see it
		// (.scratch/plugin-system issue 13).
		cfg.ProviderActive[e.Slug] = active(e.Slug)
		// The KEY, for an active source only. An inactive source's key stays in the DB
		// and out of the composition, which is what makes "switched off" mean zero
		// calls rather than "built but hopefully unused" (ADR-0001).
		if active(e.Slug) {
			cfg.ProviderKeys[e.Slug] = byslug[e.Slug].APIKey
		}
	}

	// THE ONE REMAINING SPECIAL CASE (.scratch/bundled-plugins: issue 01), and issue
	// 06 deletes it. The Cover Art Archive registration has no client of its own: it
	// is the artwork HOST of the music lead's Plugin, so its row's base-URL override
	// is resolved into THAT Plugin's second URL — which is the same thing TMDB's
	// image host is, arriving from a neighbouring registration instead of its own.
	// Issue 06 folds the host into the MusicBrainz plugin's manifest as
	// settings.defaultUrl2 and this, and the `coverart` row, go together.
	if _, ok := descs[SlugCoverArt]; ok {
		if e, ok := cfg.ProviderEndpoints[SlugMusicBrainz]; ok {
			e.URL2 = baseURL(SlugCoverArt)
			cfg.ProviderEndpoints[SlugMusicBrainz] = e
		}
	}
	return cfg
}
