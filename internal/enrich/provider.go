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

// ArtworkFetcher downloads image bytes for a remote URL the provider returned,
// with content-type + size guards. The enrich service writes the bytes into the
// on-disk artwork cache (ADR-0007).
type ArtworkFetcher interface {
	Fetch(ctx context.Context, url string) (data []byte, contentType string, err error)
}
