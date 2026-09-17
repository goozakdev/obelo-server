package v1

import "context"

// The Metadata provider Extension point (ADR-0027 behind ADR-0057): resolve a
// locally-parsed identity to a descriptive record, offer candidates for an Admin
// correcting one, and offer images for an Artwork role. A Metadata provider
// decorates a Title the Scanner already filed; it NEVER supplies identity
// (ADR-0002), which is why nothing here can name a file or change what one is.
//
// Three mandatory calls — Lookup, Search, ArtworkCandidates — plus the optional
// EpisodeLister. Search and ArtworkCandidates are mandatory on the Go interface
// but OPTIONAL in behavior: a Plugin that does not declare CapabilitySearch or
// CapabilityArtworkCandidates is never asked, and the host answers
// OutcomeUnavailable for it without a call.

// MediaRef is the locally-parsed identity a Metadata provider is asked about: the
// kind, the parsed title/year, whatever external ids the host already holds, and
// the kind-specific coordinates (season/episode for TV, artist/album/track for
// music). It is the wire shape of what the Scanner filed plus what a prior
// Enrichment or an Admin's correction pinned.
//
// When it carries an id the Plugin recognizes, the Plugin RESOLVES BY ID and does
// not search — an id is the identification (ADR-0049). When it does not, a source
// that has to search returns its best candidate with MetadataRecord.FromSearch set
// and lets the host judge it (ADR-0057 decision 3).
type MediaRef struct {
	// Kind is the fine entity kind: "movie" | "show" | "season" | "episode" |
	// "artist" | "album" | "track". A Plugin that does not serve a kind answers
	// OutcomeNoMatch for it rather than guessing.
	Kind string `json:"kind"`
	// Title and Year are the parsed identity, always present.
	Title string `json:"title,omitempty"`
	Year  int    `json:"year,omitempty"`

	// The external ids the host already holds for this entity, each empty unless a
	// curated token, a prior enrichment or an Admin's correction supplied it. A
	// Plugin uses the one it owns and ignores the rest.
	TMDBID        string `json:"tmdbId,omitempty"`
	IMDBID        string `json:"imdbId,omitempty"`
	MusicbrainzID string `json:"musicbrainzId,omitempty"`
	TheTVDBID     string `json:"thetvdbId,omitempty"`
	AniDBID       string `json:"anidbId,omitempty"`

	// TV coordinates. EpisodeLabel is the raw on-disk label for a file whose
	// numbering the parser could not resolve to a season/episode pair.
	SeasonNumber  int    `json:"seasonNumber,omitempty"`
	EpisodeNumber int    `json:"episodeNumber,omitempty"`
	EpisodeLabel  string `json:"episodeLabel,omitempty"`

	// Music coordinates: the artist, album and track names the files assert.
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	Track  string `json:"track,omitempty"`
	// ReleaseMBID pins one EDITION for an album lookup; the Plugin resolves it to
	// the album it belongs to (a release-group is what an album IS, ADR-0038).
	ReleaseMBID string `json:"releaseMbid,omitempty"`
	// AlbumHints are a few of the albums this library files under an ARTIST, offered
	// as evidence so a source can identify the artist through its DISCOGRAPHY rather
	// than through its name (ADR-0053). They are evidence, not a pin: a Plugin is
	// free to use none of them.
	AlbumHints []AlbumHint `json:"albumHints,omitempty"`
}

// AlbumHint is one local album offered as corroboration for its artist: the title
// the library holds, plus the release-group id the FILES assert when they do.
type AlbumHint struct {
	Title            string `json:"title,omitempty"`
	ReleaseGroupMBID string `json:"releaseGroupMbid,omitempty"`
}

// ArtworkRef points at one remote image a record carries for an Artwork role
// ("poster" | "background" | "logo" | "cover"). The Plugin returns a URL, never
// bytes: the host downloads through its own guarded fetcher into the identity-keyed
// artwork cache (ADR-0007), so a Plugin can never hand the cache something the host
// did not fetch itself.
type ArtworkRef struct {
	Role string `json:"role"`
	URL  string `json:"url"`
}

// Credit is one cast or crew member of a record, normalized. PersonRef is the
// source-namespaced stable person id (e.g. "tmdb:12345") that keys the person's
// headshot in the host's cache, so one actor's photo is stored once across every
// Title they appear in; ImageURL is that headshot, which the host downloads exactly
// as it downloads an ArtworkRef.
type Credit struct {
	Person    string `json:"person"`
	Role      string `json:"role,omitempty"`
	Character string `json:"character,omitempty"`
	Kind      string `json:"kind,omitempty"`
	PersonRef string `json:"personRef,omitempty"`
	ImageURL  string `json:"imageUrl,omitempty"`
}

