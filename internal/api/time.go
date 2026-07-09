package api

import "time"

// sqliteDateTime is the layout SQLite's datetime('now') produces for TEXT
// timestamp columns: "2006-01-02 15:04:05" in UTC, with no zone marker.
const sqliteDateTime = "2006-01-02 15:04:05"

// formatTimestamp normalizes a stored timestamp string to RFC3339 (UTC, e.g.
// "2026-06-22T21:00:17Z") for the JSON API. Stored values are mixed: some
// columns default to SQLite's datetime('now') (space-separated, no zone),
// others are written from Go already in RFC3339. This is the single boundary
// where every timestamp the API emits is canonicalized, so clients always parse
// a standard format regardless of how a value was stored.
//
// It is intentionally forgiving: an empty value stays empty (so omitempty
// fields drop), an already-RFC3339 value is returned in UTC, a SQLite
// datetime value is interpreted as UTC, and anything unrecognized is returned
// unchanged rather than discarded.
func formatTimestamp(stored string) string {
	if stored == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, stored); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(sqliteDateTime, stored); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return stored
}
