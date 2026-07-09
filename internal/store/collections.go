package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Admin-curated, shared Collections (collections-playlists 01). A Collection is
// a named, unordered, cross-kind grouping of Titles keyed to the stable Title id
// (ADR-0014). These methods are the persistence the organize domain reads/writes;
// the organize Service speaks domain values over them and the api layer decorates
// the resolved member Titles exactly like a browse grid.

// ErrUnknownTitle means an item-add named a Title id that does not exist. It is
// distinct from ErrNotFound (an unknown Collection) so the api layer can answer
// 422 (a bad title in the set) versus 404 (no such Collection). The add validates
// every id before inserting any, so a bad id leaves the membership set unchanged.
var ErrUnknownTitle = errors.New("store: unknown title")

// Collection is a shared, Admin-curated grouping of Titles (CONTEXT.md). It has
// no owner — it belongs to the server. Members live in collection_items.
type Collection struct {
	ID          string
	Name        string
	Description string
	CreatedAt   string
	UpdatedAt   string
}

// CreateCollection inserts a Collection with the given id, name, and description
// and returns the stored row (with its server-assigned timestamps).
func (db *DB) CreateCollection(id, name, description string) (Collection, error) {
	if _, err := db.Exec(
		`INSERT INTO collections (id, name, description) VALUES (?, ?, ?)`,
		id, name, description,
	); err != nil {
		return Collection{}, fmt.Errorf("store: inserting collection: %w", err)
	}
	return db.CollectionByID(id)
}

