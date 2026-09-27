package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Marker detection (ADR-0065 §4) reads a TV Library one Season at a time and
// writes Detected Markers beside the Local ones, in the same path-keyed table.

// DetectionFile is one File of a Season as Marker detection sees it: the Episode
// it belongs to (two Files of the same Episode are never compared — they are the
// same sound), where it is, how long it is, and whether it has already been
// listened to at its current mtime.
type DetectionFile struct {
	TitleID    string
	Path       string
	DurationMs int64
	Analyzed   bool
	// DecodeFailures is how many detection runs in a row failed to decode the
	// File at its current mtime (RecordDecode).
	DecodeFailures int
	// AudioChannels is the channel count of the File's first audio stream, the
	// one detection listens to; 0 when unknown. Two Files with different counts
	// are most likely different sources of the Season.
	AudioChannels int
}

// DetectionSeason is one Season's worth of Files, in Episode order.
type DetectionSeason struct {
	ID     string
	ShowID string
	Files  []DetectionFile
}

// ErrNoMarkerDetection is returned for the detection toggle of a Library that
// has none: only a TV Library has one (ADR-0065 §4 — for music the concept does
// not apply, and a Movie has no other episodes to compare with).
var ErrNoMarkerDetection = errors.New("store: library has no marker detection")

// MarkerDetectionEnabled reports whether Marker detection runs after a scan of
// the Library: on unless an Admin turned it off, for a local TV Library, and
// ErrNoMarkerDetection for any other. An unknown Library is ErrNotFound.
func (db *DB) MarkerDetectionEnabled(libraryID string) (bool, error) {
	var kind, source string
	var enabled sql.NullInt64
	err := db.QueryRow(
		`SELECT l.kind, l.source, d.enabled
		   FROM libraries l LEFT JOIN library_marker_detection d ON d.library_id = l.id
		  WHERE l.id = ?`, libraryID).Scan(&kind, &source, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: reading marker detection of %q: %w", libraryID, err)
	}
	if kind != "tv" || source != LibrarySourceLocal {
		return false, ErrNoMarkerDetection
	}
	return !enabled.Valid || enabled.Int64 != 0, nil
}

// SetMarkerDetectionEnabled turns Marker detection on or off for a TV Library.
// Any other Library is ErrNoMarkerDetection, an unknown one ErrNotFound.
func (db *DB) SetMarkerDetectionEnabled(libraryID string, on bool) error {
	if _, err := db.MarkerDetectionEnabled(libraryID); err != nil {
		return err
	}
	if _, err := db.Exec(
		`INSERT INTO library_marker_detection (library_id, enabled) VALUES (?, ?)
		 ON CONFLICT (library_id) DO UPDATE SET enabled = excluded.enabled`,
		libraryID, boolToInt(on)); err != nil {
		return fmt.Errorf("store: saving marker detection of %q: %w", libraryID, err)
	}
	return nil
}

// DetectionSeasonsOfLibrary lists every Season of a Library with the present,
// local Files of its Episodes, Shows and Seasons in order.
func (db *DB) DetectionSeasonsOfLibrary(libraryID string) ([]DetectionSeason, error) {
	return db.detectionSeasons(`sh.library_id = ?`, libraryID)
}

// DetectionSeasonsOfShow is DetectionSeasonsOfLibrary for one Show. An unknown
// Show is ErrNotFound; a Show with no Files answers an empty list.
func (db *DB) DetectionSeasonsOfShow(showID string) ([]DetectionSeason, error) {
	var one int
	if err := db.QueryRow(`SELECT 1 FROM shows WHERE id = ?`, showID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("store: reading show %q: %w", showID, err)
	}
	return db.detectionSeasons(`sh.id = ?`, showID)
}

