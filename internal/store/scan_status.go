package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Scan states for the per-Library scan_status row (the pollable resource behind
// GET /libraries/{id}/scan).
const (
	ScanStateIdle    = "idle"
	ScanStateRunning = "running"
	ScanStateError   = "error"
)

// ScanStatus is the pollable progress/result of a Library's last or active scan.
// StartedAt/FinishedAt are empty before they occur. Scope is the display label of
// a Targeted scan's entity (ADR-0030/0031) while one is running, "" for a full
// Library scan; the admin surface reads it only while State is running.
type ScanStatus struct {
	LibraryID    string
	State        string
	TitlesFound  int
	FilesFound   int
	ErrorMessage string
	StartedAt    string
	FinishedAt   string
	Scope        string
}

// ScanStatusByLibrary returns the scan status for a Library. A Library that has
// never been scanned has no row yet; this returns a synthesized idle status so
// the API always has something to report, and ErrNotFound is reserved for an
// unknown Library (checked by the caller).
func (db *DB) ScanStatusByLibrary(libraryID string) (ScanStatus, error) {
	s := ScanStatus{LibraryID: libraryID, State: ScanStateIdle}
	var started, finished, errMsg, scope sql.NullString
	err := db.QueryRow(
		`SELECT state, titles_found, files_found, error_message, started_at, finished_at, scope
		   FROM scan_status WHERE library_id = ?`, libraryID,
	).Scan(&s.State, &s.TitlesFound, &s.FilesFound, &errMsg, &started, &finished, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return ScanStatus{}, fmt.Errorf("store: reading scan status: %w", err)
	}
	s.ErrorMessage = errMsg.String
	s.StartedAt = started.String
	s.FinishedAt = finished.String
	s.Scope = scope.String
	return s, nil
}

// MarkScanRunning records that a full-Library scan has begun: state=running,
// counts reset, scope cleared, started_at=now, finished_at/error cleared. Upserts
// the per-Library row.
func (db *DB) MarkScanRunning(libraryID string) error {
	return db.markScanRunning(libraryID, "")
}

// MarkScanRunningScope is MarkScanRunning for a Targeted scan (ADR-0030/0031): it
// tags the running row with the entity's display label so the admin surface shows
// "Scanning <scope>…" instead of a whole-Library "Scanning…". A full scan clears
// the tag (scope="").
func (db *DB) MarkScanRunningScope(libraryID, scope string) error {
	return db.markScanRunning(libraryID, scope)
}

func (db *DB) markScanRunning(libraryID, scope string) error {
	_, err := db.Exec(
		`INSERT INTO scan_status (library_id, state, titles_found, files_found, error_message, started_at, finished_at, scope)
		   VALUES (?, 'running', 0, 0, '', datetime('now'), NULL, ?)
		 ON CONFLICT(library_id) DO UPDATE SET
		   state = 'running', titles_found = 0, files_found = 0,
		   error_message = '', started_at = datetime('now'), finished_at = NULL, scope = ?`,
		libraryID, scope, scope)
	if err != nil {
		return fmt.Errorf("store: marking scan running: %w", err)
	}
	return nil
}

// MarkScanFinished records a successful scan completion with its result counts.
func (db *DB) MarkScanFinished(libraryID string, titlesFound, filesFound int) error {
	_, err := db.Exec(
		`UPDATE scan_status
		    SET state = 'idle', titles_found = ?, files_found = ?,
		        error_message = '', finished_at = datetime('now')
		  WHERE library_id = ?`,
		titlesFound, filesFound, libraryID)
	if err != nil {
		return fmt.Errorf("store: marking scan finished: %w", err)
	}
	return nil
}

// MarkScanError records that a scan failed, preserving the message for polling.
func (db *DB) MarkScanError(libraryID, message string) error {
	_, err := db.Exec(
		`UPDATE scan_status
		    SET state = 'error', error_message = ?, finished_at = datetime('now')
		  WHERE library_id = ?`,
		message, libraryID)
	if err != nil {
		return fmt.Errorf("store: marking scan error: %w", err)
	}
	return nil
}
