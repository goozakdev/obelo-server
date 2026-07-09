package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// User-owned, ordered, single-media-kind Playlists (collections-playlists 03). A
// Playlist is the PRIVATE counterpart to the shared Collection (collections.go):
// it has an owner, an explicit order (position), and one media kind. These methods
// are the persistence the organize domain reads/writes; the organize Service
// applies ownership + the single-kind rule over them and the api layer decorates
// the resolved member Titles exactly like a browse grid. Membership is keyed to the
// stable Title id (ADR-0014); duplicates are allowed (no UNIQUE on title_id).

// ErrItemSetMismatch means a reorder payload's item-id set did not EXACTLY match
// the Playlist's current item ids — it omitted an id, named a foreign/unknown id,
// or repeated one. The reorder is rejected as a no-op that leaves the existing
// order unchanged (the api layer answers 422 ITEM_SET_MISMATCH).
var ErrItemSetMismatch = errors.New("store: playlist reorder item set mismatch")

// Playlist is a User-owned, ordered, single-media-kind queue of Titles (CONTEXT.md
// "Playlist"). Items live in playlist_items.
type Playlist struct {
	ID          string
	OwnerUserID string
	// Kind is the single media kind the Playlist holds — "movie" | "tv" | "music"
	// — or "" while the Playlist is still untyped (NULL in the DB, before its first
	// item). It is NOT the raw Title kind: an Episode maps to "tv", a Track to
	// "music" (the mapping lives in the organize service).
	Kind      string
	Name      string
	CreatedAt string
	UpdatedAt string
	// System is the slug of the system Playlist this is ("watchlist"), or "" for an
	// ordinary User-created Playlist. A non-empty System marks a Playlist the User
	// owns but may NOT rename or delete (the organize service enforces this), and is
	// what makes each system Playlist unique per owner (migration 0021).
	System string
	// ItemCount is the raw number of item rows (including Missing/duplicates). It is
	// populated only by ListPlaylistsByOwner (the list-card metadata); single-row
	// reads (PlaylistByID) leave it 0.
	ItemCount int
}

// PlaylistItem is one ordered entry in a Playlist: its own id (so duplicates are
// distinguishable and issue 04 can remove-by-item-id), the Title it points at, and
// its position in the sequence.
type PlaylistItem struct {
	ID       string
	TitleID  string
	Position int
}

// CreatePlaylist inserts an empty, untyped (kind NULL) Playlist owned by
// ownerUserID and returns the stored row.
func (db *DB) CreatePlaylist(id, ownerUserID, name string) (Playlist, error) {
	if _, err := db.Exec(
		`INSERT INTO playlists (id, owner_user_id, name) VALUES (?, ?, ?)`,
		id, ownerUserID, name,
	); err != nil {
		return Playlist{}, fmt.Errorf("store: inserting playlist: %w", err)
	}
	return db.PlaylistByID(id)
}

// PlaylistByID returns one Playlist (without ItemCount), or ErrNotFound. The
// caller checks ownership; this reader is owner-agnostic so the organize service
// can compare owner_user_id and hide a foreign Playlist as 404.
func (db *DB) PlaylistByID(id string) (Playlist, error) {
	var (
		p      Playlist
		kind   sql.NullString
		system sql.NullString
	)
	err := db.QueryRow(
		`SELECT id, owner_user_id, name, kind, system, created_at, updated_at
		   FROM playlists WHERE id = ?`, id,
	).Scan(&p.ID, &p.OwnerUserID, &p.Name, &kind, &system, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrNotFound
	}
	if err != nil {
		return Playlist{}, fmt.Errorf("store: reading playlist: %w", err)
	}
	p.Kind = kind.String     // "" when NULL (untyped)
	p.System = system.String // "" when NULL (ordinary playlist)
	return p, nil
}

// systemPlaylist returns ownerUserID's system Playlist with the given slug, or
// ErrNotFound. It underpins EnsureSystemPlaylist and is owner-scoped by column.
func (db *DB) systemPlaylist(ownerUserID, system string) (Playlist, error) {
	var (
		p    Playlist
		kind sql.NullString
	)
	err := db.QueryRow(
		`SELECT id, owner_user_id, name, kind, created_at, updated_at
		   FROM playlists WHERE owner_user_id = ? AND system = ?`, ownerUserID, system,
	).Scan(&p.ID, &p.OwnerUserID, &p.Name, &kind, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Playlist{}, ErrNotFound
	}
	if err != nil {
		return Playlist{}, fmt.Errorf("store: reading system playlist: %w", err)
	}
	p.Kind = kind.String
	p.System = system
	return p, nil
}

