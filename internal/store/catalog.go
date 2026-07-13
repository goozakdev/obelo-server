package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
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
	// TMDBID / IMDBID are the embedded external ids parsed from the folder name
	// (empty when absent); recorded as identity authority + Enrichment hook.
	TMDBID string
	IMDBID string
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
	// EnrichmentStatus ∈ pending|matched|unmatched|failed|disabled (CONTEXT.md
	// "Enrichment"). EnrichedAt / EnrichmentSource record the last successful pass.
	EnrichmentStatus string
	EnrichedAt       string
	EnrichmentSource string
	// Genres is the enriched genre list (loaded by the enriched read paths; empty
	// otherwise). Cast lives on TitleDetail (heavier, detail-only).
	Genres []string
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
	Streams []Stream
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
	Editions []Edition
	Extras   []Extra
	Artwork  []Artwork
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
// (UpsertTitleTree) and the Episode path (upsertEpisodeTitle) so identity
// stability is identical for both kinds. Returns the resolved Title id.
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
			    tmdb_id, imdb_id, needs_review, ambiguous, hidden,
			    season_id, season_number, episode_number, episode_label,
			    album_id, disc_number, track_number)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?)`,
			titleID, tree.LibraryID, tree.Kind, tree.Title.Title, nullableYear(tree.Year),
			tree.IdentityKey, tree.SortTitle, tree.TMDBID, tree.IMDBID,
			boolToInt(tree.NeedsReview), boolToInt(tree.Ambiguous),
			seasonID, ep.seasonNumber, ep.episodeNumber, ep.episodeLabel,
			albumID, ep.discNumber, ep.trackNumber,
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
			    album_id = ?, disc_number = ?, track_number = ?
			  WHERE id = ?`,
			tree.Title.Title, nullableYear(tree.Year), tree.SortTitle,
			tree.TMDBID, tree.IMDBID, boolToInt(tree.NeedsReview), boolToInt(tree.Ambiguous),
			seasonID, ep.seasonNumber, ep.episodeNumber, ep.episodeLabel,
			albumID, ep.discNumber, ep.trackNumber,
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
					    bitrate, duration_ms, size_bytes, mtime, present)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
					fileID, ed.ID, f.Path, f.Container, f.VideoCodec, f.AudioCodec,
					f.Width, f.Height, f.Bitrate, f.DurationMs, f.SizeBytes, f.Mtime,
				); err != nil {
					return fmt.Errorf("store: inserting file %q: %w", f.Path, err)
				}
			} else if _, err := tx.Exec(
				`INSERT INTO files
				   (id, edition_id, path, container, video_codec, audio_codec, width, height,
				    bitrate, duration_ms, size_bytes, mtime, present, added_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				fileID, ed.ID, f.Path, f.Container, f.VideoCodec, f.AudioCodec,
				f.Width, f.Height, f.Bitrate, f.DurationMs, f.SizeBytes, f.Mtime, addedAt,
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
			`INSERT INTO unmatched_files (id, library_id, path, reason) VALUES (?, ?, ?, ?)`,
			f.ID, libraryID, f.Path, f.Reason,
		); err != nil {
			return fmt.Errorf("store: inserting unmatched %q: %w", f.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit replace unmatched: %w", err)
	}
	return nil
}

// UnmatchedFile is a recognized-media File with no extractable identity, listed
// for the Admin (CONTEXT.md "Unmatched").
type UnmatchedFile struct {
	ID      string
	Path    string
	Reason  string
	AddedAt string
}

// ListUnmatched returns the Library's Unmatched files, ordered by path.
func (db *DB) ListUnmatched(libraryID string) ([]UnmatchedFile, error) {
	rows, err := db.Query(
		`SELECT id, path, reason, added_at FROM unmatched_files
		   WHERE library_id = ? ORDER BY path`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing unmatched: %w", err)
	}
	defer rows.Close()

	var out []UnmatchedFile
	for rows.Next() {
		var u UnmatchedFile
		if err := rows.Scan(&u.ID, &u.Path, &u.Reason, &u.AddedAt); err != nil {
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
		        bitrate, duration_ms, size_bytes, added_at, mtime, present
		   FROM files WHERE id = ?`, id)
	if err := row.Scan(&f.ID, &f.EditionID, &f.Path, &f.Container, &f.VideoCodec,
		&f.AudioCodec, &f.Width, &f.Height, &f.Bitrate, &f.DurationMs, &f.SizeBytes,
		&f.AddedAt, &f.Mtime, &present); err != nil {
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

func (db *DB) filesForEdition(editionID string) ([]File, error) {
	rows, err := db.Query(
		`SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
		        bitrate, duration_ms, size_bytes, added_at, mtime, present
		   FROM files WHERE edition_id = ? ORDER BY path`, editionID)
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
			&f.AddedAt, &f.Mtime, &present); err != nil {
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
const enrichedTitleColumns = `id, library_id, kind, title, year, identity_key, sort_title, added_at,
	        tmdb_id, imdb_id, needs_review, ambiguous, hidden,
	        overview, tagline, content_rating, release_date, runtime_minutes, studio,
	        musicbrainz_id, enrichment_status, enriched_at, enrichment_source, enriched_title`

// scanEnrichedTitle scans a row selected with enrichedTitleColumns: the base
// Title plus its descriptive Enrichment fields. Genres/Cast are loaded
// separately (multi-valued); a list scan leaves Genres nil.
func scanEnrichedTitle(s scanner) (Title, error) {
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
		&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &needsReview, &ambiguous, &hidden,
		&t.Overview, &t.Tagline, &t.ContentRating, &t.ReleaseDate, &t.RuntimeMinutes, &t.Studio,
		&t.MusicbrainzID, &t.EnrichmentStatus, &t.EnrichedAt, &t.EnrichmentSource, &t.EnrichedTitle); err != nil {
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
