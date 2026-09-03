package store_test

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The store half of ADR-0050's reason column: WHEN a reason is written, and — the
// part that actually costs something to get wrong — when it is taken away again.
//
// A stale reason is worse than none, because the row then confidently explains a
// problem that no longer exists. Every test below is about one of the four moments
// the column has to move: a settled failure writes one, a match clears one, a
// settled outcome with nothing to say clears one, and an Admin's re-pin clears one.
// The fifth moment — a transient failure — is the one where it must NOT move.

// reasonOf reads one Title's settled-failure reason back through the ordinary
// enriched projection, so a column missing from enrichedTitleColumns fails here
// rather than silently reading "" forever.
func reasonOf(t *testing.T, db *store.DB, id string) (status, reason string) {
	t.Helper()
	got, err := db.TitleForEnrichmentByID(id)
	if err != nil {
		t.Fatalf("TitleForEnrichmentByID(%s): %v", id, err)
	}
	return got.EnrichmentStatus, got.EnrichmentReason
}

// Each of the five values round-trips exactly, in the same statement that writes
// the status. The set is closed, so a value that cannot be stored is a value the
// client can never be asked to render.
func TestEverySettledReasonRoundTrips(t *testing.T) {
	for _, reason := range []string{
		store.EnrichmentReasonAlbumUnmatched,
		store.EnrichmentReasonNotInTracklist,
		store.EnrichmentReasonTagIDUnresolved,
		store.EnrichmentReasonSearchNoMatch,
		store.EnrichmentReasonSearchRejected,
	} {
		t.Run(reason, func(t *testing.T) {
			db := seedTitle(t, "m1")
			if err := db.SetTitleEnrichmentStatus("m1", "unmatched", reason); err != nil {
				t.Fatalf("SetTitleEnrichmentStatus: %v", err)
			}
			status, got := reasonOf(t, db, "m1")
			if status != "unmatched" || got != reason {
				t.Fatalf("read back (%q, %q), want (unmatched, %q) — the status and the reason "+
					"are written in one statement precisely so they can never disagree",
					status, got, reason)
			}
			// The attention list is where the reason is actually consumed; a projection
			// that drops it there renders the generic sentence on every row forever.
			needing, err := db.TitlesNeedingMatch("lib")
			if err != nil {
				t.Fatalf("TitlesNeedingMatch: %v", err)
			}
			if len(needing) != 1 || needing[0].EnrichmentReason != reason {
				t.Fatalf("attention list carried %+v, want one row with reason %q", needing, reason)
			}
		})
	}
}

// THE stale-row case, at the store: a Title that failed with a diagnosis and then
// MATCHED carries no reason at all. Without this the row is gone from the queue but
// the column still names an album problem nobody has, and the next failure — of any
// kind — is read against a sentence from the last one.
func TestAMatchClearsTheReason(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonAlbumUnmatched); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Overview: "A thief who steals corporate secrets.", Source: "tmdb",
	}, nil); err != nil {
		t.Fatalf("WriteTitleEnrichment: %v", err)
	}
	status, reason := reasonOf(t, db, "m1")
	if status != "matched" || reason != store.EnrichmentReasonNone {
		t.Fatalf("after a match: status %q, reason %q — want (matched, \"\"). A reason that "+
			"outlives the failure it described is worse than none: the row confidently "+
			"explains a problem that no longer exists (ADR-0050)", status, reason)
	}
}

// A settled outcome with NOTHING to diagnose still writes the column, and writing
// "" is the whole point — it is an overwrite, not a skip. 'disabled' is the clean
// case: nobody asked a provider anything, so any sentence left standing is a
// sentence about a question that is no longer being asked.
func TestASettledOutcomeWithNoDiagnosisClearsAStaleOne(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonTagIDUnresolved); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := db.SetTitleEnrichmentStatus("m1", "disabled", store.EnrichmentReasonNone); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if status, reason := reasonOf(t, db, "m1"); status != "disabled" || reason != "" {
		t.Fatalf("after 'disabled': status %q, reason %q — want (disabled, \"\"); the empty "+
			"reason is a value that must be WRITTEN, not a call that may be skipped",
			status, reason)
	}
}

// A TRANSIENT failure is not a diagnosis and does not touch the column — in either
// direction (ADR-0048). It writes none, because nothing was learned about the item;
// and it clears none, because throwing away a real diagnosis every time the network
// hiccups on the way to re-confirming it would lose the reason for the whole
// duration of an outage — exactly when the queue is longest.
func TestATransientFailureNeitherWritesNorClearsAReason(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonSearchRejected); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := db.SetTitleEnrichmentRetry("m1", 1, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("SetTitleEnrichmentRetry: %v", err)
	}
	status, reason := reasonOf(t, db, "m1")
	if status != "failed" {
		t.Fatalf("status %q, want failed", status)
	}
	if reason != store.EnrichmentReasonSearchRejected {
		t.Fatalf("reason %q, want %q — a 503 says nothing about the item, so it must not "+
			"overwrite what the last SETTLED outcome concluded about it",
			reason, store.EnrichmentReasonSearchRejected)
	}
}

// An Admin re-pointing the record hands the Title back to the pass as 'pending', so
// the old diagnosis is void by construction: the whole question is about to be
// re-asked against a different id.
func TestAnAdminsRePinClearsTheReason(t *testing.T) {
	db := seedTitle(t, "m1")
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonSearchNoMatch); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{TMDBID: "27205"}, store.OriginChosen); err != nil {
		t.Fatalf("SetTitleExternalMatch: %v", err)
	}
	if status, reason := reasonOf(t, db, "m1"); status != "pending" || reason != "" {
		t.Fatalf("after a re-pin: status %q, reason %q — want (pending, \"\"). The reason "+
			"described a lookup that is about to be replaced", status, reason)
	}
}
