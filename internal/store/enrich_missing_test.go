package store_test

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The store half of ADR-0062: which rows a MISSING pass picks up. It is
// EnrichRecheck's twin population WITHOUT the retry-at exclusion — a 'failed'
// row is taken regardless of whether its scheduled retry has come due, because
// the whole point of the mode is to pre-empt that wait rather than honor it.

// EnrichMissing selects every visible, non-matched, non-disabled row — pending,
// unmatched, and failed in every retry state — and nothing else. Reuses
// recheckPopulation (enrich_recheck_test.go): one each of pending / matched /
// unmatched / failed-no-retry / failed-due-retry / failed-future-retry /
// disabled / hidden-unmatched.
func TestMissingSelectsEveryNonMatchedNonDisabledVisibleRow(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	want := []string{"t_parked", "t_pending", "t_retry_due", "t_retry_future", "t_unmatched"}
	if got := selectIDs(t, db, store.EnrichMissing, now); !sameIDs(got, want) {
		t.Fatalf("missing selected %v, want %v — a missing pass takes every "+
			"pending/unmatched/failed row, INCLUDING one whose retry is still in the "+
			"future (ADR-0062), and excludes only 'matched' and 'disabled'", got, want)
	}
}

// The boundary against EnrichRecheck: missing is a strict superset of it, the
// extra row being exactly the future-retry failure recheck deliberately leaves
// alone (ADR-0048).
func TestMissingIsRecheckPlusTheFutureRetryFailure(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	recheck := selectIDs(t, db, store.EnrichRecheck, now)
	missing := selectIDs(t, db, store.EnrichMissing, now)

	contains := func(set []string, id string) bool {
		for _, s := range set {
			if s == id {
				return true
			}
		}
		return false
	}
	for _, id := range recheck {
		if !contains(missing, id) {
			t.Errorf("recheck took %s and missing did not — missing must select at least "+
				"everything recheck does", id)
		}
	}
	if !contains(missing, "t_retry_future") {
		t.Errorf("missing did not take t_retry_future — pre-empting a scheduled retry is " +
			"the entire reason this mode exists (ADR-0062)")
	}
	if contains(recheck, "t_retry_future") {
		t.Errorf("recheck took t_retry_future — that would mean this test's fixture drifted " +
			"from ADR-0048's boundary, not that missing is doing anything wrong")
	}
	if len(missing) != len(recheck)+1 {
		t.Errorf("missing selected %v and recheck selected %v — missing should be exactly "+
			"one row larger (the future retry)", missing, recheck)
	}
}

// Matched/disabled/hidden stay out, exactly as they do for every other
// selection.
func TestMissingExcludesMatchedDisabledAndHidden(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	got := selectIDs(t, db, store.EnrichMissing, now)
	for _, bad := range []string{"t_matched", "t_disabled", "t_hidden"} {
		for _, id := range got {
			if id == bad {
				t.Fatalf("missing selected %s (%v) — matched/disabled/hidden rows are never "+
					"in scope for any selection", bad, got)
			}
		}
	}
}
