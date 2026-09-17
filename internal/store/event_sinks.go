package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Event sink settings (ADR-0057 decision 6): the MUTABLE per-sink state, the
// subtitle/metadata-provider row pattern with one extra field. The sink's static
// facts — its name, that it needs a secret, the copy the settings screen shows —
// live in its Descriptor in the Plugin registry, NOT here.

// EventSinkRow is one row of the event_sinks table. Secret is the HMAC signing
// key and is NEVER surfaced by the API — only a hasSecret boolean. An empty URL
// means the sink has nowhere to post, which is why it cannot be enabled; an empty
// Events means it subscribes to nothing and the translator does no work for it.
type EventSinkRow struct {
	Slug      string
	Enabled   bool
	Secret    string
	URL       string
	Events    []string
	UpdatedAt string
}

// EventSinkUpsert is the desired mutable state to persist for one sink. The
// settings API resolves the partial-update secret semantics (omit=unchanged /
// ""=clear / value=set) into the FULL desired state before calling — the store
// writes exactly what it is given.
type EventSinkUpsert struct {
	Slug    string
	Enabled bool
	Secret  string
	URL     string
	Events  []string
}

// EventSinks lists every persisted Event sink row, ordered by slug. A sink with no
// row has never been configured — the caller treats it as disabled (ADR-0001
// offline-first).
func (db *DB) EventSinks() ([]EventSinkRow, error) {
	rows, err := db.Query(
		`SELECT slug, enabled, secret, url, events, updated_at
		   FROM event_sinks ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: listing event sinks: %w", err)
	}
	defer rows.Close()

	var out []EventSinkRow
	for rows.Next() {
		var (
			r              EventSinkRow
			secret, target sql.NullString
			events         string
		)
		if err := rows.Scan(&r.Slug, &r.Enabled, &secret, &target, &events, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning event sink: %w", err)
		}
		r.Secret = secret.String
		r.URL = target.String
		r.Events = splitEventList(events)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertEventSink writes one sink's full mutable state, inserting or replacing it
// by slug. An empty Secret/URL is stored as NULL so "cleared" and "never set" read
// back the same.
func (db *DB) UpsertEventSink(u EventSinkUpsert) error {
	_, err := db.Exec(
		`INSERT INTO event_sinks (slug, enabled, secret, url, events, updated_at)
		      VALUES (?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(slug) DO UPDATE SET
		      enabled    = excluded.enabled,
		      secret     = excluded.secret,
		      url        = excluded.url,
		      events     = excluded.events,
		      updated_at = datetime('now')`,
		u.Slug, u.Enabled, nullString(u.Secret), nullString(u.URL), joinEventList(u.Events))
	if err != nil {
		return fmt.Errorf("store: upserting event sink %q: %w", u.Slug, err)
	}
	return nil
}

// joinEventList renders the subscribed-event list for storage. Order is the
// Admin's, preserved, so the settings screen reads back what it wrote.
func joinEventList(events []string) string { return strings.Join(events, ",") }

// splitEventList parses the stored list back, dropping empties so a "" column and
// a never-written one both read as no subscription at all.
func splitEventList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
