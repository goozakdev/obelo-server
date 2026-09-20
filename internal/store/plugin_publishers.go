package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Pinned publisher keys and the operator's catalog URL (.scratch/plugin-system
// issue 15): the two rows an Admin writes to opt into trust and discovery, and
// the two this server ships none of.
//
// ADR-0001: an empty plugin_publishers table and an empty catalog URL are the
// shipped state, so an Admin who opts into neither gets no signature trust and no
// discovery — both features stay fully off until deliberately turned on.

// PluginPublisher is one publisher an Admin has pinned a key against.
//
// PublicKey is returned to the API IN FULL and is not masked. It is the one
// credential-shaped column in this database that is genuinely public — an
// operator has to read back what they pinned to compare it against what a
// publisher advertises, and a masked public key would be a masked fact.
type PluginPublisher struct {
	// Publisher is the name a signature document names, matched case-insensitively
	// (the column is COLLATE NOCASE).
	Publisher string
	// PublicKey is the base64 ed25519 public key, 44 characters.
	PublicKey string
	// KeyID is the short fingerprint, derived from PublicKey when the row was
	// written and stored so a screen need not recompute it. Nothing verifies
	// against it.
	KeyID   string
	AddedAt string
}

// PluginPublishers lists every pinned publisher, in name order.
//
// THE EMPTY LIST IS THE SIGNAL. It is not "no data yet" — it is the default
// policy, under which no install is signature-checked at all. Every caller reads
// it that way, so this returns an empty slice and never an error for a server
// that has pinned nothing.
func (db *DB) PluginPublishers() ([]PluginPublisher, error) {
	rows, err := db.Query(
		`SELECT publisher, public_key, key_id, added_at FROM plugin_publishers ORDER BY publisher`)
	if err != nil {
		return nil, fmt.Errorf("store: listing pinned plugin publishers: %w", err)
	}
	defer rows.Close()

	var out []PluginPublisher
	for rows.Next() {
		var p PluginPublisher
		if err := rows.Scan(&p.Publisher, &p.PublicKey, &p.KeyID, &p.AddedAt); err != nil {
			return nil, fmt.Errorf("store: scanning a pinned plugin publisher: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertPluginPublisher pins a key, replacing whatever that publisher had.
//
// Replacing rather than refusing a duplicate is the right behaviour for a key
// ROTATION, which is the only reason to pin the same publisher twice. The old key
// stops being accepted the moment this returns, and an Admin who meant to keep
// both pins the second under a second name.
func (db *DB) UpsertPluginPublisher(p PluginPublisher) error {
	_, err := db.Exec(
		`INSERT INTO plugin_publishers (publisher, public_key, key_id, added_at)
		      VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(publisher) DO UPDATE SET
		      public_key = excluded.public_key,
		      key_id     = excluded.key_id,
		      added_at   = datetime('now')`,
		p.Publisher, p.PublicKey, p.KeyID)
	if err != nil {
		return fmt.Errorf("store: pinning the publisher %q: %w", p.Publisher, err)
	}
	return nil
}

// DeletePluginPublisher unpins a key, reporting whether there was one to unpin so
// the API can tell "removed" from "no such publisher" without a second read.
//
// Unpinning the LAST key returns the server to its default policy — nothing is
// verified — which is a consequential act and is why the screen says so rather
// than treating it as one row among many.
func (db *DB) DeletePluginPublisher(publisher string) (bool, error) {
	res, err := db.Exec(`DELETE FROM plugin_publishers WHERE publisher = ?`, publisher)
	if err != nil {
		return false, fmt.Errorf("store: unpinning the publisher %q: %w", publisher, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: unpinning the publisher %q: %w", publisher, err)
	}
	return n > 0, nil
}

// SetPluginSigner records WHO SIGNED an installed plugin, and is called only
// after a signature has verified against a pinned key. An unverified claim never
// reaches this function — see the plugins table comment (0001_init.sql).
func (db *DB) SetPluginSigner(id, publisher, keyID string) error {
	_, err := db.Exec(
		`UPDATE plugins SET publisher = ?, key_id = ? WHERE id = ?`, publisher, keyID, id)
	if err != nil {
		return fmt.Errorf("store: recording the publisher of plugin %q: %w", id, err)
	}
	return nil
}

// PluginCatalogURL is the index an Admin pointed this server at, or "" for the
// shipped default, which is none.
//
// The read is TOTAL: a missing row is not an error and not a state a caller has
// to handle, it is simply "no catalog", exactly as TailnetSettings reads a
// missing row as "off" (ADR-0043). Nothing about this feature may require a row
// somebody remembered to write.
func (db *DB) PluginCatalogURL() (string, error) {
	var url sql.NullString
	err := db.QueryRow(`SELECT url FROM plugin_catalog_settings WHERE id = 1`).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: reading the plugin catalog URL: %w", err)
	}
	return url.String, nil
}

// SetPluginCatalogURL writes the singleton row. An empty URL is stored as NULL so
// "cleared" and "never set" read back identically — both mean the server browses
// no catalog.
func (db *DB) SetPluginCatalogURL(url string) error {
	_, err := db.Exec(
		`INSERT INTO plugin_catalog_settings (id, url, updated_at)
		      VALUES (1, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      url        = excluded.url,
		      updated_at = datetime('now')`,
		nullString(url))
	if err != nil {
		return fmt.Errorf("store: setting the plugin catalog URL: %w", err)
	}
	return nil
}
