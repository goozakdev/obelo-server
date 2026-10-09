package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Uninstalling a Sign-in provider (ADR-0063 decision 10). Every External
// identity the provider issued goes with it, and a User left with no sign-in
// path at all goes too — watch state and all. Who that is must be decided in the
// same transaction that deletes them, so the list an Admin confirmed is checked
// against the list the delete is about to act on, and a mismatch changes
// nothing.

// SignInCasualty is a User an uninstall of a Sign-in provider would delete.
type SignInCasualty struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// UnconfirmedUninstallError is what DeleteSignInPlugin answers when the Users
// it would delete are not exactly the ones the caller confirmed. Users is who
// it would delete now. Nothing was changed.
type UnconfirmedUninstallError struct {
	Users []SignInCasualty
}

func (e *UnconfirmedUninstallError) Error() string {
	return "store: the uninstall would delete users that were not confirmed"
}

// ErrUninstallWouldLeaveNoAdmin is an uninstall refused because deleting its
// Users would leave no Admin, or no Admin holding a Local password.
var ErrUninstallWouldLeaveNoAdmin = errors.New("store: the uninstall would leave no admin")

// ErrSignInProviderUninstalled is a sign-in's write refused because its
// provider was uninstalled after it answered. Nothing was written.
var ErrSignInProviderUninstalled = errors.New("store: the sign-in provider was uninstalled")

// signInProviderUninstalled reports whether pluginID was uninstalled as a
// Sign-in provider and not installed again since. q is the caller's
// transaction, so the answer holds for the writes that follow it.
func signInProviderUninstalled(q queryRower, pluginID string) (bool, error) {
	var gone bool
	if err := q.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM uninstalled_sign_in_providers WHERE plugin_id = ?)`, pluginID,
	).Scan(&gone); err != nil {
		return false, fmt.Errorf("store: checking the sign-in provider is installed: %w", err)
	}
	return gone, nil
}

// SignInCasualties lists, by username, the Users an uninstall of pluginID would
// delete: each holds an External identity pluginID issued and has no other
// sign-in path. Another path is a Local password, the Link a 'remote' User
// signs in over, or an External identity at any of otherProviders — the other
// installed Sign-in providers, whether or not they are enabled right now.
func (db *DB) SignInCasualties(pluginID string, otherProviders []string) ([]SignInCasualty, error) {
	return signInCasualties(db, pluginID, otherProviders)
}

// ExternalIdentityCount is how many External identities pluginID has issued, whether
// or not their Users have another way in.
func (db *DB) ExternalIdentityCount(pluginID string) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM external_identities WHERE plugin_id = ?`, pluginID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting external identities: %w", err)
	}
	return n, nil
}

// DeleteSignInPlugin uninstalls the Sign-in provider pluginID from the
// database, in ONE transaction: it revokes every session of each User it
// deletes, deletes those Users, deletes every External identity pluginID
// issued and its Group mapping, re-check interval and sign-in order, marks it
// uninstalled, and removes the Plugin's rows as DeletePlugin does. confirmed is the
// ids of the Users the caller was shown; unless it is exactly the set this
// transaction finds, the answer is *UnconfirmedUninstallError and nothing
// changes. Any failure part way leaves everything as it was.
func (db *DB) DeleteSignInPlugin(pluginID string, otherProviders, confirmed []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: uninstalling sign-in provider %q: %w", pluginID, err)
	}
	defer func() { _ = tx.Rollback() }()

	casualties, err := signInCasualties(tx, pluginID, otherProviders)
	if err != nil {
		return err
	}
	if !CasualtiesConfirmed(casualties, confirmed) {
		return &UnconfirmedUninstallError{Users: casualties}
	}
	for _, u := range casualties {
		if err := deleteSessionsForUser(tx, u.ID); err != nil {
			return err
		}
		res, err := tx.Exec(deleteUserKeepingAnAdminSQL, u.ID)
		if err != nil {
			return fmt.Errorf("store: deleting user: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrUninstallWouldLeaveNoAdmin
		}
	}
	if _, err := tx.Exec(`DELETE FROM external_identities WHERE plugin_id = ?`, pluginID); err != nil {
		return fmt.Errorf("store: deleting external identities: %w", err)
	}
	// What the Admin set up for this provider goes with it: a Plugin installed
	// later under the same id must not inherit a Group mapping that makes its
	// first sign-ins Admins, nor this one's place in the order or its interval.
	// The marker is what refuses a sign-in whose writes land after this commits.
	for _, stmt := range []string{
		`DELETE FROM group_mapping_libraries WHERE plugin_id = ?`,
		`DELETE FROM group_mappings WHERE plugin_id = ?`,
		`DELETE FROM sign_in_recheck_intervals WHERE plugin_id = ?`,
		`DELETE FROM sign_in_provider_order WHERE plugin_id = ?`,
		`INSERT OR IGNORE INTO uninstalled_sign_in_providers (plugin_id) VALUES (?)`,
	} {
		if _, err := tx.Exec(stmt, pluginID); err != nil {
			return fmt.Errorf("store: uninstalling sign-in provider %q: %w", pluginID, err)
		}
	}
	if err := deletePluginRows(tx, pluginID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: uninstalling sign-in provider %q: %w", pluginID, err)
	}
	return nil
}

// querier is what a multi-row read needs: a *sql.Tx or the DB itself.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func signInCasualties(q querier, pluginID string, otherProviders []string) ([]SignInCasualty, error) {
	query := `SELECT u.id, u.username FROM users u
	           WHERE u.role <> 'remote'
	             AND COALESCE(u.password_hash, '') = ''
	             AND EXISTS (SELECT 1 FROM external_identities x
	                          WHERE x.user_id = u.id AND x.plugin_id = ?)`
	args := []any{pluginID}
	var others []string
	for _, id := range otherProviders {
		if id != pluginID {
			others = append(others, id)
		}
	}
	if len(others) > 0 {
		query += `
	             AND NOT EXISTS (SELECT 1 FROM external_identities x
	                              WHERE x.user_id = u.id AND x.plugin_id IN (?` +
			strings.Repeat(", ?", len(others)-1) + `))`
		for _, id := range others {
			args = append(args, id)
		}
	}
	query += ` ORDER BY u.username`
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing users an uninstall would delete: %w", err)
	}
	defer rows.Close()
	out := []SignInCasualty{}
	for rows.Next() {
		var c SignInCasualty
		if err := rows.Scan(&c.ID, &c.Username); err != nil {
			return nil, fmt.Errorf("store: scanning user: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CasualtiesConfirmed reports whether confirmed names exactly the casualties,
// in any order.
func CasualtiesConfirmed(casualties []SignInCasualty, confirmed []string) bool {
	want := make([]string, 0, len(casualties))
	for _, c := range casualties {
		want = append(want, c.ID)
	}
	got := map[string]bool{}
	for _, id := range confirmed {
		got[id] = true
	}
	if len(got) != len(want) {
		return false
	}
	for _, id := range want {
		if !got[id] {
			return false
		}
	}
	return true
}
