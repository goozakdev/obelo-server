package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Music catalog persistence (issue tv-music/03): the explicit Artist → Album
// parent entities and the Track-as-Title linkage. A Track is a Title (kind
// 'track') that owns the existing Edition → File → Stream chain UNCHANGED and
// references its Album → Artist. These types mirror the TV catalog shapes
// (tv.go) so the scanner writes one resolved Artist subtree per Artist grouping
// and the browse API reads the hierarchy back. Watch state stays per-(User,
// Title) — no music-specific watch schema.

// Artist is the top-level browse unit of a Music Library (CONTEXT.md: Artist →
// Album → Track). Identity is the normalized Album-Artist (falling back to
// Artist) name from embedded tags, scoped to the Library; identity_key dedups
// across rescans (ADR-0014, amended for music: tags are authority).
type Artist struct {
	ID          string
	LibraryID   string
	Name        string
	IdentityKey string
	SortName    string
	Hidden      bool
	AddedAt     string
	// AlbumCount is computed for the browse list (not stored).
	AlbumCount int
}

// Album groups an Artist's Tracks. IdentityKey re-resolves the Album on rescan.
// ArtworkPath is the local cover.jpg/folder.jpg when present (embedded cover art
// fallback), empty otherwise.
type Album struct {
	ID          string
	ArtistID    string
	Title       string
	Year        int
	IdentityKey string
	SortTitle   string
	ArtworkPath string
	Hidden      bool
	AddedAt     string
	// TrackCount is computed for the browse list (not stored).
	TrackCount int
}

// TrackTree is one resolved Track: its Title identity (with the Music ordering
// fields) plus the Edition→File→Stream/Extras/Artwork subtree the scanner built,
// exactly as for a Movie. DiscNumber/TrackNumber carry the parsed Music
// ordering; the store writes them onto the titles row.
type TrackTree struct {
	TitleTree
	DiscNumber  int
	TrackNumber int
}

// AlbumTree is one Album and the Tracks resolved within it. ArtworkPath/Year are
// the album-level descriptive fields the scanner derived from tags + local art.
type AlbumTree struct {
	Title       string
	Year        int
	IdentityKey string
	SortTitle   string
	ArtworkPath string
	Tracks      []TrackTree
}

// ArtistTree is the complete result of resolving one Artist grouping: the Artist
// identity plus its Albums, each with its Track subtrees. The scanner builds this
// and hands it to UpsertArtistTree, which resolves/reuses the Artist and Album
// ids by identity and rewrites the Track subtrees atomically (reusing the same
// per-File identity-stability rules as the Movie upsert).
type ArtistTree struct {
	Artist Artist
	Albums []AlbumTree
}

// UpsertArtistTree persists one resolved Artist grouping in a single transaction.
// The Artist is keyed by (library_id, identity_key) and each Album by (artist_id,
// identity_key) so a rescan re-resolves to the same rows instead of duplicating
// (identity stability, ADR-0014). Track Titles are upserted via the same
// path-stable rules UpsertTitleTree uses, with the Album linkage + disc/track
// ordering written onto the titles row.
func (db *DB) UpsertArtistTree(tree ArtistTree) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin upsert artist tree: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	artistID, err := upsertArtist(tx, tree.Artist)
	if err != nil {
		return err
	}

	written := map[string]bool{}
	for _, at := range tree.Albums {
		albumID, err := upsertAlbum(tx, artistID, at)
		if err != nil {
			return err
		}
		for _, tr := range at.Tracks {
			if err := upsertTrackTitle(tx, albumID, tr, written); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert artist tree: %w", err)
	}
	return nil
}

