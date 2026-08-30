package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The `ambiguous` flag on the Admin's identity attention list.
//
// docs/naming-convention.md promises a collision is "flagged ambiguous in the web
// app, never silently guessed". The flag was written by the scanner, stored, and
// read back on every Title — and then nothing selected it, so no queue, list or
// screen ever mentioned it and the Admin's only route to the conflict was to open
// the file matcher and notice two files reporting one Slot. A MOVIE collision had
// no such screen at all.
//
// These pin the read half of that promise: the flagged Title is on the list, it
// stays there through a "looks right" dismissal, and it names the files that
// actually collide.

// ambiguousTree is a Movie Title flagged ambiguous, holding one Edition with the
// given files — the shape groupEditions produces for two files that parse to one
// Edition identity and are not parts.
func ambiguousTree(id, key, title string, ambiguous bool, files []store.File) store.TitleTree {
	for i := range files {
		files[i].Container = "matroska"
		files[i].Mtime = "2024-01-01T00:00:00Z"
		files[i].SizeBytes = 1000
		files[i].Present = true
	}
	return store.TitleTree{
		Title: store.Title{
			ID: id, LibraryID: "libmov", Kind: "movie",
			Title: title, IdentityKey: key, SortTitle: title,
			Ambiguous: ambiguous,
		},
		Editions: []store.Edition{{ID: "ed-" + id, Files: files}},
	}
}

func ambiguousFixture(t *testing.T) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov', 'Movies', 'movie')`)
	return db
}

// TestAmbiguousMovieIsOnTheAttentionList: a Movie whose two files collide is
// flagged and NOT needs-review — its name parsed perfectly, there are simply two
// files claiming it. Selecting on needs_review alone (which is what the list did)
// leaves it invisible forever.
func TestAmbiguousMovieIsOnTheAttentionList(t *testing.T) {
	db := ambiguousFixture(t)
	tree := ambiguousTree("t1", "dune|2021", "Dune", true, []store.File{
		{ID: "f1", Path: "/media/Dune (2021)/Dune (2021).mkv", DurationMs: 60_000},
		{ID: "f2", Path: "/media/Dune (2021)/Dune (2021) (repack).mkv", DurationMs: 61_000},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	items, err := db.TitlesNeedingReview("libmov")
	if err != nil {
		t.Fatalf("TitlesNeedingReview: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want the one ambiguous Movie", items)
	}
	it := items[0]
	if !it.Ambiguous {
		t.Error("the item does not report itself ambiguous, so no row can say what is wrong")
	}
	if it.NeedsReview {
		t.Error("the item claims an uncertain parse it never had")
	}
}

// TestDismissingAParseDoesNotDismissACollision: "Looks right" answers a question
// about the PARSE. Two files still collide afterwards and one of them still is not
// played, so the item stays listed — otherwise the queue would offer a button that
// hides a real conflict.
func TestDismissingAParseDoesNotDismissACollision(t *testing.T) {
	db := ambiguousFixture(t)
	tree := ambiguousTree("t1", "dune", "Dune", true, []store.File{
		{ID: "f1", Path: "/media/Dune/Dune.mkv", DurationMs: 60_000},
		{ID: "f2", Path: "/media/Dune/Dune (repack).mkv", DurationMs: 61_000},
	})
	tree.Title.NeedsReview = true // yearless AND colliding
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	items, err := db.TitlesNeedingReview("libmov")
	if err != nil {
		t.Fatalf("TitlesNeedingReview: %v", err)
	}
	if len(items) != 1 || !items[0].NeedsReview || !items[0].Ambiguous {
		t.Fatalf("items = %+v, want one item carrying both flags", items)
	}

	if err := db.MarkTitleReviewed("t1"); err != nil {
		t.Fatalf("MarkTitleReviewed: %v", err)
	}
	items, err = db.TitlesNeedingReview("libmov")
	if err != nil {
		t.Fatalf("TitlesNeedingReview: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want the collision to survive the dismissal", items)
	}
	if items[0].NeedsReview {
		t.Error("the dismissed parse flag came back")
	}
	if !items[0].Ambiguous {
		t.Error("the collision was dismissed along with the parse")
	}
}

// TestCollidingFilePathsNamesTheConflict: the row has to say WHICH files collide.
// A genuine multi-part Edition beside it must name nothing — it is not a conflict,
// and listing its parts would tell the Admin to go and fix correct files.
func TestCollidingFilePathsNamesTheConflict(t *testing.T) {
	db := ambiguousFixture(t)
	colliding := ambiguousTree("t1", "dune|2021", "Dune", true, []store.File{
		{ID: "f1", Path: "/media/Dune (2021)/Dune (2021) (repack).mkv", DurationMs: 61_000},
		{ID: "f2", Path: "/media/Dune (2021)/Dune (2021).mkv", DurationMs: 60_000},
	})
	joined := ambiguousTree("t2", "long|2020", "Long", false, []store.File{
		{ID: "f3", Path: "/media/Long (2020)/Long (2020) - part1.mkv", PartOrdinal: 1},
		{ID: "f4", Path: "/media/Long (2020)/Long (2020) - part2.mkv", PartOrdinal: 2},
	})
	for _, tree := range []store.TitleTree{colliding, joined} {
		if err := db.UpsertTitleTree(tree); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	got, err := db.CollidingFilePaths([]string{"t1", "t2"})
	if err != nil {
		t.Fatalf("CollidingFilePaths: %v", err)
	}
	if _, ok := got["t2"]; ok {
		t.Errorf("a multi-part Edition was reported as a collision: %v", got["t2"])
	}
	want := []string{
		"/media/Dune (2021)/Dune (2021) (repack).mkv",
		"/media/Dune (2021)/Dune (2021).mkv",
	}
	if len(got["t1"]) != len(want) {
		t.Fatalf("colliding paths = %v, want both files %v", got["t1"], want)
	}
	for i := range want {
		if got["t1"][i] != want[i] {
			t.Fatalf("colliding paths = %v, want %v in play order", got["t1"], want)
		}
	}
}
