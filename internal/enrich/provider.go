// Package enrich is the Enrichment domain (CONTEXT.md, ADR-0002): the separate,
// optional step that decorates a Title the scanner already filed with descriptive
// metadata (overview, cast, genres, content rating, …) and fetched artwork from
// an external public source, keyed off the locally-parsed identity. It NEVER
// affects identity and degrades gracefully offline (ADR-0001).
//
// The network is isolated behind two narrow seams — MetadataProvider (the
// lookup) and ArtworkFetcher (the image download) — mirroring how the scanner
// fakes the whole Prober rather than ffprobe's stdout. app.New wires the real
// TMDB provider + HTTP fetcher; tests inject fakes, so the black-box HTTP tests
// drive enrichment with zero network. The service depends only on these
// interfaces + a Store, never on net/http (ADR-0006 modular monolith).
package enrich

import (
	"context"
	"errors"
	"fmt"
)

// ErrNoMatch is the normal "the source has no record for this Title" outcome — a
// provider returns it (or a TitleMetadata with Matched=false) rather than a
// fatal error. The pass records the Title as 'unmatched' and moves on.
var ErrNoMatch = errors.New("enrich: no external match")

// ErrMatchRejected is the other half of "no match": the source ANSWERED, and its
// best answer is not this item. A relevance-ranked text search essentially always
// returns something, so its top hit only becomes a record when it passes an
// acceptance test — its normalized title must be the local one's (ADR-0050). A hit
// that fails is discarded rather than stored, because a confident wrong overview is
// worse than an empty one (ADR-0049).
//
// It WRAPS ErrNoMatch, so every errors.Is(err, ErrNoMatch) caller is unaffected —
// the pass still files the item 'unmatched' and moves on. The distinction exists so
// a settled failure can eventually say WHY: 'search-rejected' ("MusicBrainz has
// candidates, none of them is this song — pick one by hand") is a different next
// action from 'search-no-match' ("it found nothing at all"), and without the
// separate value both render as the same useless sentence.
var ErrMatchRejected = fmt.Errorf("%w: no candidate passed the title check", ErrNoMatch)

// ErrSearchUnavailable is the "this kind cannot be searched right now" outcome of
// Search — the authoritative provider for the kind is unconfigured (no key /
// disabled) or absent. It is distinct from a successful search with zero
// candidates (nil, nil): the Edit-item box reports the "unavailable" reason to
// the Admin instead of an empty result set (issue item-editing/01). A provider
// that can't serve a kind (a supplement, or a Composite sub that is nil) returns
// it; the service also short-circuits with it when the kind's enrichment is off.
var ErrSearchUnavailable = errors.New("enrich: provider search unavailable")

// ErrExternalRefInvalid is the paste-a-MusicBrainz-ID/URL escape hatch's "I can't
// read that" outcome (item-editing/search-improvements): the pasted string is
// neither a bare UUID/id nor a recognized provider entity URL, so no lookup can be
// keyed by it. The handler maps it to 400 so the Admin can correct the paste.
var ErrExternalRefInvalid = errors.New("enrich: unrecognized external id or url")

// ErrExternalRefKindMismatch is the paste escape hatch's "right shape, wrong kind"
// outcome: the pasted URL names a provider entity of a different kind than the item
// being corrected (e.g. an artist URL pasted on a Track), which must never be pinned
// — the handler maps it to 400 rather than silently pinning a nonsensical id. It is
// the sentinel an ExternalRefKindMismatchError matches via errors.Is.
var ErrExternalRefKindMismatch = errors.New("enrich: external id kind does not match item")

// ExternalRefKindMismatchError carries which entity kind was pasted (Got) versus the
// kind the item needs (Want), so the handler can tell the Admin exactly what to paste
// instead ("that's an album link, but this item is an artist") rather than a bare
// mismatch. Both are item kinds (movie/show/artist/album/track). errors.Is against
// ErrExternalRefKindMismatch still holds.
type ExternalRefKindMismatchError struct{ Got, Want string }

