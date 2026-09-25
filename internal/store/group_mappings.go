package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Group mappings and the re-check of External identities (ADR-0063 decisions 4
// and 5). The rules are the Admin's; which User a mapping may govern is decided
// in internal/auth, and ApplyMappedAccess refuses a User with a Local password
// again here, in the same statement that writes, so no caller can get it wrong.

// GroupMappingRule is one of a Sign-in provider's groups, mapped to a role and
// the Libraries it grants.
type GroupMappingRule struct {
	Group      string
	Role       string
	LibraryIDs []string
}

// GroupMapping returns the Admin's mapping for pluginID, by group name. A
// provider with none answers an empty list.
func (db *DB) GroupMapping(pluginID string) ([]GroupMappingRule, error) {
	rows, err := db.Query(
		`SELECT group_name, role FROM group_mappings WHERE plugin_id = ? ORDER BY group_name`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: reading group mapping: %w", err)
	}
	var out []GroupMappingRule
	for rows.Next() {
		var r GroupMappingRule
		if err := rows.Scan(&r.Group, &r.Role); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scanning group mapping: %w", err)
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading group mapping: %w", err)
	}
	for i := range out {
		libs, err := db.Query(
			`SELECT library_id FROM group_mapping_libraries
			  WHERE plugin_id = ? AND group_name = ? ORDER BY library_id`, pluginID, out[i].Group)
		if err != nil {
			return nil, fmt.Errorf("store: reading group mapping libraries: %w", err)
		}
		out[i].LibraryIDs = []string{}
		for libs.Next() {
			var id string
			if err := libs.Scan(&id); err != nil {
				libs.Close()
				return nil, fmt.Errorf("store: scanning group mapping library: %w", err)
			}
			out[i].LibraryIDs = append(out[i].LibraryIDs, id)
		}
		libs.Close()
		if err := libs.Err(); err != nil {
			return nil, fmt.Errorf("store: reading group mapping libraries: %w", err)
		}
	}
	return out, nil
}

// SetGroupMapping replaces pluginID's whole mapping with rules. A Library id
// that does not exist fails the whole write with ErrNotFound and leaves the
// previous mapping as it was.
func (db *DB) SetGroupMapping(pluginID string, rules []GroupMappingRule) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: setting group mapping: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM group_mappings WHERE plugin_id = ?`, pluginID); err != nil {
		return fmt.Errorf("store: setting group mapping: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.Exec(
			`INSERT INTO group_mappings (plugin_id, group_name, role) VALUES (?, ?, ?)`,
			pluginID, r.Group, r.Role); err != nil {
			return fmt.Errorf("store: setting group mapping: %w", err)
		}
		for _, lid := range r.LibraryIDs {
			var one int
			err := tx.QueryRow(`SELECT 1 FROM libraries WHERE id = ?`, lid).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("store: validating library %q: %w", lid, err)
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO group_mapping_libraries (plugin_id, group_name, library_id)
				 VALUES (?, ?, ?)`, pluginID, r.Group, lid); err != nil {
				return fmt.Errorf("store: setting group mapping: %w", err)
			}
		}
	}
	return tx.Commit()
}

// RecheckInterval is the Admin's override of how often pluginID's identities
// are re-checked, or 0 when there is none.
func (db *DB) RecheckInterval(pluginID string) (time.Duration, error) {
	var seconds int64
	err := db.QueryRow(`SELECT seconds FROM sign_in_recheck_intervals WHERE plugin_id = ?`, pluginID).Scan(&seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: reading re-check interval: %w", err)
	}
	return time.Duration(seconds) * time.Second, nil
}

// SetRecheckInterval stores pluginID's override, whole seconds; 0 removes it.
func (db *DB) SetRecheckInterval(pluginID string, d time.Duration) error {
	seconds := int64(d / time.Second)
	if seconds <= 0 {
		if _, err := db.Exec(`DELETE FROM sign_in_recheck_intervals WHERE plugin_id = ?`, pluginID); err != nil {
			return fmt.Errorf("store: clearing re-check interval: %w", err)
		}
		return nil
	}
	if _, err := db.Exec(
		`INSERT INTO sign_in_recheck_intervals (plugin_id, seconds) VALUES (?, ?)
		 ON CONFLICT (plugin_id) DO UPDATE SET seconds = excluded.seconds`, pluginID, seconds); err != nil {
		return fmt.Errorf("store: setting re-check interval: %w", err)
	}
	return nil
}

