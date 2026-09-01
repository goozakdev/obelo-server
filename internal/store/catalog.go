package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/goozakdev/obelo-server/internal/naming"
)

// The catalog rows produced by a scan: Title → Edition → File → Stream
// (CONTEXT.md). These typed structs mirror the 0004_catalog schema and are the
// persistence shapes the scanner writes and the browse API reads. Identity
// (title + year) is derived by the scanner from on-disk paths only (ADR-0002);
// the technical attributes come from ffprobe.

// Title is a Movie in this slice: the logical entity a user browses.
type Title struct {
	ID          string
	LibraryID   string
	Kind        string
	Title       string
	Year        int // 0 when no year was parsed
	IdentityKey string
	SortTitle   string
	AddedAt     string
	// TMDBID / IMDBID are the external ids of the RECORD this Title resolves
	// against: the Admin's Enrichment override when there is one, else the id the
	// folder name asserts (ADR-0045). Every read path selects them through
	// recordExternalIDs, so a reader never has to know which of the two answered.
	//
	// On the WRITE side — a TitleTree the Scanner hands to writeTitleRow — they are
	// the identity id and nothing else: the embedded {tmdb-…}/{imdb-tt…} token or a
	// folder-anchored Match override. A tree that wants to seed the enrichment
	// record instead sets TitleTree.RecordTMDBID.
	TMDBID string
	IMDBID string
	// EnrichmentIDOrigin says WHOSE choice this Title's enrichment record is: nobody's
	// (an id a pass resolved), the Admin's own on this Title (Fix info, an Episode
	// pin), or its parent's, applied by a Cascade (ADR-0045, ADR-0046). Both choices
	// are durable — .Locked() is the old enrichment_id_locked bit — but only
	// .OwnChoice() outranks a Cascade. The per-Title twin of
	// entity_enrichment.external_id_origin. Populated only by the reads that feed
	// enrichment (enrichedTitleColumns, EpisodesForSeason, TracksForAlbum);
	// OriginDerived elsewhere.
	EnrichmentIDOrigin RecordOrigin
	// NeedsReview flags a Title filed from a partial best-effort parse (e.g. no
	// year) — browsable, but surfaced in the Admin attention list (CONTEXT.md).
	NeedsReview bool
	// Ambiguous flags a Title where two files parsed to the same Edition identity
	// and were not parts — surfaced, never silently picked (collision rule).
	Ambiguous bool
	// Hidden is the derived all-Files-Missing state: every File of this Title is
	// absent from disk, so it is excluded from browse but still fetchable by id
	// (soft-delete, ADR-0008). Recomputed by the scanner after each scan.
	Hidden bool
	// SeasonNumber / EpisodeNumber / EpisodeLabel are the TV ordering fields,
	// populated only for an Episode (kind 'episode'); a Movie leaves them 0/"".
	// SeasonNumber 0 = Specials; EpisodeLabel carries a date / absolute number for
	// a degraded-offline episode (see tv.go).
	SeasonNumber  int
	EpisodeNumber int
	EpisodeLabel  string
	// DiscNumber / TrackNumber are the Music ordering fields, populated only for a
	// Track (kind 'track'); a Movie/Episode leaves them 0. A Track lists in
	// disc-then-track order (music.go).
	DiscNumber  int
	TrackNumber int

	// --- Enrichment (external-metadata-enrichment) -----------------------------
	// These descriptive fields are written by the optional Enrichment step, never
	// the scanner, and never affect identity (ADR-0002). They are zero on an
	// un-enriched Title and populated only by the enriched read paths (ListTitles /
	// TitleByID); the other readers (search/home) leave them zero. MusicbrainzID is
	// the Music external-match anchor (analogous to TMDBID for video).
	Overview       string
	Tagline        string
	ContentRating  string
	ReleaseDate    string
	RuntimeMinutes int
	Studio         string
	MusicbrainzID  string
	// MusicbrainzRecordingID is the recording MBID the FILE asserts, read from its
	// tags by the Scanner and re-derived on every scan (ADR-0049). It is the LOCAL
	// claim, the music twin of TMDBID — decoration only, never identity — and it
	// loses to MusicbrainzID above, which is the enrichment RECORD (a pass's own
	// result, or the Admin's Fix info). Enrichment prefers either over a text
	// search: a lookup by id is exact and hits a far healthier MusicBrainz endpoint
	// than `/ws/2/recording?query=`.
	MusicbrainzRecordingID string
	// EnrichmentStatus ∈ pending|matched|unmatched|failed|disabled (CONTEXT.md
	// "Enrichment"). EnrichedAt / EnrichmentSource record the last successful pass.
	EnrichmentStatus string
	EnrichedAt       string
	EnrichmentSource string
	// EnrichmentAttempts / EnrichmentRetryAt are the retry bookkeeping for a
	// 'failed' Title (ADR-0048). Attempts counts CONSECUTIVE failed lookups (reset
	// by any settled outcome); RetryAt is the RFC3339 instant from which the next
	// only-new pass will pick the Title up again, or "" when nothing will — the
	// difference between a transient failure the server is coming back for and a
	// permanent one parked on the Admin's attention list. Both are zero for every
	// non-failed status. Populated only by the reads that feed enrichment.
	EnrichmentAttempts int
	EnrichmentRetryAt  string
	// Genres is the enriched genre list (loaded by the enriched read paths; empty
	// otherwise). Cast lives on TitleDetail (heavier, detail-only).
	Genres []string
	// EnrichmentSeason / EnrichmentEpisode pin WHICH provider episode decorates this
	// Episode, overriding the season/episode numbers parsed from its filename for
	// the LOOKUP ONLY. They exist because a provider may number a series differently
	// from the files on disk (a run of episodes moved into the next season), which
	// otherwise leaves those files permanently unmatchable: pinning the right show
	// still asks for the wrong episode.
	//
	// This is an Enrichment override, not an identity one (CONTEXT.md): identity_key,
	// season_id, SeasonNumber, EpisodeNumber and watch state are all untouched, so
	// the Episode keeps its place in the library and its history. Both are
	// NoEpisodePin when unset, which is the default.
	EnrichmentSeason  int
	EnrichmentEpisode int
	// EnrichedTitle is the canonical DISPLAY title an Episode/Track may gain from
	// Enrichment (e.g. a real episode name for a date-based episode) — DISPLAY
	// ONLY, never identity (the parsed Title and identity_key are untouched,
	// ADR-0014). Empty for a Movie and for an un-enriched Episode/Track. A reader
	// shows EnrichedTitle when present, else Title.
	EnrichedTitle string
}

// Edition is a specific version/cut of a Title. A Title may have several
// (distinct quality tokens or a named {edition-…}); a multi-part Edition holds
// more than one File.
type Edition struct {
	ID      string
	TitleID string
	Name    string
	AddedAt string
	Files   []File
}

// Extra is a recognized clip (trailer/featurette/…) attached to a Title. Never a
// browsable Title; excluded from the titles list (CONTEXT.md).
type Extra struct {
	ID         string
	TitleID    string
	Type       string
	Path       string
	Container  string
	DurationMs int64
	SizeBytes  int64
}

// Artwork is a poster/background image associated with a Title. role is
// "poster" or "background"; path is the absolute on-disk image. Source is
// "local" (a scanner-recorded poster.jpg/cover.jpg next to the media) or
// "fetched" (an Enrichment-downloaded image in the artwork cache, ADR-0007).
// Local wins over fetched for the same role (CONTEXT.md).
type Artwork struct {
	ID      string
	TitleID string
	Role    string
	Path    string
	Source  string
	// AddedAt is when this row was written (re-defaults to datetime('now') on
	// every insert, so it advances on a re-fetch/pick/upload). The Title detail
	// derives its cache-bust token from MAX(added_at) over the resolved rows —
	// the row `path` is a stable per-(Title,role) cache filename and can't bust.
	// Populated by artworkForTitle; empty from readers that don't select it.
	AddedAt string
}