func upsertArtist(tx *sql.Tx, a Artist) (string, error) {
	var artistID string
	err := tx.QueryRow(
		`SELECT id FROM artists WHERE library_id = ? AND identity_key = ?`,
		a.LibraryID, a.IdentityKey,
	).Scan(&artistID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		artistID = a.ID
		if _, err := tx.Exec(
			`INSERT INTO artists (id, library_id, name, identity_key, sort_name, hidden)
			 VALUES (?, ?, ?, ?, ?, 0)`,
			artistID, a.LibraryID, a.Name, a.IdentityKey, a.SortName,
		); err != nil {
			return "", fmt.Errorf("store: inserting artist: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("store: resolving artist identity: %w", err)
	default:
		if _, err := tx.Exec(
			`UPDATE artists SET name = ?, sort_name = ?, hidden = 0 WHERE id = ?`,
			a.Name, a.SortName, artistID,
		); err != nil {
			return "", fmt.Errorf("store: updating artist: %w", err)
		}
	}
	return artistID, nil
}

func upsertAlbum(tx *sql.Tx, artistID string, at AlbumTree) (string, error) {
	var albumID string
	err := tx.QueryRow(
		`SELECT id FROM albums WHERE artist_id = ? AND identity_key = ?`,
		artistID, at.IdentityKey,
	).Scan(&albumID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		albumID = uuid.NewString()
		if _, err := tx.Exec(
			`INSERT INTO albums (id, artist_id, title, year, identity_key, sort_title, artwork_path, hidden)
			 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
			albumID, artistID, at.Title, nullableYear(at.Year), at.IdentityKey, at.SortTitle, at.ArtworkPath,
		); err != nil {
			return "", fmt.Errorf("store: inserting album: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("store: resolving album: %w", err)
	default:
		if _, err := tx.Exec(
			`UPDATE albums SET title = ?, year = ?, sort_title = ?, artwork_path = ?, hidden = 0 WHERE id = ?`,
			at.Title, nullableYear(at.Year), at.SortTitle, at.ArtworkPath, albumID,
		); err != nil {
			return "", fmt.Errorf("store: updating album: %w", err)
		}
	}
	return albumID, nil
}

// upsertTrackTitle writes one Track Title (kind 'track') and its subtree, reusing
// the exact Movie subtree-rewrite logic (writeTitleSubtree) so a File keeps its
// id/added_at by path across rescans; it then sets the Album linkage and
// disc/track ordering onto the titles row.
func upsertTrackTitle(tx *sql.Tx, albumID string, tr TrackTree, written map[string]bool) error {
	titleID, err := writeTitleRow(tx, tr.TitleTree, episodeColumns{
		albumID:     albumID,
		discNumber:  tr.DiscNumber,
		trackNumber: tr.TrackNumber,
	})
	if err != nil {
		return err
	}
	return writeTitleSubtree(tx, titleID, tr.TitleTree, written)
}

// --- Browse reads ----------------------------------------------------------

// ArtistPage is one page of a cursor-paginated Artist listing.
type ArtistPage struct {
	Artists []Artist
	HasMore bool
}

// ListArtists returns one page of a Music Library's Artists, ordered by sort_name
// then id (the same stable order Titles/Shows use), excluding hidden Artists,
// seeking strictly past the cursor. It fetches limit+1 to detect HasMore. Each
// Artist is the top-level list entry for a Music Library (the analogue of a Show).
func (db *DB) ListArtists(libraryID string, cursor *TitleCursor, limit int, genre string) (ArtistPage, error) {
	if limit <= 0 {
		limit = 20
	}
	args := []any{libraryID}
	where := "library_id = ? AND hidden = 0"
	// filter[genre]: keep only Artists carrying that enriched genre (issue 03).
	if genre != "" {
		where += " AND id IN (SELECT entity_id FROM entity_genres WHERE entity_type = 'artist' AND genre = ?)"
		args = append(args, genre)
	}
	if cursor != nil {
		where += " AND (sort_name, id) > (?, ?)"
		args = append(args, cursor.SortKey, cursor.ID)
	}
	query := `SELECT id, library_id, name, identity_key, sort_name, hidden, added_at
	            FROM artists WHERE ` + where + ` ORDER BY sort_name ASC, id ASC LIMIT ?`
	args = append(args, limit+1)

	rows, err := db.Query(query, args...)
	if err != nil {
		return ArtistPage{}, fmt.Errorf("store: listing artists: %w", err)
	}
	defer rows.Close()

	var out []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return ArtistPage{}, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return ArtistPage{}, err
	}
	page := ArtistPage{}
	if len(out) > limit {
		page.HasMore = true
		out = out[:limit]
	}
	page.Artists = out
	return page, nil
}

// ArtistByID returns one Artist, or ErrNotFound. The catalog service guards the
// result against the caller's access Scope — an Artist in a Library not granted
// to the caller is hidden as 404 — and uses this for the albums listing.
func (db *DB) ArtistByID(id string) (Artist, error) {
	row := db.QueryRow(
		`SELECT id, library_id, name, identity_key, sort_name, hidden, added_at
		   FROM artists WHERE id = ?`, id)
	a, err := scanArtist(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Artist{}, ErrNotFound
	}
	if err != nil {
		return Artist{}, fmt.Errorf("store: reading artist: %w", err)
	}
	return a, nil
}

// AlbumsForArtist returns an Artist's Albums in (year, sort_title) order,
// excluding hidden Albums, each with its visible-Track count. Returns ErrNotFound
// when the Artist does not exist.
func (db *DB) AlbumsForArtist(artistID string) ([]Album, error) {
	if _, err := db.ArtistByID(artistID); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT a.id, a.artist_id, a.title, a.year, a.identity_key, a.sort_title,
		        a.artwork_path, a.hidden, a.added_at,
		        (SELECT COUNT(*) FROM titles t WHERE t.album_id = a.id AND t.hidden = 0) AS track_count
		   FROM albums a WHERE a.artist_id = ? AND a.hidden = 0
		  ORDER BY a.year ASC, a.sort_title ASC, a.id ASC`, artistID)
	if err != nil {
		return nil, fmt.Errorf("store: listing albums: %w", err)
	}
	defer rows.Close()
	var out []Album
	for rows.Next() {
		al, err := scanAlbum(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, al)
	}
	return out, rows.Err()
}

// AlbumByID returns one Album, or ErrNotFound.
func (db *DB) AlbumByID(id string) (Album, error) {
	var al Album
	var year sql.NullInt64
	var hidden int
	err := db.QueryRow(
		`SELECT id, artist_id, title, year, identity_key, sort_title, artwork_path, hidden, added_at
		   FROM albums WHERE id = ?`, id,
	).Scan(&al.ID, &al.ArtistID, &al.Title, &year, &al.IdentityKey, &al.SortTitle,
		&al.ArtworkPath, &hidden, &al.AddedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Album{}, ErrNotFound
	}
	if err != nil {
		return Album{}, fmt.Errorf("store: reading album: %w", err)
	}
	if year.Valid {
		al.Year = int(year.Int64)
	}
	al.Hidden = hidden != 0
	return al, nil
}

// TracksForAlbum returns an Album's Tracks (Titles) in (disc, track, then title)
// order, excluding hidden ones. Returns ErrNotFound when the Album does not
// exist. The Title rows carry the Music ordering fields so the browse API can
// label them.
func (db *DB) TracksForAlbum(albumID string) ([]Title, error) {
	if _, err := db.AlbumByID(albumID); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
		        tmdb_id, imdb_id, musicbrainz_id, needs_review, ambiguous, hidden,
		        disc_number, track_number,
		        overview, enrichment_status, enriched_title
		   FROM titles WHERE album_id = ? AND hidden = 0
		  ORDER BY disc_number ASC, track_number ASC, sort_title ASC, id ASC`, albumID)
	if err != nil {
		return nil, fmt.Errorf("store: listing tracks: %w", err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		t, err := scanTrackTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TrackDurationsForAlbum returns each Track's playable duration (ms) for an
// Album, keyed by Track (Title) id: the MAX file duration per Track (a Track
// normally has a single file; MAX is a safe pick if several editions/files
// exist). Tracks with no file are omitted from the map, so the caller treats a
// missing entry as an unknown (0) duration.
func (db *DB) TrackDurationsForAlbum(albumID string) (map[string]int64, error) {
	rows, err := db.Query(
		`SELECT t.id, MAX(f.duration_ms)
		   FROM titles t
		   JOIN editions e ON e.title_id = t.id
		   JOIN files    f ON f.edition_id = e.id
		  WHERE t.album_id = ? AND t.hidden = 0
		  GROUP BY t.id`, albumID)
	if err != nil {
		return nil, fmt.Errorf("store: track durations: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var dur sql.NullInt64
		if err := rows.Scan(&id, &dur); err != nil {
			return nil, fmt.Errorf("store: scanning track duration: %w", err)
		}
		if dur.Valid {
			out[id] = dur.Int64
		}
	}
	return out, rows.Err()
}

// TrackContext is the parent chain of a Track Title (Artist / Album / disc /
// track), attached to the Track's GET /titles/{id} detail so a client can render
// "Artist · Album · 03" without extra round-trips.
type TrackContext struct {
	ArtistID    string
	ArtistName  string
	AlbumID     string
	AlbumTitle  string
	AlbumYear   int
	DiscNumber  int
	TrackNumber int
}

// TrackContextForTitle returns the parent context for a Track Title, or
// ErrNotFound when the Title is not a Track (no album linkage). Movies/Episodes
// return ErrNotFound here and the caller simply omits the context.
func (db *DB) TrackContextForTitle(titleID string) (TrackContext, error) {
	var c TrackContext
	var year sql.NullInt64
	err := db.QueryRow(
		`SELECT t.disc_number, t.track_number,
		        al.id, al.title, al.year, ar.id, ar.name
		   FROM titles t
		   JOIN albums  al ON al.id = t.album_id
		   JOIN artists ar ON ar.id = al.artist_id
		  WHERE t.id = ?`, titleID,
	).Scan(&c.DiscNumber, &c.TrackNumber, &c.AlbumID, &c.AlbumTitle, &year,
		&c.ArtistID, &c.ArtistName)
	if errors.Is(err, sql.ErrNoRows) {
		return TrackContext{}, ErrNotFound
	}
	if err != nil {
		return TrackContext{}, fmt.Errorf("store: reading track context: %w", err)
	}
	if year.Valid {
		c.AlbumYear = int(year.Int64)
	}
	return c, nil
}

// AlbumArtworkByID returns an Album's local cover path, or ErrNotFound when the
// Album has no recorded artwork (the API serves the bytes). Mirrors
// ArtworkByTitleRole for the Album browse unit.
func (db *DB) AlbumArtworkByID(albumID string) (Artwork, error) {
	al, err := db.AlbumByID(albumID)
	if err != nil {
		return Artwork{}, err
	}
	if al.ArtworkPath == "" {
		return Artwork{}, ErrNotFound
	}
	return Artwork{Role: "cover", Path: al.ArtworkPath}, nil
}

// RecomputeHiddenArtists recomputes each Album's and Artist's hidden flag from
// the visibility of the Titles beneath them, after a scan's MarkFilesMissing +
// RecomputeHiddenTitles pass. An Album with no visible Track is hidden; an Artist
// with no visible Album is hidden — so an Artist whose every file went Missing
// drops out of the list but stays fetchable (ADR-0008). Mirrors
// RecomputeHiddenShows.
func (db *DB) RecomputeHiddenArtists(libraryID string) error {
	if _, err := db.Exec(
		`UPDATE albums SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM titles t WHERE t.album_id = albums.id AND t.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE artist_id IN (SELECT id FROM artists WHERE library_id = ?)`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden albums: %w", err)
	}
	if _, err := db.Exec(
		`UPDATE artists SET hidden = CASE
		     WHEN (SELECT COUNT(*) FROM albums a WHERE a.artist_id = artists.id AND a.hidden = 0) > 0
		     THEN 0 ELSE 1 END
		   WHERE library_id = ?`, libraryID); err != nil {
		return fmt.Errorf("store: recomputing hidden artists: %w", err)
	}
	return nil
}

func scanArtist(s scanner) (Artist, error) {
	var a Artist
	var hidden int
	if err := s.Scan(&a.ID, &a.LibraryID, &a.Name, &a.IdentityKey, &a.SortName,
		&hidden, &a.AddedAt); err != nil {
		return Artist{}, err
	}
	a.Hidden = hidden != 0
	return a, nil
}

func scanAlbum(s scanner) (Album, error) {
	var al Album
	var year sql.NullInt64
	var hidden int
	if err := s.Scan(&al.ID, &al.ArtistID, &al.Title, &year, &al.IdentityKey,
		&al.SortTitle, &al.ArtworkPath, &hidden, &al.AddedAt, &al.TrackCount); err != nil {
		return Album{}, err
	}
	if year.Valid {
		al.Year = int(year.Int64)
	}
	al.Hidden = hidden != 0
	return al, nil
}

// scanTrackTitle scans a titles row including the Music ordering columns plus the
// enriched display fields (overview / enriched_title / status), so the Album's
// Track list can show an enriched synopsis / sparse-title fill without a fetch.
func scanTrackTitle(s scanner) (Title, error) {
	var t Title
	var year sql.NullInt64
	var needsReview, ambiguous, hidden int
	if err := s.Scan(&t.ID, &t.LibraryID, &t.Kind, &t.Title, &year, &t.IdentityKey,
		&t.SortTitle, &t.AddedAt, &t.TMDBID, &t.IMDBID, &t.MusicbrainzID, &needsReview, &ambiguous, &hidden,
		&t.DiscNumber, &t.TrackNumber,
		&t.Overview, &t.EnrichmentStatus, &t.EnrichedTitle); err != nil {
		return Title{}, err
	}
	if year.Valid {
		t.Year = int(year.Int64)
	}
	t.NeedsReview = needsReview != 0
	t.Ambiguous = ambiguous != 0
	t.Hidden = hidden != 0
	return t, nil
}
