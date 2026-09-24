package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Fetched lyrics and the Lyric provider order, at the layer they are stored on.

// TestFetchedLyricsRoundTripHitsAndMisses: a Synced hit, a Plain hit and a miss
// each read back as written, and each write replaces the one before — one
// fetched row per Track.
func TestFetchedLyricsRoundTripHitsAndMisses(t *testing.T) {
	db := seedTitle(t, "t1")
	if _, ok, err := db.FetchedLyrics("t1"); err != nil || ok {
		t.Fatalf("FetchedLyrics before any write = (%v, %v), want none", ok, err)
	}
	for _, want := range []store.FetchedLyrics{
		{Lyrics: &lyrics.Lyrics{Kind: lyrics.Synced, Lines: []lyrics.Line{{StartMs: 100, Text: "One"}, {StartMs: 900, Text: ""}}},
			Provider: "a", Question: "q1"},
		{Lyrics: &lyrics.Lyrics{Kind: lyrics.Plain, Text: "Words"}, Provider: "b", Question: "q2"},
		{Question: "q3"},
	} {
		if err := db.WriteFetchedLyrics("t1", want); err != nil {
			t.Fatalf("WriteFetchedLyrics: %v", err)
		}
		got, ok, err := db.FetchedLyrics("t1")
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("FetchedLyrics = (%+v, %v, %v), want %+v", got, ok, err, want)
		}
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM lyrics WHERE title_id = 't1'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("lyrics rows for t1 = %d (%v), want 1", rows, err)
	}
}

// TestAMissIsNeverALocalLyric: the schema refuses a Local row that says 'none'.
func TestAMissIsNeverALocalLyric(t *testing.T) {
	db := seedTitle(t, "t1")
	if _, err := db.Exec(`INSERT INTO lyrics (title_id, source, kind, body) VALUES ('t1', 'local', 'none', '')`); err == nil {
		t.Fatal("a local 'none' row was accepted")
	}
}

// TestLyricProviderOrderIsReplacedWholeAndForgottenOnUninstall: the order reads
// back as set, a second set replaces it, and uninstalling a provider forgets its
// place.
func TestLyricProviderOrderIsReplacedWholeAndForgottenOnUninstall(t *testing.T) {
	db := openTemp(t)
	if err := db.SetLyricProviderOrder([]string{"b", "a", "c"}); err != nil {
		t.Fatalf("SetLyricProviderOrder: %v", err)
	}
	if err := db.SetLyricProviderOrder([]string{"c", "a"}); err != nil {
		t.Fatalf("SetLyricProviderOrder: %v", err)
	}
	if got, err := db.LyricProviderOrder(); err != nil || !reflect.DeepEqual(got, []string{"c", "a"}) {
		t.Fatalf("LyricProviderOrder = %v (%v), want [c a]", got, err)
	}
	if err := db.DeletePlugin("c"); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}
	if got, err := db.LyricProviderOrder(); err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("LyricProviderOrder after uninstalling c = %v (%v), want [a]", got, err)
	}
}
