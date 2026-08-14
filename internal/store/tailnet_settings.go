package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TailnetSettings is the persisted Tailnet remote-access configuration (ADR-0043),
// read from the singleton tailnet_settings row. Enabled is the DESIRED state —
// the single input the connect/disconnect/forget/boot state machine reconciles
// against — not an observation of whether the node is currently up; the live
// condition is tailnet.Status, which is never persisted.
//
// There is no auth-key field here and no column behind one. A pre-authorized join
// key is read from the environment at join time and dropped.
type TailnetSettings struct {
	Enabled bool
	// Hostname is the MagicDNS label this Server joins under, already sanitized to
	// a legal DNS label when it was written.
	Hostname string
	// ControlURL is the coordination server, "" for Tailscale's own.
	ControlURL string
	// HTTPSEnabled opts into tailnet :443 (issue 03).
	HTTPSEnabled bool
	UpdatedAt    string
}

// TailnetSettingsUpsert is the full desired state to persist. The settings API
// resolves partial-update semantics (omit = unchanged) against the current row
// BEFORE calling — the store writes exactly what it is given, mirroring
// UpsertMetadataProvider.
type TailnetSettingsUpsert struct {
	Enabled      bool
	Hostname     string
	ControlURL   string
	HTTPSEnabled bool
}

// TailnetSettings reads the singleton settings row. A missing row — the state of
// every install that has never seeded, and of any narrow test that skipped the
// seed — comes back as the zero value: disabled, no hostname, no control URL. The
// read is TOTAL on purpose: nothing about this feature may require a row to exist
// before the server can answer a question about it (ADR-0043's "off by default"
// is a property of the shipped default, not of a row somebody remembered to write).
func (db *DB) TailnetSettings() (TailnetSettings, error) {
	var (
		out        TailnetSettings
		controlURL sql.NullString
	)
	err := db.QueryRow(
		`SELECT enabled, hostname, control_url, https_enabled, updated_at
		   FROM tailnet_settings WHERE id = 1`,
	).Scan(&out.Enabled, &out.Hostname, &controlURL, &out.HTTPSEnabled, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TailnetSettings{}, nil
	}
	if err != nil {
		return TailnetSettings{}, fmt.Errorf("store: reading tailnet settings: %w", err)
	}
	out.ControlURL = controlURL.String
	return out, nil
}

// SetTailnetSettings writes the full desired state into the guarded singleton row
// (id = 1). An empty control URL is stored as NULL so "cleared" and "never set"
// read back identically (both mean Tailscale's own coordination server).
func (db *DB) SetTailnetSettings(u TailnetSettingsUpsert) error {
	_, err := db.Exec(
		`INSERT INTO tailnet_settings (id, enabled, hostname, control_url, https_enabled, updated_at)
		      VALUES (1, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      enabled       = excluded.enabled,
		      hostname      = excluded.hostname,
		      control_url   = excluded.control_url,
		      https_enabled = excluded.https_enabled,
		      updated_at    = datetime('now')`,
		u.Enabled, u.Hostname, nullString(u.ControlURL), u.HTTPSEnabled)
	if err != nil {
		return fmt.Errorf("store: setting tailnet settings: %w", err)
	}
	return nil
}

// SetTailnetEnabled writes ONLY the desired-state flag, leaving the hostname,
// control URL, and HTTPS opt-in untouched. It exists as its own writer because
// Connect and Disconnect are about one column: a read-modify-write of the whole
// row would let a connect racing a settings save quietly restore the hostname the
// operator had just changed.
//
// It creates the row if absent, so connecting on an install whose settings were
// never seeded still records the desire — otherwise a restart would silently drop
// the operator back out of their own Tailnet.
func (db *DB) SetTailnetEnabled(enabled bool) error {
	_, err := db.Exec(
		`INSERT INTO tailnet_settings (id, enabled, updated_at)
		      VALUES (1, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      enabled    = excluded.enabled,
		      updated_at = datetime('now')`,
		enabled)
	if err != nil {
		return fmt.Errorf("store: setting tailnet enabled: %w", err)
	}
	return nil
}

// TailnetSettingsEmpty reports whether the settings row has never been written —
// the first-boot signal. The app seeds from config.Config exactly once, only when
// this is true, after which the DB is authoritative and the OBELO_TAILSCALE_* env
// vars are ignored at runtime (ADR-0043, the metadata-provider pattern).
func (db *DB) TailnetSettingsEmpty() (bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tailnet_settings`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: checking tailnet settings empty: %w", err)
	}
	return n == 0, nil
}
