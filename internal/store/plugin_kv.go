package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The plugin-scoped key-value namespace (ADR-0058 decision 5): the one durable
// thing an Installed plugin may keep for itself.
//
// Every method takes the plugin id FIRST and it is never optional. That is not a
// convention — it is the whole of the isolation property: the id comes from the
// manifest on disk, the host supplies it, and a guest never spells it, so two
// Plugins writing the same key cannot see each other's value. There is
// deliberately no "list every namespace" or "read another plugin's key" method,
// because nothing in this server has a reason for one and the absence is what
// makes the isolation reviewable.
//
// Size caps live in the CALLER (internal/plugins), not here, for the reason the
// artwork cap does: this is the storage, and the policy about what a guest may put
// in it belongs beside the guest.

// PluginKV reads one key from a Plugin's namespace. found=false is "never
// written", which is deliberately distinct from a zero-length value — a guest
// caching "I asked and there was nothing" needs both.
func (db *DB) PluginKV(pluginID, key string) (value []byte, found bool, err error) {
	if pluginID == "" {
		return nil, false, fmt.Errorf("store: reading a plugin key with no plugin id")
	}
	var v []byte
	err = db.QueryRow(
		`SELECT value FROM plugin_kv WHERE plugin_id = ? AND key = ?`,
		pluginID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: reading plugin key %q: %w", key, err)
	}
	// A NULL-free column still scans an empty BLOB as nil; normalize so a caller
	// that only checks the slice sees the same thing it wrote.
	if v == nil {
		v = []byte{}
	}
	return v, true, nil
}

// SetPluginKV writes one key in a Plugin's namespace, replacing whatever was
// there. An upsert rather than a delete+insert so a concurrent reader never sees
// the key briefly absent.
func (db *DB) SetPluginKV(pluginID, key string, value []byte) error {
	if pluginID == "" {
		return fmt.Errorf("store: writing a plugin key with no plugin id")
	}
	if value == nil {
		value = []byte{}
	}
	_, err := db.Exec(
		`INSERT INTO plugin_kv (plugin_id, key, value, updated_at)
		      VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(plugin_id, key) DO UPDATE SET
		      value = excluded.value, updated_at = excluded.updated_at`,
		pluginID, key, value)
	if err != nil {
		return fmt.Errorf("store: writing plugin key %q: %w", key, err)
	}
	return nil
}

// DeletePluginKV removes one key. Removing a key that was never written is not an
// error: it is the state the caller asked for.
func (db *DB) DeletePluginKV(pluginID, key string) error {
	if pluginID == "" {
		return fmt.Errorf("store: deleting a plugin key with no plugin id")
	}
	if _, err := db.Exec(`DELETE FROM plugin_kv WHERE plugin_id = ? AND key = ?`, pluginID, key); err != nil {
		return fmt.Errorf("store: deleting plugin key %q: %w", key, err)
	}
	return nil
}

// deletePluginNamespaceSQL is the whole of dropping a namespace, named because it
// is run from two places: this file's DeletePluginNamespace, and the uninstall
// transaction in DeletePlugin, which needs the statement rather than the method so
// that it commits or rolls back with the Plugin's other three deletes.
const deletePluginNamespaceSQL = `DELETE FROM plugin_kv WHERE plugin_id = ?`

// DeletePluginNamespace drops everything one Plugin ever stored. It is what
// UNINSTALL runs (as part of DeletePlugin's transaction, through the statement
// above), and it is the reason the namespace is a column rather than a key prefix:
// dropping it is one statement that cannot get the prefix subtly wrong.
//
// Uninstalling and reinstalling a Plugin therefore gives it a clean namespace,
// which is the honest reading of "uninstall" — the identity-keyed artwork and
// subtitles it produced survive in the normal caches (they are the server's, not
// the Plugin's), but the Plugin's own scratch space does not.
func (db *DB) DeletePluginNamespace(pluginID string) error {
	if pluginID == "" {
		return fmt.Errorf("store: dropping a plugin namespace with no plugin id")
	}
	if _, err := db.Exec(deletePluginNamespaceSQL, pluginID); err != nil {
		return fmt.Errorf("store: dropping the namespace of plugin %q: %w", pluginID, err)
	}
	return nil
}
