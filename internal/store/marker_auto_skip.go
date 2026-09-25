package store

import "fmt"

// Auto-skip (ADR-0065 §6): the Marker kinds a User skips automatically. The
// vocabulary lives in internal/markers; the schema's CHECK holds the store to it.

// MarkerAutoSkipKinds lists the Marker kinds the User skips automatically, in
// kind order. A User who never chose any answers an empty list.
func (db *DB) MarkerAutoSkipKinds(userID string) ([]string, error) {
	rows, err := db.Query(`SELECT kind FROM user_marker_auto_skip WHERE user_id = ? ORDER BY kind`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing auto-skip of %q: %w", userID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, fmt.Errorf("store: scanning auto-skip: %w", err)
		}
		out = append(out, kind)
	}
	return out, rows.Err()
}

// SetMarkerAutoSkipKinds sets the Marker kinds the User skips automatically to
// exactly kinds — the whole choice, not a change to it. An empty kinds turns
// auto-skip off for every kind; an unknown kind stores nothing.
func (db *DB) SetMarkerAutoSkipKinds(userID string, kinds []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin setting auto-skip: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM user_marker_auto_skip WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clearing auto-skip of %q: %w", userID, err)
	}
	for _, kind := range kinds {
		if _, err := tx.Exec(`INSERT INTO user_marker_auto_skip (user_id, kind) VALUES (?, ?)`, userID, kind); err != nil {
			return fmt.Errorf("store: setting %s auto-skip of %q: %w", kind, userID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing auto-skip of %q: %w", userID, err)
	}
	return nil
}
