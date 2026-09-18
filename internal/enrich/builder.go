package enrich

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ProviderConfig carries everything BuildProvider needs to compose the per-kind
// metadata sources: every registered Plugin's key, endpoints and on/off fact, the
// preferred metadata language, which provider LEADS each kind, and the operator's
// pacing policy. It is deliberately decoupled from config.Config (ADR-0006 modular
// monolith — the domain never imports config; app.New maps one to the other,
// exactly as it does for playback.Governance).
//
// IT NAMES NO PROVIDER (.scratch/bundled-plugins: issue 01). It used to carry a
// named field per shipped source — TMDBAPIKey, MusicBrainzBaseURL, and six more —
// with a `case` per slug in providerKey and providerSettings to read them, plus a
// `default:` case for "any Plugin this binary was not written around" that
// deliberately omitted the rate limit because the host had no policy to hand a
// stranger. That shape is what made a shipped provider a different KIND of thing
// from an installed one: the operator's pacing setting reached exactly one source
// by name, and deleting a Built-in would have left a field behind in a struct
// nothing would complain about. Now there is only the open map, every provider is
// in it, and the `default:` case is the only case (ADR-0059 decisions 5 and 11).
type ProviderConfig struct {
	// MetadataLanguage is the preferred language/region for every source.
	MetadataLanguage string

	// AuthoritativeVideo is the registry slug of the Full video provider that LEADS
	// the video chain (ADR-0027). Empty means the kind's default lead. A Library's
	// Enrichment policy can repoint it at another keyed Full video provider, which
	// then leads while the remaining keyed video providers run as fill-only
	// Supplements in registry order. The pointed-at provider's key must be present
	// in this config (the resolver injects it — always-active-if-keyed — even when
	// the provider is globally disabled).
	AuthoritativeVideo string

	// AuthoritativeMusic is the slug of the Full provider that LEADS the music
	// chain, the twin of AuthoritativeVideo (ADR-0027). Empty means the kind's
	// default lead. It exists so that ADR-0057 decision 4 is true of music as well
	// as video: a Library may be repointed at any Full music source, and nothing
	// about that is a special case (the anime case for video is the classical/jazz
	// case for music).
	AuthoritativeMusic string

	// ProviderKeys holds the API key of every registered Plugin that HOLDS one and
	// is switched on. A source with no entry is unkeyed, which for a key-requiring
	// source is also the answer to "is it active" (see providerActive).
	//
	// Written COPY-ON-WRITE by the per-Library resolver, which works on a struct
	// copy that shares this map with the global config. Nil is empty.
	ProviderKeys map[string]string

	// ProviderEndpoints is the URL twin of ProviderKeys: every registered Plugin's
	// effective hosts, resolved from its row's overrides or its Descriptor's
	// defaults.
	//
	// Unlike ProviderKeys it is filled for every Plugin whether or not it is active,
	// because a URL is not a credential: knowing where a source lives costs nothing
	// and a Plugin that is built at all must know it. Without it a source is built
	// with an EMPTY url however carefully its manifest declared a default and
	// however plainly the operator typed an override, and a source with no endpoint
	// makes no requests, silently — the worst of the ways this could fail. It is
	// READ-ONLY to the per-Library resolver, so unlike ProviderKeys it needs no
	// copy-on-write. Nil is empty.
	ProviderEndpoints map[string]ProviderEndpoint

	// ProviderActive is the EXPLICIT per-provider "this source is switched on" fact
	// (.scratch/plugin-system issue 13).
	//
	// It exists because until it did, "active" was inferred from KEY PRESENCE, and
	// a source that honestly declares it needs no credential then had no way to be
	// on at all: `requiresSecret: false` made a Plugin registered, configurable,
	// offered in the Authoritative-provider dropdown — and never composed, because
	// every gate asked whether its key was non-empty and a keyless source has no
	// key to put there.
	//
	// The rule it replaces the inference with is the one an Admin would state: a
	// provider is composed when it is ENABLED and either holds a secret or needs
	// none. An entry is present for every Plugin the settings derivation saw, true
	// or false; the absence of an entry means "ask the old question", which is what
	// keeps a bare ProviderConfig that only sets keys behaving as it always did.
	//
	// Written copy-on-write by the per-Library resolver, beside the key, for the
	// reason ProviderKeys is.
	ProviderActive map[string]bool

	// RateLimitMillis is the operator's minimum interval between two requests to a
	// source's host, in milliseconds — the ADR-0049 pacing policy, stated to EVERY
	// provider rather than to the one the host used to know by name.
	//
	// It is a POINTER because the two things this server can mean are both
	// meaningful and neither is the other's zero: nil is "this server holds no rate
	// policy — use your own default pacing", and 0 is the operator explicitly
	// saying "do not throttle", which is what they set for a self-hosted mirror
	// with no rate policy. The settings derivation always states it (the value is
	// DB-authoritative, so leaving it absent would silently substitute a Plugin's
	// own default for a saved setting); a narrow test that builds a bare config
	// leaves it nil and every Plugin paces itself.
	//
	// It is ONE number and not one per provider because that is what the settings
	// surface persists (store.EnrichmentBehavior); a per-Plugin rate setting is the
	// generic settings schema's (.scratch/plugin-system issue 13).
	RateLimitMillis *int
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
// chain: the configured AuthoritativeVideo, or this binary's shipped default lead
// when unset. It is the single place the "which video source leads" decision reads.
func (c ProviderConfig) videoAuthoritativeSlug() string {
	if c.AuthoritativeVideo != "" {
		return c.AuthoritativeVideo
	}
	return defaultVideoLeadSlug
}

// musicAuthoritativeSlug is the slug of the Full provider that leads the music
// chain: the configured AuthoritativeMusic, or this binary's shipped default lead
// when unset. Twin of videoAuthoritativeSlug, and the single place the "which
// music source leads" decision reads.
func (c ProviderConfig) musicAuthoritativeSlug() string {
	if c.AuthoritativeMusic != "" {
		return c.AuthoritativeMusic
	}
	return defaultMusicLeadSlug
}

// providerKey returns the API key configured for a key-bearing provider in this
// config (empty ⇒ not active, and empty for a KEYLESS source, which has no key to
// configure — providerActive is where that difference is honored).
func (c ProviderConfig) providerKey(slug string) string {
	return c.ProviderKeys[slug]
}

// providerSettings is the fixed Settings shape (ADR-0057) the host resolves for one
// Plugin out of this config: the key it holds, its effective hosts, the server-wide
// metadata language and the operator's pacing policy. Enabled is true because the
// host only builds a Plugin it means to use.
//
// There is no case in it and there is no source it was written around. Every field
// it fills, it fills the same way for every Plugin — which is the whole of what
// "the operator's rate limit reaches every provider" needed (ADR-0059 decision 5),
// because the rate limit was the one thing this function used to hand to exactly
// one source by name and withhold from everything else.
func (c ProviderConfig) providerSettings(slug string) pluginapi.Settings {
	e := c.ProviderEndpoints[slug]
	s := pluginapi.Settings{
		Enabled:  true,
		Secret:   c.providerKey(slug),
		URL:      e.URL,
		URL2:     e.URL2,
		Language: c.MetadataLanguage,
	}
	if c.RateLimitMillis != nil {
		// Copied, not aliased: a Plugin holds its Settings for its lifetime and two
		// Plugins must never share one operator's number through a pointer.
		ms := *c.RateLimitMillis
		s.RateLimitMillis = &ms
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
// There are two ways a provider can answer it:
//
//   - An EXPLICIT fact, in ProviderActive, derived as "enabled, and holding a
//     secret or needing none". It is what lets a keyless provider be on, and the
//     settings derivation states one for every registered Plugin.
//   - KEY PRESENCE, when there is no explicit fact — a bare ProviderConfig a test
//     built by hand, or a Plugin that arrived after the derivation ran. This is
//     the rule that was the ONLY rule before .scratch/plugin-system issue 13, kept
//     for exactly the case it still answers correctly.
//
// The explicit fact wins wherever there is one, which is what makes a per-Library
// force-off mean "off" even though a URL is still in the config beside it.
func (c ProviderConfig) providerActive(slug string) bool {
	if on, stated := c.ProviderActive[slug]; stated {
		return on
	}
	return c.providerKey(slug) != ""
}

// providerReachable reports whether a provider is usable in this effective config —
// it is switched on, or (for a KIND'S LEAD, whose activation may ride the kind's
// own enablement) the kind is on. It is how the pass decides a pinned Title's
// record provider is still reachable (issue 06): a policy change that cleared or
// muted the provider makes it inactive here.
func (c ProviderConfig) providerReachable(slug string) bool {
	if slug == c.musicAuthoritativeSlug() {
		return c.musicEnabled()
	}
	return c.providerActive(slug)
}

// videoEnabled reports whether the Movie/TV kinds enrich: video is on exactly when
// the Library's AUTHORITATIVE video provider is ACTIVE (mirrors the old "TMDB has a
// key" rule when the authoritative is the default lead, because key presence is
// still what answers that for a key-requiring source). A repointed authoritative
// that is active turns video on even if the default lead itself is unkeyed; a
// supplement never turns video on by itself.
//
// "Active" rather than "keyed" is .scratch/plugin-system issue 13's change, and it
// is the whole of what a keyless Full provider needed: a source that declares it
// requires no secret is on when the Admin switched it on, where before it could not
// be on at all (see ProviderConfig.ProviderActive).
func (c ProviderConfig) videoEnabled() bool {
	return c.providerActive(c.videoAuthoritativeSlug())
}

// musicEnabled reports whether the Music kind enriches: the Library's authoritative
// music provider is ACTIVE, exactly as videoEnabled gates on the video lead's.
//
// The second clause is the original single-switch behaviour, kept: a configured
// VIDEO lead historically turned on every kind, so a server that keyed its video
// source and never touched the music one still enriches music. It used to read
// "MusicBrainzEnabled || TMDBAPIKey != """ and now reads "the music lead is active
// or the video lead is" — the same sentence with the two names taken out of it.
func (c ProviderConfig) musicEnabled() bool {
	if c.providerActive(c.musicAuthoritativeSlug()) {
		return true
	}
	return c.videoEnabled()
}

// musicImageSupplements builds the fill-only artist sources that decorate the music
// lead: the ACTIVE artwork-only music providers, in registry order, minus the lead
// itself. The music chain still has exactly two slots — a preferred MBID-keyed
// image source and a fallback image-plus-biography source — so at most two are
// taken, and registration order is what decides which is which (ADR-0059 decision
// 3 makes that order the server's own). Composing an arbitrary number of
// Supplements is a follow-up (.scratch/bundled-plugins issue 11), not this one.
//
// A registration with no factory builds to nil and is skipped here exactly as it
// is everywhere else, so it can never occupy a slot. Cover Art Archive was the one
// such registration and it is gone (.scratch/bundled-plugins: issue 06) — it was
// never a source, and it is now the music lead's second URL.
func (cat Catalog) musicImageSupplements(cfg ProviderConfig, lead string) []MetadataProvider {
	var out []MetadataProvider
	for _, e := range cat.entries() {
		if len(out) == musicChainSupplementSlots {
			break
		}
		if e.Slug == lead || e.Class != ClassArtworkOnly || !e.Serves(KindMusic) {
			continue
		}
		if !cfg.providerActive(e.Slug) {
			continue // switched off → zero calls to it (ADR-0001)
		}
		if p := cat.newProvider(cfg, e.Slug, KindMusic); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// musicChainSupplementSlots is how many fill-only artist sources MusicChainProvider
// composes: the preferred image source and the image-plus-biography fallback.
const musicChainSupplementSlots = 2

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

// videoSupplements returns the fill-only video supplements to compose behind the
// authoritative lead: every OTHER active video-serving provider, in registry order
// (ADR-0027 keeps the global order — there is no per-Library reordering). The
// authoritative slug is excluded (it leads, it doesn't also fill), so repointing
// the authoritative at another source makes the old lead a supplement and vice
// versa. A supplement never turns the video kinds on by itself — that stays the
// authoritative's job.
func (cat Catalog) videoSupplements(cfg ProviderConfig, authoritative string) []MetadataProvider {
	var out []MetadataProvider
	for _, e := range cat.entries() {
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
//   - Video composes the Library's Authoritative provider plus the active fill-only
//     Supplements, each built THROUGH THE CONTRACT: the Catalog asks the registered
//     Plugin's factory for a source and wraps what comes back in the host-side
//     adapter, so every Plugin reaches the chain by exactly the same path
//     (ADR-0057 decision 5).
//   - Music composes the Library's Authoritative music provider, wrapped in the
//     fill-only MusicChainProvider only when an artwork-only music source is active
//     AND Music is enabled — so an enriched artist also gets a poster (the
//     preferred, MBID-keyed source) and a real bio (the name-capable one). With no
//     such source (or Music off) it stays the plain lead, making zero calls to
//     either host (ADR-0001 explicit opt-in).
//
// It names no provider. Which source leads a kind is the config's pointer or the
// catalog's registration order; which sources fill behind it is what the catalog
// says they ARE (a Full video source, an artwork-only music source) and whether the
// operator switched them on.
func (cat Catalog) BuildProvider(cfg ProviderConfig) (MetadataProvider, Enablement) {
	// Music leads with the Library's Authoritative music provider — the kind's
	// default unless an Enrichment policy repointed it (ADR-0027) — built from its
	// registration exactly as the video lead is. The operator's pacing policy and
	// the lead's second host travel in that Plugin's Settings.
	musicSlug := cfg.musicAuthoritativeSlug()
	music := cat.newProvider(cfg, musicSlug, KindMusic)
	if music == nil {
		// A pointer at a slug no music Plugin claims can't lead; fall back to the
		// kind's default lead so the composite is always well-formed (the resolver
		// never sets such a pointer, but BuildProvider stays total). On a Catalog
		// holding no music Plugin at all this stays nil, and a nil sub-provider is the
		// all-off posture CompositeProvider already documents (ADR-0001) — the same
		// answer the video half gives for the same reason.
		musicSlug = cat.DefaultAuthoritativeForKind(KindMusic)
		music = cat.newProvider(cfg, musicSlug, KindMusic)
	}
	if music != nil && cfg.musicEnabled() {
		// At least one artist source active: wrap the lead in the fill-only chain.
		// They are artwork-only, so neither can BE the lead; excluding the lead when
		// they are gathered is what keeps a source from supplementing itself. With
		// none active this stays the plain lead, making zero calls to either host
		// (ADR-0001 explicit opt-in).
		if supplements := cat.musicImageSupplements(cfg, musicSlug); len(supplements) > 0 {
			var imageBio MetadataProvider
			if len(supplements) > 1 {
				imageBio = supplements[1]
			}
			music = NewMusicChainProvider(music, supplements[0], imageBio)
		}
	}

	// Video composes the Library's Authoritative provider (the kind's default lead,
	// or a repointed Full provider — ADR-0027) as the lead, wrapping it in the
	// fill-only chain when at least one other video source is active. The lead is
	// always built (an unconfigured lead simply makes no calls when video is off);
	// the chain wrap is added only when video is on AND a supplement is active, so an
	// all-supplements-off Library is plain lead with zero calls to the others.
	authSlug := cfg.videoAuthoritativeSlug()
	video := cat.newProvider(cfg, authSlug, KindVideo)
	if video == nil {
		// A pointer at a slug no video Plugin claims can't lead the video chain; fall
		// back to the kind's default lead so the composite is always well-formed.
		authSlug = cat.DefaultAuthoritativeForKind(KindVideo)
		video = cat.newProvider(cfg, authSlug, KindVideo)
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
