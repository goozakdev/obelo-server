package store

import "fmt"

// CountUsers returns the number of Users in the database. The handshake uses
// this to decide setupRequired: a fresh server with zero Users still needs its
// first Admin bootstrapped via the claim token (ADR-0013).
func (db *DB) CountUsers() (int, error) {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting users: %w", err)
	}
	return n, nil
}