func (e *ExternalRefKindMismatchError) Error() string {
	return fmt.Sprintf("enrich: external id kind %q does not match item kind %q", e.Got, e.Want)
}

func (e *ExternalRefKindMismatchError) Is(target error) bool {
	return target == ErrExternalRefKindMismatch
}

// ErrExternalRefUnsupportedKind is the paste escape hatch's "recognized provider URL,
// but an entity type we can't pin" outcome: the pasted string IS a MusicBrainz URL,
// just for an entity we don't identify by (a work/release/label/…, not a release-group
// /artist/recording). Distinguished from ErrExternalRefInvalid so the handler can tell
// the Admin which kind of link to paste instead, rather than "that's not a URL".
var ErrExternalRefUnsupportedKind = errors.New("enrich: external ref names an unsupported entity kind")

// SearchOptions carries the optional narrowing + paging knobs a provider Search may
// honor beyond the free-text query (item-editing/search-improvements). Every zero
// value means "provider default": no artist narrowing, the source's own page size,
// and the first page. It is threaded from the Edit-item picker so a broad
// common-title music search stays usable.
type SearchOptions struct {
	// Artist optionally narrows an album/track music search to a specific artist,
	// AND-ed into the query as a field-scoped clause (the relevance-safe pattern —
	// `<terms> AND artist:"<artist>"`). It is pre-filled from the item's parsed
	// artist; blank means no narrowing. Kinds with no artist axis (video, the artist
	// search itself) ignore it.
	Artist string
	// Release optionally narrows a TRACK search to the album the recording sits on,
	// AND-ed in as `release:"<release>"` — the same relevance-safe field-scoped
	// pattern Artist uses, and verified against the live recording index before it
	// was built on (needs-fixing/06). It is pre-filled from the item's parsed album;
	// blank means no narrowing. Kinds with no release axis (video, album, artist)
	// ignore it. It is what makes a recording title that a hundred releases share —
	// "Intro", "She" — answerable from one row.
	Release string
	// Limit caps the candidate page the provider returns (0 → the source default).
	// Offset skips that many results, so the picker can page through a broad query
	// ("show more") instead of only ever seeing the first page.
	Limit  int
	Offset int
}

// Candidate is one result of a provider Search — the Enrichment-override picker's
// unit (CONTEXT.md "Enrichment override", ADR-0019). It carries just enough for an
// Admin to disambiguate two same-named works before applying: the authoritative
// ExternalID to pin, the source's Title + Year, a ThumbnailURL for the card, and a
// Disambiguation hint (TMDB overview / MusicBrainz disambiguation comment). Kind is
// the fine entity kind the search targeted (movie/episode/track for this slice).
// Applying a Candidate is a durable Enrichment override: it never touches identity
// or watch state (ADR-0002/0014).
type Candidate struct {
	ExternalID     string
	Title          string
	Year           int
	ThumbnailURL   string
	Disambiguation string
	Kind           string
	// TypeLabel is a short record-type hint that disambiguates same-titled hits
	// (item-editing/search-improvements): for a release-group the primary + secondary
	// types (e.g. "Album · Soundtrack" — the tell that separates the Anastasia
	// soundtrack from the many other "Anastasia"s), for an artist its type
	// ("Group"/"Person"). Empty when the source reports none. Surfaced as a badge on
	// the picker card, distinct from the free-text Disambiguation line.
	TypeLabel string
	// Tracklist is the ordered track preview an ALBUM candidate carries so an Admin
	// can confirm the positional map before applying (surfaced as an expandable
	// preview in the picker; consumed by slice 05's positional cascade — ADR-0019).
	// Nil for every non-album kind.
	Tracklist []TrackCandidate
	// ReleaseID is the exact EDITION this candidate came from, when the Admin named
	// one: a pasted MusicBrainz /release/ URL resolves to its parent release-group
	// (ExternalID, which is what an album IS — ADR-0038) and carries the release here
	// rather than dropping it (ADR-0052). Empty for a search hit, for a pasted
	// /release-group/ URL, and for every non-album kind — and an empty value applied
	// to an album CLEARS any edition it had, because the Admin just named a less
	// specific thing.
	ReleaseID string
}