// Credit is one cast/crew member attached to a Title by Enrichment. Kind is
// "cast" or "crew"; Character is the role played (cast only). Order preserves
// the provider's billing order.
//
// PersonRef is the provider-namespaced person id ("tmdb:<id>", empty when the
// provider supplied none) that links this credit to the person's headshot in
// entity_artwork (cast-photos/01). PhotoVersion is the opaque cache-bust token
// for that headshot (its entity_artwork added_at), populated on read when a
// cached photo exists and empty otherwise — analogous to a poster's artwork
// version. A read-only field: it is never written back by the enrichment path.
type Credit struct {
	Person       string
	Role         string
	Character    string
	Kind         string
	PersonRef    string
	PhotoVersion string
}

// File is one physical file on disk with its ffprobed technical attributes.
type File struct {
	ID         string
	EditionID  string
	Path       string
	Container  string
	VideoCodec string
	AudioCodec string
	Width      int
	Height     int
	Bitrate    int64
	DurationMs int64
	SizeBytes  int64
	AddedAt    string
	// Mtime is the file's on-disk modification time (RFC3339 UTC), the cheap
	// change signal for incremental scans alongside SizeBytes.
	Mtime string
	// Present is false when the File is Missing — absent from disk but kept as a
	// soft-delete so it (and its Title) can return on a later scan (ADR-0008).
	Present bool
	// PartOrdinal is this File's 1-based position among the parts of its Edition,
	// 0 when nothing numbered it. It is the STORED play order (migration 0049):
	// Edition.Files is read back in (part_ordinal, path) order, so a multi-part
	// Edition an Admin assembled by Placement plays in the order they chose rather
	// than in filename order (ADR-0044). It mirrors file_decisions.ordinal for a
	// placed File and filename.go's partNumber() for a parse-numbered one.
	PartOrdinal int
	Streams     []Stream
}

// PresentFiles returns the Edition's on-disk Files in stored order — the parts of a
// multi-part Edition, in play order. Missing Files are skipped: they cannot be
// streamed (soft delete, ADR-0008).
//
// Order is the caller's stored order, which the scanner writes in part order
// (filename.go partNumber), so parts[0] is part 1.
func (e Edition) PresentFiles() []File {
	out := make([]File, 0, len(e.Files))
	for _, f := range e.Files {
		if f.Present {
			out = append(out, f)
		}
	}
	return out
}

// Parts returns the Files that make up this Edition's ONE playback timeline, in
// play order: every present File of a genuine multi-part Edition, and just the
// first present File of anything else. It is the single definition of "what plays"
// that TotalDurationMs, PartAt, PartStartMs, the HLS escalation and the concat list
// all read, so none of them can disagree about it.
//
// A multi-part Edition is one whose Files are NUMBERED — that is what the
// convention means by a part suffix joining several Files into one Edition
// (`- part1` / `- cd1`). More-than-one-File is NOT the same test, and the
// difference is the collision rule: two files that parse to the same Edition
// identity and are not parts are "flagged ambiguous in the web app, never silently
// guessed" (naming-convention.md). Concatenating THOSE plays `S01E05-E06` followed
// by `S01E06` again, or the same episode twice from two equal rips — an unrelated
// second work spliced onto the first, presented as one timeline. So an unsettled
// Edition plays its first File and nothing else, and the Ambiguous flag on its
// Title is what tells the Admin why (needsreview.go).
//
// Numbered means, in order:
//
//   - part_ordinal > 0 on EVERY present File — the stored order, written by the
//     scanner from the filename and by Placement from the Admin's own choice
//     (migration 0049). Some-but-not-all numbered is a collision, not a part set:
//     `Movie - part1.mkv` beside a bare `Movie.mkv` is exactly the ambiguous case.
//   - failing that — NO present File carries an ordinal at all — the FILENAMES.
//     part_ordinal was added with no backfill, so every File row written before
//     migration 0049 sits at 0 until a scan rewrites it. Reading only the column
//     would make every legitimate multi-part Movie and Episode on an install that
//     has not rescanned play its first part and stop, and would put the ~90%
//     Watched threshold back on that one part — a far worse regression than the bug
//     this rule fixes (see internal/playback/multipart_test.go). Before 0049 the
//     ONLY way to get a multi-part Edition was to name the files for it, so the
//     names still carry the answer for exactly the rows the column cannot.
func (e Edition) Parts() []File {
	present := e.PresentFiles()
	if len(present) < 2 || !numberedParts(present) {
		if len(present) == 0 {
			return nil
		}
		return present[:1:1]
	}
	return present
}

// numberedParts reports whether these Files are a numbered part set — see Parts
// for the rule and for why the filename fallback exists.
func numberedParts(present []File) bool {
	stored := 0
	for _, f := range present {
		if f.PartOrdinal > 0 {
			stored++
		}
	}
	if stored > 0 {
		return stored == len(present)
	}
	for _, f := range present {
		if naming.PartNumber(f.Path) == 0 {
			return false
		}
	}
	return true
}

// IsMultiPart reports whether the Edition is a multi-part one: more than one
// PRESENT File, joined by a part suffix (`- part1` / `cd1`, naming-convention.md).
// One File plus one Missing sibling is NOT multi-part — there is nothing to join —
// and neither is a pair of unnumbered Files claiming one Edition, which is the
// ambiguous collision Parts describes.
func (e Edition) IsMultiPart() bool { return len(e.Parts()) > 1 }

// TotalDurationMs is the Edition's whole playable duration: the sum of the parts it
// actually plays (Parts). For an ordinary single-File Edition it is simply that
// File's duration.
//
// This is the duration the Watched threshold and the resume position must be
// measured against. Measuring against ONE PART instead is not a cosmetic error: a
// viewer who finishes part 1 of a two-part episode would cross the ~90% ceiling of
// that part and have the whole episode marked watched at its halfway point, with
// the resume cleared (CONTEXT.md "Watched threshold", ADR-0028 — a wrong watched
// flag also moves the Up Next anchor). Returns 0 when no part reports a duration,
// which callers already treat as "unknown, best-effort".
func (e Edition) TotalDurationMs() int64 {
	var total int64
	for _, f := range e.Parts() {
		total += f.DurationMs
	}
	return total
}

// PartAt maps a whole-Edition position onto the part that contains it, returning
// that part and the offset WITHIN it. It is the inverse of the running offset the
// parts are laid out on, and is what turns a stored resume position (which is
// whole-Edition, see TotalDurationMs) back into "open part 2 at 3m12s".
//
// A position past the end clamps to the last part so a resume can never dangle.
// ok is false only for an Edition with no present part at all.
func (e Edition) PartAt(positionMs int64) (part File, offsetMs int64, ok bool) {
	parts := e.Parts()
	if len(parts) == 0 {
		return File{}, 0, false
	}
	if positionMs < 0 {
		positionMs = 0
	}
	var start int64
	for _, f := range parts {
		// A part with an unknown duration cannot be spanned; treat the position as
		// landing inside it rather than skipping past it on bad data.
		if f.DurationMs <= 0 || positionMs < start+f.DurationMs {
			return f, positionMs - start, true
		}
		start += f.DurationMs
	}
	last := parts[len(parts)-1]
	return last, last.DurationMs, true
}

// PartStartMs is the whole-Edition offset at which `index` (0-based) begins — the
// value added to a position reported within that part to get the whole-Edition
// position. Out-of-range indexes clamp to 0.
func (e Edition) PartStartMs(index int) int64 {
	parts := e.Parts()
	var start int64
	for i, f := range parts {
		if i >= index {
			break
		}
		start += f.DurationMs
	}
	return start
}

