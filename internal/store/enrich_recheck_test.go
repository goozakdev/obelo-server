package store_test

import (
	"sort"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The store half of ADR-0051: which rows a RECHECK pass picks up.
//
// A recheck exists because a matching improvement makes a settled non-answer
// wrong, and nothing else in the system ever re-asks one. It must therefore
// select strictly more than an only-new pass and strictly less than a full one —
// getting either boundary wrong turns the feature back into one of the two things
// ADR-0051 rejected (a scan that re-asks everything forever, or ModeFull's 16x
// cost).

// recheckPopulation seeds one Library holding one Title in every enrichment state
// the selection has to distinguish, and returns the db.
func recheckPopulation(t *testing.T, now time.Time) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	add := func(id, status, retryAt string, hidden int) {
		t.Helper()
		mustExec(t, db,
			`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
			                     enrichment_status, enrichment_retry_at, hidden, added_at)
			 VALUES (?, 'lib', 'movie', ?, ?, ?, ?, ?, ?, ?)`,
			id, id, id+"|key", id, status, retryAt, hidden, id)
	}
	add("t_pending", "pending", "", 0)
	add("t_matched", "matched", "", 0)
	add("t_unmatched", "unmatched", "", 0)
	add("t_parked", "failed", "", 0)
	add("t_retry_due", "failed", now.Add(-time.Minute).UTC().Format(time.RFC3339), 0)
	add("t_retry_future", "failed", now.Add(time.Hour).UTC().Format(time.RFC3339), 0)
	add("t_disabled", "disabled", "", 0)
	add("t_hidden", "unmatched", "", 1)
	return db
}

func sortedTitleIDs(titles []store.Title) []string {
	out := titleIDs(titles)
	sort.Strings(out)
	return out
}

func selectIDs(t *testing.T, db *store.DB, sel store.EnrichSelect, now time.Time) []string {
	t.Helper()
	got, err := db.TitlesForEnrichment("lib", sel, now)
	if err != nil {
		t.Fatalf("TitlesForEnrichment: %v", err)
	}
	return sortedTitleIDs(got)
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// EnrichRecheck is EnrichPending plus the two SETTLED non-answers, and nothing
// else. The 'matched' and 'disabled' rows are the "nothing else" — a recheck that
// swept those in would be ModeFull wearing a different name.
func TestRecheckSelectsTheSettledNonAnswersAndNothingElse(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	want := []string{"t_parked", "t_pending", "t_retry_due", "t_unmatched"}
	if got := selectIDs(t, db, store.EnrichRecheck, now); !sameIDs(got, want) {
		t.Fatalf("recheck selected %v, want %v — a recheck re-asks the settled non-answers "+
			"('unmatched', and 'failed' with no retry scheduled) on top of everything an "+
			"only-new pass takes, and nothing beyond them", got, want)
	}
}

// The boundary that keeps a recheck cheap: it never takes a row nothing is wrong
// with. Asserted as a RELATION between the three selections rather than as three
// literal lists, so it still holds when the population grows.
func TestRecheckSitsStrictlyBetweenOnlyNewAndFull(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	pending := selectIDs(t, db, store.EnrichPending, now)
	recheck := selectIDs(t, db, store.EnrichRecheck, now)
	all := selectIDs(t, db, store.EnrichAll, now)

	contains := func(set []string, id string) bool {
		for _, s := range set {
			if s == id {
				return true
			}
		}
		return false
	}
	for _, id := range pending {
		if !contains(recheck, id) {
			t.Errorf("only-new took %s and recheck did not — a recheck that skipped new work "+
				"to redo old work would be a strange thing to hand an operator (ADR-0051)", id)
		}
	}
	if len(recheck) <= len(pending) {
		t.Errorf("recheck selected %v and only-new selected %v — if they are the same set the "+
			"mode does nothing, which is the exact bug ADR-0051 was written from", recheck, pending)
	}
	if len(recheck) >= len(all) {
		t.Errorf("recheck selected %v and full selected %v — a recheck that costs what a full "+
			"pass costs is ModeFull, and ADR-0051 exists because ModeFull was 16x too "+
			"expensive for this job", recheck, all)
	}
	if contains(recheck, "t_matched") || contains(recheck, "t_disabled") {
		t.Errorf("recheck took a matched/disabled row (%v): neither is a non-answer from the "+
			"provider, and re-asking them is what makes a recheck expensive", recheck)
	}
}

// ADR-0048's distinction, kept intact. A 'failed' row with a retry still in the
// future is IN-FLIGHT WORK the server already owns; a 'failed' row with no retry
// is parked and is nobody's job until someone re-asks it.
func TestRecheckTakesAParkedFailureButNotOneStillAwaitingItsRetry(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	got := selectIDs(t, db, store.EnrichRecheck, now)
	found := map[string]bool{}
	for _, id := range got {
		found[id] = true
	}
	if !found["t_parked"] {
		t.Errorf("a parked 'failed' row was not re-asked (%v) — nothing else in the system "+
			"will ever look at it again", got)
	}
	if found["t_retry_future"] {
		t.Errorf("a 'failed' row whose retry is still in the future was re-asked (%v) — that "+
			"is in-flight work, and re-asking it early erases the distinction between "+
			"'no record' and 'could not reach the provider' (ADR-0048)", got)
	}
}

// Hidden (all-Files-Missing) Titles stay out, exactly as they do for every other
// selection: enrichment does not spend calls on soft-deleted media (ADR-0008).
func TestRecheckStillSkipsHiddenTitles(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	db := recheckPopulation(t, now)

	for _, id := range selectIDs(t, db, store.EnrichRecheck, now) {
		if id == "t_hidden" {
			t.Fatal("recheck selected a hidden Title — a soft-deleted item is not work")
		}
	}
}
