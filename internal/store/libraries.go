package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// The two things a Library can be (ADR-0056 §1). `local` is the ordinary one:
// root folders on this machine, walked by the Scanner. `linked` is a mirror of a
// Library on another household's Server — no roots, no Scanner, no writers, and
// the other Server is the identity authority for everything under it (ADR-0002,
// ADR-0019 — "local disk wins", and here local means theirs).
const (
	LibrarySourceLocal  = "local"
	LibrarySourceLinked = "linked"
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

	// Source is LibrarySourceLocal or LibrarySourceLinked. It is THE field every
	// writer filters on: the Scanner, the enrichment pass and the attention queue
	// skip a linked Library, and the write handlers refuse it with 409
	// LINKED_LIBRARY (ADR-0056 §1).
	Source string
	// LinkID and RemoteLibraryID are set only on a linked Library: the Link it
	// arrived over (store.Link.ID) and its id on the sharing Server. The second is
	// what the Export is addressed by.
	LinkID          string
	RemoteLibraryID string
	// RemoteCheckpoint is the sharer's export cursor this mirror has consumed up
	// to — the value handed back as `since` on the next pull (ADR-0056 §4). Empty
	// means "nothing pulled yet", which is a full pull.
	RemoteCheckpoint string
}

// Linked reports whether this Library is a mirror of another Server's.
func (l Library) Linked() bool { return l.Source == LibrarySourceLinked }

// libraryColumns is the one projection every Library read uses, so a new column
// cannot reach one reader and miss another.
const libraryColumns = `id, name, kind, created_at, source, link_id, remote_library_id, remote_checkpoint`

