package access

// The maturity ladder maps both rating systems — movie (US MPAA) and US TV —
// onto one comparable rank, so a single Rating ceiling caps a Movie Library and
// a TV Library alike (a "PG-13" ceiling and a "TV-14" Title are the same rung).
//
// A Title is visible iff its rank is at or below the ceiling's rank. A label NOT
// on the ladder — an empty Content rating, or 'NR' / 'Unrated' / a foreign
// system — is "no rating information" and is deliberately NOT hidden by a
// ceiling: Content rating comes from optional Enrichment, and hiding un-enriched
// Titles would make a capped Member's freshly-scanned Library look empty. The
// strict "hide what we can't classify" posture is a future per-ceiling toggle,
// out of scope here.
var maturityLadder = map[string]int{
	// rank 1
	"G": 1, "TV-Y": 1, "TV-G": 1,
	// rank 2
	"PG": 2, "TV-Y7": 2, "TV-PG": 2,
	// rank 3
	"PG-13": 3, "TV-14": 3,
	// rank 4
	"R": 4, "TV-MA": 4,
	// rank 5
	"NC-17": 5,
}

// maturityRank returns a Content-rating label's rank on the ladder, and whether
// the label is known (on the ladder). An unknown/empty label is (0, false) —
// "no rating information", treated as visible under any ceiling.
func maturityRank(label string) (rank int, known bool) {
	r, ok := maturityLadder[label]
	return r, ok
}

// ceilingRank maps a stored ceiling label to its rank, or 0 (uncapped) when the
// label is empty or not on the ladder. Resolve uses it to turn the User's stored
// ceiling into the rank carried on the Scope.
func ceilingRank(label string) int {
	r, ok := maturityRank(label)
	if !ok {
		return 0
	}
	return r
}

// isLadderLabel reports whether label is a settable ceiling — a known ladder
// label. The grant-management layer validates a ceiling against this.
func isLadderLabel(label string) bool {
	_, ok := maturityRank(label)
	return ok
}

// AllowsRating reports whether the Scope may see a Title with the given Content
// rating. Uncapped (ceiling 0) and unknown/unrated labels are always visible;
// otherwise the Title is visible iff its rank is at or below the ceiling.
func (s Scope) AllowsRating(contentRating string) bool {
	if s.RatingCeiling == 0 {
		return true
	}
	rank, known := maturityRank(contentRating)
	if !known {
		return true // unrated / unknown is not hidden by a ceiling (documented policy)
	}
	return rank <= s.RatingCeiling
}

// blockedRatings returns the ladder labels strictly above the ceiling — the set
// the aggregate SQL reads exclude (content_rating IN these → hidden). Empty when
// uncapped, so the predicate is a no-op. Computed from the ladder so the store
// never needs to know ranks, only labels.
func (s Scope) blockedRatings() []string {
	if s.RatingCeiling == 0 {
		return nil
	}
	var out []string
	for label, rank := range maturityLadder {
		if rank > s.RatingCeiling {
			out = append(out, label)
		}
	}
	return out
}
