package store

import "fmt"

// DiscoveryItem is one Movie or Show a Discovery row can draw on, with every
// metadata dimension a Row source reads. Kind is "movie" or "show"; Watched is
// the asking User's state (a Show is watched when all its visible Episodes are).
type DiscoveryItem struct {
	ID        string
	LibraryID string
	Kind      string
	Title     string
	Year      int
	Watched   bool
	Genres    []string
	// Studios (Movie) or Networks (Show), whichever applies to Kind.
	Companies []string
	// Cast is the top five billed cast that carry a provider person ref.
	Cast []DiscoveryPerson
}

// DiscoveryPerson is one billed cast member: the provider ref and display name.
type DiscoveryPerson struct{ Ref, Name string }

// castBilled is how many billed cast count for the Actor Row source; it matches
// the filter[person] browse filter (topBilledCast).
const castBilled = 5

// DiscoveryCatalog returns every Movie and Show visible under the access filter
// (hidden excluded) with the facts the Discovery row engine groups by, plus the
// asking User's watched state. Episodes and Tracks are never returned.
func (db *DB) DiscoveryCatalog(userID string, filter AccessFilter) ([]DiscoveryItem, error) {
	var out []DiscoveryItem
	byID := map[string]*DiscoveryItem{}

	clause, args := filter.titleClauses("t.library_id", "t.content_rating")
	rows, err := db.Query(
		`SELECT t.id, t.library_id, t.title, COALESCE(t.year, 0), COALESCE(w.watched, 0)
		   FROM titles t LEFT JOIN watch_state w ON w.title_id = t.id AND w.user_id = ?
		  WHERE t.kind = 'movie' AND t.hidden = 0`+clause, append([]any{userID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store: discovery movies: %w", err)
	}
	for rows.Next() {
		it := DiscoveryItem{Kind: "movie"}
		if err := rows.Scan(&it.ID, &it.LibraryID, &it.Title, &it.Year, &it.Watched); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, it)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}

	clause, args = filter.showClauses("s.library_id", "s.id")
	rows, err = db.Query(
		`SELECT s.id, s.library_id, s.title, COALESCE(s.year, 0),
		        COALESCE((SELECT COUNT(*) = SUM(COALESCE(w.watched, 0))
		                    FROM titles e
		                    JOIN seasons se ON se.id = e.season_id
		                    LEFT JOIN watch_state w ON w.title_id = e.id AND w.user_id = ?
		                   WHERE se.show_id = s.id AND e.kind = 'episode' AND e.hidden = 0), 0)
		   FROM shows s WHERE s.hidden = 0`+clause, append([]any{userID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store: discovery shows: %w", err)
	}
	for rows.Next() {
		it := DiscoveryItem{Kind: "show"}
		if err := rows.Scan(&it.ID, &it.LibraryID, &it.Title, &it.Year, &it.Watched); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, it)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}

	// Each side table is read whole and joined in Go: the access filter already
	// decided the item set, and one scan beats a filtered query per dimension.
	scan := func(label, query string, add func(it *DiscoveryItem, a, b string)) error {
		rows, err := db.Query(query)
		if err != nil {
			return fmt.Errorf("store: discovery %s: %w", label, err)
		}
		for rows.Next() {
			var id, a, b string
			if err := rows.Scan(&id, &a, &b); err != nil {
				rows.Close()
				return err
			}
			if it, ok := byID[id]; ok {
				add(it, a, b)
			}
		}
		return closeRows(rows)
	}
	genre := func(it *DiscoveryItem, g, _ string) { it.Genres = append(it.Genres, g) }
	company := func(it *DiscoveryItem, c, _ string) { it.Companies = append(it.Companies, c) }
	cast := func(it *DiscoveryItem, ref, name string) {
		if ref != "" {
			it.Cast = append(it.Cast, DiscoveryPerson{Ref: ref, Name: name})
		}
	}
	for _, q := range []struct {
		label, sql string
		add        func(*DiscoveryItem, string, string)
	}{
		{"movie genres", `SELECT title_id, genre, '' FROM title_genres`, genre},
		{"show genres", `SELECT entity_id, genre, '' FROM entity_genres WHERE entity_type = 'show'`, genre},
		{"studios", `SELECT title_id, studio, '' FROM title_studios ORDER BY title_id, ord`, company},
		{"networks", `SELECT entity_id, network, '' FROM entity_networks WHERE entity_type = 'show' ORDER BY entity_id, ord`, company},
		// Rank over ALL cast rows (ref or not) so a ref-less billing still uses a slot.
		{"movie cast", fmt.Sprintf(`SELECT title_id, person_ref, person FROM (
			SELECT title_id, person_ref, person, ord, rowid AS rid,
			       ROW_NUMBER() OVER (PARTITION BY title_id ORDER BY ord, rowid) AS n
			  FROM title_credits WHERE kind = 'cast') WHERE n <= %d ORDER BY title_id, ord, rid`, castBilled), cast},
		{"show cast", fmt.Sprintf(`SELECT entity_id, person_ref, person FROM (
			SELECT entity_id, person_ref, person, ord, id,
			       ROW_NUMBER() OVER (PARTITION BY entity_id ORDER BY ord, id) AS n
			  FROM entity_credits WHERE entity_type = 'show' AND kind = 'cast') WHERE n <= %d ORDER BY entity_id, ord, id`, castBilled), cast},
	} {
		if err := scan(q.label, q.sql, q.add); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// closeRows closes rows and returns the iteration error, whichever came first.
func closeRows(rows interface {
	Err() error
	Close() error
}) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
