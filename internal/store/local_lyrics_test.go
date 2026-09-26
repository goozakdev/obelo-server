package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Local lyrics at the layer they are stored on: the Scanner's row for a Track,
// written by the Track's upsert.

// scanWithLyrics upserts the one-track album with the Track's Local lyrics set
// to l (nil for a scan that found none), as a scan does.
func scanWithLyrics(t *testing.T, db *store.DB, l *lyrics.Lyrics) {
	t.Helper()
	tree := albumTree(standardRelease)
	tree.Albums[0].Tracks[0].Lyrics = l
	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatalf("UpsertArtistTree: %v", err)
	}
}

// TestLocalLyricsRoundTripSyncedAndPlain: what a scan stores reads back as it
// was, in either shape, and a Track never scanned with lyrics has none.
func TestLocalLyricsRoundTripSyncedAndPlain(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	scanWithLyrics(t, db, nil)
	if _, ok, err := db.LocalLyrics("tr1"); err != nil || ok {
		t.Fatalf("LocalLyrics with none scanned = (%v, %v), want none", ok, err)
	}
	for _, want := range []lyrics.Lyrics{
		{Kind: lyrics.Synced, Lines: []lyrics.Line{{StartMs: 0, Text: ""}, {StartMs: 86_460_000, Text: "Late, \"quoted\""}}},
		{Kind: lyrics.Plain, Text: "Verse\n\nChorus"},
	} {
		scanWithLyrics(t, db, &want)
		got, ok, err := db.LocalLyrics("tr1")
		if err != nil || !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("LocalLyrics = (%+v, %v, %v), want %+v", got, ok, err, want)
		}
	}
}

// TestARescanThatFindsNoLyricsClearsThemAndLeavesTheFetchedRow: the Scanner
// owns the Local row, so a rescan that finds no lyrics removes it — and never
// touches what a Lyric provider answered for the same Track.
func TestARescanThatFindsNoLyricsClearsThemAndLeavesTheFetchedRow(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	scanWithLyrics(t, db, &lyrics.Lyrics{Kind: lyrics.Plain, Text: "Local words"})
	fetched := store.FetchedLyrics{Lyrics: &lyrics.Lyrics{Kind: lyrics.Plain, Text: "Fetched words"}, Provider: "a", Question: "q"}
	if err := db.WriteFetchedLyrics("tr1", fetched); err != nil {
		t.Fatalf("WriteFetchedLyrics: %v", err)
	}

	scanWithLyrics(t, db, nil)
	if l, ok, err := db.LocalLyrics("tr1"); err != nil || ok {
		t.Fatalf("LocalLyrics after a rescan that found none = (%+v, %v, %v), want none", l, ok, err)
	}
	if got, ok, err := db.FetchedLyrics("tr1"); err != nil || !ok || !reflect.DeepEqual(got, fetched) {
		t.Fatalf("FetchedLyrics after the rescan = (%+v, %v, %v), want %+v kept", got, ok, err, fetched)
	}
}
