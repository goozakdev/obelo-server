package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Fetched Markers (ADR-0065) are what the Marker providers answered for a File,
// already judged by the host. They live in the markers table beside the Local
// and Detected ones; marker_fetches remembers the question they answered.

// MarkerFetchQuestion returns the question the Marker providers were last asked
// about the File at path, and false when they never were.
func (db *DB) MarkerFetchQuestion(path string) (string, bool, error) {
	var q string
	err := db.QueryRow(`SELECT question FROM marker_fetches WHERE file_path = ?`, path).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: reading marker fetch of %q: %w", path, err)
	}
	return q, true, nil
}

// SaveFetchedMarkers sets the Fetched Markers of the File at path to exactly ms
// and remembers the question they answered. An empty ms is a remembered miss.
// Local and Detected Markers are left alone.
func (db *DB) SaveFetchedMarkers(path, question string, ms []Marker) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin saving fetched markers: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM markers WHERE file_path = ? AND source = 'fetched'`, path); err != nil {
		return fmt.Errorf("store: clearing fetched markers of %q: %w", path, err)
	}
	for _, m := range ms {
		if _, err := tx.Exec(
			`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
			 VALUES (?, ?, ?, 'fetched', ?, ?)`,
			uuid.NewString(), path, m.Kind, m.StartMs, m.EndMs,
		); err != nil {
			return fmt.Errorf("store: inserting fetched %s marker of %q: %w", m.Kind, path, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO marker_fetches (file_path, question) VALUES (?, ?)
		 ON CONFLICT (file_path) DO UPDATE SET question = excluded.question`, path, question); err != nil {
		return fmt.Errorf("store: recording marker fetch of %q: %w", path, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing fetched markers of %q: %w", path, err)
	}
	return nil
}
