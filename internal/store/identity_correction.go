package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Wrong-item identity correction (item-editing/04, ADR-0019/ADR-0014). The
// destructive Match-override apply on a Movie / Show: the folder-keyed override
// (match_overrides) is written by the match domain, but the ALREADY-scanned live
// row must also be re-keyed NOW so browse reflects the new work immediately (a
// rescan would otherwise be needed), and — because identity actually changed — the
// row's watch state is reset and its Locked fields cleared (a genuinely different
// work is a clean slate). The re-keyed identity_key is computed the same way a
// scan would (scanner.IdentityKeyFor), so the next rescan re-resolves to the SAME
// row rather than duplicating it. Everything here touches ONLY the target's own
// rows; nothing else in the catalog moves.

// RekeyTitleIdentity re-points a leaf Movie Title to a corrected identity: its
// display/parsed title, year, external id, and identity_key all become the picked
// work's, and its enrichment is reset to 'pending' so the caller's re-enrich
// resolves the new record cleanly. The caller passes the identity_key the match
// override was keyed with (scanner.IdentityKeyFor), so the live row and the
// folder override agree and a rescan updates this same row. Returns ErrNotFound
// for an unknown Title.
func (db *DB) RekeyTitleIdentity(titleID, title string, year int, tmdbID, identityKey string) error {
	res, err := db.Exec(
		`UPDATE titles SET
		     title = ?, year = ?, sort_title = ?, identity_key = ?,
		     tmdb_id = ?, imdb_id = '', enriched_title = '',
		     enrichment_status = 'pending', enrichment_source = ''
		   WHERE id = ?`,
		title, nullableYear(year), sortKey(title), identityKey, tmdbID, titleID,
	)
	if err != nil {
		return fmt.Errorf("store: rekeying title identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rekey title rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RekeyShowIdentity is RekeyTitleIdentity for a Show parent: it re-points the
// Show's title/year/external id/identity_key to the picked work and resets the
// Show's entity enrichment to 'pending' so the caller's re-enrich resolves the new
// record. Episodes re-resolve under the new identity on the next rescan (their
// watch state having been reset here). Returns ErrNotFound for an unknown Show.
func (db *DB) RekeyShowIdentity(showID, title string, year int, tmdbID, identityKey string) error {
	res, err := db.Exec(
		`UPDATE shows SET
		     title = ?, year = ?, sort_title = ?, identity_key = ?,
		     tmdb_id = ?, imdb_id = ''
		   WHERE id = ?`,
		title, nullableYear(year), sortKey(title), identityKey, tmdbID, showID,
	)
	if err != nil {
		return fmt.Errorf("store: rekeying show identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rekey show rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	// Reset the Show's entity enrichment to 'pending' so a re-enrich re-resolves the
	// new record (idempotent no-op when the Show was never enriched).
	if _, err := db.Exec(
		`UPDATE entity_enrichment SET enrichment_status = 'pending'
		   WHERE entity_type = ? AND entity_id = ?`,
		EntityShow, showID,
	); err != nil {
		return fmt.Errorf("store: resetting show enrichment: %w", err)
	}
	return nil
}

// ResetWatchStateForTitle deletes every User's watch state for one Title — the
// clean-slate step of a Wrong-item correction (a genuinely different work carries
// no prior resume/watched history, ADR-0014). Idempotent: a Title with no rows is
// a no-op.
func (db *DB) ResetWatchStateForTitle(titleID string) error {
	if _, err := db.Exec(`DELETE FROM watch_state WHERE title_id = ?`, titleID); err != nil {
		return fmt.Errorf("store: resetting watch state for title: %w", err)
	}
	return nil
}

// ResetWatchStateForShow deletes every User's watch state for every Episode Title
// under a Show — the Show-grain clean slate of a Wrong-item correction (the new
// work's episodes start unwatched). Idempotent.
func (db *DB) ResetWatchStateForShow(showID string) error {
	if _, err := db.Exec(
		`DELETE FROM watch_state WHERE title_id IN (
		     SELECT t.id FROM titles t
		       JOIN seasons s ON s.id = t.season_id
		      WHERE s.show_id = ?)`,
		showID,
	); err != nil {
		return fmt.Errorf("store: resetting watch state for show: %w", err)
	}
	return nil
}

// ClearTitleFieldLocks removes ALL of a Title's Locked fields at once — the
// bulk clear a Wrong-item correction needs (hand-edits made for the previous,
// wrong work must not stick to a genuinely different one). Distinct from the
// per-field ReleaseFieldLock (Fix label). Idempotent.
func (db *DB) ClearTitleFieldLocks(titleID string) error {
	if _, err := db.Exec(`DELETE FROM title_field_locks WHERE title_id = ?`, titleID); err != nil {
		return fmt.Errorf("store: clearing title field locks: %w", err)
	}
	return nil
}

// ClearEntityFieldLocks removes ALL of a browse-parent's Locked fields at once —
// the parent analogue of ClearTitleFieldLocks for a Show Wrong-item correction.
// Idempotent.
func (db *DB) ClearEntityFieldLocks(entityType, entityID string) error {
	if _, err := db.Exec(
		`DELETE FROM entity_field_locks WHERE entity_type = ? AND entity_id = ?`,
		entityType, entityID,
	); err != nil {
		return fmt.Errorf("store: clearing entity field locks: %w", err)
	}
	return nil
}

// AnyFilePathForShow returns one on-disk file path from any Episode under a Show,
// so the caller can derive the Show's fix-match folder anchor (the scanner keys a
// Show override by its top-level folder). ErrNotFound when the Show has no files.
func (db *DB) AnyFilePathForShow(showID string) (string, error) {
	var path string
	err := db.QueryRow(
		`SELECT f.path FROM files f
		    JOIN editions e ON e.id = f.edition_id
		    JOIN titles   t ON t.id = e.title_id
		    JOIN seasons  s ON s.id = t.season_id
		   WHERE s.show_id = ?
		   ORDER BY f.path
		   LIMIT 1`,
		showID,
	).Scan(&path)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: reading show file path: %w", err)
	}
	return path, nil
}

// sortKey is a lightweight sort-title for a re-keyed row (lower-cased title). A
// subsequent scan recomputes the canonical sort_title; this only keeps the live
// row's browse ordering sane in the interim.
func sortKey(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	// Mirror the scanner's sortTitle: strip a leading article ("the "/"an "/"a ")
	// so article-prefixed titles sort by the following word (e.g. "The Matrix"
	// files under M). Longest prefix first so "an" wins over "a".
	for _, article := range []string{"the ", "an ", "a "} {
		if strings.HasPrefix(s, article) {
			return strings.TrimSpace(s[len(article):])
		}
	}
	return s
}
