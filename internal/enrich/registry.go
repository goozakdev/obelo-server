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

// THERE ARE NO BUILT-IN METADATA PROVIDERS LEFT, and this note is the whole trace
// the seven of them leave in this package (.scratch/bundled-plugins: issue 08).
//
// This file used to hold MetadataPlugins() — the ordered list of Built-in
// registrations, each a Descriptor plus a factory that built a Go client living a
// few files away. All seven are Bundled plugins now (ADR-0059): WebAssembly
// modules built from plugins/<id>/, carried in the binary by internal/bundled, and
// installed into <dataDir>/plugins/<id>/ on first boot exactly as an Admin's
// upload would be. Each one's Descriptor — its name, its kinds, its Role and
// Class, its capabilities, its key requirement, its default URLs, the copy the
// settings screen shows and its connection probe — is that module's
// manifest.json, word for word.
//
// Three facts the deleted list used to carry, because each is still load-bearing
// somewhere else:
//
//   - ORDER IS THE CATALOG ORDER, and it is now internal/bundled's ordered `ids`
//     rather than a literal here. The settings screen lists sources in it, the
//     fill-only Supplements are composed behind the Authoritative provider in it
//     (ADR-0027 keeps one global order), and the first authoritative-role Full
//     provider of a kind is that kind's default lead — which is what keeps TMDB
//     leading video and MusicBrainz leading music with no provider name in the
//     host. plugins.Set.RegisterEnabledAround registers the Bundled ids in that
//     order, then the Built-ins, then every other Installed plugin alphabetically.
//     Keeping fanart.tv ahead of TheAudioDB there is what keeps fanart.tv the
//     music chain's preferred image source (Catalog.musicImageSupplements fills
//     its two slots in registration order); internal/enrich's
//     musicimagesupplements_test.go is what holds it.
//   - THE COVER ART ARCHIVE WAS NEVER A SOURCE. It was the artwork HOST the music
//     lead's cover URLs point at, registered as a provider so an Admin could
//     override its base URL — the one entry with no factory, resolved into the
//     music lead's second URL by a special case in three files. It is the
//     MusicBrainz plugin's own `settings.defaultUrl2` now, the way image.tmdb.org
//     is TMDB's, and migration 0071 carried a mirrored host across
//     (.scratch/bundled-plugins: issue 06).
//   - FANART.TV IS ONE INSTANCE, not two. It serves `kinds: [video, music]` from
//     one module with one linear memory, and the host's factory hands each chain a
//     view over it (internal/plugins/metadata.go), serializing every call — the
//     same one key and one pace the two Go instances used to share by convention,
//     made structural.
//
// What composes the catalog now is internal/builtins.Register (OpenSubtitles and
// the Webhook sink) plus the Installed-plugin loader, and NewCatalog below reads
// whatever that produced.

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
// (MusicBrainz) have no independent per-Library toggle (their
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
// factory (a registration that is facts only; nothing this binary ships is in that
// state since .scratch/bundled-plugins issue 06 retired the Cover Art Archive
// row), or when the factory refuses these settings: a Plugin that cannot be built
// makes no calls at all rather than half-working (ADR-0001), and the caller
// composes without it.
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
			// (MusicBrainz) is always keyed (nothing to configure).
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

	// THERE IS NO SPECIAL CASE LEFT (.scratch/bundled-plugins: issue 06). The Cover
	// Art Archive's row used to be resolved into the music lead's SECOND URL here,
	// because that registration had no client of its own and was really a base URL
	// wearing a provider's clothes. It is now the MusicBrainz plugin's own second
	// host, declared by its manifest and read from its row's image_base_url by the
	// same imageBaseURL closure every other Plugin's second host goes through — so
	// this derivation treats every registered Plugin identically, with no exceptions
	// at all.
	return cfg
}
