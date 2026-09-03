package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Per-User library-access grants (access-control 03) and the entity→Library
// resolution the access guard needs. A Member is limited to the Libraries
// granted here; an Admin has no rows (Admin = all Libraries, resolved by role).

// LibraryAccessForUser returns the ids of the Libraries granted to a User, in a
// stable order. An empty result means "no Libraries" (a Member with no grants
// sees nothing) — never treated as an error.
func (db *DB) LibraryAccessForUser(userID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT library_id FROM user_library_access WHERE user_id = ? ORDER BY library_id`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("store: reading library access: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scanning library access: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReplaceLibraryAccess sets a User's grant set to exactly libraryIDs, atomically
// (delete-all + insert-set in one transaction). Every id is validated to exist
// first, so an unknown Library id fails the whole write (ErrNotFound) and leaves
// the prior set unchanged — desired-state, idempotent, no partial application.
// Duplicate ids in the input collapse (INSERT OR IGNORE over the UNIQUE).
func (db *DB) ReplaceLibraryAccess(userID string, libraryIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: replacing library access: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Validate every referenced Library exists before mutating, so a bad id is a
	// clean ErrNotFound and the transaction rolls back with the prior set intact.
	for _, lid := range libraryIDs {
		var one int
		err := tx.QueryRow(`SELECT 1 FROM libraries WHERE id = ?`, lid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: validating library %q: %w", lid, err)
		}
	}

	if _, err := tx.Exec(`DELETE FROM user_library_access WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: clearing library access: %w", err)
	}
	for _, lid := range libraryIDs {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO user_library_access (user_id, library_id) VALUES (?, ?)`,
			userID, lid,
		); err != nil {
			return fmt.Errorf("store: granting library %q: %w", lid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing library access: %w", err)
	}
	return nil
}

// RatingCeilingForUser returns a User's stored Rating-ceiling label, or "" when
// unset (uncapped). ErrNotFound for an unknown User.
func (db *DB) RatingCeilingForUser(userID string) (string, error) {
	var label sql.NullString
	err := db.QueryRow(`SELECT rating_ceiling FROM users WHERE id = ?`, userID).Scan(&label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: reading rating ceiling: %w", err)
	}
	return label.String, nil // NULL → ""
}

// SetRatingCeiling stores a User's ceiling label, or clears it to NULL (uncapped)
// when label is "". ErrNotFound for an unknown User.
func (db *DB) SetRatingCeiling(userID, label string) error {
	var val any
	if label != "" {
		val = label
	}
	res, err := db.Exec(`UPDATE users SET rating_ceiling = ? WHERE id = ?`, val, userID)
	if err != nil {
		return fmt.Errorf("store: setting rating ceiling: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: setting rating ceiling: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PlaybackCeiling is a User's cap on HOW a Title may play (CONTEXT.md "Playback
// ceiling", ADR-0054 §2) — as opposed to the Rating ceiling, which caps WHAT
// they may see. The zero value is "uncapped" in all three dimensions, which is
// exactly what the three NULL columns read back as.
type PlaybackCeiling struct {
	// MaxResolution is a negotiation resolution token ("720p", "1080p", "2160p");
	// "" = uncapped.
	MaxResolution string
	// MaxBitrate is bits/sec; 0 = uncapped.
	MaxBitrate int64
	// MaxStreams is the most concurrent Playback sessions the User may hold;
	// 0 = uncapped.
	MaxStreams int
}

// PlaybackCeilingForUser returns a User's stored Playback ceiling; the zero value
// means uncapped in that dimension (the columns are NULL until set).
// ErrNotFound for an unknown User.
func (db *DB) PlaybackCeilingForUser(userID string) (PlaybackCeiling, error) {
	var (
		res     sql.NullString
		bitrate sql.NullInt64
		streams sql.NullInt64
	)
	err := db.QueryRow(
		`SELECT max_resolution, max_bitrate, max_streams FROM users WHERE id = ?`,
		userID).Scan(&res, &bitrate, &streams)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaybackCeiling{}, ErrNotFound
	}
	if err != nil {
		return PlaybackCeiling{}, fmt.Errorf("store: reading playback ceiling: %w", err)
	}
	return PlaybackCeiling{
		MaxResolution: res.String,         // NULL → ""
		MaxBitrate:    bitrate.Int64,      // NULL → 0
		MaxStreams:    int(streams.Int64), // NULL → 0
	}, nil
}

// SetPlaybackCeiling stores a User's whole Playback ceiling, writing NULL for
// each dimension left at its zero value (uncapped). It is a replace of all three
// — the ceiling is one setting with three knobs, not three settings — so a PUT
// that omits a dimension clears it, exactly as the grant set is a replace-set.
// ErrNotFound for an unknown User.
func (db *DB) SetPlaybackCeiling(userID string, c PlaybackCeiling) error {
	var res, bitrate, streams any
	if c.MaxResolution != "" {
		res = c.MaxResolution
	}
	if c.MaxBitrate > 0 {
		bitrate = c.MaxBitrate
	}
	if c.MaxStreams > 0 {
		streams = c.MaxStreams
	}
	r, err := db.Exec(
		`UPDATE users SET max_resolution = ?, max_bitrate = ?, max_streams = ? WHERE id = ?`,
		res, bitrate, streams, userID)
	if err != nil {
		return fmt.Errorf("store: setting playback ceiling: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: setting playback ceiling: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LibraryOfTitle returns the Library id a Title belongs to, or ErrNotFound. The
// access guard uses it to hide a Title's artwork (and the Title) when the Library
// is outside the caller's Scope.
func (db *DB) LibraryOfTitle(id string) (string, error) {
	return db.scanLibraryID(`SELECT library_id FROM titles WHERE id = ?`, id)
}

// LibraryOfEntity returns the Library id a browse parent (Show/Season/Artist/
// Album) belongs to, or ErrNotFound. Shows/Artists carry library_id directly; a
// Season resolves through its Show and an Album through its Artist.
func (db *DB) LibraryOfEntity(entityType, id string) (string, error) {
	var query string
	switch entityType {
	case EntityShow:
		query = `SELECT library_id FROM shows WHERE id = ?`
	case EntityArtist:
		query = `SELECT library_id FROM artists WHERE id = ?`
	case EntitySeason:
		query = `SELECT sh.library_id FROM seasons s JOIN shows sh ON sh.id = s.show_id WHERE s.id = ?`
	case EntityAlbum:
		query = `SELECT ar.library_id FROM albums a JOIN artists ar ON ar.id = a.artist_id WHERE a.id = ?`
	default:
		return "", ErrNotFound
	}
	return db.scanLibraryID(query, id)
}

func (db *DB) scanLibraryID(query, id string) (string, error) {
	var lib string
	err := db.QueryRow(query, id).Scan(&lib)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: resolving library: %w", err)
	}
	return lib, nil
}
