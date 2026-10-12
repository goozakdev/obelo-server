package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// Seek preview generation (ADR-0070) reads the eligible Files of Movie and TV
// Libraries and records, per File, the set it generated or why it could not.

// ErrNoSeekPreviews is returned for the generation toggle of a Library that has
// none: only a local Movie or TV Library has one (music has no picture, a Linked
// Library's previews are the origin's — ADR-0056 §5).
var ErrNoSeekPreviews = errors.New("store: library has no seek previews")

// Seek preview statuses.
const (
	SeekPreviewReady  = "ready"
	SeekPreviewFailed = "failed"
)

// SeekPreviewSet is the record of one File's tile sheets: what the manifest needs,
// and the File's mtime and size when it was made. For a failed File only
// Mtime, SizeBytes, Status and Reason are meaningful.
type SeekPreviewSet struct {
	FileID      string
	Mtime       string
	SizeBytes   int64
	Status      string
	Reason      string
	IntervalMs  int
	FrameWidth  int
	FrameHeight int
	Columns     int
	Rows        int
	FrameCount  int
	SheetCount  int
}

// SeekPreviewFile is one eligible File as generation sees it: a present, local
// video File of a Movie or TV Library, with its record, if any.
type SeekPreviewFile struct {
	FileID     string
	LibraryID  string
	Path       string
	Mtime      string
	SizeBytes  int64
	DurationMs int64
	AddedAt    string
	// Enabled is the Library's toggle.
	Enabled bool
	// Set is the File's record, nil when it was never tried.
	Set *SeekPreviewSet
}

// SeekPreviewsEnabled reports whether Seek preview generation runs for the
// Library: on unless an Admin turned it off, for a local Movie or TV Library, and
// ErrNoSeekPreviews for any other. An unknown Library is ErrNotFound.
func (db *DB) SeekPreviewsEnabled(libraryID string) (bool, error) {
	var kind, source string
	var enabled sql.NullInt64
	err := db.QueryRow(
		`SELECT l.kind, l.source, d.enabled
		   FROM libraries l LEFT JOIN library_seek_previews d ON d.library_id = l.id
		  WHERE l.id = ?`, libraryID).Scan(&kind, &source, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: reading seek previews of %q: %w", libraryID, err)
	}
	if kind == "music" || source != LibrarySourceLocal {
		return false, ErrNoSeekPreviews
	}
	return !enabled.Valid || enabled.Int64 != 0, nil
}

// SetSeekPreviewsEnabled turns generation on or off for a Movie or TV Library.
// Any other Library is ErrNoSeekPreviews, an unknown one ErrNotFound.
func (db *DB) SetSeekPreviewsEnabled(libraryID string, on bool) error {
	if _, err := db.SeekPreviewsEnabled(libraryID); err != nil {
		return err
	}
	if _, err := db.Exec(
		`INSERT INTO library_seek_previews (library_id, enabled) VALUES (?, ?)
		 ON CONFLICT (library_id) DO UPDATE SET enabled = excluded.enabled`,
		libraryID, boolToInt(on)); err != nil {
		return fmt.Errorf("store: saving seek previews of %q: %w", libraryID, err)
	}
	return nil
}

const seekPreviewFileSelect = `
	SELECT f.id, t.library_id, f.path, f.mtime, f.size_bytes, f.duration_ms, f.added_at,
	       COALESCE(d.enabled, 1),
	       p.file_id, COALESCE(p.mtime, ''), COALESCE(p.size_bytes, 0), COALESCE(p.status, ''),
	       COALESCE(p.reason, ''), COALESCE(p.interval_ms, 0), COALESCE(p.frame_width, 0),
	       COALESCE(p.frame_height, 0), COALESCE(p.grid_columns, 0), COALESCE(p.grid_rows, 0),
	       COALESCE(p.frame_count, 0), COALESCE(p.sheet_count, 0)
	  FROM files f
	  JOIN editions e  ON e.id = f.edition_id
	  JOIN titles t    ON t.id = e.title_id
	  JOIN libraries l ON l.id = t.library_id
	  LEFT JOIN library_seek_previews d ON d.library_id = l.id
	  LEFT JOIN seek_previews p ON p.file_id = f.id
	 WHERE l.kind IN ('movie', 'tv') AND l.source = 'local'
	   AND f.path <> '' AND f.present = 1 AND f.duration_ms > 0
	   AND EXISTS (SELECT 1 FROM streams s WHERE s.file_id = f.id AND s.kind = 'video')
	   AND `

