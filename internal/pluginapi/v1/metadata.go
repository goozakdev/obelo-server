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
// registered — so the settings screen renders it and the Authoritative-provider
// pointer can be validated against it — while the host still constructs the source
// itself. Cover Art Archive is the permanent case (it has no client of its own; it
// is reached through the MusicBrainz Plugin), and the music sources are the
// temporary one until the music chain moves behind the contract. The builder skips
// a nil factory rather than treating it as an error.
type MetadataProviderRegistration struct {
	Descriptor Descriptor
	New        MetadataProviderFactory
}
