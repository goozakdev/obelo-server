package store_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
)

// TestRejectingFetchedLyricsForgetsThemAndRemembersTheRejection: a rejection
// removes the Track's fetched row — its Local row stays — and is kept, with every
// earlier one, until the Track itself is gone. Rejecting the same answer twice
// keeps it once.
func TestRejectingFetchedLyricsForgetsThemAndRemembersTheRejection(t *testing.T) {
	db := seedTitle(t, "t1")
	mustExec(t, db, `INSERT INTO lyrics (title_id, source, kind, body) VALUES ('t1', 'local', 'plain', 'Local words')`)
	if err := db.WriteFetchedLyrics("t1", store.FetchedLyrics{
		Lyrics: &lyrics.Lyrics{Kind: lyrics.Plain, Text: "Wrong"}, Provider: "a", Question: "q",
	}); err != nil {
		t.Fatalf("WriteFetchedLyrics: %v", err)
	}
	if got, err := db.RejectedLyrics("t1"); err != nil || len(got) != 0 {
		t.Fatalf("RejectedLyrics before any rejection = (%v, %v), want none", got, err)
	}

	for _, answer := range []string{"answer-1", "answer-2", "answer-1"} {
		if err := db.RejectFetchedLyrics("t1", answer); err != nil {
			t.Fatalf("RejectFetchedLyrics(%q): %v", answer, err)
		}
	}
	got, err := db.RejectedLyrics("t1")
	sort.Strings(got)
	if err != nil || !reflect.DeepEqual(got, []string{"answer-1", "answer-2"}) {
		t.Fatalf("RejectedLyrics = (%v, %v), want answer-1 and answer-2", got, err)
	}
	if _, ok, err := db.FetchedLyrics("t1"); err != nil || ok {
		t.Fatalf("FetchedLyrics after a rejection = (%v, %v), want none", ok, err)
	}
	if l, ok, err := db.LocalLyrics("t1"); err != nil || !ok || l.Text != "Local words" {
		t.Fatalf("LocalLyrics after a rejection = (%+v, %v, %v), want the Local words kept", l, ok, err)
	}

	mustExec(t, db, `DELETE FROM titles WHERE id = 't1'`)
	if got, err := db.RejectedLyrics("t1"); err != nil || len(got) != 0 {
		t.Fatalf("RejectedLyrics after the Track is gone = (%v, %v), want none", got, err)
	}
}