// scanLibrary reads libraryColumns off a row. link_id is NULL on a local
// Library, which is what makes the partial unique index on
// (link_id, remote_library_id) ignore every local row.
func scanLibrary(sc interface{ Scan(...any) error }) (Library, error) {
	var (
		l      Library
		linkID sql.NullString
	)
	if err := sc.Scan(&l.ID, &l.Name, &l.Kind, &l.CreatedAt, &l.Source, &linkID,
		&l.RemoteLibraryID, &l.RemoteCheckpoint); err != nil {
		return Library{}, err
	}
	l.LinkID = linkID.String
	return l, nil
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

// UpdateLibrary renames a Library and/or appends root folders, in one
// transaction. A nil name leaves the name unchanged; an empty addRoots adds
// nothing. The caller supplies pre-normalized paths and pre-generated root IDs,
// having already validated the name and rejected folder overlap. Returns
// ErrNotFound if no such Library exists (checked up front so a no-op update on a
// missing Library still reports it), and re-reads the Library so the returned
// value reflects the merged roots.
func (db *DB) UpdateLibrary(id string, name *string, addRoots []LibraryRootInput) (Library, error) {
	tx, err := db.Begin()
	if err != nil {
		return Library{}, fmt.Errorf("store: begin update library: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Confirm the Library exists before mutating (a rename with no rows, or an
	// add of zero roots, would otherwise silently succeed on a missing id).
	var exists string
	err = tx.QueryRow(`SELECT id FROM libraries WHERE id = ?`, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return Library{}, ErrNotFound
	}
	if err != nil {
		return Library{}, fmt.Errorf("store: loading library for update: %w", err)
	}

	if name != nil {
		if _, err := tx.Exec(
			`UPDATE libraries SET name = ? WHERE id = ?`, *name, id,
		); err != nil {
			return Library{}, fmt.Errorf("store: renaming library: %w", err)
		}
	}
	for _, r := range addRoots {
		if _, err := tx.Exec(
			`INSERT INTO library_roots (id, library_id, path) VALUES (?, ?, ?)`,
			r.ID, id, r.Path,
		); err != nil {
			return Library{}, fmt.Errorf("store: adding library root %q: %w", r.Path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Library{}, fmt.Errorf("store: commit update library: %w", err)
	}
	return db.LibraryByID(id)
}

// Libraries lists all Libraries with their root folders, most-recently-created
// first.
func (db *DB) Libraries() ([]Library, error) {
	rows, err := db.Query(
		`SELECT ` + libraryColumns + ` FROM libraries ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries: %w", err)
	}
	defer rows.Close()

	var libs []Library
	byID := make(map[string]int)
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
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
	l, err := scanLibrary(db.QueryRow(
		`SELECT `+libraryColumns+` FROM libraries WHERE id = ?`, id))
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

// LibraryTitleCount returns the number of top-level browsable entries in a
// Library, chosen by kind to match what a User sees at the top of the browse
// tree: Movies for a movie Library, Shows (series) for a TV Library, and Albums
// for a music Library. This is deliberately NOT the leaf count the scanner
// reports as titlesFound (which counts Episodes for TV and Tracks for music).
// Hidden rows (every File Missing) are excluded, matching the browse listings. An
// unknown Library yields ErrNotFound.
func (db *DB) LibraryTitleCount(libraryID string) (int, error) {
	var kind string
	err := db.QueryRow(`SELECT kind FROM libraries WHERE id = ?`, libraryID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: reading library kind: %w", err)
	}

	var query string
	switch kind {
	case "tv":
		query = `SELECT COUNT(*) FROM shows WHERE library_id = ? AND hidden = 0`
	case "music":
		query = `SELECT COUNT(*) FROM albums a
		           JOIN artists ar ON ar.id = a.artist_id
		          WHERE ar.library_id = ? AND a.hidden = 0`
	default: // movie
		query = `SELECT COUNT(*) FROM titles
		          WHERE library_id = ? AND kind = 'movie' AND hidden = 0`
	}

	var n int
	if err := db.QueryRow(query, libraryID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting library titles: %w", err)
	}
	return n, nil
}

// --- linked Libraries (ADR-0056 §1) -----------------------------------------

// UpsertLinkedLibrary creates — or, on a later pull, refreshes — the Library row
// that mirrors one of a sharer's granted Libraries. It is keyed by
// (link_id, remote_library_id), which is what makes a re-pull and a re-key find
// the SAME shelf instead of building a second one beside it.
//
// The sharer's name is taken ONCE, at creation. A later pull leaves it alone,
// because renaming is the one thing PATCH /libraries/{id} still allows on a
// linked Library (ADR-0056 §1) and a household that called this shelf "Dave's
// cartoons" should not find it renamed back overnight.
//
// It never touches remote_checkpoint either: that is the mirror's own bookmark
// and a refresh of the Library row is not a pull.
func (db *DB) UpsertLinkedLibrary(id, name, kind, linkID, remoteLibraryID string) (Library, error) {
	if linkID == "" || remoteLibraryID == "" {
		return Library{}, fmt.Errorf("store: a linked library needs a link and a remote library id")
	}
	existing, err := db.LinkedLibrary(linkID, remoteLibraryID)
	switch {
	case err == nil:
		return existing, nil
	case errors.Is(err, ErrNotFound):
		if _, err := db.Exec(
			`INSERT INTO libraries (id, name, kind, source, link_id, remote_library_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			id, name, kind, LibrarySourceLinked, linkID, remoteLibraryID,
		); err != nil {
			return Library{}, fmt.Errorf("store: inserting linked library: %w", err)
		}
		return db.LibraryByID(id)
	default:
		return Library{}, err
	}
}

// LinkedLibrary returns the Library mirroring one remote Library over one Link,
// or ErrNotFound.
func (db *DB) LinkedLibrary(linkID, remoteLibraryID string) (Library, error) {
	l, err := scanLibrary(db.QueryRow(
		`SELECT `+libraryColumns+` FROM libraries WHERE link_id = ? AND remote_library_id = ?`,
		linkID, remoteLibraryID))
	if errors.Is(err, sql.ErrNoRows) {
		return Library{}, ErrNotFound
	}
	if err != nil {
		return Library{}, fmt.Errorf("store: scanning linked library: %w", err)
	}
	return l, nil
}

// LibrariesForLink lists the linked Libraries one Link brought, oldest first.
// Roots are not loaded: a linked Library has none by construction.
func (db *DB) LibrariesForLink(linkID string) ([]Library, error) {
	rows, err := db.Query(
		`SELECT `+libraryColumns+` FROM libraries WHERE link_id = ?
		  ORDER BY created_at, id`, linkID)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries for link: %w", err)
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning library for link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LinkedLibraryStates maps every linked Library's id to whether its sharing
// Server is currently reachable — `available` on the wire (ADR-0056 §6). Today
// that is exactly "its Link is connected"; issue 08 owns the state machine that
// moves it.
//
// It returns nil (not an empty map) when nothing here came over a Link, which is
// the common case and lets a caller skip the decoration entirely.
func (db *DB) LinkedLibraryStates() (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT l.id, COALESCE(k.state, '') FROM libraries l
		    LEFT JOIN links k ON k.id = l.link_id
		   WHERE l.source = ?`, LibrarySourceLinked)
	if err != nil {
		return nil, fmt.Errorf("store: reading linked library states: %w", err)
	}
	defer rows.Close()
	var out map[string]bool
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, fmt.Errorf("store: scanning linked library state: %w", err)
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[id] = state == LinkStateConnected
	}
	return out, rows.Err()
}

// IsLinkedLibrary reports whether a Library is a mirror. An unknown Library is
// false, never an error: every caller is a guard asking "may I write here", and
// "there is no such Library" is not that guard's refusal to make.
func (db *DB) IsLinkedLibrary(id string) (bool, error) {
	var source string
	err := db.QueryRow(`SELECT source FROM libraries WHERE id = ?`, id).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: reading library source: %w", err)
	}
	return source == LibrarySourceLinked, nil
}

// SetLibraryCheckpoint records how far the mirror has consumed the sharer's feed.
func (db *DB) SetLibraryCheckpoint(id, checkpoint string) error {
	if _, err := db.Exec(
		`UPDATE libraries SET remote_checkpoint = ? WHERE id = ?`, checkpoint, id,
	); err != nil {
		return fmt.Errorf("store: recording library checkpoint: %w", err)
	}
	return nil
}

// DeleteLibrariesForLink removes every Library a Link brought and, by cascade,
// its whole mirrored catalog and the Watch state hanging off it (ADR-0056 §6:
// unlinking is the only thing that deletes what came over a Link). It returns
// how many shelves went.
func (db *DB) DeleteLibrariesForLink(linkID string) (int, error) {
	res, err := db.Exec(`DELETE FROM libraries WHERE link_id = ?`, linkID)
	if err != nil {
		return 0, fmt.Errorf("store: deleting libraries for link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: deleting libraries for link: %w", err)
	}
	return int(n), nil
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