// EnsureSystemPlaylist returns ownerUserID's system Playlist with the given slug,
// creating it (named `name`) if it does not exist yet — the lazy get-or-create that
// makes a system Playlist "always exist" for a User created after migration 0021's
// back-fill (or whose row was somehow removed). Idempotent: a second call returns
// the same row. The partial unique index on (owner_user_id, system) is the backstop
// against a concurrent double-create — a losing INSERT is swallowed and the existing
// row re-read.
func (db *DB) EnsureSystemPlaylist(ownerUserID, system, name string) (Playlist, error) {
	if p, err := db.systemPlaylist(ownerUserID, system); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Playlist{}, err
	}
	// Not there yet — create it. A concurrent creator may win the unique index; in
	// that case the INSERT errors and we simply re-read the winner's row.
	_, insErr := db.Exec(
		`INSERT INTO playlists (id, owner_user_id, name, system) VALUES (?, ?, ?, ?)`,
		uuid.NewString(), ownerUserID, name, system,
	)
	p, err := db.systemPlaylist(ownerUserID, system)
	if err != nil {
		if insErr != nil {
			return Playlist{}, fmt.Errorf("store: ensuring system playlist: %w", insErr)
		}
		return Playlist{}, err
	}
	return p, nil
}

// ListPlaylistsByOwner returns every Playlist owned by ownerUserID, newest-created
// first, each with its raw ItemCount (a correlated subquery, so one round-trip).
func (db *DB) ListPlaylistsByOwner(ownerUserID string) ([]Playlist, error) {
	rows, err := db.Query(
		`SELECT p.id, p.owner_user_id, p.name, p.kind, p.system, p.created_at, p.updated_at,
		        (SELECT COUNT(*) FROM playlist_items pi WHERE pi.playlist_id = p.id)
		   FROM playlists p
		  WHERE p.owner_user_id = ?
		  ORDER BY p.created_at DESC, p.id DESC`, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("store: listing playlists: %w", err)
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		var (
			p      Playlist
			kind   sql.NullString
			system sql.NullString
		)
		if err := rows.Scan(&p.ID, &p.OwnerUserID, &p.Name, &kind, &system,
			&p.CreatedAt, &p.UpdatedAt, &p.ItemCount); err != nil {
			return nil, fmt.Errorf("store: scanning playlist: %w", err)
		}
		p.Kind = kind.String
		p.System = system.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePlaylistName renames a Playlist, bumping updated_at. ErrNotFound for an
// unknown id. (Ownership is checked by the caller before this runs.)
func (db *DB) UpdatePlaylistName(id, name string) (Playlist, error) {
	res, err := db.Exec(
		`UPDATE playlists SET name = ?, updated_at = datetime('now') WHERE id = ?`,
		name, id,
	)
	if err != nil {
		return Playlist{}, fmt.Errorf("store: updating playlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Playlist{}, fmt.Errorf("store: updating playlist: %w", err)
	}
	if n == 0 {
		return Playlist{}, ErrNotFound
	}
	return db.PlaylistByID(id)
}

// DeletePlaylist removes a Playlist; its item rows cascade away (migration 0017).
// ErrNotFound for an unknown id.
func (db *DB) DeletePlaylist(id string) error {
	res, err := db.Exec(`DELETE FROM playlists WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: deleting playlist: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: deleting playlist: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TitleKind returns the raw kind of a Title ("movie" | "episode" | "track"), or
// ErrUnknownTitle if no such Title exists. The organize service maps the raw kind
// onto the Playlist kind and decides the single-kind rule.
func (db *DB) TitleKind(id string) (string, error) {
	var kind string
	err := db.QueryRow(`SELECT kind FROM titles WHERE id = ?`, id).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownTitle
	}
	if err != nil {
		return "", fmt.Errorf("store: reading title kind: %w", err)
	}
	return kind, nil
}

// AppendPlaylistItem appends a Title to a Playlist at the end (position =
// MAX(position)+1) in one transaction, returning the new item id. mappedKind is the
// Playlist kind the appended Title maps to (already validated against the single-
// kind rule by the service); if the Playlist is still untyped (kind NULL) this
// fixes it to mappedKind. ErrNotFound for an unknown Playlist; ErrUnknownTitle for
// an unknown Title (validated before any write, so a bad id leaves the Playlist
// unchanged). Duplicates are allowed — re-appending the same Title yields a fresh
// item id at the next position.
func (db *DB) AppendPlaylistItem(playlistID, titleID, mappedKind string) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin append playlist item: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var one int
	err = tx.QueryRow(`SELECT 1 FROM playlists WHERE id = ?`, playlistID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: validating playlist %q: %w", playlistID, err)
	}
	if err := tx.QueryRow(`SELECT 1 FROM titles WHERE id = ?`, titleID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUnknownTitle
		}
		return "", fmt.Errorf("store: validating title %q: %w", titleID, err)
	}

	var nextPos int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(position), 0) + 1 FROM playlist_items WHERE playlist_id = ?`,
		playlistID,
	).Scan(&nextPos); err != nil {
		return "", fmt.Errorf("store: computing next position: %w", err)
	}

	itemID := uuid.NewString()
	if _, err := tx.Exec(
		`INSERT INTO playlist_items (id, playlist_id, title_id, position) VALUES (?, ?, ?, ?)`,
		itemID, playlistID, titleID, nextPos,
	); err != nil {
		return "", fmt.Errorf("store: inserting playlist item: %w", err)
	}
	// Fix the kind on the first item (kind IS NULL) and always bump updated_at.
	if _, err := tx.Exec(
		`UPDATE playlists SET kind = COALESCE(kind, ?), updated_at = datetime('now') WHERE id = ?`,
		mappedKind, playlistID,
	); err != nil {
		return "", fmt.Errorf("store: fixing playlist kind: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit append playlist item: %w", err)
	}
	return itemID, nil
}

// ReorderPlaylistItems rewrites the Playlist's order to exactly the sequence in
// itemIDs, transactionally and atomically. The payload is the FULL desired order
// of the Playlist's item ids; each item's position is rewritten to its index in
// itemIDs (1-based, consistent with append's MAX+1). It validates — inside the
// transaction, BEFORE any write — that itemIDs is EXACTLY the Playlist's current
// item-id set: same count, every id belongs to this Playlist, and no duplicate in
// the payload. Any mismatch returns ErrItemSetMismatch and rolls back, so a
// rejected reorder is a true no-op leaving the existing order intact. Idempotent
// (same input → same order). Bumps updated_at on success. ErrNotFound for an
// unknown Playlist. (Ownership is checked by the caller before this runs.)
func (db *DB) ReorderPlaylistItems(playlistID string, itemIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin reorder playlist items: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	var one int
	err = tx.QueryRow(`SELECT 1 FROM playlists WHERE id = ?`, playlistID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: validating playlist %q: %w", playlistID, err)
	}

	// Read the Playlist's current item-id set to validate the payload against.
	current, err := func() (map[string]bool, error) {
		rows, err := tx.Query(`SELECT id FROM playlist_items WHERE playlist_id = ?`, playlistID)
		if err != nil {
			return nil, fmt.Errorf("store: reading playlist items: %w", err)
		}
		defer rows.Close()
		set := make(map[string]bool)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("store: scanning playlist item: %w", err)
			}
			set[id] = true
		}
		return set, rows.Err()
	}()
	if err != nil {
		return err
	}

	// No partial reorder: the payload must be EXACTLY the current set. Same count,
	// no duplicate in the payload, and every id a current member of this Playlist.
	if len(itemIDs) != len(current) {
		return ErrItemSetMismatch
	}
	seen := make(map[string]bool, len(itemIDs))
	for _, id := range itemIDs {
		if seen[id] || !current[id] {
			return ErrItemSetMismatch
		}
		seen[id] = true
	}

	for i, id := range itemIDs {
		if _, err := tx.Exec(
			`UPDATE playlist_items SET position = ? WHERE id = ? AND playlist_id = ?`,
			i+1, id, playlistID,
		); err != nil {
			return fmt.Errorf("store: rewriting playlist item position: %w", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE playlists SET updated_at = datetime('now') WHERE id = ?`, playlistID,
	); err != nil {
		return fmt.Errorf("store: bumping playlist updated_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit reorder playlist items: %w", err)
	}
	return nil
}

// RemovePlaylistItem removes exactly the one playlist_items row whose id is itemID
// AND whose playlist_id is playlistID, returning ErrNotFound when no such row
// exists — an unknown item id, OR an item that belongs to a DIFFERENT Playlist
// (hide-existence, consistent with the 404 for a non-member). Removal is by ITEM
// id, so a duplicate Title's OTHER entry is untouched. Remaining entries keep their
// positions (gaps are harmless — GET orders by position and never renumbers). Bumps
// updated_at when a row was actually removed. The Playlist's fixed kind PERSISTS
// even when the last item is removed (a Playlist's kind is sticky once typed).
// (Ownership is checked by the caller before this runs.)
func (db *DB) RemovePlaylistItem(playlistID, itemID string) error {
	res, err := db.Exec(
		`DELETE FROM playlist_items WHERE id = ? AND playlist_id = ?`,
		itemID, playlistID,
	)
	if err != nil {
		return fmt.Errorf("store: removing playlist item: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: removing playlist item: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := db.Exec(
		`UPDATE playlists SET updated_at = datetime('now') WHERE id = ?`, playlistID,
	); err != nil {
		return fmt.Errorf("store: bumping playlist updated_at: %w", err)
	}
	return nil
}

// PlaylistItemsInOrder returns a Playlist's item rows in position order (itemId,
// titleId, position). It includes Missing members and duplicates — the caller pairs
// each ordered item with its resolved Title via ResolveVisibleTitles, preserving
// order AND duplicates while omitting items whose Title is Missing/out-of-scope.
func (db *DB) PlaylistItemsInOrder(playlistID string) ([]PlaylistItem, error) {
	rows, err := db.Query(
		`SELECT id, title_id, position FROM playlist_items
		  WHERE playlist_id = ?
		  ORDER BY position, id`, playlistID)
	if err != nil {
		return nil, fmt.Errorf("store: listing playlist items: %w", err)
	}
	defer rows.Close()
	var out []PlaylistItem
	for rows.Next() {
		var it PlaylistItem
		if err := rows.Scan(&it.ID, &it.TitleID, &it.Position); err != nil {
			return nil, fmt.Errorf("store: scanning playlist item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
