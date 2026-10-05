package store

import (
	"fmt"
	"strings"
)

// Bulk reads for API list endpoints that would otherwise issue one query per row.

// CollectionVisibleSummary is what a Collection card needs of one viewer's
// visible membership: how many members, and the first in sort_title order (its
// poster).
type CollectionVisibleSummary struct {
	Count        int
	FirstTitleID string
}

// CollectionVisibleSummaries answers, in one read, every Collection's visible
// member count and first member for a viewer. A member counts exactly when
// ResolveVisibleTitles would return it (not Missing, inside the filter), and the
// first is the head of CollectionMemberIDs' (sort_title, id) order restricted to
// those. A Collection with no visible member has no entry.
func (db *DB) CollectionVisibleSummaries(filter AccessFilter) (map[string]CollectionVisibleSummary, error) {
	accessClause, args := filter.titleClauses("t.library_id", "t.content_rating")
	// MIN over sort_title NUL id picks the (sort_title, id) head: NUL sorts below
	// every other byte, so a shorter sort_title still precedes a longer one it
	// prefixes, as ORDER BY sort_title, id would put it.
	rows, err := db.Query(
		`SELECT ci.collection_id, COUNT(*), MIN(t.sort_title || char(0) || t.id)
		   FROM collection_items ci
		   JOIN titles t ON t.id = ci.title_id
		  WHERE t.hidden = 0`+accessClause+`
		  GROUP BY ci.collection_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: summarising collections: %w", err)
	}
	defer rows.Close()
	out := map[string]CollectionVisibleSummary{}
	for rows.Next() {
		var id, head string
		var s CollectionVisibleSummary
		if err := rows.Scan(&id, &s.Count, &head); err != nil {
			return nil, fmt.Errorf("store: scanning collection summary: %w", err)
		}
		s.FirstTitleID = head[strings.LastIndexByte(head, 0)+1:]
		out[id] = s
	}
	return out, rows.Err()
}

// LibrariesForLinks lists the linked Libraries of every given Link in one read,
// keyed by Link id, each oldest first (LibrariesForLink's order). A Link with
// none has no entry.
func (db *DB) LibrariesForLinks(linkIDs []string) (map[string][]Library, error) {
	out := map[string][]Library{}
	if len(linkIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(
		`SELECT `+libraryColumns+` FROM libraries WHERE link_id IN (`+placeholders(len(linkIDs))+`)
		  ORDER BY created_at, id`, toArgs(linkIDs)...)
	if err != nil {
		return nil, fmt.Errorf("store: listing libraries for links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scanning library for links: %w", err)
		}
		out[l.LinkID] = append(out[l.LinkID], l)
	}
	return out, rows.Err()
}
