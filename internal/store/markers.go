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
			`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
			 VALUES (?, ?, ?, 'local', ?, ?)`,
			uuid.NewString(), path, m.Kind, m.StartMs, m.EndMs,
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