// ApplyMappedAccess sets userID's role and granted Libraries to what a Group
// mapping says, in one transaction, and answers whether either moved. It writes
// only to a person with no Local password — the WHERE clause says so, whatever
// the caller checked — and answers false, changing nothing, for anybody else.
// An Admin holds no grants, because an Admin reaches every Library by role.
func (db *DB) ApplyMappedAccess(userID, role string, libraryIDs []string) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var before string
	err = tx.QueryRow(`SELECT role FROM users WHERE id = ?`, userID).Scan(&before)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	res, err := tx.Exec(
		`UPDATE users SET role = ?, role_mapped = 1
		  WHERE id = ? AND COALESCE(password_hash, '') = '' AND role <> 'remote' AND external_origin = 1`,
		role, userID)
	if err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	held := map[string]bool{}
	rows, err := tx.Query(`SELECT library_id FROM user_library_access WHERE user_id = ?`, userID)
	if err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	for rows.Next() {
		var lid string
		if err := rows.Scan(&lid); err != nil {
			rows.Close()
			return false, fmt.Errorf("store: applying group mapping: %w", err)
		}
		held[lid] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	want := map[string]bool{}
	if role != "admin" {
		for _, lid := range libraryIDs {
			want[lid] = true
		}
	}
	changed := before != role || len(held) != len(want)
	for lid := range want {
		changed = changed || !held[lid]
	}
	if _, err := tx.Exec(`DELETE FROM user_library_access WHERE user_id = ?`, userID); err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	for lid := range want {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO user_library_access (user_id, library_id) VALUES (?, ?)`,
			userID, lid); err != nil {
			return false, fmt.Errorf("store: granting library %q: %w", lid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: applying group mapping: %w", err)
	}
	return changed, nil
}

// CountLocalPasswordAdmins returns how many Admins hold a Local password — the
// input to the guard that the Server never loses its last one (ADR-0063
// decision 5).
func (db *DB) CountLocalPasswordAdmins() (int, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND COALESCE(password_hash, '') <> ''`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting local-password admins: %w", err)
	}
	return n, nil
}

// ErrWouldLeaveNoAdmin is a delete DeleteUserKeepingAnAdmin refused.
var ErrWouldLeaveNoAdmin = errors.New("store: the delete would leave no admin")

