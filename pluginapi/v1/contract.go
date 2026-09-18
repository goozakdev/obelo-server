package v1

// The contract's shared vocabulary: what a Plugin may be asked to do (Extension
// point), what it optionally implements (Capability), how it reports what happened
// (Outcome), what an Admin configures (Settings), and the static facts it declares
// about itself (Descriptor). Every type here is plain data with JSON tags.

// ExtensionPoint names one of the closed set of seams a Plugin may implement
// (ADR-0057 decision 1). The set grows by decision, not by a Plugin asking:
// identity is the naming convention's (ADR-0002), transcoding and probing stay in
// core, and authentication is a trust-model question for its own ADR.
type ExtensionPoint string

const (
	// ExtensionMetadataProvider supplies descriptive records and artwork candidates
	// to Enrichment — never identity. Its wire types arrive with the metadata chain.
	ExtensionMetadataProvider ExtensionPoint = "metadata-provider"
	// ExtensionSubtitleProvider searches an external source for a subtitle matched
	// to the exact release and downloads one candidate's bytes (ADR-0021).
	ExtensionSubtitleProvider ExtensionPoint = "subtitle-provider"
	// ExtensionEventSink consumes the curated terminal server events and may only
	// emit outbound HTTP in response (ADR-0057 decision 6).
	ExtensionEventSink ExtensionPoint = "event-sink"
)

// Capability names an OPTIONAL operation a Plugin declares it implements, so the
// host never calls what was not built (ADR-0057 decision 3). An operation that is
// the whole point of an Extension point — a Subtitle provider searching and
// downloading — is not a capability: it is mandatory, and a Plugin that cannot do
// it has no business registering. An undeclared capability is a KNOWN state, not
// an error: the host renders it exactly as it renders "search unavailable" today.
type Capability string

const (
	// CapabilitySearch is free-text search for an item by title (and year).
	CapabilitySearch Capability = "search"
	// CapabilityArtworkCandidates is offering images for an Artwork role.
	CapabilityArtworkCandidates Capability = "artwork-candidates"
	// CapabilityAlbumTracklist is answering what an Album holds: the ordered tracks
	// of the release the album actually IS (ADR-0050) and the EDITIONS that album
	// has to choose from (ADR-0052). One capability covers both because they are
	// the automatic and the manual half of a single question — the source that can
	// pick a release by track-count fit is the source that can list the releases to
	// pick from, and both come out of the same browse — so a Plugin that can answer
	// one and not the other does not exist. The two calls keep their own
	// absent-answers (see AlbumTracklister), which is a per-call fact, not a second
	// declaration.
	CapabilityAlbumTracklist Capability = "album-tracklist"
	// CapabilityExternalRef is parsing a pasted external id or URL into a reference
	// the Plugin can then look up — a CALL and not a manifest pattern list, because
	// the mapping is logic: MusicBrainz's /release/ URL names an edition and has to
	// be resolved to the release-group an album IS (ADR-0038), and a typed URL of
	// the wrong entity kind has to be told apart from an unreadable paste.
	CapabilityExternalRef Capability = "external-ref"
	// CapabilityEpisodeList is listing a series' seasons and one season's episodes,
	// so an Admin can pick the exact provider episode a file is decorated from. Only
	// an authoritative VIDEO source can answer it; it is a capability rather than a
	// mandatory call for the same reason the enrichment domain kept it off its own
	// provider interface — folding it in would force every music source and every
	// artwork-only supplement to carry a stub.
	CapabilityEpisodeList Capability = "episode-list"
)

// Outcome is what happened, as a VALUE at the contract edge (ADR-0057 decision 2).
// Today's wrapped error sentinels — enrich.ErrNoMatch, enrich.ErrMatchRejected,
// enrich.ErrSearchUnavailable, the external-ref errors, subfetch.ErrNoMatch — are
// this enum on the wire, mapped back to those sentinels by an adapter beside the
// service that consumes them, so no `errors.Is` caller inside the server changes.
//
// A Plugin author never has to reproduce the server's error identity to be
// understood, and a Go error returned ALONGSIDE an outcome means something else
// entirely: the transport or the Plugin failed, which the host retries rather than
// parking the item (ADR-0048).
type Outcome string