func (db *DB) seekPreviewFiles(where string, args ...any) ([]SeekPreviewFile, error) {
	rows, err := db.Query(seekPreviewFileSelect+where+` ORDER BY f.added_at DESC, f.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing files for seek previews: %w", err)
	}
	defer rows.Close()
	var out []SeekPreviewFile
	for rows.Next() {
		var f SeekPreviewFile
		var enabled int
		var setID sql.NullString
		var s SeekPreviewSet
		if err := rows.Scan(&f.FileID, &f.LibraryID, &f.Path, &f.Mtime, &f.SizeBytes, &f.DurationMs, &f.AddedAt,
			&enabled, &setID, &s.Mtime, &s.SizeBytes, &s.Status, &s.Reason, &s.IntervalMs, &s.FrameWidth,
			&s.FrameHeight, &s.Columns, &s.Rows, &s.FrameCount, &s.SheetCount); err != nil {
			return nil, fmt.Errorf("store: scanning seek preview file: %w", err)
		}
		f.Enabled = enabled != 0
		if setID.Valid {
			s.FileID = setID.String
			f.Set = &s
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SeekPreviewFilesOfLibrary lists the eligible Files of a Library, most recently
// added first, whatever the Library's toggle.
func (db *DB) SeekPreviewFilesOfLibrary(libraryID string) ([]SeekPreviewFile, error) {
	return db.seekPreviewFiles(`t.library_id = ?`, libraryID)
}

// SeekPreviewFilesAll lists the eligible Files of every Library, most recently
// added first.
func (db *DB) SeekPreviewFilesAll() ([]SeekPreviewFile, error) {
	return db.seekPreviewFiles(`1 = 1`)
}

// SeekPreviewFilesOfTitle is SeekPreviewFilesOfLibrary for one Movie or Episode.
// An unknown Title is ErrNotFound.
func (db *DB) SeekPreviewFilesOfTitle(titleID string) ([]SeekPreviewFile, error) {
	if err := db.exists(`SELECT 1 FROM titles WHERE id = ?`, titleID); err != nil {
		return nil, err
	}
	return db.seekPreviewFiles(`t.id = ?`, titleID)
}

// SeekPreviewFilesOfShow is SeekPreviewFilesOfLibrary for every Episode of a Show.
// An unknown Show is ErrNotFound.
func (db *DB) SeekPreviewFilesOfShow(showID string) ([]SeekPreviewFile, error) {
	if err := db.exists(`SELECT 1 FROM shows WHERE id = ?`, showID); err != nil {
		return nil, err
	}
	return db.seekPreviewFiles(`t.season_id IN (SELECT id FROM seasons WHERE show_id = ?)`, showID)
}

// SeekPreviewFile returns one eligible File; a File that does not exist or is not
// eligible (Missing, music, linked) is ErrNotFound.
func (db *DB) SeekPreviewFile(fileID string) (SeekPreviewFile, error) {
	fs, err := db.seekPreviewFiles(`f.id = ?`, fileID)
	if err != nil {
		return SeekPreviewFile{}, err
	}
	if len(fs) == 0 {
		return SeekPreviewFile{}, ErrNotFound
	}
	return fs[0], nil
}

func (db *DB) exists(query string, arg string) error {
	var one int
	err := db.QueryRow(query, arg).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// SeekPreviewFailure is one eligible File whose generation failed, as the admin
// list shows it.
type SeekPreviewFailure struct {
	FileID  string
	TitleID string
	// Label is "Title (Year)" for a Movie and "Show — S01E02 · Episode title" for an
	// Episode.
	Label  string
	Path   string
	Reason string
}

// SeekPreviewCoverage is how much of a Library has Seek previews: its eligible
// Files, those with a fresh ready set (the record's mtime and size equal the File
// row's), and the failed ones ordered by label.
type SeekPreviewCoverage struct {
	Eligible int
	Ready    int
	Failed   []SeekPreviewFailure
}

// SeekPreviewCoverage counts a Library's eligible Files, whatever its toggle.
func (db *DB) SeekPreviewCoverage(libraryID string) (SeekPreviewCoverage, error) {
	files, err := db.SeekPreviewFilesOfLibrary(libraryID)
	if err != nil {
		return SeekPreviewCoverage{}, err
	}
	cov := SeekPreviewCoverage{Eligible: len(files)}
	for _, f := range files {
		switch {
		case f.Set == nil:
		case f.Set.Status == SeekPreviewReady:
			if f.Set.Mtime == f.Mtime && f.Set.SizeBytes == f.SizeBytes {
				cov.Ready++
			}
		case f.Set.Status == SeekPreviewFailed:
			var (
				titleID, kind, title, show string
				year                       sql.NullInt64
				season, episode            int
			)
			err := db.QueryRow(
				`SELECT t.id, t.kind, t.title, t.year, t.season_number, t.episode_number, COALESCE(sh.title, '')
				   FROM files f
				   JOIN editions e ON e.id = f.edition_id
				   JOIN titles t   ON t.id = e.title_id
				   LEFT JOIN seasons se ON se.id = t.season_id
				   LEFT JOIN shows sh   ON sh.id = se.show_id
				  WHERE f.id = ?`, f.FileID).Scan(&titleID, &kind, &title, &year, &season, &episode, &show)
			if err != nil {
				return SeekPreviewCoverage{}, fmt.Errorf("store: labelling seek preview of %q: %w", f.FileID, err)
			}
			label := title
			if kind == "episode" {
				label = fmt.Sprintf("%s — S%02dE%02d", show, season, episode)
				if title != "" {
					label += " · " + title
				}
			} else if year.Valid && year.Int64 > 0 {
				label = fmt.Sprintf("%s (%d)", title, year.Int64)
			}
			cov.Failed = append(cov.Failed, SeekPreviewFailure{
				FileID: f.FileID, TitleID: titleID, Label: label, Path: f.Path, Reason: f.Set.Reason})
		}
	}
	sort.SliceStable(cov.Failed, func(i, j int) bool {
		a, b := cov.Failed[i], cov.Failed[j]
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Path < b.Path
	})
	return cov, nil
}

// SeekPreviewRecord is a record beside the current state of its File row.
type SeekPreviewRecord struct {
	SeekPreviewSet
	// FileExists is false for a purged File; FileMtime and FileSizeBytes are the
	// row's current change signal otherwise.
	FileExists    bool
	FileMtime     string
	FileSizeBytes int64
}

// SeekPreviewRecords lists every record with its File's current mtime and size.
func (db *DB) SeekPreviewRecords() ([]SeekPreviewRecord, error) {
	rows, err := db.Query(
		`SELECT p.file_id, p.mtime, p.size_bytes, p.status, p.reason, p.interval_ms, p.frame_width,
		        p.frame_height, p.grid_columns, p.grid_rows, p.frame_count, p.sheet_count,
		        f.id IS NOT NULL, COALESCE(f.mtime, ''), COALESCE(f.size_bytes, 0)
		   FROM seek_previews p LEFT JOIN files f ON f.id = p.file_id
		  ORDER BY p.file_id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing seek preview records: %w", err)
	}
	defer rows.Close()
	var out []SeekPreviewRecord
	for rows.Next() {
		var r SeekPreviewRecord
		if err := rows.Scan(&r.FileID, &r.Mtime, &r.SizeBytes, &r.Status, &r.Reason,
			&r.IntervalMs, &r.FrameWidth, &r.FrameHeight, &r.Columns, &r.Rows, &r.FrameCount, &r.SheetCount,
			&r.FileExists, &r.FileMtime, &r.FileSizeBytes); err != nil {
			return nil, fmt.Errorf("store: scanning seek preview record: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeekPreviewFileIDsOfLibrary lists the id of every File, Missing ones included,
// that has a record and belongs to the Library.
func (db *DB) SeekPreviewFileIDsOfLibrary(libraryID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT p.file_id
		   FROM seek_previews p
		   JOIN files f    ON f.id = p.file_id
		   JOIN editions e ON e.id = f.edition_id
		   JOIN titles t   ON t.id = e.title_id
		  WHERE t.library_id = ?
		  ORDER BY p.file_id`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing seek preview records of %q: %w", libraryID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SaveSeekPreview records a File's set (replacing any record it had).
func (db *DB) SaveSeekPreview(s SeekPreviewSet) error {
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO seek_previews
		   (file_id, mtime, size_bytes, status, reason, interval_ms, frame_width, frame_height,
		    grid_columns, grid_rows, frame_count, sheet_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.FileID, s.Mtime, s.SizeBytes, s.Status, s.Reason, s.IntervalMs, s.FrameWidth, s.FrameHeight,
		s.Columns, s.Rows, s.FrameCount, s.SheetCount); err != nil {
		return fmt.Errorf("store: saving seek preview of %q: %w", s.FileID, err)
	}
	return nil
}

// DeleteSeekPreviewRecord forgets a File's record.
func (db *DB) DeleteSeekPreviewRecord(fileID string) error {
	if _, err := db.Exec(`DELETE FROM seek_previews WHERE file_id = ?`, fileID); err != nil {
		return fmt.Errorf("store: deleting seek preview of %q: %w", fileID, err)
	}
	return nil
}