// Stream is an elementary stream inside a File's container (video/audio/subtitle).
type Stream struct {
	ID        string
	FileID    string
	Index     int
	Kind      string
	Codec     string
	Language  string
	Width     int
	Height    int
	Channels  int
	IsDefault bool
	// Forced marks a subtitle Stream whose disposition is forced (auto-displayed
	// for text subs); captured from ffprobe. False for video/audio and unmarked
	// subtitle streams.
	Forced bool
	// Title is the stream's embedded title tag (ffprobe tags.title), e.g.
	// "Director's Commentary" on an audio Stream — the label the Audio menu shows
	// (audio-streams/01). "" when untagged. The row stays FFmpeg-pure: the ISO-639
	// language normalization happens at read/projection time, not here.
	Title string
	// Commentary and HearingImpaired are the ffprobe "comment"/"hearing_impaired"
	// dispositions on an audio Stream, so the menu can label a commentary or SDH
	// mix that carried no title tag, and later slices can disambiguate a
	// Remembered-audio pick by trait. False for video/subtitle and ordinary audio.
	Commentary      bool
	HearingImpaired bool
}

// Subtitle is a persisted Subtitle track from a NON-stream source: a Sidecar
// subtitle discovered next to the media, or a Fetched subtitle downloaded from a
// provider (slice 05). Embedded subtitle tracks are projected from a File's
// subtitle Streams and are not stored here. Source is "sidecar" or "fetched";
// a rescan rewrites only "sidecar" rows, so a "fetched" row survives (the
// artwork 'local'|'fetched' pattern). Title-scoped (survives the File rebuild a
// rescan performs). Kind is "text" or "image"; Language is ISO-639-1 ("" =
// Unknown); Path is the on-disk subtitle file.
type Subtitle struct {
	ID         string
	TitleID    string
	Source     string
	Kind       string
	Language   string
	Forced     bool
	IsDefault  bool
	Codec      string
	Path       string
	ProviderID string
}

// TitleDetail is a Title with its full nested catalog (Editions → Files →
// Streams) plus its Extras and Artwork, the shape behind GET /titles/{id}.
type TitleDetail struct {
	Title
	Editions []Edition
	Extras   []Extra
	Artwork  []Artwork
	// Subtitles are the Title's Sidecar/Fetched Subtitle tracks (the non-stream
	// sources). Embedded subtitle tracks are derived from the Editions' Files'
	// subtitle Streams, not carried here.
	Subtitles []Subtitle
	// Cast is the enriched cast/crew list (empty on an un-enriched Title). Genres
	// live on the embedded Title.
	Cast []Credit
	// LockedFields lists the descriptive fields an Admin hand-edited and Locked
	// (CONTEXT.md): re-enrichment skips them. Empty on a Title with no manual
	// edits. A client reads it to show which fields are pinned (and releasable).
	LockedFields []string
}

// TitleTree is the complete result of resolving one on-disk movie folder: the
// Title identity plus all its Editions (each with its Files/Streams), Extras,
// and Artwork. The scanner builds this and hands it to UpsertTitleTree; the
// store assigns/reuses the Title id (by identity) and rewrites the subtree
// atomically. Callers supply pre-generated ids on the children.
type TitleTree struct {
	Title
	// RecordTMDBID seeds the ENRICHMENT record of a Title this tree INSERTS, as
	// opposed to Title.TMDBID, which on a tree is the identity id (ADR-0045). It is
	// set by exactly one caller — the file matcher's Apply, carrying a Show's pinned
	// series onto the co-File sibling rows a split creates, which have no prior row
	// to keep it on. The Scanner leaves it empty, and writeTitleRow honors it on
	// INSERT only: an existing row's enrichment columns are never written by a tree.
	//
	// It carries the record and NOT enrichment_id_origin, so an inherited record
	// reads as one nobody chose: the sibling keeps its anchor and stays eligible for
	// its Show's next Cascade. Carrying the origin too would be more faithful when
	// the survivor's record really was the Admin's pick, and wants ShowEpisodeSlots
	// to report it first.
	RecordTMDBID string
	Editions     []Edition
	Extras       []Extra
	Artwork      []Artwork
	// Subtitles are the Sidecar Subtitle tracks the scanner found next to the
	// media. They are local rows: a rescan rewrites them and leaves any Fetched
	// (source='fetched') track intact.
	Subtitles []Subtitle
}

// UpsertTitleTree persists one resolved movie folder in a single transaction,
// keyed by the Title's (library_id, identity_key) so a rescan re-resolves to the
// same Title row instead of duplicating (identity stability, ADR-0014).
//
// The subtree is rewritten to reflect the folder's current present files: the
// Editions/Streams/Extras/Artwork rows are rebuilt, but Files are upserted by
// their UNIQUE path so a File's identity (id, added_at) survives a rescan —
// only its mtime/attributes and edition membership refresh. Every File written
// here is on disk, so it is set present=1; a previously-Missing File that has
// returned flips back to present. Absent files (not in any current tree) are
// left for MarkFilesMissing, never hard-deleted (soft-delete, ADR-0008).
func (db *DB) UpsertTitleTree(tree TitleTree) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin upsert title tree: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A Movie carries no episode columns (all zero/empty), so writeTitleRow leaves
	// season_id NULL and the ordering fields at their defaults — the Movie row is
	// byte-for-byte what it was before TV (additive).
	titleID, err := writeTitleRow(tx, tree, episodeColumns{})
	if err != nil {
		return err
	}
	if err := writeTitleSubtree(tx, titleID, tree, map[string]bool{}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert title tree: %w", err)
	}
	return nil
}

// episodeColumns carries the optional parent linkage written onto a titles row:
// the TV Season linkage + episode ordering (an Episode) OR the Music Album
// linkage + disc/track ordering (a Track). The zero value (used for a Movie)
// leaves both season_id and album_id NULL and the ordering columns at defaults.
type episodeColumns struct {
	seasonID      string // "" ⇒ NULL (a Movie/Track)
	seasonNumber  int
	episodeNumber int
	episodeLabel  string
	// Music linkage (a Track). albumID "" ⇒ NULL (a Movie/Episode); disc/track
	// numbers default 0 when absent from tags.
	albumID     string
	discNumber  int
	trackNumber int
}