const (
	// OutcomeMatched is the success case: the response's payload is present.
	OutcomeMatched Outcome = "matched"
	// OutcomeNoMatch is the normal "the source has nothing for this" answer. It is
	// not a failure — an empty candidate list means the same thing.
	OutcomeNoMatch Outcome = "no-match"
	// OutcomeRejected is "the source answered, and the answer was not good enough".
	// The HOST owns this judgment (ADR-0057 decision 3); a Plugin returns it only
	// when the source itself declined, never as its own acceptance test.
	OutcomeRejected Outcome = "rejected"
	// OutcomeUnavailable is "this Plugin cannot serve this call at all" — an
	// undeclared capability, or a provider that is configured off. It is a known
	// state the host renders, not an error.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomeRefInvalid is "that pasted id or URL is not one I recognize".
	OutcomeRefInvalid Outcome = "ref-invalid"
	// OutcomeRefKindMismatch is "that id names a real entity, but of the wrong kind
	// for the item being edited" — the response carries the got and want kinds so
	// the host can keep rendering the same specific message it does today.
	OutcomeRefKindMismatch Outcome = "ref-kind-mismatch"
	// OutcomeRefUnsupportedKind is "that id names an entity kind this server does
	// not enrich".
	OutcomeRefUnsupportedKind Outcome = "ref-unsupported-kind"
)

// AllOutcomes is every Outcome the contract defines, in declaration order. It
// exists so an adapter's mapping test can be exhaustive BY CONSTRUCTION: adding an
// outcome breaks every table-driven mapping test until each service decides what
// its sentinel for the new value is, which is the point of an enum at the edge.
func AllOutcomes() []Outcome {
	return []Outcome{
		OutcomeMatched,
		OutcomeNoMatch,
		OutcomeRejected,
		OutcomeUnavailable,
		OutcomeRefInvalid,
		OutcomeRefKindMismatch,
		OutcomeRefUnsupportedKind,
	}
}

// Media-kind groups a Plugin declares it serves. These are the coarse Enrichment
// kinds the settings screen groups by, not the finer CONTEXT.md kinds
// (movie/show/artist/album/track).
const (
	KindVideo = "video"
	KindMusic = "music"
)

// Role is a Plugin's DEFAULT position in the provider chain for the kinds it
// serves: the authoritative source that leads, or the fill-only supplement that
// only adds what the leader left empty.
type Role string

const (
	RoleAuthoritative Role = "authoritative"
	RoleSupplement    Role = "supplement"
)

// Class is the capability distinction a Library's Authoritative provider pointer
// is constrained against (ADR-0027): only a Full provider may LEAD a Library's
// Enrichment. It is carried here exactly as the registry carries it today, which
// is what makes ADR-0057 decision 4 — an Installed plugin may be a Library's
// Authoritative provider — a registration fact rather than a special case.
type Class string

const (
	ClassFull        Class = "full"
	ClassArtworkOnly Class = "artwork"
)

// Settings is the FIXED shape an Admin configures for every Plugin, Built-in or
// Installed, at every Extension point (ADR-0057 consequences): an enabled toggle,
// one secret, one URL, an optional second URL, and — for an Event sink — the list
// of event types it subscribes to. One shape to learn, one dialog to render, and
// one place a manifest-declared schema would have to replace in Phase 2.
//
// The host resolves each field before calling the factory: Secret is the decrypted
// key on file, URL is the operator's override or the Descriptor's default, and
// Enabled is true for any Plugin the host actually builds.
type Settings struct {
	// Enabled is whether the Admin turned this Plugin on.
	Enabled bool `json:"enabled"`
	// Secret is the one credential — an API key, or an Event sink's signing key.
	// Stored encrypted at rest and NEVER returned by the settings API, which
	// reports only whether one is on file.
	Secret string `json:"secret,omitempty"`
	// URL is the effective endpoint: the operator's base-URL override when set,
	// otherwise Descriptor.DefaultURL. For an Event sink it is the target to post to.
	URL string `json:"url,omitempty"`
	// URL2 is the optional second endpoint for a source whose images come from a
	// host distinct from its API (today only TMDB). Empty for everything else.
	URL2 string `json:"url2,omitempty"`
	// Events is an Event sink's subscribed event types. Defined here so the shape
	// is fixed from the first slice; empty for every provider, and for a sink until
	// the sink translator lands.
	Events []string `json:"events,omitempty"`
	// Language is the server-wide preferred metadata language/region the host
	// resolves for a Metadata provider (e.g. "en-US"). It is a HOST-resolved setting
	// like Secret and URL rather than something an Admin types per Plugin: it is one
	// field on the enrichment settings screen and it reaches every source at once.
	//
	// It is here, and not on each call, because that is where the Built-ins put it:
	// a source caches responses under a language-dependent key, so the language is
	// part of what a Plugin IS, and a language change is a settings save the host
	// answers by rebuilding every Plugin — exactly what it already does for a key.
	// Empty for a Subtitle provider (the wanted language is per search) and for an
	// Event sink.
	Language string `json:"language,omitempty"`
	// RateLimitMillis is the operator's minimum interval between two requests to
	// this source's HOST, in milliseconds. Like Secret, URL and Language it is
	// HOST-resolved — one number on the enrichment settings screen, persisted in
	// milliseconds — rather than something the Plugin decides for itself.
	//
	// It is a POINTER because the two things a host can mean are both meaningful
	// and neither is the other's zero: absent is "use your own default pacing",
	// which is what a Plugin the host has no policy for must get, while 0 is the
	// explicit "do not throttle at all" an operator sets for a self-hosted mirror
	// that has no rate policy (ADR-0049). An int with 0 meaning "off" would make a
	// caller that simply forgot the field hammer a public host.
	//
	// It is a construction input and not a per-call argument for Language's reason
	// and one more: the limiter is keyed by HOST, not held on the Plugin (ADR-0049
	// — the per-instance version meant a three-Library server sent 3 req/sec at one
	// host with each Library correctly believing it was well behaved), so the
	// interval has to be known when the Plugin is built, before any call.
	RateLimitMillis *int `json:"rateLimitMillis,omitempty"`
	// Values is the SECOND VARIANT of this field (.scratch/plugin-system issue 13):
	// the values of the settings an Installed plugin's manifest declared for
	// itself, keyed by the SettingsField.Key that declared them, in the JSON shape
	// that field named (see settings_schema.go for each type's encoding).
	//
	// It is beside the fixed fields above and never instead of them. A Built-in has
	// no manifest and therefore no declared fields, so this is empty for every one
	// of them and the eight dialogs are unchanged; an Installed plugin that
	// declares nothing is identical to one written before this field existed.
	//
	// A field the Admin never filled is ABSENT rather than present as a zero,
	// unless the manifest declared a default — the same absent-vs-zero distinction
	// RateLimitMillis makes, and for the same reason.
	//
	// SECRETS AT CALL TIME ONLY applies here exactly as it does to Secret: a
	// declared secret field's value is in this map while a call the host made is on
	// the stack, and the settings API never returns one.
	Values map[string]any `json:"values,omitempty"`
}

