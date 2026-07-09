package store

import "strings"

// AccessFilter is the persistence-side access predicate the cross-library browse
// reads (search, the Home rows) apply: the visible Library set and the maturity
// ceiling. It is the store projection of a User's access scope — the access
// package owns the domain type and converts to this one, so store never imports
// access (avoiding an import cycle, since access reads users via store).
//
// An all-access filter (AllLibraries true, RatingCeiling 0) adds no SQL, so the
// reads behave exactly as they did before per-User access existed. The enforcing
// slices populate LibraryIDs / RatingCeiling and the predicate starts biting.
type AccessFilter struct {
	AllLibraries bool
	LibraryIDs   []string
	// BlockedRatings is the set of Content-rating labels above the Rating ceiling
	// — a Title carrying one is hidden. Empty = uncapped (the access package maps
	// the ceiling rank to these labels, so the store filters by label, not rank).
	BlockedRatings []string
}

// AllAccess is the unrestricted filter (every Library, uncapped) — what an Admin
// always resolves to. Tests that exercise a store read directly pass this.
func AllAccess() AccessFilter { return AccessFilter{AllLibraries: true} }

// libraryClause returns an ANDable SQL fragment (with a leading " AND ") plus its
// positional args restricting libraryCol to the filter's visible Library set, or
// ("", nil) when the filter imposes no Library restriction (all-access). A
// restrictive filter with no visible Libraries returns a predicate matching
// nothing. The rating dimension is added by a later slice; RatingCeiling is
// carried but not yet applied here.
func (f AccessFilter) libraryClause(libraryCol string) (string, []any) {
	if f.AllLibraries {
		return "", nil
	}
	if len(f.LibraryIDs) == 0 {
		return " AND 1 = 0", nil // no Libraries visible → match nothing
	}
	var b strings.Builder
	b.WriteString(" AND ")
	b.WriteString(libraryCol)
	b.WriteString(" IN (")
	args := make([]any, 0, len(f.LibraryIDs))
	for i, id := range f.LibraryIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteByte(')')
	return b.String(), args
}

// titleRatingClause returns an ANDable fragment (leading " AND ") excluding rows
// whose ratingCol is an above-ceiling Content rating, or ("", nil) when uncapped.
// An unrated/unknown label is not in the blocked set, so it survives — the
// "unrated is visible" policy falls out of NOT IN.
func (f AccessFilter) titleRatingClause(ratingCol string) (string, []any) {
	if len(f.BlockedRatings) == 0 {
		return "", nil
	}
	return " AND " + ratingCol + " NOT IN (" + placeholders(len(f.BlockedRatings)) + ")",
		toArgs(f.BlockedRatings)
}

// showRatingClause returns an ANDable fragment excluding Shows whose ENRICHED
// Content rating (in entity_enrichment, not on the shows row) is above the
// ceiling, or ("", nil) when uncapped. A Show with no enriched rating is not
// matched by the subquery, so it survives (unrated is visible).
func (f AccessFilter) showRatingClause(showIDCol string) (string, []any) {
	if len(f.BlockedRatings) == 0 {
		return "", nil
	}
	return " AND " + showIDCol + ` NOT IN (
		SELECT entity_id FROM entity_enrichment
		 WHERE entity_type = 'show' AND content_rating IN (` + placeholders(len(f.BlockedRatings)) + "))",
		toArgs(f.BlockedRatings)
}

// titleClauses combines the Library and Title-rating predicates for a query over
// the titles table (both apply at the same WHERE level). Either part is empty
// when its dimension is unrestricted.
func (f AccessFilter) titleClauses(libraryCol, ratingCol string) (string, []any) {
	lc, la := f.libraryClause(libraryCol)
	rc, ra := f.titleRatingClause(ratingCol)
	return lc + rc, append(la, ra...)
}

// showClauses combines the Library and Show-rating predicates for a query over
// the shows table.
func (f AccessFilter) showClauses(libraryCol, showIDCol string) (string, []any) {
	lc, la := f.libraryClause(libraryCol)
	rc, ra := f.showRatingClause(showIDCol)
	return lc + rc, append(la, ra...)
}

// placeholders returns "?, ?, …" with n marks for an IN-list.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// toArgs widens a []string to the []any a query expects.
func toArgs(ss []string) []any {
	args := make([]any, len(ss))
	for i, s := range ss {
		args[i] = s
	}
	return args
}
