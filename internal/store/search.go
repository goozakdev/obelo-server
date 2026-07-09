package store

import (
	"fmt"
	"strings"
)

// Cross-kind search (issue tv-music/04). A single query matches across every
// browse entity — Movies, Shows, Artists, Albums, and (drilling in) Episodes and
// Tracks — by a case-insensitive substring on the display name. It mirrors the
// browse reads: hidden (all-Files-Missing) entities are excluded, so a
// soft-deleted Title disappears from search exactly as it does from a library
// grid (existence-hiding, api-contract.md). It is also access-filtered like the
// browse reads: the AccessFilter excludes Titles/Shows outside the caller's
// granted Libraries or above their Rating ceiling, in SQL, so a restricted match
// never leaves the store.
//
// Unmatched files never appear here: they are not Titles, so there is nothing to
// match — consistent with "never auto-guessed into a browsable Title".

// SearchResults groups the matches by browse kind. Each slice is independently
// capped at the requested limit so one prolific kind cannot starve the others.
type SearchResults struct {
	Movies   []Title
	Shows    []Show
	Artists  []Artist
	Albums   []Album
	Episodes []Title
	Tracks   []Title
}

// Search returns the catalog entities whose display name contains query
// (case-insensitive), grouped by kind, excluding hidden entities. An empty query
// yields empty results (the caller decides whether to call). limit caps each
// group; it is clamped to a sane range by the caller.
func (db *DB) Search(query string, limit int, filter AccessFilter) (SearchResults, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return SearchResults{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	pattern := likePattern(q)

	var res SearchResults
	var err error
	if res.Movies, err = db.searchTitles("movie", pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	if res.Episodes, err = db.searchTitles("episode", pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	if res.Tracks, err = db.searchTitles("track", pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	if res.Shows, err = db.searchShows(pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	if res.Artists, err = db.searchArtists(pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	if res.Albums, err = db.searchAlbums(pattern, limit, filter); err != nil {
		return SearchResults{}, err
	}
	return res, nil
}

// searchTitles matches visible Titles of one kind (movie|episode|track) by title,
// ordered by sort_title for a stable result. Reuses scanTitle (the 13-column
// Title projection) so the result rows carry the same fields the browse list does.
func (db *DB) searchTitles(kind, pattern string, limit int, filter AccessFilter) ([]Title, error) {
	clause, clauseArgs := filter.titleClauses("library_id", "content_rating")
	args := []any{kind, pattern}
	args = append(args, clauseArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
		        tmdb_id, imdb_id, needs_review, ambiguous, hidden
		   FROM titles
		  WHERE kind = ? AND hidden = 0 AND title LIKE ? ESCAPE '\'`+clause+`
		  ORDER BY sort_title ASC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching %s titles: %w", kind, err)
	}
	defer rows.Close()
	var out []Title
	for rows.Next() {
		t, err := scanTitle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (db *DB) searchShows(pattern string, limit int, filter AccessFilter) ([]Show, error) {
	clause, clauseArgs := filter.showClauses("library_id", "id")
	args := []any{pattern}
	args = append(args, clauseArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT id, library_id, title, year, identity_key, sort_title,
		        tmdb_id, imdb_id, needs_review, hidden, added_at
		   FROM shows
		  WHERE hidden = 0 AND title LIKE ? ESCAPE '\'`+clause+`
		  ORDER BY sort_title ASC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching shows: %w", err)
	}
	defer rows.Close()
	var out []Show
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (db *DB) searchArtists(pattern string, limit int, filter AccessFilter) ([]Artist, error) {
	libClause, libArgs := filter.libraryClause("library_id")
	args := []any{pattern}
	args = append(args, libArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT id, library_id, name, identity_key, sort_name, hidden, added_at
		   FROM artists
		  WHERE hidden = 0 AND name LIKE ? ESCAPE '\'`+libClause+`
		  ORDER BY sort_name ASC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching artists: %w", err)
	}
	defer rows.Close()
	var out []Artist
	for rows.Next() {
		a, err := scanArtist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (db *DB) searchAlbums(pattern string, limit int, filter AccessFilter) ([]Album, error) {
	// An Album carries no library_id of its own; its Library is its Artist's, so a
	// restrictive filter joins through artists. Under all-access the clause is
	// empty and no join is added, leaving the query byte-identical to before.
	libClause, libArgs := filter.libraryClause("ar.library_id")
	join := ""
	if libClause != "" {
		join = " JOIN artists ar ON ar.id = a.artist_id"
	}
	args := []any{pattern}
	args = append(args, libArgs...)
	args = append(args, limit)
	rows, err := db.Query(
		`SELECT a.id, a.artist_id, a.title, a.year, a.identity_key, a.sort_title,
		        a.artwork_path, a.hidden, a.added_at,
		        (SELECT COUNT(*) FROM titles t WHERE t.album_id = a.id AND t.hidden = 0) AS track_count
		   FROM albums a`+join+`
		  WHERE a.hidden = 0 AND a.title LIKE ? ESCAPE '\'`+libClause+`
		  ORDER BY a.sort_title ASC, a.id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching albums: %w", err)
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

// likePattern wraps a query for a case-insensitive substring LIKE, escaping the
// LIKE metacharacters (% and _) and the escape char itself so a literal query is
// matched verbatim (the SQL uses ESCAPE '\').
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}