// TrackCandidate is one track in an album candidate's tracklist preview: its
// disc/track position, title, and the recording's authoritative external id. It is
// display + positional-map data only, never identity (embedded tags stay the Music
// identity authority, ADR-0002). ExternalID is the MusicBrainz recording MBID, so
// slice 05's album→track cascade can pin each mapped track a DURABLE per-track
// Enrichment override (the recording it maps to positionally); it may be empty when
// the source did not report it.
type TrackCandidate struct {
	Disc       int
	Position   int
	Title      string
	ExternalID string
}

// TitleRef is the locally-parsed identity handed to a provider for lookup. When
// an external id is present (a curated {tmdb-…} token or a fix-match-assigned id)
// the provider resolves BY id without a fuzzy search (CONTEXT.md "Locked field"
// is unrelated; this is the external-match anchor). Kind selects the source
// (movie → TMDB). TV/Music fields are carried for later slices.
type TitleRef struct {
	Kind  string // "movie" | "episode" | "track"
	Title string
	Year  int

	TMDBID        string
	IMDBID        string
	MusicbrainzID string
	TheTVDBID     string // series id for the TheTVDB supplement (show/season/episode)
	AniDBID       string // anime id for the AniDB provider (not naming-derived; pinned)

	// TV (unused by the Movie slice).
	SeasonNumber  int
	EpisodeNumber int
	EpisodeLabel  string

	// Music (unused by the Movie slice).
	Artist string
	Album  string
	Track  string

	// ReleaseMBID pins a MusicBrainz *release* (one edition) for an album Lookup: the
	// provider resolves it to its parent release-group (the album we actually pin), so
	// a pasted /release/ URL identifies the album. Empty for the normal release-group
	// path (MusicbrainzID).
	ReleaseMBID string

	// AlbumHints are a few of the Albums this library files under an ARTIST, carried
	// on an artist ref so the provider can identify the artist through its
	// DISCOGRAPHY instead of through its name (ADR-0053). They are evidence, not a
	// pin: the provider is free to use none of them and fall back to the name search.
	//
	// They exist because every discriminator built out of an artist's name fails on
	// the case that motivated them. MusicBrainz holds a 1958 British instrumental
	// group literally named "The Eagles"; the American band is named "Eagles". An
	// exact-phrase name search finds the British one, a name acceptance test accepts
	// it, and an exact match scores 100. The two are told apart by what they
	// recorded, and the library is holding one of those records.
	//
	// Capped and deterministically ordered by the caller so two passes ask the same
	// question of the same album (collectMusicLeaves' musicAlbumHints). Empty for
	// every non-artist ref, and for an artist whose albums are unidentifiable — a
	// soundtrack filed under the film's name, an "Unknown Artist" pile — which is
	// exactly where corroboration has nothing to offer.
	AlbumHints []AlbumHint
}

// AlbumHint is one local Album offered as corroboration for its Artist (ADR-0053):
// the title the library holds for it, plus the release-group MBID the FILES assert
// when they assert one.
//
// The id is the better half by a distance — it resolves with a single
// /release-group/<id> LOOKUP and no search at all, which is ADR-0049's
// lookup-beats-search preference applied one level up. The title is the fallback,
// and it is searched for UNNARROWED: narrowing that search by the artist would
// reintroduce the very name corroboration exists not to trust.
type AlbumHint struct {
	Title            string
	ReleaseGroupMBID string
}

// ArtworkRef points at a remote image the provider found for a role
// ("poster" | "background"). The enrich service downloads the bytes via the
// ArtworkFetcher and caches them on disk (ADR-0007).
type ArtworkRef struct {
	Role string
	URL  string
}

