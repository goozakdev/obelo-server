package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Per-Library Enrichment policy (ADR-0027): a SPARSE set of deltas over the
// server-wide Enrichment configuration. Each key is nullable — NULL (or an absent
// row) means "inherit the global configuration live"; a stored value is a
// deliberate override. This first slice carries only enrich_enabled; later slices
// add metadata_language, authoritative_provider, and the per-provider override
// table. It follows the library_access / metadata_settings typed-store pattern.

// LibraryEnrichmentPolicy is one Library's stored Enrichment-policy deltas. Every
// field is a POINTER so a NULL column ("inherit the global config live") is
// distinguishable from a deliberately-set value — the Model A invariant that keeps
// "inherits the defaults" from being confused with "froze the defaults" (ADR-0027).
// The zero value (every field nil) is the empty policy: inherit everything.
type LibraryEnrichmentPolicy struct {
	// EnrichEnabled overrides whether the Library enriches at all. nil = inherit
	// (enrich per the global config); false is the ONLY way to stop a Library
	// enriching (no outbound calls, nothing filed); true forces the Library on
	// (but still inherits which providers/keys are globally usable).
	EnrichEnabled *bool
}

// LibraryEnrichmentPolicy reads a Library's stored policy. A Library with no row
// has an empty policy — every key inherits the global config — so an absent row is
// the zero value with no error (never treated as ErrNotFound; "no overrides" is a
// valid, common state). The caller validates the Library exists separately.
func (db *DB) LibraryEnrichmentPolicy(libraryID string) (LibraryEnrichmentPolicy, error) {
	var enrichEnabled sql.NullBool
	err := db.QueryRow(
		`SELECT enrich_enabled FROM library_enrichment_policy WHERE library_id = ?`,
		libraryID).Scan(&enrichEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return LibraryEnrichmentPolicy{}, nil // no row = empty policy = inherit all
	}
	if err != nil {
		return LibraryEnrichmentPolicy{}, fmt.Errorf("store: reading library enrichment policy: %w", err)
	}
	var out LibraryEnrichmentPolicy
	if enrichEnabled.Valid {
		v := enrichEnabled.Bool
		out.EnrichEnabled = &v
	}
	return out, nil
}

// SetLibraryEnrichEnabled sets (or clears) the Library's enrich-on/off override.
// A nil enabled clears the key back to inherit (stored as NULL); a non-nil value
// is a deliberate override. Upserts the policy row, touching ONLY the
// enrich_enabled column so a future language / authoritative override on the same
// row is left intact. Inheritance is NULL, never a sentinel value — so clearing is
// genuinely distinguishable from "set to off" on read.
func (db *DB) SetLibraryEnrichEnabled(libraryID string, enabled *bool) error {
	var val any // nil → SQL NULL (inherit); *bool → 0/1 (deliberate override)
	if enabled != nil {
		val = *enabled
	}
	_, err := db.Exec(
		`INSERT INTO library_enrichment_policy (library_id, enrich_enabled, updated_at)
		      VALUES (?, ?, datetime('now'))
		 ON CONFLICT(library_id) DO UPDATE SET
		      enrich_enabled = excluded.enrich_enabled,
		      updated_at     = datetime('now')`,
		libraryID, val)
	if err != nil {
		return fmt.Errorf("store: setting library enrich-enabled: %w", err)
	}
	return nil
}
