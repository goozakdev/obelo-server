package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// This file holds the persistence behind the incremental rescan + soft-delete
// model (issue 06, ADR-0008) and the fix-match Match override (ADR-0002/0014):
//
//   * FileSnapshot / ListFileSnapshots — the cheap (path → mtime, size, present)
//     view the scanner reads up front to decide which files changed, so an
//     unchanged file is never re-ffprobed.
//   * MarkFilesMissing — soft-delete: files not seen on the latest walk are set
//     present=0 rather than deleted (an unmounted drive must not wipe history).
//   * RecomputeHidden* — derive each Title's all-Files-Missing hidden state.
//   * MatchOverride CRUD + orphan marking — an Admin identity correction keyed
//     to the folder path that persists across rescans (the scanner applies it).

// FileSnapshot is the lightweight per-File state an incremental scan compares
// against the current on-disk file to decide new / changed / unchanged. mtime is
// RFC3339 UTC; SizeBytes mirrors the stat size. Present is the current soft-
// delete state. The full ffprobe detail is loaded only when a file changed.
type FileSnapshot struct {
	Path        string
	Mtime       string
	SizeBytes   int64
	Present     bool
	IdentityKey string // the owning Title's identity_key, so a no-op tree can be skipped
}

// ListFileSnapshots returns the change-detection view of every File in a
// Library (across all its Titles), keyed by path. The scanner reads this once at
// the start of a scan to skip re-probing unchanged files.
func (db *DB) ListFileSnapshots(libraryID string) (map[string]FileSnapshot, error) {
	rows, err := db.Query(
		`SELECT f.path, f.mtime, f.size_bytes, f.present, t.identity_key
		   FROM files f
		   JOIN editions e ON f.edition_id = e.id
		   JOIN titles   t ON e.title_id   = t.id
		  WHERE t.library_id = ?`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing file snapshots: %w", err)
	}
	defer rows.Close()

	out := map[string]FileSnapshot{}
	for rows.Next() {
		var s FileSnapshot
		var present int
		if err := rows.Scan(&s.Path, &s.Mtime, &s.SizeBytes, &present, &s.IdentityKey); err != nil {
			return nil, fmt.Errorf("store: scanning file snapshot: %w", err)
		}
		s.Present = present != 0
		out[s.Path] = s
	}
	return out, rows.Err()
}

// LoadStoredFile returns the full stored File (with its Streams) for a path, so
// an incremental scan can reuse the prior ffprobe result for an unchanged file
// instead of re-probing. Returns ErrNotFound when no File has that path.
func (db *DB) LoadStoredFile(path string) (File, error) {
	var f File
	var present int
	row := db.QueryRow(
		`SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
		        bitrate, duration_ms, size_bytes, added_at, mtime, present
		   FROM files WHERE path = ?`, path)
	if err := row.Scan(&f.ID, &f.EditionID, &f.Path, &f.Container, &f.VideoCodec,
		&f.AudioCodec, &f.Width, &f.Height, &f.Bitrate, &f.DurationMs, &f.SizeBytes,
		&f.AddedAt, &f.Mtime, &present); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return File{}, ErrNotFound
		}
		return File{}, fmt.Errorf("store: loading stored file: %w", err)
	}
	f.Present = present != 0
	streams, err := db.streamsForFile(f.ID)
	if err != nil {
		return File{}, err
	}
	f.Streams = streams
	return f, nil
}

// underAny reports whether path lies within one of the given directory prefixes
// (an exact match, or a descendant path/prefix + separator). Used to spare files
// beneath an unreadable subtree from the soft-delete pass.
func underAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// MarkFilesMissing flips present=0 on every File of the Library whose path is
// NOT in seenPaths — they were absent from the latest walk (soft-delete,
// ADR-0008). A File already present that returns is restored to present=1 by
// UpsertTitleTree, so this only ever marks, never restores. Returns the number
// of Files newly marked Missing.
//
// unresolvedDirs are subtrees the walk could not read this pass (a transient
// network-FS failure that outlasted retries). A File beneath one is left present:
// an unreadable subtree is not evidence of deletion, so pruning it would wrongly
// hide real content on a spurious blip — the subtree analogue of the
// unreachable-root guard (ADR-0008).
func (db *DB) MarkFilesMissing(libraryID string, seenPaths map[string]bool, unresolvedDirs []string) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin mark missing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(
		`SELECT f.id, f.path FROM files f
		   JOIN editions e ON f.edition_id = e.id
		   JOIN titles   t ON e.title_id   = t.id
		  WHERE t.library_id = ? AND f.present = 1`, libraryID)
	if err != nil {
		return 0, fmt.Errorf("store: scanning present files: %w", err)
	}
	var toMiss []string
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: scanning present file: %w", err)
		}
		if !seenPaths[path] && !underAny(path, unresolvedDirs) {
			toMiss = append(toMiss, id)
		}
	}
	_ = rows.Close()

	for _, id := range toMiss {
		if _, err := tx.Exec(`UPDATE files SET present = 0 WHERE id = ?`, id); err != nil {
			return 0, fmt.Errorf("store: marking file missing: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit mark missing: %w", err)
	}
	return len(toMiss), nil
}

// RecomputeHiddenTitles sets each Title's hidden flag to reflect whether all its
// Files are Missing. A Title with at least one present File is visible (hidden=0);
// a Title whose every File is Missing — or that has no Files at all — is hidden
// (hidden=1) so it drops out of browse but stays fetchable by id (ADR-0008).
// Run after MarkFilesMissing + the upsert pass so the file present-states are final.
func (db *DB) RecomputeHiddenTitles(libraryID string) error {
	// A Title is hidden when it has zero present Files. The correlated subquery
	// counts present files per title; 0 ⇒ hidden.
	_, err := db.Exec(
		`UPDATE titles SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM editions e JOIN files f ON f.edition_id = e.id
		             WHERE e.title_id = titles.id AND f.present = 1) > 0
		     THEN 0 ELSE 1 END
		   WHERE library_id = ?`, libraryID)
	if err != nil {
		return fmt.Errorf("store: recomputing hidden titles: %w", err)
	}
	return nil
}