// ArtworkCandidate is one image the authoritative provider offers for a role
// (poster/background/cover), the unit the Edit-item image picker lists so an
// Admin can pick a specific one and lock the role (Fix label, ADR-0019). Unlike
// the single ArtworkRef the enrichment pass auto-picks, a role usually has many
// candidates; the picker shows them all. Width/Height are the source dimensions
// (0 when the provider doesn't report them) so the picker can hint resolution;
// Source is the provider name ("tmdb" / "coverartarchive").
type ArtworkCandidate struct {
	URL    string
	Width  int
	Height int
	Source string
}

// Credit is one cast/crew member in normalized form (kind "cast" | "crew").
//
// PersonRef is the provider-namespaced stable person id (e.g. "tmdb:12345"),
// empty when the provider supplies none. It keys the person's headshot in the
// generic entity_artwork table, so one actor's photo is stored + cached once
// across every Title they appear in (cast-photos/01). ImageURL is the absolute
// headshot URL the provider found (built from its image base exactly like a
// poster URL), empty when there's no headshot; the enrich service downloads it
// through the ArtworkFetcher into the on-disk artwork cache, keyed by PersonRef.
type Credit struct {
	Person    string
	Role      string
	Character string
	Kind      string
	PersonRef string
	ImageURL  string
}

// TitleMetadata is the normalized, provider-agnostic result of a lookup: the
// descriptive fields plus artwork references. Matched is false (or ErrNoMatch is
// returned) when the source has no record. ExternalID is the resolved provider
// id (e.g. the TMDB id), Source the provider name ("tmdb").
type TitleMetadata struct {
	Matched bool

	// Name is the canonical title the source has for this entity. For an Episode
	// (its real name) or a sparse Track it is applied as a display-only override
	// during enrichment (never identity, ADR-0014). For a Movie/Show it carries the
	// source's canonical title but enrichment does NOT apply it as the display
	// title (identity owns that); it exists so an Admin's explicit by-id identity
	// fix can resolve the canonical title (see Service.ResolveIdentity).
	Name string
	// Year is the source's release / first-air year (0 when unknown). Like Name it
	// is surfaced for by-id identity resolution, not written by enrichment.
	Year           int
	Overview       string
	Tagline        string
	ContentRating  string
	ReleaseDate    string
	RuntimeMinutes int
	Studio         string

	Genres  []string
	Cast    []Credit
	Artwork []ArtworkRef

	ExternalID string
	Source     string
}

// MetadataProvider resolves a parsed identity to normalized descriptive metadata
// + artwork references. The real implementation owns all external HTTP, search-
// vs-id resolution, language/region preference, rate limiting, and response
// caching. It NEVER returns identity; a no-match is (TitleMetadata{}, ErrNoMatch)
// or a result with Matched=false, not a fatal error.
type MetadataProvider interface {
	Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error)

	// Search returns the authoritative provider's candidates for a free-text query
	// scoped to the fine entity kind (movie/episode/track for this slice) — the
	// search half of the Edit-item Enrichment-override flow (ADR-0019). Only the
	// authoritative source per kind answers (TMDB for video, MusicBrainz for music);
	// a provider that does not own the kind returns ErrSearchUnavailable. A query
	// with no hits is (nil, nil); an unreachable/unconfigured source is a real error
	// (or ErrSearchUnavailable) the caller reports rather than a hang. Results are
	// best-effort ordered by the source's relevance; the service caps the count. opts
	// carries optional artist narrowing + paging (item-editing/search-improvements);
	// a provider that has no such axis for the kind ignores it.
	Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error)

	// ArtworkCandidates lists the images the authoritative provider offers for a
	// role (poster/background/cover) on the record ref points at — the Edit-item
	// image picker's data (Fix label, ADR-0019). ref must carry the pinned external
	// id (a role has no candidates without a resolved record); a record with no
	// images for the role is (nil, nil). Only the authoritative source per kind
	// answers; a provider that doesn't own the kind (a supplement, or a nil
	// Composite sub) returns ErrSearchUnavailable. An unreachable/unconfigured
	// source is a real error the caller reports rather than a hang. This never
	// touches identity — picking a listed image sets+locks the role only.
	ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error)
}