// writeTitleRow resolves the Title id by (library_id, identity_key) — reusing the
// existing row on a rescan, else inserting — and writes its descriptive fields
// plus the (optional) TV linkage. It is shared by the Movie path
// (UpsertTitleTree), the Episode path (upsertEpisodeTitle) and the Track path
// (upsertTrackTitle) so identity stability is identical for all three. Returns
// the resolved Title id.
//
// THE TREE OWNS tmdb_id/imdb_id AND NOTHING ELSE (ADR-0045). Those two columns
// carry one claim and have one writer class: the id LOCAL NAMING asserts — an
// embedded `{tmdb-…}`/`{imdb-tt…}` token or a folder-anchored Match override. That
// is an identity claim (ADR-0002), and it is the SAME claim identity_key already
// carries: whenever a tree arrives with a TMDBID the key IS "tmdb:<id>"
// (scanner.identityKey), so the row this update finds was keyed by that very id.
// They are therefore written unconditionally, insert and update alike.
//
// The record an Admin chose — "Fix info" on a Movie or Track, an Episode pin's
// series, a Cascade — and the record an enrichment pass resolved live in
// enrichment_tmdb_id / enrichment_imdb_id / musicbrainz_id, which this function
// NEVER writes on an existing row. That is what makes an Enrichment override
// durable across a scan (ADR-0019, CONTEXT.md): not a guard that has to be
// remembered, but a column the Scanner has no statement to make about.
//
// Before ADR-0045 the two claims shared one column, and writing `tmdb_id = ?`
// unconditionally made every scan assert "" — nothing — over the Admin's answer.
// The damage was worse than loss: an Episode pin is a PAIR (which series, plus
// enrichment_season/enrichment_episode within it), and blanking only the series
// half silently re-aimed the lookup at the Show's own series at the borrowed
// numbering — a real record, confidently wrong (ADR-0044).
//
// The INSERT branch additionally seeds enrichment_tmdb_id from tree.RecordTMDBID,
// which only the file matcher's Apply sets (see TitleTree.RecordTMDBID); a
// brand-new row has no override of its own to protect.
func writeTitleRow(tx *sql.Tx, tree TitleTree, ep episodeColumns) (string, error) {
	var seasonID any
	if ep.seasonID != "" {
		seasonID = ep.seasonID
	}
	var albumID any
	if ep.albumID != "" {
		albumID = ep.albumID
	}

	var titleID string
	err := tx.QueryRow(
		`SELECT id FROM titles WHERE library_id = ? AND identity_key = ?`,
		tree.LibraryID, tree.IdentityKey,
	).Scan(&titleID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		titleID = tree.Title.ID
		if _, err := tx.Exec(
			`INSERT INTO titles
			   (id, library_id, kind, title, year, identity_key, sort_title,
			    tmdb_id, imdb_id, enrichment_tmdb_id, needs_review, ambiguous, hidden,
			    season_id, season_number, episode_number, episode_label,
			    album_id, disc_number, track_number, musicbrainz_recording_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
			titleID, tree.LibraryID, tree.Kind, tree.Title.Title, nullableYear(tree.Year),
			tree.IdentityKey, tree.SortTitle, tree.TMDBID, tree.IMDBID, tree.RecordTMDBID,
			boolToInt(tree.NeedsReview), boolToInt(tree.Ambiguous),
			seasonID, ep.seasonNumber, ep.episodeNumber, ep.episodeLabel,
			albumID, ep.discNumber, ep.trackNumber, tree.MusicbrainzRecordingID,
		); err != nil {
			return "", fmt.Errorf("store: inserting title: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("store: resolving title identity: %w", err)
	default:
		// Existing Title: refresh fields. A folder we just resolved has present
		// files, so the Title is no longer hidden. The subtree is rebuilt by caller.
		// needs_review is recomputed from the parse EXCEPT on a row an Admin has
		// dismissed (reviewed = 1), where it stays cleared so the dismissal survives
		// rescans (migration 0012). reviewed itself is never written by the scanner.
		if _, err := tx.Exec(
			`UPDATE titles SET title = ?, year = ?, sort_title = ?,
			    tmdb_id = ?, imdb_id = ?,
			    needs_review = CASE WHEN reviewed = 1 THEN 0 ELSE ? END,
			    ambiguous = ?, hidden = 0,
			    season_id = ?, season_number = ?, episode_number = ?, episode_label = ?,
			    album_id = ?, disc_number = ?, track_number = ?,
			    -- The recording id the file currently asserts. Written unconditionally
			    -- for the same reason tmdb_id is: it is the LOCAL claim, re-derived
			    -- from the file's tags on every scan, so a retagged file must be able
			    -- to change it and a de-tagged one to withdraw it. The Admin's record
			    -- lives in musicbrainz_id and is untouched here (ADR-0045/0049).
			    musicbrainz_recording_id = ?
			  WHERE id = ?`,
			tree.Title.Title, nullableYear(tree.Year), tree.SortTitle,
			tree.TMDBID, tree.IMDBID,
			boolToInt(tree.NeedsReview), boolToInt(tree.Ambiguous),
			seasonID, ep.seasonNumber, ep.episodeNumber, ep.episodeLabel,
			albumID, ep.discNumber, ep.trackNumber, tree.MusicbrainzRecordingID,
			titleID,
		); err != nil {
			return "", fmt.Errorf("store: updating title: %w", err)
		}
	}
	return titleID, nil
}

// writeTitleSubtree rebuilds a Title's Editions → Files → Streams plus Extras and
// Artwork, preserving each File's id/added_at by path (identity stability across
// rescans, ADR-0008/0014). Shared by the Movie and Episode upsert paths.
//
// written tracks file paths already INSERTED earlier in THIS transaction. It is
// load-bearing for a multi-episode file (S01E05-E06): the two Episode Titles are
// written one after another in one UpsertShowTree transaction and legitimately
// share a path, so the second write must NOT reclaim (delete) the first's row.
// A path already written this tx is treated as genuinely-new (its own fresh row
// under the new edition). For the Movie path each Title is its own transaction,
// so written starts empty and the cross-title reclaim (fix-match) is unchanged.
func writeTitleSubtree(tx *sql.Tx, titleID string, tree TitleTree, written map[string]bool) error {
	// Capture the existing Files of this Title by path so an upsert preserves
	// their id + added_at (a renamed file is a new path → a fresh row; the old
	// path will be marked Missing by MarkFilesMissing). Then rebuild editions/
	// extras/artwork wholesale; their old rows (and the streams under them via
	// cascade) are dropped first.
	type fileMeta struct {
		id      string
		addedAt string
	}
	existing := map[string]fileMeta{}
	rows, err := tx.Query(
		`SELECT f.path, f.id, f.added_at FROM files f
		   JOIN editions e ON f.edition_id = e.id WHERE e.title_id = ?`, titleID)
	if err != nil {
		return fmt.Errorf("store: reading existing files: %w", err)
	}
	for rows.Next() {
		var path string
		var fm fileMeta
		if err := rows.Scan(&path, &fm.id, &fm.addedAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scanning existing file: %w", err)
		}
		existing[path] = fm
	}
	_ = rows.Close()

	// Drop the subtree (files/streams cascade off editions; extras direct).
	for _, tbl := range []string{"editions", "extras"} {
		if _, err := tx.Exec(`DELETE FROM `+tbl+` WHERE title_id = ?`, titleID); err != nil {
			return fmt.Errorf("store: clearing %s: %w", tbl, err)
		}
	}
	// Clear only LOCAL artwork (the scanner owns local poster.jpg/cover.jpg). Any
	// Enrichment-fetched artwork (source='fetched') is left untouched so a rescan
	// never drops it (external-metadata-enrichment).
	if _, err := tx.Exec(`DELETE FROM artwork WHERE title_id = ? AND source = 'local'`, titleID); err != nil {
		return fmt.Errorf("store: clearing local artwork: %w", err)
	}
	// Same for Subtitle tracks: the scanner owns the local (sidecar) rows, so a
	// rescan rewrites them; a Fetched subtitle (source='fetched', slice 05) is
	// left untouched so it survives rescans (ADR-0021, the artwork pattern).
	if _, err := tx.Exec(`DELETE FROM subtitles WHERE title_id = ? AND source = 'sidecar'`, titleID); err != nil {
		return fmt.Errorf("store: clearing local subtitles: %w", err)
	}

	for _, ed := range tree.Editions {
		if _, err := tx.Exec(
			`INSERT INTO editions (id, title_id, name) VALUES (?, ?, ?)`,
			ed.ID, titleID, ed.Name,
		); err != nil {
			return fmt.Errorf("store: inserting edition: %w", err)
		}
		for _, f := range ed.Files {
			// Reuse the prior row's id/added_at for this path so the File's
			// identity is stable across rescans. The prior row may belong to THIS
			// title (rebuild) or — when a Match override re-points identity — to a
			// different Title; either way the path is UNIQUE, so reclaim it: drop
			// the old row (cascading its streams) and re-insert under this edition.
			fileID := f.ID
			addedAt := ""
			if fm, ok := existing[f.Path]; ok {
				fileID = fm.id
				addedAt = fm.addedAt
			} else if written[f.Path] {
				// A co-File sibling (multi-episode) already wrote this path THIS tx:
				// keep both — insert a distinct fresh row under this edition, never
				// reclaim the sibling's row. Mint a fresh id rather than trust f.ID:
				// on an incremental rescan the scanner reuses ONE stored File row for
				// the shared path (LoadStoredFile), so both siblings arrive with the
				// SAME f.ID — reusing it here would collide on files.id. This row has
				// no existing identity for THIS Title anyway (existing[path] missed),
				// so a fresh id is correct and becomes stable-by-path on the next scan.
				fileID = uuid.NewString()
			} else {
				var priorID, priorAdded string
				switch err := tx.QueryRow(
					`SELECT id, added_at FROM files WHERE path = ?`, f.Path,
				).Scan(&priorID, &priorAdded); {
				case err == nil:
					fileID = priorID
					addedAt = priorAdded
					if _, err := tx.Exec(`DELETE FROM files WHERE id = ?`, priorID); err != nil {
						return fmt.Errorf("store: reclaiming file %q: %w", f.Path, err)
					}
				case errors.Is(err, sql.ErrNoRows):
					// genuinely new path
				default:
					return fmt.Errorf("store: looking up file %q: %w", f.Path, err)
				}
			}
			written[f.Path] = true
			if addedAt == "" {
				if _, err := tx.Exec(
					`INSERT INTO files
					   (id, edition_id, path, container, video_codec, audio_codec, width, height,
					    bitrate, duration_ms, size_bytes, mtime, present, part_ordinal)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
					fileID, ed.ID, f.Path, f.Container, f.VideoCodec, f.AudioCodec,
					f.Width, f.Height, f.Bitrate, f.DurationMs, f.SizeBytes, f.Mtime,
					f.PartOrdinal,
				); err != nil {
					return fmt.Errorf("store: inserting file %q: %w", f.Path, err)
				}
			} else if _, err := tx.Exec(
				`INSERT INTO files
				   (id, edition_id, path, container, video_codec, audio_codec, width, height,
				    bitrate, duration_ms, size_bytes, mtime, present, added_at, part_ordinal)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
				fileID, ed.ID, f.Path, f.Container, f.VideoCodec, f.AudioCodec,
				f.Width, f.Height, f.Bitrate, f.DurationMs, f.SizeBytes, f.Mtime, addedAt,
				f.PartOrdinal,
			); err != nil {
				return fmt.Errorf("store: re-inserting file %q: %w", f.Path, err)
			}
			for _, s := range f.Streams {
				if _, err := tx.Exec(
					`INSERT INTO streams
					   (id, file_id, stream_index, kind, codec, language, width, height, channels, is_default, forced, title, commentary, hearing_impaired)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					s.ID, fileID, s.Index, s.Kind, s.Codec, s.Language, s.Width, s.Height,
					s.Channels, boolToInt(s.IsDefault), boolToInt(s.Forced),
					s.Title, boolToInt(s.Commentary), boolToInt(s.HearingImpaired),
				); err != nil {
					return fmt.Errorf("store: inserting stream %d of %q: %w", s.Index, f.Path, err)
				}
			}
		}
	}

	for _, ex := range tree.Extras {
		if _, err := tx.Exec(
			`INSERT INTO extras (id, title_id, extra_type, path, container, duration_ms, size_bytes)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ex.ID, titleID, ex.Type, ex.Path, ex.Container, ex.DurationMs, ex.SizeBytes,
		); err != nil {
			return fmt.Errorf("store: inserting extra %q: %w", ex.Path, err)
		}
	}

	for _, art := range tree.Artwork {
		if _, err := tx.Exec(
			`INSERT INTO artwork (id, title_id, role, path, source) VALUES (?, ?, ?, ?, 'local')`,
			art.ID, titleID, art.Role, art.Path,
		); err != nil {
			return fmt.Errorf("store: inserting artwork %q: %w", art.Path, err)
		}
	}

	// Sidecar Subtitle tracks. The scanner only ever produces local (sidecar)
	// rows here; source is hard-coded so a stray value can't slip past the
	// local/fetched split the DELETE above relies on.
	for _, sub := range tree.Subtitles {
		if _, err := tx.Exec(
			`INSERT INTO subtitles
			   (id, title_id, source, kind, language, forced, is_default, codec, path)
			 VALUES (?, ?, 'sidecar', ?, ?, ?, ?, ?, ?)`,
			sub.ID, titleID, sub.Kind, sub.Language, boolToInt(sub.Forced),
			boolToInt(sub.IsDefault), sub.Codec, sub.Path,
		); err != nil {
			return fmt.Errorf("store: inserting subtitle %q: %w", sub.Path, err)
		}
	}
	return nil
}

// ReplaceUnmatched replaces the Unmatched-files list for a Library with the
// given set, in one transaction. A rescan recomputes the whole set, so the old
// rows are cleared first (the list reflects the current on-disk reality).
func (db *DB) ReplaceUnmatched(libraryID string, files []UnmatchedFile) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replace unmatched: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM unmatched_files WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("store: clearing unmatched: %w", err)
	}
	for _, f := range files {
		if _, err := tx.Exec(
			`INSERT INTO unmatched_files (id, library_id, path, reason, kind) VALUES (?, ?, ?, ?, ?)`,
			f.ID, libraryID, f.Path, f.Reason, f.KindOrDefault(),
		); err != nil {
			return fmt.Errorf("store: inserting unmatched %q: %w", f.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit replace unmatched: %w", err)
	}
	return nil
}

// The two ways a recognized media File can fail to become a Title. They share a
// list because they share a consequence — the file is on disk and in no Title —
// but they are different problems with different fixes, and only one of them is
// about identity at all.
const (
	// UnmatchedUnidentified: nothing in the file's name yielded an identity. The
	// Admin says what the work is, and it resolves (CONTEXT.md "Unmatched").
	UnmatchedUnidentified = "unidentified"
	// UnmatchedUnreadable: the name parsed; the BYTES did not. ffprobe could not
	// read the file — truncated, corrupt, not really the container it claims. No
	// identity correction can fix it, so nothing above this layer may offer one
	// (CONTEXT.md "Unreadable").
	UnmatchedUnreadable = "unreadable"
)

// UnmatchedFile is a recognized-media File that produced no Title, listed for the
// Admin (CONTEXT.md "Unmatched", "Unreadable").
type UnmatchedFile struct {
	ID   string
	Path string
	// Kind is why: UnmatchedUnidentified or UnmatchedUnreadable. Empty reads as
	// unidentified, which is what every row written before the distinction existed
	// meant.
	Kind    string
	Reason  string
	AddedAt string
}

// KindOrDefault is Kind with the empty value resolved to unidentified, so a caller
// that predates the distinction (or a test that does not care) still writes a
// meaningful row.
func (u UnmatchedFile) KindOrDefault() string {
	if u.Kind == "" {
		return UnmatchedUnidentified
	}
	return u.Kind
}

// Unreadable reports whether this row is the ffprobe failure rather than the
// naming one — the question every layer above asks, so none of them has to know
// how it is spelled.
func (u UnmatchedFile) Unreadable() bool { return u.Kind == UnmatchedUnreadable }

// ListUnmatched returns the Library's Unmatched files, ordered by path.
func (db *DB) ListUnmatched(libraryID string) ([]UnmatchedFile, error) {
	rows, err := db.Query(
		`SELECT id, path, reason, kind, added_at FROM unmatched_files
		   WHERE library_id = ? ORDER BY path`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing unmatched: %w", err)
	}
	defer rows.Close()

	var out []UnmatchedFile
	for rows.Next() {
		var u UnmatchedFile
		if err := rows.Scan(&u.ID, &u.Path, &u.Reason, &u.Kind, &u.AddedAt); err != nil {
			return nil, fmt.Errorf("store: scanning unmatched: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ArtworkByTitleRole returns the artwork path for a Title+role, or ErrNotFound.
// The API artwork-serving endpoint uses it to locate the local image bytes.
func (db *DB) ArtworkByTitleRole(titleID, role string) (Artwork, error) {
	var a Artwork
	// Serve precedence uploaded > local > fetched (ADR-0026): an Admin-uploaded
	// image outranks everything (including a library-folder poster.jpg), then
	// local folder art beats an in-app fetch/pick. Order accordingly and take a
	// single row, so a lower source is served only when no higher one exists.
	err := db.QueryRow(
		`SELECT id, title_id, role, path, source FROM artwork
		   WHERE title_id = ? AND role = ?
		   ORDER BY CASE source WHEN 'uploaded' THEN 0 WHEN 'local' THEN 1 ELSE 2 END LIMIT 1`,
		titleID, role,
	).Scan(&a.ID, &a.TitleID, &a.Role, &a.Path, &a.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return Artwork{}, ErrNotFound
	}
	if err != nil {
		return Artwork{}, fmt.Errorf("store: reading artwork: %w", err)
	}
	return a, nil
}

// TitleSort selects the stable ordering for a paginated title listing.
type TitleSort int

const (
	// SortByTitle orders by sort_title then id (case-insensitive A→Z).
	SortByTitle TitleSort = iota
	// SortByDateAdded orders by added_at descending then id (newest first).
	SortByDateAdded
)

// TitlePage is one page of a cursor-paginated title listing plus the cursor
// fields the api layer encodes into nextCursor.
type TitlePage struct {
	Titles []Title
	// HasMore is true when more rows exist beyond this page.
	HasMore bool
}

// TitleCursor is the decoded position within a sorted listing: the sort key of
// the last row returned plus its id, used as a stable "seek" predicate (no
// OFFSET). SortKey holds sort_title for SortByTitle and added_at for
// SortByDateAdded.
type TitleCursor struct {
	SortKey string
	ID      string
}

// ListTitles returns one page of Titles in the given Library, ordered by sort,
// starting strictly after cursor (nil for the first page), at most limit rows.
// It fetches limit+1 to detect HasMore without a separate count. The seek
// predicate uses the composite (sortKey, id) so pagination is stable as the
// catalog mutates between pages.
func (db *DB) ListTitles(libraryID string, sort TitleSort, cursor *TitleCursor, limit int, genre string, filter AccessFilter) (TitlePage, error) {
	if limit <= 0 {
		limit = 20
	}

	var (
		orderBy string
		keyCol  string
	)
	switch sort {
	case SortByDateAdded:
		// Newest first: added_at DESC, id DESC. The seek predicate flips to "<".
		orderBy = "added_at DESC, id DESC"
		keyCol = "added_at"
	default:
		orderBy = "sort_title ASC, id ASC"
		keyCol = "sort_title"
	}

	args := []any{libraryID}
	// hidden = 0 excludes all-Files-Missing Titles from browse (soft-delete,
	// ADR-0008); they remain fetchable by id via TitleByID so state recovers.
	where := "library_id = ? AND hidden = 0"
	// filter[genre]: keep only Titles carrying that enriched genre (external-
	// metadata-enrichment). An un-enriched Title has no genres, so it is excluded.
	if genre != "" {
		where += " AND EXISTS (SELECT 1 FROM title_genres g WHERE g.title_id = titles.id AND g.genre = ?)"
		args = append(args, genre)
	}
	if cursor != nil {
		// Row-value comparison against the (key, id) tuple gives a clean strict
		// seek past the cursor. Direction matches the ORDER BY above.
		if sort == SortByDateAdded {
			where += fmt.Sprintf(" AND (%s, id) < (?, ?)", keyCol)
		} else {
			where += fmt.Sprintf(" AND (%s, id) > (?, ?)", keyCol)
		}
		args = append(args, cursor.SortKey, cursor.ID)
	}
	// Rating ceiling (access-control 04): hide a Title whose content_rating is
	// above the caller's ceiling. The Library dimension is enforced by the service
	// guard (the library is fixed by the path). Empty under all-access.
	rateClause, rateArgs := filter.titleRatingClause("content_rating")
	where += rateClause
	args = append(args, rateArgs...)

	query := fmt.Sprintf(
		`SELECT `+enrichedTitleColumns+`
		   FROM titles WHERE %s ORDER BY %s LIMIT ?`, where, orderBy)
	args = append(args, limit+1)

	rows, err := db.Query(query, args...)
	if err != nil {
		return TitlePage{}, fmt.Errorf("store: listing titles: %w", err)
	}
	defer rows.Close()

	var out []Title
	for rows.Next() {
		t, err := scanEnrichedTitle(rows)
		if err != nil {
			return TitlePage{}, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return TitlePage{}, err
	}

	page := TitlePage{}
	if len(out) > limit {
		page.HasMore = true
		out = out[:limit]
	}
	page.Titles = out
	return page, nil
}

// LibraryExists reports whether a Library with the given id exists, so the
// browse layer can return 404 for an unknown Library without loading it whole.
func (db *DB) LibraryExists(id string) (bool, error) {
	var one int
	err := db.QueryRow(`SELECT 1 FROM libraries WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: checking library existence: %w", err)
	}
	return true, nil
}