// Descriptor is the static self-description a Plugin registers with: the facts
// that live in code rather than the database, which the settings API renders and
// the builder composes from. It replaces the package-level registry entries the
// metadata and subtitle domains each kept their own copy of.
//
// It is pure data and carries NO factory — the function that builds a Plugin from
// its Settings sits beside the Descriptor in the per-Extension-point registration
// struct, so that everything in this contract that claims to be wire-shaped
// actually round-trips.
type Descriptor struct {
	// Slug is the stable key persisted in settings rows and used in the settings
	// API routes. It is the identity of the Plugin.
	Slug string `json:"slug"`
	// Name is the human name shown in the provider dialog.
	Name string `json:"name"`
	// ExtensionPoint is which seam this registration fills.
	ExtensionPoint ExtensionPoint `json:"extensionPoint"`
	// Kinds are the coarse media-kind groups the Plugin serves (KindVideo /
	// KindMusic). Empty for an Extension point that is not kind-scoped.
	Kinds []string `json:"kinds,omitempty"`
	// Role and Class are the chain position and the Authoritative eligibility
	// (ADR-0027). Empty for an Extension point that has no chain.
	Role  Role  `json:"role,omitempty"`
	Class Class `json:"class,omitempty"`
	// RequiresKey reports whether the source needs a secret to be enabled. The
	// settings API refuses to turn on a key-requiring Plugin with no key on file,
	// and its test-connection probe fails fast without making a call.
	RequiresKey bool `json:"requiresKey"`
	// Capabilities are the OPTIONAL operations this Plugin implements. The host
	// consults the declaration before calling; anything absent answers
	// OutcomeUnavailable without the Plugin being asked.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// DefaultURL is the public endpoint used when the operator sets no override,
	// and DefaultURL2 the default image host for the rare source that has one.
	DefaultURL  string `json:"defaultUrl,omitempty"`
	DefaultURL2 string `json:"defaultUrl2,omitempty"`
	// Description and DocsURL are the human-facing copy the settings screen shows.
	Description string `json:"description,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
}

// Serves reports whether the Plugin declared a coarse media kind (KindVideo /
// KindMusic). It is what the per-kind chain composition and the Authoritative-
// provider candidate list read, so "which kinds does this source serve" is a
// registration fact for a Built-in and an Installed plugin alike.
func (d Descriptor) Serves(kind string) bool {
	for _, k := range d.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// HasCapability reports whether the Plugin declared an optional operation. The
// host asks BEFORE calling, so an undeclared operation costs no call.
func (d Descriptor) HasCapability(c Capability) bool {
	for _, have := range d.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// Page is the contract's only paging shape (ADR-0057 decision 2): a limit and an
// offset, never a cursor, so "show more" in a picker works against any source
// without a stateful protocol. Zero means "the Plugin's own default".
type Page struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}