// EpisodeCandidate is one episode offered when an Admin picks WHICH provider
// episode should decorate a file — the second step of correcting a TV episode
// whose on-disk numbering doesn't line up with the provider's.
type EpisodeCandidate struct {
	Season   int
	Episode  int
	Name     string
	Overview string
	AirDate  string
	// StillURL is the provider's own episode-still URL (the API layer rewrites it
	// to the same-origin image proxy before it reaches a browser).
	StillURL string
}

// SeasonSummary is one season of a series, for the season chooser.
type SeasonSummary struct {
	Season       int
	EpisodeCount int
}

// EpisodeLister is an OPTIONAL provider capability: listing a series' seasons and
// the episodes within one, so an Admin can pick the exact episode a file should be
// decorated from.
//
// It is deliberately NOT part of MetadataProvider. Only the authoritative video
// source can answer it, and folding it into the main interface would force eight
// providers that have no notion of an episode list — the music chain, the artwork-
// only supplements — to carry a stub. Callers type-assert and degrade to "no
// episode picking" when the provider doesn't implement it, which is the same
// graceful posture the rest of enrichment takes (ADR-0001).
type EpisodeLister interface {
	// SeriesSeasons lists the seasons of the series named by its provider id.
	SeriesSeasons(ctx context.Context, showExternalID string) ([]SeasonSummary, error)
	// SeasonEpisodes lists one season's episodes, in episode order.
	SeasonEpisodes(ctx context.Context, showExternalID string, season int) ([]EpisodeCandidate, error)
}

// ErrNoTracklist is the AlbumTracklister's "this album has no tracklist" outcome:
// the album named no release-group, the release-group holds no releases, or the
// release it does hold carries no tracks. It is DELIBERATELY distinct from an
// empty tracklist, because the caller's two failures need different words
// (ADR-0050): "this album has no tracklist" points at the Album — fix the Album,
// or its release — while "this tracklist has no room for this track" points at the
// one file. A provider that returns (nil, nil) collapses them into the same
// silence, so AlbumTracklist never does: a nil error always means at least one
// track.
//
// Every OTHER error (a transport failure, a 503 load shed — ADR-0049) flows out as
// itself and does NOT match this sentinel, so a caller that wants to retry a
// transient failure can still tell it apart from a settled "there is nothing here".
var ErrNoTracklist = errors.New("enrich: album has no tracklist")

// TracklistRequest names the album whose tracklist is wanted. Two ids and a count,
// because that is exactly what choosing the right RELEASE takes (ADR-0050):
//
//   - ReleaseGroupID is the album itself — required, and the authority the tagged
//     release is checked against. Empty means the album is unresolved, which is
//     "no tracklist" without a single call.
//   - ReleaseID is the exact edition to read: the one an ADMIN chose
//     (entity_enrichment.external_release_id, ADR-0052) or, failing that, the one
//     the FILES name (albums.musicbrainz_release_id, from the musicbrainz_albumid
//     tag). Empty for an untagged album nobody has pinned. It is used ONLY when its
//     parent release-group is ReleaseGroupID: neither a mis-tagged file nor a stale
//     pin naming a stranger's release may renumber the album.
//   - ReleaseIDChosen says WHOSE assertion ReleaseID is — a human's, or a file's.
//     It does not change how the release is fetched or checked. It changes what
//     happens when the release does not apply: a file's release falls through to
//     fit-selection silently, while a human's is reported as ErrNoTracklist so the
//     caller can re-ask WITHOUT the pin and know that the tracklist it finally got
//     is not the one the human asserted. That distinction is the whole licence
//     ADR-0052 grants position-alone mapping, and it is not recoverable from a
//     tracklist after the fact — every release's tracklist looks the same.
//   - LocalTrackCount is how many Tracks the LOCAL album holds. It is an input
//     rather than something the provider could derive, and it is what separates a
//     12-track standard edition from its 15-track deluxe. Zero means "unknown",
//     which selects the earliest release rather than guessing.
type TracklistRequest struct {
	ReleaseGroupID  string
	ReleaseID       string
	ReleaseIDChosen bool
	LocalTrackCount int
}

