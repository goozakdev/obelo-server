package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Installed plugins (ADR-0058, .scratch/plugin-system issue 10): the record of
// what an Admin installed, which is NOT the record of what is loaded. The
// directory under <dataDir>/plugins/ is the truth about what this server will
// compile; these rows carry what the files cannot say — the Admin's enable
// switch, the provenance, the install time, and the last thing that went wrong.
//
// See migrations/0001_init.sql's `plugins` table for why each column exists.

// PluginRow is one row of the plugins table. Name, Version, APIVersion and
// Provides are copied from the manifest at install time so the Plugins screen can
// name a Plugin whose module will not load at all — which is the Plugin an Admin
// most needs named.
type PluginRow struct {
	ID          string
	Name        string
	Version     string
	APIVersion  int
	Provides    []string
	Enabled     bool
	LastError   string
	Source      string
	InstalledAt string
	// Publisher and KeyID are who SIGNED this Plugin, and they are written only
	// when the signature verified against a key an Admin had pinned
	// (.scratch/plugin-system issue 15). Empty means nobody verified anything —
	// either no signature travelled with it, or none was pinned at the time — and
	// a screen must read them that way rather than as "unsigned".
	Publisher string
	KeyID     string
	// Origin is who put this plugin here: "bundled" for one the server shipped
	// (ADR-0059), "admin" for one a person uploaded or pasted a URL for. It is the
	// fact the boot-time re-assert turns on — a bundled row's files are replaced
	// when the server ships a newer version, an admin row's are never touched — and
	// the one sentence the Plugins screen shows instead of an upload name.
	//
	// See migrations/0001_init.sql's `plugins` table for why the column defaults
	// to "admin" rather than to the empty string.
	Origin string
}

// PluginInsert is a newly installed Plugin. There is no update form: a Plugin is
// installed, switched on or off, and uninstalled — reinstalling over an existing
// id is refused before anything is written, so this only ever inserts.
type PluginInsert struct {
	ID         string
	Name       string
	Version    string
	APIVersion int
	Provides   []string
	Source     string
	// Origin is "bundled" or "admin" (see PluginRow.Origin). The EMPTY value means
	// "admin", so every caller written before Bundled plugins existed keeps
	// recording what it always recorded.
	Origin string
}