func (db *DB) detectionSeasons(where string, arg string) ([]DetectionSeason, error) {
	rows, err := db.Query(
		`SELECT s.id, s.show_id, t.id, f.path, f.duration_ms,
		        EXISTS (SELECT 1 FROM marker_detection_files d
		                 WHERE d.file_path = f.path AND d.mtime = f.mtime),
		        COALESCE((SELECT x.failures FROM marker_detection_failures x
		                   WHERE x.file_path = f.path AND x.mtime = f.mtime), 0),
		        COALESCE((SELECT st.channels FROM streams st
		                   WHERE st.file_id = f.id AND st.kind = 'audio'
		                   ORDER BY st.stream_index LIMIT 1), 0)
		   FROM seasons s
		   JOIN shows sh   ON sh.id = s.show_id
		   JOIN titles t   ON t.season_id = s.id AND t.kind = 'episode'
		   JOIN editions e ON e.title_id = t.id
		   JOIN files f    ON f.edition_id = e.id
		  WHERE `+where+` AND f.path <> '' AND f.present = 1
		  ORDER BY sh.sort_title, sh.id, s.season_number, t.episode_number, t.id, f.part_ordinal, f.path`, arg)
	if err != nil {
		return nil, fmt.Errorf("store: listing seasons for marker detection: %w", err)
	}
	defer rows.Close()
	var out []DetectionSeason
	for rows.Next() {
		var seasonID, showID string
		var f DetectionFile
		if err := rows.Scan(&seasonID, &showID, &f.TitleID, &f.Path, &f.DurationMs, &f.Analyzed, &f.DecodeFailures, &f.AudioChannels); err != nil {
			return nil, fmt.Errorf("store: scanning season file: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].ID != seasonID {
			out = append(out, DetectionSeason{ID: seasonID, ShowID: showID})
		}
		last := &out[len(out)-1]
		last.Files = append(last.Files, f)
	}
	return out, rows.Err()
}

// SaveDetectedMarkers sets the Detected Markers of the File at path to exactly
// ms, and records that the File was listened to at its current mtime, so the
// next post-scan run can skip a Season nothing in has changed. Local and Fetched
// Markers are left alone.
func (db *DB) SaveDetectedMarkers(path string, ms []Marker) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin saving detected markers: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM markers WHERE file_path = ? AND source = 'detected'`, path); err != nil {
		return fmt.Errorf("store: clearing detected markers of %q: %w", path, err)
	}
	for _, m := range ms {
		if _, err := tx.Exec(
			`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
			 VALUES (?, ?, ?, 'detected', ?, ?)`,
			uuid.NewString(), path, m.Kind, m.StartMs, m.EndMs,
		); err != nil {
			return fmt.Errorf("store: inserting detected %s marker of %q: %w", m.Kind, path, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO marker_detection_files (file_path, mtime)
		 SELECT path, mtime FROM files WHERE path = ? LIMIT 1
		 ON CONFLICT (file_path) DO UPDATE SET mtime = excluded.mtime`, path); err != nil {
		return fmt.Errorf("store: recording detection of %q: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing detected markers of %q: %w", path, err)
	}
	return nil
}

// RecordDecode records whether detection could decode the File at path: a
// failure adds one to its consecutive failures at its current mtime (starting
// again from one if the File changed since the last), a decode clears them.
func (db *DB) RecordDecode(path string, decoded bool) error {
	if decoded {
		if _, err := db.Exec(`DELETE FROM marker_detection_failures WHERE file_path = ?`, path); err != nil {
			return fmt.Errorf("store: clearing decode failures of %q: %w", path, err)
		}
		return nil
	}
	if _, err := db.Exec(
		`INSERT INTO marker_detection_failures (file_path, mtime, failures)
		 SELECT path, mtime, 1 FROM files WHERE path = ? LIMIT 1
		 ON CONFLICT (file_path) DO UPDATE SET
		   failures = CASE WHEN mtime = excluded.mtime THEN failures + 1 ELSE 1 END,
		   mtime    = excluded.mtime`, path); err != nil {
		return fmt.Errorf("store: recording decode failure of %q: %w", path, err)
	}
	return nil
}