// AlbumTracklister is an OPTIONAL provider capability: the ordered tracks of the
// release an Album actually IS, so a matched Album can name the recording behind
// each of its own Tracks instead of every Track paying a text search (ADR-0050).
//
// Like EpisodeLister it is deliberately NOT part of MetadataProvider — only the
// authoritative MUSIC source can answer it, and folding it in would force the video
// providers and the artwork-only supplements to carry a stub. Callers type-assert
// and degrade to "no tracklist" when the provider doesn't implement it, the same
// graceful posture the rest of enrichment takes (ADR-0001).
//
// It is NOT the album-candidate preview. The preview (Candidate.Tracklist) wants a
// cheap, roughly-right sample for a page of search results and pays one call per
// candidate; this wants the RIGHT edition for one album and may pay two.
type AlbumTracklister interface {
	// AlbumTracklist returns the album's ordered tracks: never empty with a nil
	// error, ErrNoTracklist when the album has none, and any other error as itself.
	// An entry the source gave no recording id keeps its position (it still claims
	// that position for the caller's match rule) with an empty ExternalID.
	AlbumTracklist(ctx context.Context, req TracklistRequest) ([]TrackCandidate, error)
}

// ReleaseEdition is ONE edition of an album — a MusicBrainz release under the
// album's release-group — described with exactly the five facts an Admin needs to
// tell two editions apart at a glance (ADR-0052): when it came out, where, on what
// medium, how many tracks it holds, and whatever the source says to disambiguate
// it ("deluxe edition", "reissue").
//
// TrackCount is the one that does the work. The whole reason an operator is
// looking at this list is that the album's tracks did not line up, and the edition
// whose count equals the local album's is the one that will — which is why the
// picker states the local count beside the list rather than making them subtract.
//
// It is deliberately NOT a Candidate: a Candidate is a record to PIN as the
// album's identity, and an edition is never that (album identity stays the
// release-group, ADR-0038). An edition is a DECORATION refinement, applied through
// the album's existing override carrying this ReleaseID.
type ReleaseEdition struct {
	ReleaseID      string
	Date           string
	Country        string
	Format         string
	TrackCount     int
	Disambiguation string
}

// AlbumEditionLister is an OPTIONAL provider capability: the editions (releases) a
// release-group holds, so an Admin can choose the one their files actually are
// WITHOUT leaving Obelo (ADR-0052).
//
// It is the listing half of the browse AlbumTracklist already pays for. Choosing
// by track-count fit is what the automatic path does; this is the same information
// handed to the human, whose choice then outranks the fit and licenses
// position-alone mapping (ADR-0052, issue 11).
//
// Like EpisodeLister and AlbumTracklister it is deliberately NOT part of
// MetadataProvider — only the authoritative MUSIC source can answer it. A provider
// that does not implement it answers ErrSearchUnavailable, and the picker degrades
// to the pasted-URL escape hatch rather than an error page.
type AlbumEditionLister interface {
	// ReleaseGroupEditions lists the release-group's editions, best-effort ordered
	// as the source returns them. A release-group with no releases is (nil, nil) —
	// an empty list, not an error: "this album has exactly no editions to choose
	// from" is an answer, and the caller renders it as one.
	ReleaseGroupEditions(ctx context.Context, releaseGroupID string) ([]ReleaseEdition, error)
}

// ArtworkFetcher downloads image bytes for a remote URL the provider returned,
// with content-type + size guards. The enrich service writes the bytes into the
// on-disk artwork cache (ADR-0007).
type ArtworkFetcher interface {
	Fetch(ctx context.Context, url string) (data []byte, contentType string, err error)
}
