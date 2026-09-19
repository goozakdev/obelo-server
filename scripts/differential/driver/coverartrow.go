package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// plantCoverArtOverride writes a `library_provider_override` row naming
// `coverart` straight into the stopped 2026-09-17 server's database.
//
// # Why this is not cheating, and why it is not a normal thing to do
//
// Criterion B says an upgraded server's per-Library overrides survive and the
// Cover Art Archive's are scrubbed (issue 06's migration does the scrubbing).
// The first run of this harness discovered that the 2026-09-17 build's OWN API
// refuses to create the row the scrub is about:
//
//	PUT /libraries/{id}/enrichment-policy {"providerOverrides":{"coverart":false}}
//	→ 422 PROVIDER_NOT_AUTHORITATIVE "provider is not a supplement for this library"
//
// because `Catalog.SupplementProvidersForKind` only offers providers with
// `RequiresKey`, and the Cover Art Archive is keyless. So no server that was
// only ever driven through its API can hold such a row, and the scrub is
// defensive rather than load-bearing. That is a FINDING, reported as one.
//
// The scrub is still worth exercising, so the row is planted the only way it
// could ever have arrived — directly — with the server stopped, through the
// pure-Go SQLite driver the server itself uses. The harness says loudly that it
// did this; the alternative was to assert nothing and call the criterion proven.
func plantCoverArtOverride(dbPath, libraryID string) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := db.Exec(
		`INSERT OR REPLACE INTO library_provider_override (library_id, provider, enabled, updated_at)
		 VALUES (?, 'coverart', 0, datetime('now'))`, libraryID)
	if err != nil {
		return fmt.Errorf("planting the coverart override: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("planting the coverart override: %d rows affected", n)
	}
	return nil
}

// countProviderOverrides reports how many override rows a Library holds, and how
// many of them name `coverart`. It is read after the upgraded boot as the direct
// evidence for the scrub, alongside the API-level assertion.
func countProviderOverrides(dbPath, libraryID string) (total, coverart int, err error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	row := db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(provider = 'coverart'), 0)
		 FROM library_provider_override WHERE library_id = ?`, libraryID)
	if err := row.Scan(&total, &coverart); err != nil {
		return 0, 0, err
	}
	return total, coverart, nil
}
