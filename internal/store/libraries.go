package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Library is a top-level collection of media of a single kind (CONTEXT.md),
// backed by one or more root folders. Roots is populated by the read methods;
// it is the merged set of folders that make up this one logical Library.
type Library struct {
	ID        string
	Name      string
	Kind      string
	CreatedAt string
	Roots     []LibraryRoot
}

// LibraryRoot is one root folder owned by a Library. Path is stored already
// normalized (cleaned, absolute) by the library domain.
type LibraryRoot struct {
	ID        string
	LibraryID string
	Path      string
	CreatedAt string
}

// LibraryRootInput is a (rootID, path) pair to persist for a new Library. The
// caller supplies pre-generated IDs and pre-normalized paths.
type LibraryRootInput struct {
	ID   string
	Path string
}

// AllLibraryRoots returns every root folder across all Libraries, used by the
// library domain to detect folder-ownership overlap before creating a Library.
func (db *DB) AllLibraryRoots() ([]LibraryRoot, error) {
	rows, err := db.Query(
		`SELECT id, library_id, path, created_at FROM library_roots ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("store: listing library roots: %w", err)
	}
	defer rows.Close()

	var out []LibraryRoot
	for rows.Next() {
		var r LibraryRoot
		if err := rows.Scan(&r.ID, &r.LibraryID, &r.Path, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning library root: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateLibrary inserts a Library and its root folders in one transaction. The
// caller supplies the Library id, the (already-validated) name and kind, and the
// pre-normalized roots. A UNIQUE violation on a root path surfaces as a plain
// error for the caller to map; the domain layer normally catches overlap first.
func (db *DB) CreateLibrary(id, name, kind string, roots []LibraryRootInput) (Library, error) {
	tx, err := db.Begin()
	if err != nil {
		return Library{}, fmt.Errorf("store: begin create library: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`INSERT INTO libraries (id, name, kind) VALUES (?, ?, ?)`,
		id, name, kind,
	); err != nil {
		return Library{}, fmt.Errorf("store: inserting library: %w", err)
	}
	for _, r := range roots {
		if _, err := tx.Exec(
			`INSERT INTO library_roots (id, library_id, path) VALUES (?, ?, ?)`,
			r.ID, id, r.Path,
		); err != nil {
			return Library{}, fmt.Errorf("store: inserting library root %q: %w", r.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Library{}, fmt.Errorf("store: commit create library: %w", err)
	}
	return db.LibraryByID(id)
}

// Libraries lists all Libraries with their root folders, most-recently-created
// first.
func (db *DB) Libraries() ([]Library, error) {
	rows, err := db.Query(
		`SELECT id, name, kind, created_at FROM libraries ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries: %w", err)
	}
	defer rows.Close()

	var libs []Library
	byID := make(map[string]int)
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Kind, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning library: %w", err)
		}
		byID[l.ID] = len(libs)
		libs = append(libs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	allRoots, err := db.AllLibraryRoots()
	if err != nil {
		return nil, err
	}
	for _, r := range allRoots {
		if i, ok := byID[r.LibraryID]; ok {
			libs[i].Roots = append(libs[i].Roots, r)
		}
	}
	return libs, nil
}

// LibraryByID returns one Library with its root folders, or ErrNotFound.
func (db *DB) LibraryByID(id string) (Library, error) {
	var l Library
	err := db.QueryRow(
		`SELECT id, name, kind, created_at FROM libraries WHERE id = ?`, id,
	).Scan(&l.ID, &l.Name, &l.Kind, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Library{}, ErrNotFound
	}
	if err != nil {
		return Library{}, fmt.Errorf("store: scanning library: %w", err)
	}

	rows, err := db.Query(
		`SELECT id, library_id, path, created_at FROM library_roots
		   WHERE library_id = ? ORDER BY path`, id)
	if err != nil {
		return Library{}, fmt.Errorf("store: listing roots for library: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r LibraryRoot
		if err := rows.Scan(&r.ID, &r.LibraryID, &r.Path, &r.CreatedAt); err != nil {
			return Library{}, fmt.Errorf("store: scanning library root: %w", err)
		}
		l.Roots = append(l.Roots, r)
	}
	if err := rows.Err(); err != nil {
		return Library{}, err
	}
	return l, nil
}

// DeleteLibrary removes a Library and (via ON DELETE CASCADE) its root folders
// and empty catalog. Returns ErrNotFound if no such Library exists.
func (db *DB) DeleteLibrary(id string) error {
	res, err := db.Exec(`DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting library: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting library: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
