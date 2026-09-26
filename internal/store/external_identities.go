package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// External identities and the Sign-in provider order (ADR-0063). An External
// identity is keyed by (plugin id, subject) — never by username — and is held by
// exactly one User; the table's primary key and its foreign key say both.

// ErrExternalIdentityTaken is what CreateExternalMember answers when the
// identity was claimed by a concurrent first sign-in between the caller's lookup
// and its insert. The caller resolves the identity again rather than guessing.
var ErrExternalIdentityTaken = errors.New("store: external identity already held")

// ErrUsernameHeld is what CreateExternalMember answers when the username it
// would mint is already held here in any case: "Brandon" from a provider is the
// local "brandon".
var ErrUsernameHeld = errors.New("store: username already held")

// ExternalIdentityUser returns the User holding the External identity
// (pluginID, subject), or ErrNotFound for an identity this server has never seen.
func (db *DB) ExternalIdentityUser(pluginID, subject string) (User, error) {
	return db.scanUser(db.QueryRow(
		`SELECT u.id, u.username, u.role, COALESCE(u.password_hash, ''), u.created_at
		   FROM external_identities x JOIN users u ON u.id = x.user_id
		  WHERE x.plugin_id = ? AND x.subject = ?`, pluginID, subject))
}

// RecordExternalSignIn remembers what a provider said about a returning identity:
// its current username at the source and its groups. Neither is resolved by. A
// sign-in is the provider answering, so a pending re-check retry and the count
// of failed verifications are cleared. A provider uninstalled since the
// identity was found is ErrSignInProviderUninstalled, and nothing is recorded.
func (db *DB) RecordExternalSignIn(pluginID, subject, username string, groups []string) error {
	g, err := encodeGroups(groups)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: recording external sign-in: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if gone, err := signInProviderUninstalled(tx, pluginID); err != nil {
		return err
	} else if gone {
		return ErrSignInProviderUninstalled
	}
	res, err := tx.Exec(
		`UPDATE external_identities
		    SET username = ?, groups = ?, last_seen_at = datetime('now'), retry_at = '', check_failures = 0
		  WHERE plugin_id = ? AND subject = ?`, username, g, pluginID, subject)
	if err != nil {
		return fmt.Errorf("store: recording external sign-in: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: recording external sign-in: %w", err)
	}
	return nil
}

// ExternalIdentity is one row of external_identities as the host sees it.
type ExternalIdentity struct {
	PluginID string
	Subject  string
	UserID   string
	Username string
	Groups   []string
}

