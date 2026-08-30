package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Edition.Files is a PLAY order, not a listing order (Edition.PartAt /
// TotalDurationMs / PartStartMs all walk it), and Placement lets an Admin build a
// multi-part Edition out of two files whose names say nothing about part order
// (ADR-0044). These pin migration 0049: the order is a stored column, and the
// default of 0 leaves every pre-existing Edition sorting by path exactly as
// before.

// partEdition builds a one-Title tree whose single Edition holds the given files,
// in the given (id, path, part ordinal) order.
func partEdition(titleID, identityKey string, files []store.File) store.TitleTree {
	for i := range files {
		files[i].Container = "matroska"
		files[i].Mtime = "2024-01-01T00:00:00Z"
		files[i].SizeBytes = 1000
	}
	return store.TitleTree{
		Title: store.Title{
			ID: titleID, LibraryID: "libtv", Kind: "movie",
			Title: "Long Movie", IdentityKey: identityKey, SortTitle: "long movie",
		},
		Editions: []store.Edition{{ID: "ed-" + titleID, Files: files}},
	}
}

// TestPartOrdinalOverridesFilenameOrder: two Files whose part order is the
// REVERSE of their path order come back in part order. Nothing in the filenames
// could have recovered this — which is the whole reason the column exists.
func TestPartOrdinalOverridesFilenameOrder(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'movie')`)

	first := "/media/Long Movie (2020)/zeta.mkv"
	second := "/media/Long Movie (2020)/alpha.mkv"
	tree := partEdition("t1", "long movie|2020", []store.File{
		{ID: "f1", Path: first, PartOrdinal: 1, DurationMs: 60_000},
		{ID: "f2", Path: second, PartOrdinal: 2, DurationMs: 30_000},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	detail, err := db.TitleByID("t1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(detail.Editions) != 1 || len(detail.Editions[0].Files) != 2 {
		t.Fatalf("editions/files = %+v, want one Edition of two Files", detail.Editions)
	}
	ed := detail.Editions[0]
	if ed.Files[0].Path != first || ed.Files[1].Path != second {
		t.Fatalf("stored order = %q, %q; want part order (%q first), not path order",
			ed.Files[0].Path, ed.Files[1].Path, first)
	}
	if ed.Files[0].PartOrdinal != 1 || ed.Files[1].PartOrdinal != 2 {
		t.Errorf("part ordinals = %d, %d; want 1, 2", ed.Files[0].PartOrdinal, ed.Files[1].PartOrdinal)
	}
	// The joint timeline follows the slice order, so getting it wrong resumes in
	// the wrong half.
	if part, offset, ok := ed.PartAt(70_000); !ok || part.Path != second || offset != 10_000 {
		t.Errorf("PartAt(70s) = %q @ %dms (ok=%v), want the second part at 10s", part.Path, offset, ok)
	}
}

// TestUnnumberedPartsStillSortByPath: an Edition nothing numbered — every
// part_ordinal at the migration default of 0 — keeps sorting by path, exactly as
// it did before the column existed. This is what makes 0049 a no-op for every
// multi-part Edition already in a database.
func TestUnnumberedPartsStillSortByPath(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'movie')`)

	tree := partEdition("t1", "long movie|2020", []store.File{
		{ID: "f1", Path: "/media/Long Movie (2020)/zeta - part2.mkv"},
		{ID: "f2", Path: "/media/Long Movie (2020)/alpha - part1.mkv"},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	detail, err := db.TitleByID("t1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	files := detail.Editions[0].Files
	if files[0].Path != "/media/Long Movie (2020)/alpha - part1.mkv" {
		t.Errorf("unnumbered parts came back as %q first, want path order", files[0].Path)
	}
}

// TestSplitPlacementKeepsBothFileRowsWithOrdinals is the Placement split shape
// run through the real write path: one file on two Slots, so two Episode Titles
// share one path, on an incremental rescan where the scanner reuses ONE stored
// File row for both (so both arrive with the same File.ID). That is exactly the
// crash multiepisode_test.go pins — `UNIQUE constraint failed: files.id` — and
// adding part_ordinal to the insert must not reintroduce it.
func TestSplitPlacementKeepsBothFileRowsWithOrdinals(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)

	const path = "/media/The Bear (2022)/Season 01/double length finale.mkv"
	show := store.Show{
		ID: "show1", LibraryID: "libtv", Title: "The Bear", Year: 2022,
		IdentityKey: "the bear|2022", SortTitle: "the bear",
	}
	// Both Slots the Admin placed the one file on, both carrying ordinal 1 (each
	// is the only File of its Slot) and the same reused File id.
	split := store.ShowTree{
		Show: show,
		Seasons: []store.SeasonTree{{
			SeasonNumber: 1, IdentityKey: "the bear|2022|s01",
			Episodes: []store.EpisodeTree{
				partEpisode("t1", "the bear|2022|s01e01", 1, 1, "fa", path),
				partEpisode("t2", "the bear|2022|s01e02", 1, 2, "fa", path),
			},
		}},
	}
	if err := db.UpsertShowTree(split); err != nil {
		t.Fatalf("first apply of the split: %v", err)
	}
	assertDistinctPresentFiles(t, db, path, 2)

	// The scheduled rescan replays the same Placement and must be a no-op.
	if err := db.UpsertShowTree(split); err != nil {
		t.Fatalf("rescan of the split: %v", err)
	}
	assertDistinctPresentFiles(t, db, path, 2)

	for _, id := range []string{"t1", "t2"} {
		detail, err := db.TitleByID(id)
		if err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if len(detail.Editions) != 1 || len(detail.Editions[0].Files) != 1 {
			t.Fatalf("%s = %+v, want one Edition of one File", id, detail.Editions)
		}
		if got := detail.Editions[0].Files[0].Path; got != path {
			t.Errorf("%s file path = %q, want the shared path", id, got)
		}
		if got := detail.Editions[0].Files[0].PartOrdinal; got != 1 {
			t.Errorf("%s part ordinal = %d, want 1", id, got)
		}
	}
}

// partEpisode is episodeTree with the part ordinal set, for the Placement shapes.
func partEpisode(titleID, identityKey string, season, episode int, fileID, path string) store.EpisodeTree {
	et := episodeTree(titleID, identityKey, season, episode, fileID, path)
	et.Editions[0].Files[0].PartOrdinal = 1
	return et
}

// TestPreBackfillMultiPartStillPlaysTheWholeWork is the un-backfilled install, run
// through the real write/read path. Migration 0049 added part_ordinal with
// `DEFAULT 0` and no backfill, so until a scan rewrites them BOTH halves of a
// legitimate two-part movie sit at 0 — indistinguishable, by the column alone,
// from two files colliding on one Edition.
//
// Telling them apart by the column alone would truncate every such Edition to its
// first half and move the ~90% Watched threshold onto that half, which is the exact
// defect internal/playback/multipart_test.go exists to prevent. The filenames are
// what still separate the two cases here: before 0049, naming the files was the
// ONLY way to make a multi-part Edition at all.
func TestPreBackfillMultiPartStillPlaysTheWholeWork(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'movie')`)

	tree := partEdition("t1", "long movie|2020", []store.File{
		{ID: "f1", Path: "/media/Long Movie (2020)/Long Movie - part1.mkv", DurationMs: 60_000},
		{ID: "f2", Path: "/media/Long Movie (2020)/Long Movie - part2.mkv", DurationMs: 30_000},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	detail, err := db.TitleByID("t1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	ed := detail.Editions[0]
	for _, f := range ed.Files {
		if f.PartOrdinal != 0 {
			t.Fatalf("%s came back with ordinal %d; this test only means something at the migration default", f.Path, f.PartOrdinal)
		}
	}
	if !ed.IsMultiPart() {
		t.Fatal("a pre-backfill two-part Edition is no longer multi-part — every un-rescanned " +
			"multi-part title would play only its first half")
	}
	if got := ed.TotalDurationMs(); got != 90_000 {
		t.Errorf("total = %d, want the summed 90000", got)
	}
}

// TestCollidingFilesAreNotAPartSet is the other half: two files the scanner flagged
// ambiguous (docs/naming-convention.md's collision rule) share one Edition and are
// numbered by nothing — not the column, not their names. They are two claims on one
// Edition, not two halves of one work, so only the first plays.
func TestCollidingFilesAreNotAPartSet(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'movie')`)

	tree := partEdition("t1", "long movie|2020", []store.File{
		{ID: "f1", Path: "/media/Long Movie (2020)/Long Movie.mkv", DurationMs: 60_000},
		{ID: "f2", Path: "/media/Long Movie (2020)/Long Movie (repack).mkv", DurationMs: 61_000},
	})
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	detail, err := db.TitleByID("t1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	ed := detail.Editions[0]
	if ed.IsMultiPart() {
		t.Error("two colliding files reported as a multi-part Edition — they would be concatenated")
	}
	if got := len(ed.Parts()); got != 1 {
		t.Errorf("Parts() = %d files, want only the first in stored order", got)
	}
	if got, want := ed.TotalDurationMs(), ed.Files[0].DurationMs; got != want {
		t.Errorf("total = %d, want only the first file's %d", got, want)
	}
}