// MetadataRecord is one source's descriptive answer about a MediaRef: the fields a
// Title is decorated with, plus artwork references and the id the source resolved.
// It carries no identity — the host applies Name as a display-only override where
// its own rules allow and never as the file's identity (ADR-0002).
type MetadataRecord struct {
	// Matched is false for "this source has no record for that ref", which is the
	// same thing OutcomeNoMatch says and is not a failure.
	Matched bool `json:"matched"`
	// Name is the source's canonical title for the entity. It MUST be the
	// candidate's own title whenever FromSearch is set, because that is the string
	// the host judges the candidate by.
	Name           string   `json:"name,omitempty"`
	Year           int      `json:"year,omitempty"`
	Overview       string   `json:"overview,omitempty"`
	Tagline        string   `json:"tagline,omitempty"`
	ContentRating  string   `json:"contentRating,omitempty"`
	ReleaseDate    string   `json:"releaseDate,omitempty"`
	RuntimeMinutes int      `json:"runtimeMinutes,omitempty"`
	Studio         string   `json:"studio,omitempty"`
	Genres         []string `json:"genres,omitempty"`

	Cast    []Credit     `json:"cast,omitempty"`
	Artwork []ArtworkRef `json:"artwork,omitempty"`

	// ExternalID is the id this source resolved for the entity, and Source its
	// slug. Together they are what a later pass re-resolves by, and what an Admin's
	// Enrichment override pins.
	ExternalID string `json:"externalId,omitempty"`
	Source     string `json:"source,omitempty"`

	// FromSearch says the source found this record by RELEVANCE-RANKED SEARCH
	// rather than by resolving an id. It is a fact about HOW the answer was found,
	// never a judgement about whether it is right: a relevance query essentially
	// always returns something, so its top hit is a CANDIDATE, and only the HOST
	// turns a candidate into a record (ADR-0057 decision 3, ADR-0050).
	//
	// This is the one field a Plugin author must get right for the host's acceptance
	// test to work, which is why it states a fact the author cannot get wrong by
	// disagreeing with the host's policy: "I searched for this", not "I believe it".
	// A record resolved BY ID is never marked — an id IS the identification
	// (ADR-0049), so a canonical title that disagrees with the local one is a
	// spelling, not a wrong record.
	FromSearch bool `json:"fromSearch,omitempty"`
}

// LookupRequest asks one source to resolve one ref to one record.
type LookupRequest struct {
	Ref MediaRef `json:"ref"`
}

// LookupResponse is what a lookup answers. OutcomeMatched carries the Record;
// OutcomeNoMatch is the normal "this source has nothing for that"; a Go error
// alongside means the transport failed, which the host retries rather than
// settling the item (ADR-0048).
//
// A Plugin does NOT answer OutcomeRejected for its own top hit: acceptance is the
// host's judgment, and the way to hand a search hit over is a Record with
// FromSearch set.
type LookupResponse struct {
	Outcome Outcome        `json:"outcome"`
	Record  MetadataRecord `json:"record"`
	// Detail is optional human copy for a log line or a settings probe. It never
	// carries an error identity — Outcome does that.
	Detail string `json:"detail,omitempty"`
}

// SearchRequest is the Edit-item picker's free-text query, scoped to one fine
// entity kind. Artist and Release are optional narrowing axes for a music search
// pre-filled from the item's own parsed fields; a kind with no such axis ignores
// them. Page is embedded, so limit and offset are flat fields on the wire, and
// "show more" works against any source without a cursor protocol.
type SearchRequest struct {
	Kind    string `json:"kind"`
	Query   string `json:"query"`
	Artist  string `json:"artist,omitempty"`
	Release string `json:"release,omitempty"`
	Page
}