// UpdateCollection renames a Collection and replaces its description, bumping
// updated_at. ErrNotFound for an unknown id.
func (db *DB) UpdateCollection(id, name, description string) (Collection, error) {
	res, err := db.Exec(
		`UPDATE collections SET name = ?, description = ?, updated_at = datetime('now') WHERE id = ?`,
		name, description, id,
	)
	if err != nil {
		return Collection{}, fmt.Errorf("store: updating collection: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Collection{}, fmt.Errorf("store: updating collection: %w", err)
	}
	if n == 0 {
		return Collection{}, ErrNotFound
	}
	return db.CollectionByID(id)
}

// DeleteCollection removes a Collection; its membership rows cascade away
// (migration 0016). ErrNotFound for an unknown id.
func (db *DB) DeleteCollection(id string) error {
	res, err := db.Exec(`DELETE FROM collections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting collection: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting collection: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CollectionByID returns one Collection, or ErrNotFound.
func (db *DB) CollectionByID(id string) (Collection, error) {
	var c Collection
	err := db.QueryRow(
		`SELECT id, name, description, created_at, updated_at FROM collections WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Collection{}, ErrNotFound
	}
	if err != nil {
		return Collection{}, fmt.Errorf("store: reading collection: %w", err)
	}
	return c, nil
}

// ListCollections returns every Collection, newest-created first.
func (db *DB) ListCollections() ([]Collection, error) {
	rows, err := db.Query(
		`SELECT id, name, description, created_at, updated_at FROM collections
		   ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing collections: %w", err)
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning collection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddCollectionItems adds titleIDs to a Collection idempotently. It validates the
// Collection exists (ErrNotFound) and EVERY Title id exists (ErrUnknownTitle)
// BEFORE inserting any, so a single bad id is a clean rejection that leaves the
// membership set unchanged (mirrors ReplaceLibraryAccess). The insert is
// INSERT OR IGNORE over the UNIQUE(collection_id, title_id), so re-adding an
// existing member is a no-op (a Collection is a set, story 6). An empty titleIDs
// is a no-op success. Bumps updated_at when at least one id was supplied.
func (db *DB) AddCollectionItems(collectionID string, titleIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin add collection items: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var one int
	err = tx.QueryRow(`SELECT 1 FROM collections WHERE id = ?`, collectionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: validating collection %q: %w", collectionID, err)
	}

	// Validate-all-then-insert: every Title must exist before any row is written.
	for _, tid := range titleIDs {
		err := tx.QueryRow(`SELECT 1 FROM titles WHERE id = ?`, tid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownTitle
		}
		if err != nil {
			return fmt.Errorf("store: validating title %q: %w", tid, err)
		}
	}
	for _, tid := range titleIDs {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO collection_items (collection_id, title_id) VALUES (?, ?)`,
			collectionID, tid,
		); err != nil {
			return fmt.Errorf("store: adding collection item %q: %w", tid, err)
		}
	}
	if len(titleIDs) > 0 {
		if _, err := tx.Exec(
			`UPDATE collections SET updated_at = datetime('now') WHERE id = ?`, collectionID,
		); err != nil {
			return fmt.Errorf("store: bumping collection updated_at: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit add collection items: %w", err)
	}
	return nil
}

// RemoveCollectionItem removes one Title from a Collection. Removing a Title that
// is not a member is a harmless no-op (idempotent) — the caller validates the
// Collection exists separately, so this never reports membership absence as an
// error. Bumps updated_at when a row was actually removed.
func (db *DB) RemoveCollectionItem(collectionID, titleID string) error {
	res, err := db.Exec(
		`DELETE FROM collection_items WHERE collection_id = ? AND title_id = ?`,
		collectionID, titleID,
	)
	if err != nil {
		return fmt.Errorf("store: removing collection item: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		if _, err := db.Exec(
			`UPDATE collections SET updated_at = datetime('now') WHERE id = ?`, collectionID,
		); err != nil {
			return fmt.Errorf("store: bumping collection updated_at: %w", err)
		}
	}
	return nil
}

// CollectionMemberIDs returns a Collection's member Title ids in stable
// sort_title order (joined to titles for the key). It includes Missing (hidden)
// members — the caller filters those out via ResolveVisibleTitles — so the
// ordering is computed once over the full membership and the visible view is a
// stable subsequence of it.
func (db *DB) CollectionMemberIDs(collectionID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT ci.title_id FROM collection_items ci
		   JOIN titles t ON t.id = ci.title_id
		  WHERE ci.collection_id = ?
		  ORDER BY t.sort_title, t.id`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("store: listing collection members: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning collection member: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveVisibleTitles maps Title ids to their enriched-summary Title rows,
// keyed by id, OMITTING any that are Missing (hidden = 1, ADR-0008) or no longer
// exist, AND any the viewer's access filter excludes (a member in an ungranted
// Library or above the Rating ceiling). It carries the same enrichment fields the
// browse list reads (enrichedTitleColumns), so a Collection/Playlist member
// decorates to the exact same titleSummary a browse grid shows (toTitleSummary
// parity). This is the shared grouping member-resolution read both Collections and
// Playlists use.
//
// The access predicate is the SAME filter.titleClauses the catalog browse reads
// apply (library-grant AND rating-ceiling over library_id / content_rating), so a
// restricted member is filtered IN SQL and never leaves the store — a curated row
// can never become an enumeration oracle for restricted content. An all-access
// (Admin) filter contributes no predicate, so the membership is unchanged (full).
func (db *DB) ResolveVisibleTitles(titleIDs []string, filter AccessFilter) (map[string]Title, error) {
	out := make(map[string]Title, len(titleIDs))
	if len(titleIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(titleIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(titleIDs))
	for _, id := range titleIDs {
		args = append(args, id)
	}
	// The IN-list placeholders bind first (titleIDs above); the access-clause
	// placeholders bind after, so append its args last to keep them aligned.
	accessClause, accessArgs := filter.titleClauses("library_id", "content_rating")
	args = append(args, accessArgs...)
	rows, err := db.Query(
		`SELECT `+enrichedTitleColumns+` FROM titles
		  WHERE hidden = 0 AND id IN (`+placeholders+`)`+accessClause, args...)
	if err != nil {
		return nil, fmt.Errorf("store: resolving visible titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanEnrichedTitle(rows)
		if err != nil {
			return nil, err
		}
		out[t.ID] = t
	}
	return out, rows.Err()
}
