package store

import (
	"database/sql"
	"fmt"
)

// AllPluginSettings reads every Plugin's declared settings in one query, keyed by
// plugin id, each list ordered by key as PluginSettings orders it. The Plugins
// listing wants all of them at once, and one read per plugin is a query per row of
// a screen an Admin opens when something is already wrong.
func (db *DB) AllPluginSettings() (map[string][]PluginSetting, error) {
	rows, err := db.Query(
		`SELECT plugin_id, key, value, secret FROM plugin_settings ORDER BY plugin_id, key`)
	if err != nil {
		return nil, fmt.Errorf("store: reading every plugin's settings: %w", err)
	}
	defer rows.Close()

	out := map[string][]PluginSetting{}
	for rows.Next() {
		var id string
		var s PluginSetting
		var value sql.NullString
		if err := rows.Scan(&id, &s.Key, &value, &s.Secret); err != nil {
			return nil, fmt.Errorf("store: scanning a plugin setting: %w", err)
		}
		s.Value = value.String
		out[id] = append(out[id], s)
	}
	return out, rows.Err()
}