// Plugins lists every Installed plugin row, ordered by id — the same order the
// loader walks the directory in, so the screen and the log agree.
func (db *DB) Plugins() ([]PluginRow, error) {
	rows, err := db.Query(
		`SELECT id, name, version, api_version, provides, enabled, last_error, source, installed_at,
		        publisher, key_id, origin
		   FROM plugins ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing plugins: %w", err)
	}
	defer rows.Close()

	var out []PluginRow
	for rows.Next() {
		var (
			r         PluginRow
			provides  string
			lastError sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.Version, &r.APIVersion, &provides,
			&r.Enabled, &lastError, &r.Source, &r.InstalledAt, &r.Publisher, &r.KeyID, &r.Origin); err != nil {
			return nil, fmt.Errorf("store: scanning plugin: %w", err)
		}
		r.LastError = lastError.String
		r.Provides = decodeProvides(provides)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DisabledPluginIDs is the ids an Admin has switched OFF. It is the narrowest
// question the composition root asks this table, and it asks it at boot, before
// anything is registered: a Plugin switched off is never registered at all, which
// is what makes "disable stops delivery" true of the whole server rather than of
// one delivery path that remembered to check.
func (db *DB) DisabledPluginIDs() ([]string, error) {
	rows, err := db.Query(`SELECT id FROM plugins WHERE enabled = 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing disabled plugins: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning disabled plugin: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// InsertPlugin records a newly installed Plugin, enabled. It FAILS on a duplicate
// id rather than replacing — the installer refuses a duplicate before it writes a
// byte, and a conflict reaching here would mean two installs raced past that
// check, which is worth an error rather than a silent overwrite of somebody's
// provenance.
//
// It also CLEARS the declined mark for the id, in the same transaction. A plugin
// arriving under an id an Admin once declined means the Admin changed their mind
// — either by pressing "reinstall the shipped version" or by uploading one of
// their own — and a mark left behind would make the next boot's re-assert skip a
// row that is now installed and, if the Admin's copy were later removed, keep the
// shipped one away for reasons nobody could see.
func (db *DB) InsertPlugin(p PluginInsert) error {
	provides, err := json.Marshal(nonNilProvides(p.Provides))
	if err != nil {
		return fmt.Errorf("store: encoding what plugin %q provides: %w", p.ID, err)
	}
	origin := p.Origin
	if origin == "" {
		origin = "admin"
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: recording plugin %q: %w", p.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO plugins (id, name, version, api_version, provides, enabled, last_error, source, installed_at, origin)
		      VALUES (?, ?, ?, ?, ?, 1, NULL, ?, datetime('now'), ?)`,
		p.ID, p.Name, p.Version, p.APIVersion, string(provides), p.Source, origin); err != nil {
		return fmt.Errorf("store: recording plugin %q: %w", p.ID, err)
	}
	if _, err := tx.Exec(`DELETE FROM declined_plugins WHERE id = ?`, p.ID); err != nil {
		return fmt.Errorf("store: recording plugin %q: %w", p.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: recording plugin %q: %w", p.ID, err)
	}
	return nil
}

// UpdatePluginManifest re-copies the manifest facts onto an existing row after
// its files were replaced in place — which is what the boot-time re-assert does
// when this build ships a newer version of a Bundled plugin (ADR-0059 decision 1).
//
// It is NOT an upsert and it deliberately touches nothing else. The id is the
// same, so the settings row, the per-Library overrides, the item pins, the Admin's
// enable switch, the install time and the origin all belong to the plugin that is
// still there; what changed is the four facts copied out of the manifest at
// install time, and leaving them stale would make the Plugins screen name the
// version that is no longer on disk.
func (db *DB) UpdatePluginManifest(p PluginInsert) error {
	provides, err := json.Marshal(nonNilProvides(p.Provides))
	if err != nil {
		return fmt.Errorf("store: encoding what plugin %q provides: %w", p.ID, err)
	}
	_, err = db.Exec(
		`UPDATE plugins SET name = ?, version = ?, api_version = ?, provides = ?, last_error = NULL
		  WHERE id = ?`,
		p.Name, p.Version, p.APIVersion, string(provides), p.ID)
	if err != nil {
		return fmt.Errorf("store: updating plugin %q: %w", p.ID, err)
	}
	return nil
}

// DeclinedPluginIDs is every Bundled plugin id an Admin has uninstalled, sorted.
// The boot-time re-assert reads it BEFORE it writes anything: a declined id is
// left alone, which is what makes "uninstall" mean uninstalled rather than "until
// you restart".
func (db *DB) DeclinedPluginIDs() ([]string, error) {
	rows, err := db.Query(`SELECT id FROM declined_plugins ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing declined plugins: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning declined plugin: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeclinePlugin remembers that the Admin removed the shipped plugin with this id.
// Declining one that is already declined is not an error — it is the state the
// caller asked for.
func (db *DB) DeclinePlugin(id string) error {
	_, err := db.Exec(
		`INSERT INTO declined_plugins (id, declined_at) VALUES (?, datetime('now'))
		 ON CONFLICT(id) DO NOTHING`, id)
	if err != nil {
		return fmt.Errorf("store: declining plugin %q: %w", id, err)
	}
	return nil
}

// UndeclinePlugin forgets the mark, so the next re-assert installs the shipped
// plugin again. It reports whether there was a mark to forget, so the API can
// tell "brought it back" from "it was never gone".
func (db *DB) UndeclinePlugin(id string) (bool, error) {
	res, err := db.Exec(`DELETE FROM declined_plugins WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("store: reinstating plugin %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: reinstating plugin %q: %w", id, err)
	}
	return n > 0, nil
}

// SetPluginEnabled flips the Admin's switch. It reports whether a row was there
// to flip, so the API can tell "switched off" from "no such Plugin" without a
// second read.
func (db *DB) SetPluginEnabled(id string, enabled bool) (bool, error) {
	res, err := db.Exec(
		`UPDATE plugins SET enabled = ? WHERE id = ?`, enabled, id)
	if err != nil {
		return false, fmt.Errorf("store: setting plugin %q enabled: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: setting plugin %q enabled: %w", id, err)
	}
	return n > 0, nil
}

// SetPluginLastError caches the loader's last sentence on the row so the screen
// can show it after a restart, before anything has been called. Passing "" clears
// it, which is what Re-enable does.
//
// It is a CACHE and never a source of truth: the running loader owns the real
// answer and overwrites this whenever it is asked for a fresh view.
func (db *DB) SetPluginLastError(id, message string) error {
	_, err := db.Exec(
		`UPDATE plugins SET last_error = ? WHERE id = ?`, nullString(message), id)
	if err != nil {
		return fmt.Errorf("store: recording the last error of plugin %q: %w", id, err)
	}
	return nil
}

// DeletePlugin removes a Plugin's record entirely: its row, its generic settings,
// the fixed-shape settings row its Extension point wrote under the same slug, the
// key-value namespace the guest itself wrote, and every per-Library Enrichment
// override that names the slug.
//
// UNINSTALL FORGETS; DISABLE REMEMBERS. That is the decision (.scratch/
// plugin-system issue 17), and it is what makes the list below a list rather than
// a judgement call per table: "uninstall deletes files and rows" is what the PRD
// promised, a key left behind is a credential the operator believes is gone, and a
// reinstall that arrives already enabled and already keyed is a surprise in both
// directions. An operator who wants the settings kept has a verb for it — Disable
// — and it touches none of this. Every statement is keyed to the uninstalled
// Plugin's own id, so the eight Built-ins' rows and every other Installed Plugin's
// are out of reach by construction.
//
// THE EVENT SINK ROW IS THE ONE PEOPLE FORGET. An Installed Event sink's settings
// live in event_sinks keyed by the Plugin id, so leaving it behind would strand a
// signing secret and a target URL under a slug no Plugin claims — a row the sink
// Manager silently skips forever and that would quietly come back to life if the
// same Plugin were ever reinstalled.
//
// THE TWO PROVIDER TABLES ARE THE SAME ROW, ONE SEAM OVER. An Installed Metadata
// provider's enable switch, API key and base-URL override live in
// metadata_providers keyed by slug, and an Installed Subtitle provider's in
// subtitle_providers — exactly where a Built-in's do.
// Until issue 17 an uninstall left both behind, so the settings screens skipped the
// row forever (nothing registers that slug any more) while a reinstall under the
// same id picked the previous operator's credential straight back up. They go for
// the sink row's reason, and they now go WITH it: an Installed sink forgetting its
// secret while an Installed provider remembered its key was an asymmetry, not a
// policy.
//
// THE PER-LIBRARY ENRICHMENT POLICY NAMES SLUGS TOO (ADR-0027), in two shapes, and
// both stop naming a Plugin that no longer exists:
//   - library_provider_override rows, the per-Library Supplement tri-state, are
//     DELETED — a forced on/off for a source nobody can call is not an opinion the
//     next Plugin to claim that slug inherits.
//   - library_enrichment_policy.authoritative_provider, the Authoritative-provider
//     pointer, is CLEARED BACK TO NULL (inherit the kind's default lead). The
//     resolver already falls back when the slug is not in the registry, so this is
//     not what keeps enrichment running; it is what keeps the policy RESPONSE from
//     echoing a dead slug back to the Admin as their deliberate choice.
//
// THE KEY-VALUE NAMESPACE IS THE LAST ONE, and it is the odd one out, because the
// rows are the GUEST's rather than the operator's: cursors, etags and small caches
// nobody ever typed. Left behind, a Plugin reinstalled under the same id — by the
// same author, or by anyone who picks the same slug — reads them back through
// kv_get as if it had written them. Uninstall leaves exactly the identity-keyed
// artwork and subtitles the Plugin produced (those are the Library's now,
// ADR-0007) and nothing else.
//
// One transaction, because a half-removed Plugin is worse than either outcome.
func (db *DB) DeletePlugin(id string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: removing plugin %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM plugin_settings WHERE plugin_id = ?`,
		// The same one statement DeletePluginNamespace runs, shared rather than
		// spelled twice so there is one WHERE clause to get right — and run HERE,
		// inside the transaction, so a failure anywhere in this list leaves the
		// Plugin whole rather than half uninstalled.
		deletePluginNamespaceSQL,
		`DELETE FROM event_sinks WHERE slug = ?`,
		`DELETE FROM metadata_providers WHERE slug = ?`,
		`DELETE FROM subtitle_providers WHERE slug = ?`,
		`DELETE FROM library_provider_override WHERE provider = ?`,
		// The one UPDATE in the list: the Authoritative pointer is a COLUMN on a row
		// that carries a Library's other policy keys, so deleting the row would take
		// enrich_enabled and metadata_language with it. NULL is inherit (ADR-0027's
		// Model A invariant), which is precisely where a Library whose lead was
		// uninstalled belongs.
		`UPDATE library_enrichment_policy
		    SET authoritative_provider = NULL, updated_at = datetime('now')
		  WHERE authoritative_provider = ?`,
		`DELETE FROM plugins WHERE id = ?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return fmt.Errorf("store: removing plugin %q: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: removing plugin %q: %w", id, err)
	}
	return nil
}

// decodeProvides reads the JSON array back, tolerating a column that is empty or
// unreadable: what a Plugin provides is a display fact here (the manifest on disk
// is authoritative), so a bad value costs a line on a screen and never a listing.
func decodeProvides(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// nonNilProvides keeps the column a JSON array rather than the four bytes "null",
// so a reader never has to handle two spellings of "nothing".
func nonNilProvides(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