// SearchCandidate is one result an Admin may pick to correct an item's record. It
// carries just enough to tell two same-named works apart: the id to pin, the
// source's own title and year, a thumbnail, a disambiguation line and a short
// record-type badge.
type SearchCandidate struct {
	ExternalID     string `json:"externalId"`
	Title          string `json:"title,omitempty"`
	Year           int    `json:"year,omitempty"`
	ThumbnailURL   string `json:"thumbnailUrl,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
	// Kind is the fine entity kind the candidate is, which the host checks against
	// the item being corrected.
	Kind string `json:"kind,omitempty"`
	// TypeLabel is a short record-type hint ("Album · Soundtrack", "Group") shown as
	// a badge beside the free-text Disambiguation line.
	TypeLabel string `json:"typeLabel,omitempty"`
	// Tracklist is the ordered track preview an ALBUM candidate carries, so an
	// Admin can confirm the positional map before applying the pin. Nil for every
	// other kind. It is a PREVIEW and not the AlbumTracklister's answer: roughly
	// right for a page of candidates is the whole requirement here, where one
	// album resolving its own tracks needs the exact edition (ADR-0050).
	Tracklist []TrackCandidate `json:"tracklist,omitempty"`
	// ReleaseID is the exact EDITION this candidate came from, when the source was
	// given one: a pasted /release/ URL resolves to its parent release-group (which
	// is what an album IS, ADR-0038) and carries the release here rather than
	// dropping it (ADR-0052). Empty for an ordinary search hit and for every
	// non-album kind — and an empty value applied to an album CLEARS whatever
	// edition it had, because the Admin just named a less specific thing.
	ReleaseID string `json:"releaseId,omitempty"`
}

// SearchResponse is what a search answers. OutcomeMatched with an empty list is a
// query that simply found nothing — which is NOT the same as OutcomeUnavailable,
// the "this kind cannot be searched right now" the Edit-item box reports to the
// Admin instead of an empty result set. A Plugin that did not declare
// CapabilitySearch is never called and the host answers OutcomeUnavailable for it.
type SearchResponse struct {
	Outcome    Outcome           `json:"outcome"`
	Candidates []SearchCandidate `json:"candidates,omitempty"`
	Detail     string            `json:"detail,omitempty"`
}

// ArtworkCandidatesRequest asks for the images a source offers for one Artwork
// role on the record Ref points at. Ref must carry the resolved external id — a
// role has no candidates without a record.
type ArtworkCandidatesRequest struct {
	Ref MediaRef `json:"ref"`
	// Role is the Artwork role being filled: "poster" | "background" | "logo" |
	// "cover".
	Role string `json:"role"`
	Page
}

// ArtworkCandidate is one selectable image for a role. Width and Height are the
// source's dimensions (0 when it reports none) so the picker can hint resolution;
// Source is the slug of the Plugin that offered it. As everywhere else in this
// contract, an image is a URL the host fetches, never bytes a Plugin supplies.
type ArtworkCandidate struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	Source string `json:"source,omitempty"`
}

// ArtworkCandidatesResponse is what the picker's query answers. As with search,
// OutcomeMatched with an empty list is "this record has no image for that role",
// while OutcomeUnavailable is "this source owns no listable set for this kind" —
// the picker's "not now", which degrades to the upload path rather than an error.
type ArtworkCandidatesResponse struct {
	Outcome    Outcome            `json:"outcome"`
	Candidates []ArtworkCandidate `json:"candidates,omitempty"`
	Detail     string             `json:"detail,omitempty"`
}

// SeriesSeasonsRequest asks which seasons a series has, by the source's own id.
type SeriesSeasonsRequest struct {
	SeriesID string `json:"seriesId"`
	Page
}

// SeasonSummary is one season of a series, for the season chooser.
type SeasonSummary struct {
	Season       int `json:"season"`
	EpisodeCount int `json:"episodeCount,omitempty"`
}

// SeriesSeasonsResponse lists a series' seasons in season order.
type SeriesSeasonsResponse struct {
	Outcome Outcome         `json:"outcome"`
	Seasons []SeasonSummary `json:"seasons,omitempty"`
	Detail  string          `json:"detail,omitempty"`
}

// SeasonEpisodesRequest asks for one season's episodes, by the source's series id.
type SeasonEpisodesRequest struct {
	SeriesID string `json:"seriesId"`
	Season   int    `json:"season"`
	Page
}

// EpisodeCandidate is one episode an Admin may point a file at when the on-disk
// numbering does not line up with the source's. StillURL is the source's own
// episode still, which the host rewrites to its same-origin image proxy before it
// reaches a browser.
type EpisodeCandidate struct {
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Name     string `json:"name,omitempty"`
	Overview string `json:"overview,omitempty"`
	AirDate  string `json:"airDate,omitempty"`
	StillURL string `json:"stillUrl,omitempty"`
}

// SeasonEpisodesResponse lists one season's episodes in episode order.
type SeasonEpisodesResponse struct {
	Outcome  Outcome            `json:"outcome"`
	Episodes []EpisodeCandidate `json:"episodes,omitempty"`
	Detail   string             `json:"detail,omitempty"`
}

// TrackCandidate is one entry of an album's tracklist: where it sits, what it is
// called, and the id of the recording behind it. It is display and positional-map
// data only, never identity — embedded tags stay the Music identity authority
// (ADR-0002). ExternalID may be empty when the source named no recording; the
// entry still CLAIMS its position, which the host's match rule needs (ADR-0050
// rule 3 fires only when exactly one local track and exactly one position are
// unclaimed, so a position that exists must be visible even when it is anonymous).
type TrackCandidate struct {
	Disc       int    `json:"disc,omitempty"`
	Position   int    `json:"position,omitempty"`
	Title      string `json:"title,omitempty"`
	ExternalID string `json:"externalId,omitempty"`
}

// TracklistRequest names the album whose tracklist is wanted. Two ids, a flag and
// a count, because that is exactly what choosing the right RELEASE takes
// (ADR-0050):
//
//   - ReleaseGroupID is the album itself — required, and the authority the named
//     release is checked against. Empty means the album is unresolved, which is
//     "no tracklist" without a single call.
//   - ReleaseID is the exact edition to read: the one an ADMIN chose (ADR-0052)
//     or, failing that, the one the FILES name. It is used ONLY when its parent
//     release-group is ReleaseGroupID — neither a mis-tagged file nor a stale pin
//     naming a stranger's release may renumber the album.
//   - ReleaseIDChosen says WHOSE assertion ReleaseID is: a human's, or a file's.
//     It does not change how the release is fetched or checked; it changes what
//     happens when that release does not apply. A file's release falls through to
//     fit-selection silently, while a human's is reported as "no tracklist" so the
//     host can re-ask WITHOUT the pin and know that what it finally got is not the
//     edition the human asserted. That distinction is the whole licence ADR-0052
//     grants position-alone mapping, and it is NOT recoverable after the fact —
//     every release's tracklist looks the same — which is why it crosses the wire
//     rather than being inferred from the answer.
//   - LocalTrackCount is how many Tracks the LOCAL album holds, which is what
//     separates a 12-track standard edition from its 15-track deluxe. It is an
//     input because no source could derive it. Zero means "unknown".
type TracklistRequest struct {
	ReleaseGroupID  string `json:"releaseGroupId"`
	ReleaseID       string `json:"releaseId,omitempty"`
	ReleaseIDChosen bool   `json:"releaseIdChosen,omitempty"`
	LocalTrackCount int    `json:"localTrackCount,omitempty"`
}

// TracklistResponse is what a tracklist answers. OutcomeMatched ALWAYS carries at
// least one track, and OutcomeNoMatch is "this album has no tracklist" — the
// album named no release-group, the release-group holds no releases, the release
// holds no tracks, or a human's chosen edition did not apply.
//
// Those two are deliberately not collapsible: "this album has no tracklist" sends
// an Admin to the Album (or its release) while "this tracklist has no room for
// this track" sends them to the one file, and a source answering with an empty
// list and no outcome would render them as the same shrug (ADR-0050). That
// invariant is what lets this call reuse OutcomeNoMatch instead of the contract
// growing a value only one Extension point could ever mean anything by.
type TracklistResponse struct {
	Outcome Outcome          `json:"outcome"`
	Tracks  []TrackCandidate `json:"tracks,omitempty"`
	Detail  string           `json:"detail,omitempty"`
}

// ReleaseEditionsRequest asks which editions an album has, by the album's own id.
type ReleaseEditionsRequest struct {
	ReleaseGroupID string `json:"releaseGroupId"`
	Page
}

// ReleaseEdition is ONE edition of an album — a release under the album's
// release-group — described with the five facts an Admin needs to tell two
// editions apart at a glance (ADR-0052): when it came out, where, on what medium,
// how many tracks it holds, and whatever the source says to disambiguate it.
//
// TrackCount is the one that does the work: an operator is looking at this list
// precisely because their album's tracks did not line up, and the edition whose
// count equals the local album's is the one that will.
//
// An edition is NOT a SearchCandidate. A candidate is a record to pin as the
// album's identity, and an edition is never that — album identity stays the
// release-group (ADR-0038). It is a DECORATION refinement.
type ReleaseEdition struct {
	ReleaseID      string `json:"releaseId"`
	Date           string `json:"date,omitempty"`
	Country        string `json:"country,omitempty"`
	Format         string `json:"format,omitempty"`
	TrackCount     int    `json:"trackCount,omitempty"`
	Disambiguation string `json:"disambiguation,omitempty"`
}

// ReleaseEditionsResponse lists an album's editions. An empty list with
// OutcomeMatched is "this album has exactly no editions to choose from", which the
// picker renders as an answer; OutcomeUnavailable is "no listable edition set
// here", the picker's "not now", which degrades to the pasted-URL escape hatch
// rather than an error page.
//
// The pair differs from TracklistResponse's on purpose, and the difference is the
// host's, not the Plugin's: a missing tracklist is a fact about the ALBUM that the
// enrichment pass must record as a settled reason, while a missing edition list is
// a fact about the PICKER that only hides a control.
type ReleaseEditionsResponse struct {
	Outcome  Outcome          `json:"outcome"`
	Editions []ReleaseEdition `json:"editions,omitempty"`
	Detail   string           `json:"detail,omitempty"`
}

// ExternalRefRequest is a string an Admin pasted into the "paste an id when search
// isn't enough" box, plus the fine kind of the item they pasted it on. The Plugin
// reads it; the host then looks the answer up by id and shows it before anything
// is pinned.
type ExternalRefRequest struct {
	// Kind is the item being corrected: "movie" | "show" | "season" | "episode" |
	// "artist" | "album" | "track". It is what makes a wrong-kind paste detectable.
	Kind string `json:"kind"`
	// Pasted is the raw string, untrimmed and unvalidated — a bare id, a full URL
	// with any scheme, subdomain, slug, query or fragment, or nonsense.
	Pasted string `json:"pasted"`
}

// ExternalRefResponse is what parsing a pasted reference answers. The three
// failure outcomes are distinct because the host renders three different sentences
// and each names a different fix:
//
//   - OutcomeRefInvalid — "that doesn't look like an id or URL I know."
//   - OutcomeRefKindMismatch — "that names a real entity of the WRONG kind for
//     this item." GotKind and WantKind travel with it, because the useful message
//     names both ("that looks like an artist link, but this item is a track");
//     without them the host can only say "wrong kind", which is what the Admin
//     already knew.
//   - OutcomeRefUnsupportedKind — "that IS one of my URLs, for an entity kind this
//     server does not pin at all" (a work, a label). Distinguished from invalid so
//     the host can say which kind of link to paste instead, rather than "that's
//     not a URL".
//
// OutcomeMatched carries the id to resolve. ReleaseID carries the EDITION when the
// paste named one — the mapping that makes this a call rather than a pattern list:
// a /release/ URL is not itself an album pin, so the Plugin answers with the
// release-group in ExternalID and the release here (ADR-0052), and a bare
// release-group URL leaves it empty, which is what CLEARS a chosen edition.
type ExternalRefResponse struct {
	Outcome    Outcome `json:"outcome"`
	ExternalID string  `json:"externalId,omitempty"`
	ReleaseID  string  `json:"releaseId,omitempty"`
	// GotKind and WantKind are the two fine kinds of a kind mismatch: what the
	// paste names, and what the item needs. Both empty for every other outcome.
	GotKind  string `json:"gotKind,omitempty"`
	WantKind string `json:"wantKind,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// MetadataProvider is the Go call surface of the Metadata provider Extension
// point. It is a Go interface only so a Built-in can be called in-process: every
// parameter and result is a wire type, so the same three calls survive being moved
// across a sandbox boundary in Phase 2 without the contract being redesigned.
//
// The context always carries the host's deadline. An error means the transport or
// the Plugin failed; everything else is in the response's Outcome.
type MetadataProvider interface {
	// Lookup resolves one ref to one record.
	Lookup(ctx context.Context, req LookupRequest) (LookupResponse, error)
	// Search returns the candidates for a free-text query, best-first by the
	// source's own relevance. The host caps and judges; the Plugin only offers.
	Search(ctx context.Context, req SearchRequest) (SearchResponse, error)
	// ArtworkCandidates lists the images this source offers for one role.
	ArtworkCandidates(ctx context.Context, req ArtworkCandidatesRequest) (ArtworkCandidatesResponse, error)
}

// EpisodeLister is the CapabilityEpisodeList half of the Metadata provider
// Extension point: listing a series' seasons and one season's episodes, so an
// Admin can point a file at the exact provider episode it should be decorated
// from.
//
// It is a separate interface rather than two more methods on MetadataProvider for
// the same reason the enrichment domain kept it off its own seam: only an
// authoritative video source can answer it, and folding it in would force every
// music source and every artwork-only supplement to carry a stub. The host asks
// the Descriptor before it asks the Plugin, so a Plugin that does not declare
// CapabilityEpisodeList is never called.
type EpisodeLister interface {
	SeriesSeasons(ctx context.Context, req SeriesSeasonsRequest) (SeriesSeasonsResponse, error)
	SeasonEpisodes(ctx context.Context, req SeasonEpisodesRequest) (SeasonEpisodesResponse, error)
}

// AlbumTracklister is the CapabilityAlbumTracklist half of the Metadata provider
// Extension point: what an Album holds. One interface, two calls, because they are
// the automatic and the manual half of one question (ADR-0050, ADR-0052) — pick
// the release that fits, or show a human the releases to pick from — and a source
// that can do the first can do the second out of the same browse. Asking that
// question in two spellings is how the two halves would come to disagree about
// which editions there are.
//
// Only the authoritative MUSIC source can answer it, so like EpisodeLister it is a
// separate interface rather than more methods on MetadataProvider: folding it in
// would force every video source and every artwork-only supplement to carry a
// stub. The host asks the Descriptor before it asks the Plugin.
type AlbumTracklister interface {
	// AlbumTracklist returns the album's ordered tracks. OutcomeMatched is never an
	// empty list; OutcomeNoMatch is "this album has no tracklist" (see
	// TracklistResponse). A transport failure is a Go error, so the host can still
	// tell a load shed apart from a settled nothing (ADR-0049).
	AlbumTracklist(ctx context.Context, req TracklistRequest) (TracklistResponse, error)
	// ReleaseGroupEditions lists the album's editions, best-effort in the source's
	// own order. An album with no editions is OutcomeMatched and an empty list.
	ReleaseGroupEditions(ctx context.Context, req ReleaseEditionsRequest) (ReleaseEditionsResponse, error)
}

// ExternalRefParser is the CapabilityExternalRef half of the Metadata provider
// Extension point: reading a string an Admin pasted into the "paste an id when
// search isn't enough" box.
//
// It is a CALL and not a manifest list of URL patterns because the mapping is
// logic, not a regex: one of MusicBrainz's URL kinds (a /release/) has to be
// resolved to a DIFFERENT entity than the one it names before it can be pinned,
// and several others are real URLs for entities no server pins at all. A pattern
// list could not express either, and a Plugin whose source has its own id shapes
// is exactly the case this exists for.
//
// A host that asks and gets OutcomeUnavailable — an undeclared capability, or a
// Plugin that does not implement this — is free to read the paste itself for the
// id namespaces it already understands, because the columns those ids are stored
// in are the host's own (ADR-0045/0049). A Plugin that DECLARES the capability
// answers instead, and its answer stands.
type ExternalRefParser interface {
	ParseExternalRef(ctx context.Context, req ExternalRefRequest) (ExternalRefResponse, error)
}

// MetadataProviderFactory builds a Metadata provider Plugin from the Settings an
// Admin saved. Like its Subtitle provider sibling it is the one thing here that is
// not wire-shaped, which is why it lives in the registration beside the Descriptor
// rather than in it. An error means the Plugin cannot be built from these
// settings, and the host composes a chain without it rather than half-working
// (ADR-0001).
type MetadataProviderFactory func(Settings) (MetadataProvider, error)

// MetadataProviderRegistration is what a Metadata provider Plugin hands the host:
// what it is, and how to build it.
//
// New may be nil, and that means exactly one thing: this Plugin's static facts are
// registered — so the settings screen renders it, the Authoritative-provider
// pointer can be validated against it, and an operator can override its URL — while
// nothing is ever built from it. Cover Art Archive is the case, and after the music
// chain crossed this contract it is the only one: it has no client of its own,
// because it is the artwork HOST the MusicBrainz Plugin's cover URLs point at, and
// the host resolves its row into that Plugin's URL2. The builder skips a nil factory
// rather than treating it as an error.
type MetadataProviderRegistration struct {
	Descriptor Descriptor
	New        MetadataProviderFactory
}
