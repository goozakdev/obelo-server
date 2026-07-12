package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// targeted_scan.go resolves the on-disk scope of a Targeted scan (ADR-0030): given
// a browsable entity — a Movie Title, a Show, an Album, or an Artist — it returns
// that entity's Library, its display label (for the scan_status scope tag), and
// the distinct present File paths it currently occupies. The API layer maps those
// paths to their anchor folders (via NeedsReviewAnchor) and hands the folder set to
// the scanner. Paths, not folders, are returned here so the store stays free of the
// per-kind anchor rules (those live in needsreview.go).

// EntityScanScope is everything the API needs to launch a Targeted scan of one
// entity: the owning Library, a human label, and the entity's present File paths
// (empty when every File is Missing — a hidden entity, out of scope for v1).
type EntityScanScope struct {
	LibraryID string
	Label     string
	Paths     []string
}

// TitleScanScope resolves a Movie Title's scan scope. ErrNotFound for an unknown id.
func (db *DB) TitleScanScope(id string) (EntityScanScope, error) {
	return db.entityScanScope(
		`SELECT library_id, title FROM titles WHERE id = ?`,
		`SELECT DISTINCT f.path
		   FROM editions e JOIN files f ON f.edition_id = e.id
		  WHERE e.title_id = ? AND f.present = 1`,
		id)
}

// ShowScanScope resolves a Show's scan scope: every present Episode File under it.
func (db *DB) ShowScanScope(id string) (EntityScanScope, error) {
	return db.entityScanScope(
		`SELECT library_id, title FROM shows WHERE id = ?`,
		`SELECT DISTINCT f.path
		   FROM titles t
		   JOIN seasons  se ON t.season_id  = se.id
		   JOIN editions e  ON e.title_id    = t.id
		   JOIN files    f  ON f.edition_id  = e.id
		  WHERE se.show_id = ? AND f.present = 1`,
		id)
}

// AlbumScanScope resolves an Album's scan scope: every present Track File in it.
func (db *DB) AlbumScanScope(id string) (EntityScanScope, error) {
	return db.entityScanScope(
		`SELECT ar.library_id, a.title FROM albums a JOIN artists ar ON ar.id = a.artist_id WHERE a.id = ?`,
		`SELECT DISTINCT f.path
		   FROM titles t
		   JOIN editions e ON e.title_id   = t.id
		   JOIN files    f ON f.edition_id = e.id
		  WHERE t.album_id = ? AND f.present = 1`,
		id)
}

// ArtistScanScope resolves an Artist's scan scope: every present Track File across
// all of the Artist's Albums (their parent folders become the walked folder set).
func (db *DB) ArtistScanScope(id string) (EntityScanScope, error) {
	return db.entityScanScope(
		`SELECT library_id, name FROM artists WHERE id = ?`,
		`SELECT DISTINCT f.path
		   FROM titles t
		   JOIN albums   a ON t.album_id   = a.id
		   JOIN editions e ON e.title_id   = t.id
		   JOIN files    f ON f.edition_id = e.id
		  WHERE a.artist_id = ? AND f.present = 1`,
		id)
}

// entityScanScope runs the header query (library_id + label; ErrNotFound when the
// entity is gone) then the paths query, returning the assembled scope.
func (db *DB) entityScanScope(headerQuery, pathsQuery, id string) (EntityScanScope, error) {
	var sc EntityScanScope
	err := db.QueryRow(headerQuery, id).Scan(&sc.LibraryID, &sc.Label)
	if errors.Is(err, sql.ErrNoRows) {
		return EntityScanScope{}, ErrNotFound
	}
	if err != nil {
		return EntityScanScope{}, fmt.Errorf("store: resolving scan scope header: %w", err)
	}
	rows, err := db.Query(pathsQuery, id)
	if err != nil {
		return EntityScanScope{}, fmt.Errorf("store: resolving scan scope paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return EntityScanScope{}, fmt.Errorf("store: scanning scan scope path: %w", err)
		}
		sc.Paths = append(sc.Paths, p)
	}
	return sc, rows.Err()
}
