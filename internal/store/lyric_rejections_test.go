package store_test

import (
	"fmt"
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

// TestATrackKeepsOnlyItsTwentyMostRecentRejections: rejections are capped per
// Track, the oldest going first, so a Track's list cannot grow without end.
// Another Track's rejections are its own.
func TestATrackKeepsOnlyItsTwentyMostRecentRejections(t *testing.T) {
	db := seedTitle(t, "t1")
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
	                 VALUES ('t2', 'lib', 'movie', 'Tenet', 'tenet|2020', 'tenet')`)
	if err := db.RejectFetchedLyrics("t2", "other-track"); err != nil {
		t.Fatalf("RejectFetchedLyrics(t2): %v", err)
	}
	var want []string
	for i := 1; i <= 25; i++ {
		answer := fmt.Sprintf("answer-%02d", i)
		if err := db.RejectFetchedLyrics("t1", answer); err != nil {
			t.Fatalf("RejectFetchedLyrics(%q): %v", answer, err)
		}
		if i > 5 {
			want = append(want, answer)
		}
	}
	got, err := db.RejectedLyrics("t1")
	sort.Strings(got)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("RejectedLyrics = (%v, %v), want answer-06 to answer-25", got, err)
	}
	if got, err := db.RejectedLyrics("t2"); err != nil || !reflect.DeepEqual(got, []string{"other-track"}) {
		t.Fatalf("RejectedLyrics(t2) = (%v, %v), want its own one kept", got, err)
	}
}
