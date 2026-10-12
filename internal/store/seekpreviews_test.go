package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// seekPreviewFile puts one video File of the given Title in a Library; year 0 is none.
func seekPreviewFile(t *testing.T, db *store.DB, lib, id, kind, title string, year int, seasonID string, season, episode int) {
	t.Helper()
	tr := store.TitleTree{
		Title: store.Title{ID: "t-" + id, LibraryID: lib, Kind: kind, Title: title, IdentityKey: id, SortTitle: title,
			SeasonNumber: season, EpisodeNumber: episode},
		Editions: []store.Edition{{ID: "ed-" + id, Files: []store.File{{
			ID: id, Path: "/m/" + id + ".mkv", Container: "matroska", DurationMs: 60_000,
			Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000, Present: true,
			Streams: []store.Stream{{ID: "s-" + id, Index: 0, Kind: "video", Codec: "h264"}},
		}}}},
	}
	if year > 0 {
		tr.Title.Year = year
	}
	if err := db.UpsertTitleTree(tr); err != nil {
		t.Fatal(err)
	}
	if seasonID != "" {
		mustExec(t, db, `UPDATE titles SET season_id = ?, season_number = ?, episode_number = ? WHERE id = ?`,
			seasonID, season, episode, "t-"+id)
	}
}

func TestSeekPreviewCoverageCountsFreshReadyAndListsFailuresByLabel(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov', 'Movies', 'movie')`)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('sh', 'libtv', 'Andor', 'andor', 'andor')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se', 'sh', 1, 'andor|s01')`)
	seekPreviewFile(t, db, "libmov", "fresh", "movie", "Fresh", 2001, "", 0, 0)
	seekPreviewFile(t, db, "libmov", "stale", "movie", "Stale", 2002, "", 0, 0)
	seekPreviewFile(t, db, "libmov", "none", "movie", "None", 2003, "", 0, 0)
	seekPreviewFile(t, db, "libmov", "zed", "movie", "Zed", 2004, "", 0, 0)
	seekPreviewFile(t, db, "libmov", "alpha", "movie", "Alpha", 0, "", 0, 0)
	seekPreviewFile(t, db, "libtv", "ep", "episode", "Rix Road", 0, "se", 1, 12)
	seekPreviewFile(t, db, "libtv", "other", "movie", "Elsewhere", 2005, "", 0, 0)
	mustExec(t, db, `UPDATE files SET present = 0 WHERE id = 'none'`) // Missing: not eligible
	ok := func(id, mtime string, size int64) store.SeekPreviewSet {
		return store.SeekPreviewSet{FileID: id, Mtime: mtime, SizeBytes: size, Status: store.SeekPreviewReady, SheetCount: 1}
	}
	fail := func(id, reason string) store.SeekPreviewSet {
		return store.SeekPreviewSet{FileID: id, Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000, Status: store.SeekPreviewFailed, Reason: reason}
	}
	for _, s := range []store.SeekPreviewSet{
		ok("fresh", "2024-01-01T00:00:00Z", 1000),
		ok("stale", "2023-12-31T00:00:00Z", 1000), // File changed since: not counted
		fail("zed", "boom"), fail("alpha", "no frame"), fail("ep", "ffmpeg lacks zscale; cannot tone-map HDR"),
		fail("other", "elsewhere"),
	} {
		if err := db.SaveSeekPreview(s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.SeekPreviewCoverage("libmov")
	if err != nil {
		t.Fatal(err)
	}
	want := store.SeekPreviewCoverage{Eligible: 4, Ready: 1, Failed: []store.SeekPreviewFailure{
		{FileID: "alpha", TitleID: "t-alpha", Label: "Alpha", Path: "/m/alpha.mkv", Reason: "no frame"},
		{FileID: "zed", TitleID: "t-zed", Label: "Zed (2004)", Path: "/m/zed.mkv", Reason: "boom"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("movie coverage = %+v, want %+v", got, want)
	}
	tv, err := db.SeekPreviewCoverage("libtv")
	if err != nil {
		t.Fatal(err)
	}
	wantTV := store.SeekPreviewCoverage{Eligible: 2, Failed: []store.SeekPreviewFailure{
		{FileID: "ep", TitleID: "t-ep", Label: "Andor — S01E12 · Rix Road", Path: "/m/ep.mkv", Reason: "ffmpeg lacks zscale; cannot tone-map HDR"},
		{FileID: "other", TitleID: "t-other", Label: "Elsewhere (2005)", Path: "/m/other.mkv", Reason: "elsewhere"},
	}}
	if !reflect.DeepEqual(tv, wantTV) {
		t.Fatalf("tv coverage = %+v, want %+v", tv, wantTV)
	}
}