// ExternalIdentitiesByUser lists the External identities a User holds, oldest
// first.
func (db *DB) ExternalIdentitiesByUser(userID string) ([]ExternalIdentity, error) {
	rows, err := db.Query(
		`SELECT plugin_id, subject, user_id, username, groups
		   FROM external_identities WHERE user_id = ? ORDER BY created_at, plugin_id, subject`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing external identities: %w", err)
	}
	defer rows.Close()
	var out []ExternalIdentity
	for rows.Next() {
		var x ExternalIdentity
		var groups string
		if err := rows.Scan(&x.PluginID, &x.Subject, &x.UserID, &x.Username, &groups); err != nil {
			return nil, fmt.Errorf("store: scanning external identity: %w", err)
		}
		if err := json.Unmarshal([]byte(groups), &x.Groups); err != nil {
			return nil, fmt.Errorf("store: decoding external identity groups: %w", err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// CreateExternalMember mints a Member with no Local password and gives it the
// External identity (pluginID, subject), in ONE transaction: a password-less
// Member without the identity that justifies it must never exist, even for an
// instant. The row is marked external_origin, which is the one thing the users
// CHECK accepts in place of a password for a person.
//
// A username already held here, compared without regard to case, is
// ErrUsernameHeld — the collision ADR-0063 decision 7 refuses rather than
// merges — and one that wins a race to the same spelling surfaces as the
// UNIQUE-constraint error; the caller maps both.
// An identity claimed by a concurrent first sign-in is ErrExternalIdentityTaken.
// A provider uninstalled since it answered is ErrSignInProviderUninstalled, and
// nothing is created.
func (db *DB) CreateExternalMember(id, username, pluginID, subject, providerUsername string, groups []string) (User, error) {
	g, err := encodeGroups(groups)
	if err != nil {
		return User{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return User{}, fmt.Errorf("store: creating external member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if gone, err := signInProviderUninstalled(tx, pluginID); err != nil {
		return User{}, err
	} else if gone {
		return User{}, ErrSignInProviderUninstalled
	}
	if held, err := usernameHeldFolded(tx, username); err != nil {
		return User{}, err
	} else if held {
		return User{}, ErrUsernameHeld
	}
	if _, err := tx.Exec(
		`INSERT INTO users (id, username, role, password_hash, external_origin)
		 VALUES (?, ?, 'member', NULL, 1)`, id, username); err != nil {
		return User{}, fmt.Errorf("store: creating external member: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO external_identities (plugin_id, subject, user_id, username, groups)
		 VALUES (?, ?, ?, ?, ?)`, pluginID, subject, id, providerUsername, g); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrExternalIdentityTaken
		}
		return User{}, fmt.Errorf("store: linking external identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("store: creating external member: %w", err)
	}
	return db.UserByID(id)
}

// usernameHeldFolded reports whether a User here holds username in any case.
// The fold is Go's Unicode one, which SQLite's NOCASE (ASCII only) is not.
func usernameHeldFolded(tx *sql.Tx, username string) (bool, error) {
	rows, err := tx.Query(`SELECT username FROM users`)
	if err != nil {
		return false, fmt.Errorf("store: reading usernames: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var held string
		if err := rows.Scan(&held); err != nil {
			return false, fmt.Errorf("store: reading usernames: %w", err)
		}
		if strings.EqualFold(held, username) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// AttachExternalIdentity gives the existing User userID the External identity
// (pluginID, subject), which that User asked for while signed in as themselves.
// Attaching one the User already holds records what the provider said, like a
// returning sign-in. One held by a different User is ErrExternalIdentityTaken,
// and nothing changes: the insert and the check are one statement, so no
// concurrent attach or sign-in can move an identity between Users. A provider
// uninstalled since it answered is ErrSignInProviderUninstalled, and nothing
// changes.
func (db *DB) AttachExternalIdentity(userID, pluginID, subject, providerUsername string, groups []string) error {
	g, err := encodeGroups(groups)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: attaching external identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if gone, err := signInProviderUninstalled(tx, pluginID); err != nil {
		return err
	} else if gone {
		return ErrSignInProviderUninstalled
	}
	res, err := tx.Exec(
		`INSERT INTO external_identities (plugin_id, subject, user_id, username, groups)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (plugin_id, subject) DO UPDATE
		    SET username = excluded.username, groups = excluded.groups, last_seen_at = datetime('now')
		  WHERE external_identities.user_id = excluded.user_id`,
		pluginID, subject, userID, providerUsername, g)
	if err != nil {
		return fmt.Errorf("store: attaching external identity: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExternalIdentityTaken
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: attaching external identity: %w", err)
	}
	return nil
}

func encodeGroups(groups []string) (string, error) {
	if groups == nil {
		groups = []string{}
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return "", fmt.Errorf("store: encoding groups: %w", err)
	}
	return string(b), nil
}

// SignInProviderOrder returns the plugin ids the Admin ordered, first-asked
// first. A provider absent from it has no Admin-set place.
func (db *DB) SignInProviderOrder() ([]string, error) {
	rows, err := db.Query(`SELECT plugin_id FROM sign_in_provider_order ORDER BY position, plugin_id`)
	if err != nil {
		return nil, fmt.Errorf("store: reading sign-in provider order: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning sign-in provider order: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetSignInProviderOrder replaces the Admin's order with ids, whole.
func (db *DB) SetSignInProviderOrder(ids []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: setting sign-in provider order: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM sign_in_provider_order`); err != nil {
		return fmt.Errorf("store: setting sign-in provider order: %w", err)
	}
	for i, id := range ids {
		if _, err := tx.Exec(
			`INSERT INTO sign_in_provider_order (plugin_id, position) VALUES (?, ?)`, id, i); err != nil {
			return fmt.Errorf("store: setting sign-in provider order: %w", err)
		}
	}
	return tx.Commit()
}