// --- Match overrides --------------------------------------------------------

// MatchOverride is an Admin identity correction keyed to the on-disk folder path
// (ADR-0002, ADR-0014). It overrules the convention-derived parse and persists
// across rescans; Orphaned is true once a scan finds no folder at FolderPath.
type MatchOverride struct {
	ID          string
	LibraryID   string
	FolderPath  string
	Title       string
	Year        int
	TMDBID      string
	IMDBID      string
	IdentityKey string
	Orphaned    bool
	CreatedAt   string
}

// UpsertMatchOverride records (or replaces) the override for a (library, folder).
// Re-asserting the same folder updates the corrected identity in place. A fresh
// override is never orphaned. The caller supplies the corrected identity already
// resolved to its identity_key (the scanner/library layer owns key derivation).
func (db *DB) UpsertMatchOverride(o MatchOverride) (MatchOverride, error) {
	if o.ID == "" {
		o.ID = uuid.NewString()
	}
	_, err := db.Exec(
		`INSERT INTO match_overrides
		   (id, library_id, folder_path, title, year, tmdb_id, imdb_id, identity_key, orphaned)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(library_id, folder_path) DO UPDATE SET
		   title = excluded.title, year = excluded.year,
		   tmdb_id = excluded.tmdb_id, imdb_id = excluded.imdb_id,
		   identity_key = excluded.identity_key, orphaned = 0`,
		o.ID, o.LibraryID, o.FolderPath, o.Title, nullableYear(o.Year),
		o.TMDBID, o.IMDBID, o.IdentityKey)
	if err != nil {
		return MatchOverride{}, fmt.Errorf("store: upserting match override: %w", err)
	}
	return db.matchOverrideByFolder(o.LibraryID, o.FolderPath)
}

func (db *DB) matchOverrideByFolder(libraryID, folderPath string) (MatchOverride, error) {
	row := db.QueryRow(
		`SELECT id, library_id, folder_path, title, year, tmdb_id, imdb_id, identity_key, orphaned, created_at
		   FROM match_overrides WHERE library_id = ? AND folder_path = ?`, libraryID, folderPath)
	return scanMatchOverride(row)
}

// MatchOverridesByLibrary returns every override for a Library, ordered by
// folder path. The scanner reads this to apply corrections; the Admin attention
// surface reads it to list orphans.
func (db *DB) MatchOverridesByLibrary(libraryID string) ([]MatchOverride, error) {
	rows, err := db.Query(
		`SELECT id, library_id, folder_path, title, year, tmdb_id, imdb_id, identity_key, orphaned, created_at
		   FROM match_overrides WHERE library_id = ? ORDER BY folder_path`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing match overrides: %w", err)
	}
	defer rows.Close()

	var out []MatchOverride
	for rows.Next() {
		o, err := scanMatchOverride(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetMatchOverrideOrphaned flags/unflags an override as orphaned (its folder no
// longer exists on disk). The scanner calls this for each override after the walk.
func (db *DB) SetMatchOverrideOrphaned(id string, orphaned bool) error {
	_, err := db.Exec(`UPDATE match_overrides SET orphaned = ? WHERE id = ?`,
		boolToInt(orphaned), id)
	if err != nil {
		return fmt.Errorf("store: setting override orphaned: %w", err)
	}
	return nil
}

func scanMatchOverride(s scanner) (MatchOverride, error) {
	var o MatchOverride
	var year sql.NullInt64
	var orphaned int
	if err := s.Scan(&o.ID, &o.LibraryID, &o.FolderPath, &o.Title, &year,
		&o.TMDBID, &o.IMDBID, &o.IdentityKey, &orphaned, &o.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MatchOverride{}, ErrNotFound
		}
		return MatchOverride{}, fmt.Errorf("store: scanning match override: %w", err)
	}
	if year.Valid {
		o.Year = int(year.Int64)
	}
	o.Orphaned = orphaned != 0
	return o, nil
}

// TitleByFolderPath finds the Title that currently owns files under folderPath
// (the longest path prefix match against a File's directory). Used by fix-match
// to locate the Title a folder-anchored override applies to. Returns ErrNotFound
// when no present File lives under that folder.
func (db *DB) TitleByFolderPath(libraryID, folderPath string) (Title, error) {
	prefix := strings.TrimRight(folderPath, "/") + "/"
	row := db.QueryRow(
		`SELECT t.id, t.library_id, t.kind, t.title, t.year, t.identity_key, t.sort_title,
		        t.added_at, t.tmdb_id, t.imdb_id, t.needs_review, t.ambiguous, t.hidden
		   FROM titles t
		   JOIN editions e ON e.title_id = t.id
		   JOIN files    f ON f.edition_id = e.id
		  WHERE t.library_id = ? AND (f.path = ? OR f.path LIKE ? ESCAPE '\')
		  LIMIT 1`,
		libraryID, folderPath, escapeLike(prefix)+"%")
	t, err := scanTitle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Title{}, ErrNotFound
	}
	if err != nil {
		return Title{}, fmt.Errorf("store: title by folder path: %w", err)
	}
	return t, nil
}

// escapeLike escapes LIKE metacharacters so a literal path prefix matches
// exactly (paths can contain % or _).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
