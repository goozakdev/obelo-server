package access

import (
	"sort"
	"testing"
)

// TestMaturityRankLadder exhaustively pins every ladder label's rank and the
// "unknown is unknown" behavior — the heart of the cross-system ceiling.
func TestMaturityRankLadder(t *testing.T) {
	known := map[string]int{
		"G": 1, "TV-Y": 1, "TV-G": 1,
		"PG": 2, "TV-Y7": 2, "TV-PG": 2,
		"PG-13": 3, "TV-14": 3,
		"R": 4, "TV-MA": 4,
		"NC-17": 5,
	}
	for label, want := range known {
		got, ok := maturityRank(label)
		if !ok || got != want {
			t.Errorf("maturityRank(%q) = (%d,%v), want (%d,true)", label, got, ok, want)
		}
	}

	// Unknown / unrated labels are not on the ladder.
	for _, label := range []string{"", "NR", "Unrated", "Not Rated", "X", "18", "FSK-16", "pg-13"} {
		if _, ok := maturityRank(label); ok {
			t.Errorf("maturityRank(%q) reported known, want unknown", label)
		}
	}
}

// TestCeilingRank: a known label maps to its rank; anything else is 0 (uncapped).
func TestCeilingRank(t *testing.T) {
	if ceilingRank("PG-13") != 3 {
		t.Errorf("ceilingRank(PG-13) = %d, want 3", ceilingRank("PG-13"))
	}
	if ceilingRank("") != 0 || ceilingRank("NR") != 0 {
		t.Error("empty/unknown ceiling label must map to 0 (uncapped)")
	}
}

// TestAllowsRating: a PG-13 ceiling admits its rung and below across both
// systems and unrated, and blocks above; uncapped admits everything.
func TestAllowsRating(t *testing.T) {
	capped := Scope{RatingCeiling: 3} // PG-13 / TV-14
	for _, label := range []string{"G", "PG", "PG-13", "TV-Y", "TV-PG", "TV-14", "", "NR", "Unrated"} {
		if !capped.AllowsRating(label) {
			t.Errorf("PG-13 ceiling should allow %q", label)
		}
	}
	for _, label := range []string{"R", "NC-17", "TV-MA"} {
		if capped.AllowsRating(label) {
			t.Errorf("PG-13 ceiling should block %q", label)
		}
	}

	uncapped := Scope{RatingCeiling: 0}
	for _, label := range []string{"NC-17", "TV-MA", "R", "G", ""} {
		if !uncapped.AllowsRating(label) {
			t.Errorf("uncapped scope should allow %q", label)
		}
	}
}

// TestBlockedRatings: the SQL exclusion set is exactly the labels above the
// ceiling; uncapped yields none.
func TestBlockedRatings(t *testing.T) {
	got := Scope{RatingCeiling: 3}.blockedRatings()
	sort.Strings(got)
	want := []string{"NC-17", "R", "TV-MA"}
	if len(got) != len(want) {
		t.Fatalf("blockedRatings(PG-13) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blockedRatings(PG-13) = %v, want %v", got, want)
		}
	}
	if n := len(Scope{RatingCeiling: 0}.blockedRatings()); n != 0 {
		t.Errorf("uncapped blockedRatings = %d labels, want 0", n)
	}
}
