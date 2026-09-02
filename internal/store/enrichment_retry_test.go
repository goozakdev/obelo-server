package store_test

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The store half of ADR-0048: which rows an only-new pass picks up, and which
// rows reach the Admin's attention list. Before this, 'failed' was terminal in
// both — a Title lost to one provider blip was skipped by every subsequent pass
// AND listed as something a human had to hand-match.

// seedTitle inserts one Movie Title in a fresh Library and returns the db.
func seedTitle(t *testing.T, id string) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
	                 VALUES (?, 'lib', 'movie', 'Inception', 'inception|2010', 'inception')`, id)
	return db
}

func titleIDs(titles []store.Title) []string {
	out := make([]string, 0, len(titles))
	for _, t := range titles {
		out = append(out, t.ID)
	}
	return out
}

// A transient failure schedules a retry, and the only-new pass picks the Title up
// again once that instant has passed — the behavior the whole change exists for.
func TestOnlyNewPassCollectsADueRetry(t *testing.T) {
	db := seedTitle(t, "m1")
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	if err := db.SetTitleEnrichmentRetry("m1", 1, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("SetTitleEnrichmentRetry: %v", err)
	}

	// Before the retry is due the pass leaves it alone: retrying every pass would
	// hammer a provider that is already struggling.
	got, err := db.TitlesForEnrichment("lib", store.EnrichPending, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("TitlesForEnrichment: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a retry scheduled 5m out was collected after 1m (%v) — the backoff does nothing",
			titleIDs(got))
	}

	// Once due, it is in scope exactly as a 'pending' Title would be.
	got, err = db.TitlesForEnrichment("lib", store.EnrichPending, now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("TitlesForEnrichment: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("collected %v, want [m1] — a due retry is being skipped, which is the original "+
			"bug: the Title is parked forever unless somebody runs a full pass", titleIDs(got))
	}
	if got[0].EnrichmentAttempts != 1 {
		t.Errorf("attempts read back as %d, want 1 — the pass cannot compute the next backoff "+
			"step without the streak", got[0].EnrichmentAttempts)
	}
}

// A PERMANENT failure still parks. Retrying a rejected API key or a malformed
// request forever would replace one silent failure with another.
func TestPermanentFailureIsNeverCollected(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentStatus("m1", "failed", store.EnrichmentReasonNone); err != nil {
		t.Fatalf("SetTitleEnrichmentStatus: %v", err)
	}
	far := time.Now().AddDate(10, 0, 0)
	got, err := db.TitlesForEnrichment("lib", store.EnrichPending, far)
	if err != nil {
		t.Fatalf("TitlesForEnrichment: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a parked failure was collected %v — 'failed' with no scheduled retry must stay "+
			"put and wait for the Admin", titleIDs(got))
	}
}

// The attention list is for work a human must do. A Title being retried is not
// that — until the streak says the trouble has outlived every plausible blip.
func TestAttentionListSeparatesRetryingFromParked(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	for _, id := range []string{"parked", "retrying", "escalated", "nomatch", "fine"} {
		mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		                 VALUES (?, 'lib', 'movie', ?, ?, ?)`, id, id, id, id)
	}
	soon := time.Now().Add(time.Hour)
	if err := db.SetTitleEnrichmentStatus("parked", "failed", store.EnrichmentReasonNone); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitleEnrichmentRetry("retrying", 1, soon); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitleEnrichmentRetry("escalated", store.EnrichRetryEscalateAfter, soon); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitleEnrichmentStatus("nomatch", "unmatched", store.EnrichmentReasonSearchNoMatch); err != nil {
		t.Fatal(err)
	}

	got, err := db.TitlesNeedingMatch("lib")
	if err != nil {
		t.Fatalf("TitlesNeedingMatch: %v", err)
	}
	listed := map[string]bool{}
	for _, ti := range got {
		listed[ti.ID] = true
	}

	if !listed["parked"] {
		t.Error("a permanently failed Title is missing from the attention list")
	}
	if !listed["nomatch"] {
		t.Error("an unmatched Title is missing from the attention list")
	}
	if listed["retrying"] {
		t.Error("a Title scheduled for its first retry is on the attention list — the queue " +
			"fills with rows that clear themselves before anyone reads them")
	}
	if !listed["escalated"] {
		t.Errorf("a Title that has failed %d times straight is NOT on the attention list — it "+
			"retries forever with nobody told, which trades one invisible failure for another",
			store.EnrichRetryEscalateAfter)
	}
	if listed["fine"] {
		t.Error("a never-enriched Title is on the attention list")
	}
}