// TitleByID returns one Title with its full nested catalog (Editions → Files →
// Streams), or ErrNotFound.
func (db *DB) TitleByID(id string) (TitleDetail, error) {
	var d TitleDetail
	row := db.QueryRow(
		`SELECT `+enrichedTitleColumns+` FROM titles WHERE id = ?`, id)
	t, err := scanEnrichedTitle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return TitleDetail{}, ErrNotFound
	}
	if err != nil {
		return TitleDetail{}, fmt.Errorf("store: scanning title: %w", err)
	}
	d.Title = t

	editions, err := db.editionsForTitle(id)
	if err != nil {
		return TitleDetail{}, err
	}
	d.Editions = editions

	if d.Extras, err = db.extrasForTitle(id); err != nil {
		return TitleDetail{}, err
	}
	if d.Artwork, err = db.artworkForTitle(id); err != nil {
		return TitleDetail{}, err
	}
	if d.Subtitles, err = db.subtitlesForTitle(id); err != nil {
		return TitleDetail{}, err
	}
	if d.Title.Genres, err = db.genresForTitle(id); err != nil {
		return TitleDetail{}, err
	}
	if d.Cast, err = db.creditsForTitle(id); err != nil {
		return TitleDetail{}, err
	}
	if d.LockedFields, err = db.LockedFieldsSorted(id); err != nil {
		return TitleDetail{}, err
	}
	return d, nil
}

