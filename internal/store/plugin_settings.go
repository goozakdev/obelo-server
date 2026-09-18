package store

import (
	"database/sql"
	"fmt"
)

// An Installed plugin's MANIFEST-DECLARED settings (.scratch/plugin-system issue
// 13): the rows migration 0067 created and left unwritten, filled at last.
//
// The fixed five-field Settings of ADR-0057 are NOT here and must not move here.
// They live in the Extension point's own table — an Installed Event sink is an
// `event_sinks` row, an Installed Metadata provider a `metadata_providers` row —
// keyed by the Plugin id, so the settings API, the screens and the Managers work
// on a Plugin and a Built-in with no difference at all. What lives here is the
// other half: the fields a manifest declares for itself, whose names this server
// cannot know at migration time.
//
// # The value column holds JSON, uniformly
//
// A declared field can be a string, a bool, a number or an array of strings, so
// the column holds the field's value AS JSON — `"eu-west"`, `true`, `7`,
// `["a","b"]` — rather than a per-type text spelling. One encoding means one
// decoder, and it means a value survives a manifest that later retypes its field:
// the row still reads back as the JSON it was written as, and the next save is
// what refuses it. See pluginapi/v1/settings_schema.go for what each field type's
// JSON is.
//
// `secret` marks a value the settings API must never return. It is exactly the
// protection `metadata_providers.api_key` and `event_sinks.secret` have — masking
// and access control, not a column cipher; nothing in this repo encrypts a
// credential column (see migrations/0067_plugins.sql).

// PluginSetting is one manifest-declared setting of one Plugin: the key its
// manifest declared, the value as JSON, and whether it is a secret the API must
// never hand back.
type PluginSetting struct {
	Key    string
	Value  string
	Secret bool
}

// PluginSettings reads one Plugin's declared settings, ordered by key so a caller
// rendering them sees a stable order whatever the manifest's is.
func (db *DB) PluginSettings(pluginID string) ([]PluginSetting, error) {
	rows, err := db.Query(
		`SELECT key, value, secret FROM plugin_settings WHERE plugin_id = ? ORDER BY key`, pluginID)
	if err != nil {
		return nil, fmt.Errorf("store: reading the settings of plugin %q: %w", pluginID, err)
	}
	defer rows.Close()

	var out []PluginSetting
	for rows.Next() {
		var s PluginSetting
		var value sql.NullString
		if err := rows.Scan(&s.Key, &value, &s.Secret); err != nil {
			return nil, fmt.Errorf("store: scanning a setting of plugin %q: %w", pluginID, err)
		}
		s.Value = value.String
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReplacePluginSettings writes a Plugin's declared settings, replacing everything
// it had. One transaction, and a whole-set replace rather than a per-key upsert
// for one reason: a field the manifest no longer declares must stop existing, and
// a delta write leaves it in the table forever, invisible to every screen and
// still handed to the guest.
//
// The CALLER decides what the whole set is, which is where a secret is preserved:
// the settings API never returns a stored secret, so a form that submits nothing
// for a secret field means "leave it alone", and the handler merges the stored
// value back in before calling this.
func (db *DB) ReplacePluginSettings(pluginID string, values []PluginSetting) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: saving the settings of plugin %q: %w", pluginID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM plugin_settings WHERE plugin_id = ?`, pluginID); err != nil {
		return fmt.Errorf("store: saving the settings of plugin %q: %w", pluginID, err)
	}
	for _, v := range values {
		if _, err := tx.Exec(
			`INSERT INTO plugin_settings (plugin_id, key, value, secret, updated_at)
			      VALUES (?, ?, ?, ?, datetime('now'))`,
			pluginID, v.Key, v.Value, v.Secret); err != nil {
			return fmt.Errorf("store: saving the setting %q of plugin %q: %w", v.Key, pluginID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: saving the settings of plugin %q: %w", pluginID, err)
	}
	return nil
}
