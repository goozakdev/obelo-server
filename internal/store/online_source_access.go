package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// ErrRatingCapped: a grant of an Online source was refused because the User holds a
// Rating ceiling (ADR-0068 Q3): the "unrated is visible" rule would expose every
// item of a source to them.
var ErrRatingCapped = errors.New("store: user holds a rating ceiling")

// OnlineSourceAccessForUser returns the ids of the Online sources granted to a User,
// in a stable order. Empty means none.
func (db *DB) OnlineSourceAccessForUser(userID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT source_id FROM user_online_source_access WHERE user_id = ? ORDER BY source_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: reading online source access: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning online source access: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReplaceOnlineSourceAccess sets a User's granted sources to exactly sourceIDs,
// atomically. A non-empty set for a User holding a Rating ceiling is ErrRatingCapped
// with the prior set kept; the ceiling is read inside the transaction that writes, so
// no state exists in which a capped User holds a grant. ErrNotFound for an unknown User.
func (db *DB) ReplaceOnlineSourceAccess(userID string, sourceIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: replacing online source access: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var ceiling sql.NullString
	err = tx.QueryRow(`SELECT rating_ceiling FROM users WHERE id = ?`, userID).Scan(&ceiling)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: reading rating ceiling: %w", err)
	}
	if ceiling.String != "" && len(sourceIDs) > 0 {
		return ErrRatingCapped
	}
	if _, err := tx.Exec(`DELETE FROM user_online_source_access WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clearing online source access: %w", err)
	}
	for _, id := range sourceIDs {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO user_online_source_access (user_id, source_id) VALUES (?, ?)`, userID, id,
		); err != nil {
			return fmt.Errorf("store: granting online source %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing online source access: %w", err)
	}
	return nil
}

// SetRatingCeilingRemovingSourceGrants stores a User's ceiling label (or clears it,
// with ""), and, when the label is set, deletes the User's Online source grants in
// the same transaction, returning the source ids removed in order. Clearing a
// ceiling restores nothing. ErrNotFound for an unknown User.
func (db *DB) SetRatingCeilingRemovingSourceGrants(userID, label string) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: setting rating ceiling: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var val any
	if label != "" {
		val = label
	}
	res, err := tx.Exec(`UPDATE users SET rating_ceiling = ? WHERE id = ?`, val, userID)
	if err != nil {
		return nil, fmt.Errorf("store: setting rating ceiling: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: setting rating ceiling: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	var removed []string
	if label != "" {
		rows, err := tx.Query(`SELECT source_id FROM user_online_source_access WHERE user_id = ?`, userID)
		if err != nil {
			return nil, fmt.Errorf("store: reading online source grants: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scanning online source grant: %w", err)
			}
			removed = append(removed, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("store: reading online source grants: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM user_online_source_access WHERE user_id = ?`, userID); err != nil {
			return nil, fmt.Errorf("store: removing online source grants: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: committing rating ceiling: %w", err)
	}
	sort.Strings(removed)
	return removed, nil
}
