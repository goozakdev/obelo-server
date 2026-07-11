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

	// MetadataLanguage overrides the server-wide preferred metadata language for
	// just this Library (issue 02). nil = inherit the global language live; a
	// non-empty value localizes this Library's Enrichment (a foreign-film library).
	MetadataLanguage *string

	// AuthoritativeProvider is the registry slug of the Full provider that LEADS this
	// Library's Enrichment (issue 03). nil = inherit the kind's global default (TMDB
	// for video, MusicBrainz for music); a slug repoints the leader (an anime library
	// leading with AniDB). The resolver enforces the kind constraint and the
	// always-active-if-keyed / unreachable-fallback rules (ADR-0027).
	AuthoritativeProvider *string

	// SupplementOverrides is the per-provider Supplement tri-state (issue 05): a slug
	// present maps to a forced on(true)/off(false) override; a slug ABSENT inherits
	// the provider's global enabled state. nil (the common case) = inherit every
	// provider. Populated by the store from the per-(Library, provider) override table
	// in issue 05; the resolver already honors it (force-on a globally-disabled-but-
	// keyed source, force-off a noisy one — with force-off of the current
	// Authoritative provider a no-op, ADR-0027).
	SupplementOverrides map[string]bool
}

// LibraryEnrichmentPolicy reads a Library's stored policy. A Library with no row
// has an empty policy — every key inherits the global config — so an absent row is
// the zero value with no error (never treated as ErrNotFound; "no overrides" is a
// valid, common state). The caller validates the Library exists separately.
func (db *DB) LibraryEnrichmentPolicy(libraryID string) (LibraryEnrichmentPolicy, error) {
	var (
		enrichEnabled    sql.NullBool
		metadataLanguage sql.NullString
		authoritative    sql.NullString
	)
	err := db.QueryRow(
		`SELECT enrich_enabled, metadata_language, authoritative_provider
		   FROM library_enrichment_policy WHERE library_id = ?`,
		libraryID).Scan(&enrichEnabled, &metadataLanguage, &authoritative)
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
	if metadataLanguage.Valid {
		v := metadataLanguage.String
		out.MetadataLanguage = &v
	}
	if authoritative.Valid {
		v := authoritative.String
		out.AuthoritativeProvider = &v
	}
	// SupplementOverrides is populated from the per-(Library, provider) override
	// table in issue 05; until then it stays nil (inherit every provider).
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

// SetLibraryMetadataLanguage sets (or clears) the Library's metadata-language
// override (issue 02). A nil language clears the key back to inherit (stored as
// NULL); a non-nil value is a deliberate override. Upserts the policy row,
// touching ONLY the metadata_language column so a coexisting enrich_enabled (or a
// later authoritative) override on the same row is left intact. Inheritance is
// NULL, never a sentinel — so clearing is genuinely distinguishable from a set
// value on read.
func (db *DB) SetLibraryMetadataLanguage(libraryID string, language *string) error {
	var val any // nil → SQL NULL (inherit); *string → the deliberate override
	if language != nil {
		val = *language
	}
	_, err := db.Exec(
		`INSERT INTO library_enrichment_policy (library_id, metadata_language, updated_at)
		      VALUES (?, ?, datetime('now'))
		 ON CONFLICT(library_id) DO UPDATE SET
		      metadata_language = excluded.metadata_language,
		      updated_at        = datetime('now')`,
		libraryID, val)
	if err != nil {
		return fmt.Errorf("store: setting library metadata language: %w", err)
	}
	return nil
}

// SetLibraryAuthoritativeProvider sets (or clears) the Library's Authoritative-
// provider pointer (issue 03). A nil slug clears the key back to inherit the kind's
// global default (stored as NULL); a non-nil slug is a deliberate override. Upserts
// the policy row, touching ONLY the authoritative_provider column so the coexisting
// enrich_enabled / metadata_language overrides on the same row are left intact.
// Inheritance is NULL, never a sentinel — so clearing is distinguishable from a set
// value on read. The caller (API) validates the slug is a usable Full provider of
// the Library's kind before calling.
func (db *DB) SetLibraryAuthoritativeProvider(libraryID string, slug *string) error {
	var val any // nil → SQL NULL (inherit the kind default); *string → the slug
	if slug != nil {
		val = *slug
	}
	_, err := db.Exec(
		`INSERT INTO library_enrichment_policy (library_id, authoritative_provider, updated_at)
		      VALUES (?, ?, datetime('now'))
		 ON CONFLICT(library_id) DO UPDATE SET
		      authoritative_provider = excluded.authoritative_provider,
		      updated_at             = datetime('now')`,
		libraryID, val)
	if err != nil {
		return fmt.Errorf("store: setting library authoritative provider: %w", err)
	}
	return nil
}