// Any settled outcome resets the streak. A Title that fails, recovers, and fails
// again months later is at the start of a new streak — not one step from the
// daily ceiling and instant escalation.
func TestSettlingResetsTheFailureStreak(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentRetry("m1", 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Overview: "A thief who steals corporate secrets.", Source: "tmdb",
	}, nil); err != nil {
		t.Fatalf("WriteTitleEnrichment: %v", err)
	}

	got, err := db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatalf("TitleForEnrichmentByID: %v", err)
	}
	if got.EnrichmentStatus != "matched" {
		t.Fatalf("status = %q, want matched", got.EnrichmentStatus)
	}
	if got.EnrichmentAttempts != 0 || got.EnrichmentRetryAt != "" {
		t.Fatalf("a matched Title still carries retry bookkeeping (attempts=%d, retryAt=%q) — "+
			"its next failure would resume an old streak and escalate early",
			got.EnrichmentAttempts, got.EnrichmentRetryAt)
	}

	// And a settled non-match clears it too.
	if err := db.SetTitleEnrichmentRetry("m1", 4, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonSearchNoMatch); err != nil {
		t.Fatal(err)
	}
	got, err = db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnrichmentAttempts != 0 || got.EnrichmentRetryAt != "" {
		t.Fatalf("an unmatched Title still carries retry bookkeeping (attempts=%d, retryAt=%q)",
			got.EnrichmentAttempts, got.EnrichmentRetryAt)
	}
}

// The migration hands every pre-existing 'failed' row one retry. Without it the
// fix would apply only to failures that happen after the upgrade, and the rows
// already stranded by the old behavior would stay stranded.
func TestMigrationSchedulesOneRetryForRowsTheOldBehaviorParked(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	// Write the row the way the pre-0053 server did: status only, no retry columns.
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                                     enrichment_status)
	                 VALUES ('old', 'lib', 'movie', 'Heat', 'heat|1995', 'heat', 'failed')`)
	mustExec(t, db, `UPDATE titles SET enrichment_retry_at = '1970-01-01T00:00:00Z'
	                  WHERE enrichment_status = 'failed'`)

	got, err := db.TitlesForEnrichment("lib", store.EnrichPending, time.Now())
	if err != nil {
		t.Fatalf("TitlesForEnrichment: %v", err)
	}
	if len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("collected %v, want [old] — the backfilled retry is not due, so every failure "+
			"the old server recorded stays parked after the upgrade", titleIDs(got))
	}
	if got[0].EnrichmentAttempts != 0 {
		t.Errorf("backfilled attempts = %d, want 0 so the retry that follows gets the shortest "+
			"backoff rather than the ceiling", got[0].EnrichmentAttempts)
	}
}

// A parent (Show/Artist/Album) carries the same bookkeeping. It matters more
// there: enrichParent skips any non-pending parent, so one parked Show used to
// hold every Season and Episode under it un-enriched.
func TestParentEntityCarriesRetryBookkeeping(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	                 VALUES ('sh1', 'lib', 'The Bear', 'the bear', 'bear')`)

	due := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := db.SetEntityEnrichmentRetry(store.EntityShow, "sh1", 2, due); err != nil {
		t.Fatalf("SetEntityEnrichmentRetry: %v", err)
	}
	got, err := db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatalf("EntityEnrichmentByID: %v", err)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", got.Attempts)
	}
	if got.RetryAt != due.Format(time.RFC3339) {
		t.Errorf("retryAt = %q, want %q", got.RetryAt, due.Format(time.RFC3339))
	}

	// Settling clears it, exactly as on a leaf.
	if err := db.SetEntityEnrichmentStatus(store.EntityShow, "sh1", "unmatched"); err != nil {
		t.Fatal(err)
	}
	got, err = db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 0 || got.RetryAt != "" {
		t.Fatalf("a settled parent still carries retry bookkeeping (attempts=%d, retryAt=%q)",
			got.Attempts, got.RetryAt)
	}
}
