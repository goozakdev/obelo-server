package store

import (
	"fmt"

	"github.com/google/uuid"
)

// Marker is one stored Marker (ADR-0065): a timed span of a File of one kind
// (intro | recap | credits | preview) from one source (local | detected |
// fetched). The vocabulary lives in internal/markers; the schema's CHECKs hold
// the store to it.
type Marker struct {
	Kind    string
	Source  string
	StartMs int64
	EndMs   int64
}

// ReplaceLocalMarkers sets the Local Markers of the File at path to exactly ms,
// replacing whatever the Scanner stored for it before. An empty ms clears them —
// the File's chapters or `.edl` stopped naming anything. Markers from any other
// source are left alone: the Scanner owns only what it read from the File.
func (db *DB) ReplaceLocalMarkers(path string, ms []Marker) error {
	return db.replaceLocalMarkers(path, ms, false)
}

// ReplaceEDLMarkers is ReplaceLocalMarkers for Markers read from the File's
// `.edl`, remembered as such so LocalMarkersFromEDL can tell.
func (db *DB) ReplaceEDLMarkers(path string, ms []Marker) error {
	return db.replaceLocalMarkers(path, ms, true)
}

// LocalMarkersFromEDL reports whether the Local Markers stored for the File at
// path were read from its `.edl`.
func (db *DB) LocalMarkersFromEDL(path string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM markers WHERE file_path = ? AND source = 'local' AND from_edl = 1`, path,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("store: reading local markers of %q: %w", path, err)
	}
	return n > 0, nil
}

func (db *DB) replaceLocalMarkers(path string, ms []Marker, fromEDL bool) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replacing markers: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM markers WHERE file_path = ? AND source = 'local'`, path); err != nil {
		return fmt.Errorf("store: clearing local markers of %q: %w", path, err)
	}
	for _, m := range ms {
		if _, err := tx.Exec(
			`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms, from_edl)
			 VALUES (?, ?, ?, 'local', ?, ?, ?)`,
			uuid.NewString(), path, m.Kind, m.StartMs, m.EndMs, fromEDL,
		); err != nil {
			return fmt.Errorf("store: inserting %s marker of %q: %w", m.Kind, path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing markers of %q: %w", path, err)
	}
	return nil
}

// MarkersForFile lists the Markers of the File with this id, in start order. A
// File with none — or a mirrored File, which has no path on this disk — answers
// an empty list. Sources are ranked Local > Detected > Fetched (ADR-0065 §1), and
// a lower source's Marker is left out wherever a higher source already speaks:
// when one of that kind is kept for the File, or one of any kind it overlaps.
func (db *DB) MarkersForFile(fileID string) ([]Marker, error) {
	rows, err := db.Query(
		`SELECT m.kind, m.source, m.start_ms, m.end_ms
		   FROM markers m JOIN files f ON f.path = m.file_path
		  WHERE f.id = ? AND f.path <> ''
		  ORDER BY m.start_ms, m.kind`, fileID)
	if err != nil {
		return nil, fmt.Errorf("store: listing markers: %w", err)
	}
	defer rows.Close()
	var out []Marker
	for rows.Next() {
		var m Marker
		if err := rows.Scan(&m.Kind, &m.Source, &m.StartMs, &m.EndMs); err != nil {
			return nil, fmt.Errorf("store: scanning marker: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return withPrecedence(out), nil
}

// sourceRank orders the Marker sources, highest first.
var sourceRank = map[string]int{"local": 0, "detected": 1, "fetched": 2}

// withPrecedence keeps, source by source from the highest, every Marker whose
// kind no higher source kept and which overlaps nothing a higher source kept.
// Markers of one source never shadow each other. The start order of ms is kept.
func withPrecedence(ms []Marker) []Marker {
	keep := make([]bool, len(ms))
	for rank := 0; rank < len(sourceRank); rank++ {
		for i, m := range ms {
			if sourceRank[m.Source] != rank {
				continue
			}
			keep[i] = true
			for j, h := range ms {
				if !keep[j] || sourceRank[h.Source] >= rank {
					continue
				}
				if h.Kind == m.Kind || (m.StartMs < h.EndMs && h.StartMs < m.EndMs) {
					keep[i] = false
					break
				}
			}
		}
	}
	out := ms[:0]
	for i, m := range ms {
		if keep[i] {
			out = append(out, m)
		}
	}
	return out
}

// PruneOrphanedMarkers removes everything kept by path about Files that are
// gone: the Markers of every source, the question the Marker providers last
// answered, and what detection heard, of each path no files row holds and gone
// reports gone from disk. A Missing File that is its Title's only File keeps its
// row, so it keeps all of it for when it returns (ADR-0008). A missing part or
// Edition of a multi-file Title does not: the scan rebuilds the Title without
// its row, so its data is removed here — and recomputed if it returns, since it
// is then a new File: probed for its chapters, heard by detection, and asked of
// the Marker providers again. The disk check spares a File another scan has
// just read and not yet written. It returns how many paths it removed.
func (db *DB) PruneOrphanedMarkers(gone func(path string) bool) (int, error) {
	rows, err := db.Query(
		`SELECT p FROM (
		   SELECT file_path AS p FROM markers
		   UNION SELECT file_path FROM marker_fetches
		   UNION SELECT file_path FROM marker_detection_files
		   UNION SELECT file_path FROM marker_detection_failures)
		  WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.path = p)`)
	if err != nil {
		return 0, fmt.Errorf("store: listing orphaned markers: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scanning orphaned marker path: %w", err)
		}
		if gone(p) {
			paths = append(paths, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, p := range paths {
		tx, err := db.Begin()
		if err != nil {
			return 0, fmt.Errorf("store: begin removing markers of %q: %w", p, err)
		}
		for _, table := range []string{"markers", "marker_fetches", "marker_detection_files", "marker_detection_failures"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE file_path = ?`, p); err != nil {
				_ = tx.Rollback()
				return 0, fmt.Errorf("store: removing %s of %q: %w", table, p, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("store: committing removal of markers of %q: %w", p, err)
		}
	}
	return len(paths), nil
}