// DeleteUserKeepingAnAdmin deletes userID unless that would leave no Admin, or
// no Admin holding a Local password (ADR-0063 decision 5). Both counts are taken
// by the statement that deletes, so two deletes racing each other cannot each
// pass a count the other is about to make untrue. ErrWouldLeaveNoAdmin when it
// refuses, ErrNotFound for an unknown User.
func (db *DB) DeleteUserKeepingAnAdmin(userID string) error {
	res, err := db.Exec(
		`DELETE FROM users WHERE id = ?
		   AND NOT (role = 'admin'
		            AND (SELECT COUNT(*) FROM users WHERE role = 'admin') <= 1)
		   AND NOT (role = 'admin' AND COALESCE(password_hash, '') <> ''
		            AND (SELECT COUNT(*) FROM users
		                  WHERE role = 'admin' AND COALESCE(password_hash, '') <> '') <= 1)`, userID)
	if err != nil {
		return fmt.Errorf("store: deleting user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	if _, err := db.UserByID(userID); err != nil {
		return err
	}
	return ErrWouldLeaveNoAdmin
}

// DeleteSessionsForUser revokes every session userID holds: each bearer token
// on every Device, each stream token, and each Device pairing they approved that
// the Device has not yet collected — a session in waiting, which would otherwise
// be minted after the revocation. The Devices themselves stay.
func (db *DB) DeleteSessionsForUser(userID string) error {
	if _, err := db.Exec(`DELETE FROM auth_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: revoking sessions: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM stream_tokens WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: revoking stream tokens: %w", err)
	}
	if _, err := db.Exec(
		`DELETE FROM device_auth_requests WHERE approved_user_id = ? AND state = 'approved'`, userID); err != nil {
		return fmt.Errorf("store: revoking approved device pairings: %w", err)
	}
	return nil
}

// ExternalIdentityCheck is one External identity as the periodic re-check sees
// it. The refresh token is here and nowhere else a caller can list.
type ExternalIdentityCheck struct {
	PluginID     string
	Subject      string
	UserID       string
	Groups       []string
	RefreshToken string
	// LastSeenAt is when the provider last vouched for the identity.
	LastSeenAt time.Time
	// RetryAt is when a re-check that could not reach the provider is tried
	// again; zero when none is pending.
	RetryAt time.Time
	// CheckFailures counts the consecutive re-checks whose ID token failed
	// verification.
	CheckFailures int
}

// sqliteTime is the layout datetime('now') writes.
const sqliteTime = "2006-01-02 15:04:05"

// ExternalIdentityChecks lists every External identity, oldest first.
func (db *DB) ExternalIdentityChecks() ([]ExternalIdentityCheck, error) {
	rows, err := db.Query(
		`SELECT plugin_id, subject, user_id, groups, refresh_token, last_seen_at, retry_at, check_failures
		   FROM external_identities ORDER BY created_at, plugin_id, subject`)
	if err != nil {
		return nil, fmt.Errorf("store: listing external identities: %w", err)
	}
	defer rows.Close()
	var out []ExternalIdentityCheck
	for rows.Next() {
		var (
			c             ExternalIdentityCheck
			groups        string
			seen, retryAt string
		)
		if err := rows.Scan(&c.PluginID, &c.Subject, &c.UserID, &groups, &c.RefreshToken,
			&seen, &retryAt, &c.CheckFailures); err != nil {
			return nil, fmt.Errorf("store: scanning external identity: %w", err)
		}
		if err := json.Unmarshal([]byte(groups), &c.Groups); err != nil {
			return nil, fmt.Errorf("store: decoding external identity groups: %w", err)
		}
		c.LastSeenAt, _ = time.ParseInLocation(sqliteTime, seen, time.UTC)
		if retryAt != "" {
			c.RetryAt, _ = time.ParseInLocation(sqliteTime, retryAt, time.UTC)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetExternalRefreshToken stores the refresh token a sign-in handed back for
// (pluginID, subject). An empty token leaves the stored one as it was.
func (db *DB) SetExternalRefreshToken(pluginID, subject, token string) error {
	if token == "" {
		return nil
	}
	if _, err := db.Exec(
		`UPDATE external_identities SET refresh_token = ? WHERE plugin_id = ? AND subject = ?`,
		token, pluginID, subject); err != nil {
		return fmt.Errorf("store: storing refresh token: %w", err)
	}
	return nil
}

// RecordExternalCheck remembers a re-check the provider answered: the
// identity's current username (kept when empty) and groups, a rotated refresh
// token (kept when empty), and at as the time it was last vouched for. Any
// pending retry and the count of failed verifications are cleared.
func (db *DB) RecordExternalCheck(pluginID, subject, username string, groups []string, refreshToken string, at time.Time) error {
	g, err := encodeGroups(groups)
	if err != nil {
		return err
	}
	res, err := db.Exec(
		`UPDATE external_identities
		    SET username = CASE WHEN ? = '' THEN username ELSE ? END,
		        groups = ?,
		        refresh_token = CASE WHEN ? = '' THEN refresh_token ELSE ? END,
		        last_seen_at = ?, retry_at = '', check_failures = 0
		  WHERE plugin_id = ? AND subject = ?`,
		username, username, g, refreshToken, refreshToken, at.UTC().Format(sqliteTime), pluginID, subject)
	if err != nil {
		return fmt.Errorf("store: recording external re-check: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// NoteExternalCheckFailure remembers a re-check that did not answer: it is
// tried again at retryAt, and, when unverified — the answer's ID token failed
// verification — the consecutive count goes up by one. It answers the count.
func (db *DB) NoteExternalCheckFailure(pluginID, subject string, retryAt time.Time, unverified bool) (int, error) {
	add := 0
	if unverified {
		add = 1
	}
	var n int
	err := db.QueryRow(
		`UPDATE external_identities SET retry_at = ?, check_failures = check_failures + ?
		  WHERE plugin_id = ? AND subject = ? RETURNING check_failures`,
		retryAt.UTC().Format(sqliteTime), add, pluginID, subject).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: recording failed re-check: %w", err)
	}
	return n, nil
}