func (db *DB) extrasForTitle(titleID string) ([]Extra, error) {
	rows, err := db.Query(
		`SELECT id, title_id, extra_type, path, container, duration_ms, size_bytes
		   FROM extras WHERE title_id = ? ORDER BY path`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing extras: %w", err)
	}
	defer rows.Close()

	var out []Extra
	for rows.Next() {
		var e Extra
		if err := rows.Scan(&e.ID, &e.TitleID, &e.Type, &e.Path, &e.Container,
			&e.DurationMs, &e.SizeBytes); err != nil {
			return nil, fmt.Errorf("store: scanning extra: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (db *DB) artworkForTitle(titleID string) ([]Artwork, error) {
	// Order uploaded > local > fetched within each role so the dedup below keeps
	// the winning row — the detail then lists ONE artwork entry per role, matching
	// what the serving endpoint resolves (ArtworkByTitleRole, ADR-0026).
	rows, err := db.Query(
		`SELECT id, title_id, role, path, source, added_at FROM artwork
		   WHERE title_id = ?
		   ORDER BY role, CASE source WHEN 'uploaded' THEN 0 WHEN 'local' THEN 1 ELSE 2 END`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing artwork: %w", err)
	}
	defer rows.Close()

	var out []Artwork
	seen := map[string]bool{}
	for rows.Next() {
		var a Artwork
		if err := rows.Scan(&a.ID, &a.TitleID, &a.Role, &a.Path, &a.Source, &a.AddedAt); err != nil {
			return nil, fmt.Errorf("store: scanning artwork: %w", err)
		}
		if seen[a.Role] {
			continue // a higher-priority (local) row for this role already won
		}
		seen[a.Role] = true
		out = append(out, a)
	}
	return out, rows.Err()
}

// subtitlesForTitle lists a Title's Sidecar/Fetched Subtitle tracks, local
// (sidecar) before fetched so the caller can prefer a local source. Embedded
// tracks are NOT here — they are derived from the Files' subtitle Streams.
func (db *DB) subtitlesForTitle(titleID string) ([]Subtitle, error) {
	rows, err := db.Query(
		`SELECT id, title_id, source, kind, language, forced, is_default, codec, path, provider_id
		   FROM subtitles WHERE title_id = ?
		   ORDER BY CASE source WHEN 'sidecar' THEN 0 ELSE 1 END, language, id`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing subtitles: %w", err)
	}
	defer rows.Close()

	var out []Subtitle
	for rows.Next() {
		var s Subtitle
		var forced, isDefault int
		if err := rows.Scan(&s.ID, &s.TitleID, &s.Source, &s.Kind, &s.Language,
			&forced, &isDefault, &s.Codec, &s.Path, &s.ProviderID); err != nil {
			return nil, fmt.Errorf("store: scanning subtitle: %w", err)
		}
		s.Forced = forced != 0
		s.IsDefault = isDefault != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) editionsForTitle(titleID string) ([]Edition, error) {
	rows, err := db.Query(
		`SELECT id, title_id, name, added_at FROM editions
		   WHERE title_id = ? ORDER BY added_at, id`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing editions: %w", err)
	}
	defer rows.Close()

	var editions []Edition
	for rows.Next() {
		var e Edition
		if err := rows.Scan(&e.ID, &e.TitleID, &e.Name, &e.AddedAt); err != nil {
			return nil, fmt.Errorf("store: scanning edition: %w", err)
		}
		editions = append(editions, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range editions {
		files, err := db.filesForEdition(editions[i].ID)
		if err != nil {
			return nil, err
		}
		editions[i].Files = files
	}
	return editions, nil
}

// FileByID loads one File (with its Streams) by id, or ErrNotFound. It is the
// lookup behind the sessionless direct-file download route — the file is
// addressed by its stable id rather than through a Playback session.
func (db *DB) FileByID(id string) (File, error) {
	var f File
	var present int
	row := db.QueryRow(
		`SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
		        bitrate, duration_ms, size_bytes, added_at, mtime, present, part_ordinal
		   FROM files WHERE id = ?`, id)
	if err := row.Scan(&f.ID, &f.EditionID, &f.Path, &f.Container, &f.VideoCodec,
		&f.AudioCodec, &f.Width, &f.Height, &f.Bitrate, &f.DurationMs, &f.SizeBytes,
		&f.AddedAt, &f.Mtime, &present, &f.PartOrdinal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return File{}, ErrNotFound
		}
		return File{}, fmt.Errorf("store: loading file by id: %w", err)
	}
	f.Present = present != 0
	streams, err := db.streamsForFile(f.ID)
	if err != nil {
		return File{}, err
	}
	f.Streams = streams
	return f, nil
}

// LibraryAndRatingOfFile resolves the two access dimensions of the Title that
// OWNS a File — the Library it lives in and its Content rating — or ErrNotFound.
// It is the LibraryOfTitle of the direct-file download: that route addresses a
// File by its own id and never loads a Title, so this is the only place the
// caller's Scope can be applied to it. Both dimensions come back together
// because the download must check both (a Library grant does not exempt a
// Member from their Rating ceiling), and one row read is cheaper than two.
//
// Both joins are INNER on purpose, and that is the fail-closed property, not an
// optimization. A File whose edition or title row is gone (a torn write, a
// half-finished prune, a hand-edited DB) matches NO row and comes back
// ErrNotFound — refused, never served. LEFT JOINs would "succeed" with an empty
// library_id and an empty content_rating, which AllowsRating reads as unrated
// (visible) and AllowsLibrary reads as allowed under any all-access Scope: an
// orphaned File would become downloadable by everyone. Do not loosen these.
func (db *DB) LibraryAndRatingOfFile(fileID string) (libraryID, contentRating string, err error) {
	row := db.QueryRow(
		`SELECT t.library_id, t.content_rating
		   FROM files f
		   JOIN editions e ON e.id = f.edition_id
		   JOIN titles t ON t.id = e.title_id
		  WHERE f.id = ?`, fileID)
	if err := row.Scan(&libraryID, &contentRating); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("store: resolving library and rating of file: %w", err)
	}
	return libraryID, contentRating, nil
}

func (db *DB) filesForEdition(editionID string) ([]File, error) {
	// (part_ordinal, path), not path alone: Edition.Files is a PLAY order, and once
	// an Admin can assemble a multi-part Edition by Placement the filenames no
	// longer carry the part order (ADR-0044, migration 0049). part_ordinal is 0 on
	// every File nothing numbered, so a filename-derived Edition still falls
	// through to path exactly as before.
	rows, err := db.Query(
		`SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
		        bitrate, duration_ms, size_bytes, added_at, mtime, present, part_ordinal
		   FROM files WHERE edition_id = ? ORDER BY part_ordinal, path`, editionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing files: %w", err)
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var f File
		var present int
		if err := rows.Scan(&f.ID, &f.EditionID, &f.Path, &f.Container, &f.VideoCodec,
			&f.AudioCodec, &f.Width, &f.Height, &f.Bitrate, &f.DurationMs, &f.SizeBytes,
			&f.AddedAt, &f.Mtime, &present, &f.PartOrdinal); err != nil {
			return nil, fmt.Errorf("store: scanning file: %w", err)
		}
		f.Present = present != 0
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range files {
		streams, err := db.streamsForFile(files[i].ID)
		if err != nil {
			return nil, err
		}
		files[i].Streams = streams
	}
	return files, nil
}

func (db *DB) streamsForFile(fileID string) ([]Stream, error) {
	rows, err := db.Query(
		`SELECT id, file_id, stream_index, kind, codec, language, width, height, channels, is_default, forced, title, commentary, hearing_impaired
		   FROM streams WHERE file_id = ? ORDER BY stream_index`, fileID)
	if err != nil {
		return nil, fmt.Errorf("store: listing streams: %w", err)
	}
	defer rows.Close()

	var streams []Stream
	for rows.Next() {
		var s Stream
		var isDefault, forced, commentary, hearingImpaired int
		if err := rows.Scan(&s.ID, &s.FileID, &s.Index, &s.Kind, &s.Codec, &s.Language,
			&s.Width, &s.Height, &s.Channels, &isDefault, &forced,
			&s.Title, &commentary, &hearingImpaired); err != nil {
			return nil, fmt.Errorf("store: scanning stream: %w", err)
		}
		s.IsDefault = isDefault != 0
		s.Forced = forced != 0
		s.Commentary = commentary != 0
		s.HearingImpaired = hearingImpaired != 0
		streams = append(streams, s)
	}
	return streams, rows.Err()
}

// scanner is the minimal interface QueryRow and Rows both satisfy for Scan.
type scanner interface {
	Scan(dest ...any) error
}

func scanTitle(s scanner) (Title, error) {
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
		&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden); err != nil {
		return Title{}, err
	}
	if year.Valid {
		t.Year = int(year.Int64)
	}
	t.NeedsReview = needsReview != 0
	t.Ambiguous = ambiguous != 0
	t.Hidden = hidden != 0
	return t, nil
}

// enrichedTitleColumns is the SELECT list for the browse read paths (ListTitles
// + TitleByID) that carry the optional Enrichment fields. The base 13 columns
// match scanTitle's order (reused by search/home/incremental, which don't need
// the enrichment fields); the enrichment columns are appended so a single
// scanEnrichedTitle populates them. Keep this in lockstep with scanEnrichedTitle.
//
// season_number / episode_number / episode_label are here because they were MISSING,
// and their absence was a live bug rather than a tidiness issue: every single-Title
// re-enrich reads through TitleForEnrichmentByID, so an Episode corrected by hand
// (enrichmentMatch, enrichmentOverride) was looked up as season 0, episode 0 — a
// guaranteed 404 — while a full-library pass, which collects its own leaves with the
// numbers attached, resolved the same Episode correctly. Any read that builds a
// Title for a lookup must carry the fields the lookup is keyed on.
var enrichedTitleColumns = `id, library_id, kind, title, year, identity_key, sort_title, added_at,
	        ` + recordExternalIDs("") + `, needs_review, ambiguous, hidden,
	        overview, tagline, content_rating, release_date, runtime_minutes, studio,
	        musicbrainz_id, enrichment_status, enriched_at, enrichment_source, enriched_title,
	        enrichment_season, enrichment_episode, enrichment_id_origin,
	        enrichment_attempts, enrichment_retry_at,
	        season_number, episode_number, episode_label`

// recordExternalIDs is the ONE spelling of "which external record does this Title
// resolve against", as a pair of SELECT expressions in the order (tmdb, imdb) that
// every read scans into Title.TMDBID / Title.IMDBID.
//
// The Admin's Enrichment override wins over the id the folder name asserts
// (ADR-0045); with no override the two columns hold the same value or the
// enrichment one is empty, so the COALESCE is the whole rule. alias is the table
// alias with its dot ("t.") or "" for an unaliased `FROM titles`.
//
// Selecting the raw `tmdb_id, imdb_id` into a Title instead is a silent bug: it
// yields the folder's id and drops the correction an Admin made on top of it.
// The expressions are named back to `tmdb_id` / `imdb_id` so a CTE that projects
// them keeps the column names its outer SELECT already uses.
func recordExternalIDs(alias string) string {
	return recordTMDBID(alias) + " AS tmdb_id, " +
		"COALESCE(NULLIF(" + alias + "enrichment_imdb_id, ''), " + alias + "imdb_id) AS imdb_id"
}

// recordTMDBID is recordExternalIDs' first half alone, for the reads that only
// need the series/record id (ShowEpisodeSlots).
func recordTMDBID(alias string) string {
	return "COALESCE(NULLIF(" + alias + "enrichment_tmdb_id, ''), " + alias + "tmdb_id)"
}

// EpisodePin reports the provider season/episode this Episode is pinned to, and
// whether it is pinned at all.
//
// The test is `EnrichmentEpisode > 0`, NOT the NoEpisodePin sentinel, and
// deliberately so: only the enriched projection reads these columns, so a Title
// built by any leaner read (scanTitle, and every struct literal in tests) carries
// a zero value. Testing against the sentinel would make every one of those look
// like "pinned to season 0, episode 0" and silently redirect its lookup. Episode
// numbers start at 1 in every provider, so a pin is only ever real above zero —
// while season 0 stays valid, because it is Specials.
func (t Title) EpisodePin() (season, episode int, ok bool) {
	if t.EnrichmentEpisode <= 0 {
		return 0, 0, false
	}
	return t.EnrichmentSeason, t.EnrichmentEpisode, true
}

// NoEpisodePin marks an Episode with no pinned provider season/episode — the
// default, meaning "look the episode up by the numbers parsed from its filename".
// It is -1 rather than 0 because season 0 is the real Specials season.
const NoEpisodePin = -1

// scanEnrichedTitle scans a row selected with enrichedTitleColumns: the base
// Title plus its descriptive Enrichment fields. Genres/Cast are loaded
// separately (multi-valued); a list scan leaves Genres nil.
func scanEnrichedTitle(s scanner) (Title, error) {
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	var idOrigin string
	var pinSeason, pinEpisode sql.NullInt64
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
		&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden,
		&t.Overview, &t.Tagline, &t.ContentRating, &t.ReleaseDate, &t.RuntimeMinutes, &t.Studio,
		&t.MusicbrainzID, &t.EnrichmentStatus, &t.EnrichedAt, &t.EnrichmentSource, &t.EnrichedTitle,
		&pinSeason, &pinEpisode, &idOrigin,
		&t.EnrichmentAttempts, &t.EnrichmentRetryAt,
		&t.SeasonNumber, &t.EpisodeNumber, &t.EpisodeLabel); err != nil {
		return Title{}, err
	}
	t.EnrichmentIDOrigin = RecordOrigin(idOrigin)
	if year.Valid {
		t.Year = int(year.Int64)
	}
	// NULL means "not pinned"; -1 is the in-memory sentinel because season 0 is a
	// real value (Specials) and so cannot mean "unset".
	t.EnrichmentSeason, t.EnrichmentEpisode = NoEpisodePin, NoEpisodePin
	if pinSeason.Valid && pinEpisode.Valid {
		t.EnrichmentSeason, t.EnrichmentEpisode = int(pinSeason.Int64), int(pinEpisode.Int64)
	}
	t.NeedsReview = needsReview != 0
	t.Ambiguous = ambiguous != 0
	t.Hidden = hidden != 0
	return t, nil
}

// genresForTitle returns a Title's enriched genres in billing order (empty when
// un-enriched).
func (db *DB) genresForTitle(titleID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT genre FROM title_genres WHERE title_id = ? ORDER BY ord, genre`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing genres: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("store: scanning genre: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// creditsForTitle returns a Title's enriched cast/crew in billing order. Each
// credit's person_ref is joined to the person's fetched headshot in
// entity_artwork (role='profile'); the join's added_at surfaces as PhotoVersion
// (a cache-bust token, empty when the person has no cached photo), so the detail
// JSON can point a client at a headshot that busts when a re-enrich swaps it.
func (db *DB) creditsForTitle(titleID string) ([]Credit, error) {
	rows, err := db.Query(
		`SELECT tc.person, tc.role, tc.character, tc.kind, tc.person_ref,
		        COALESCE(pa.added_at, '')
		   FROM title_credits tc
		   LEFT JOIN entity_artwork pa
		     ON pa.entity_type = 'person' AND pa.entity_id = tc.person_ref
		        AND pa.role = 'profile' AND tc.person_ref <> ''
		   WHERE tc.title_id = ? ORDER BY tc.kind, tc.ord, tc.person`, titleID)
	if err != nil {
		return nil, fmt.Errorf("store: listing credits: %w", err)
	}
	defer rows.Close()
	var out []Credit
	for rows.Next() {
		var c Credit
		if err := rows.Scan(&c.Person, &c.Role, &c.Character, &c.Kind, &c.PersonRef, &c.PhotoVersion); err != nil {
			return nil, fmt.Errorf("store: scanning credit: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nullableYear(year int) any {
	if year == 0 {
		return nil
	}
	return year
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
