package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Regression tests for the internal/store code-review findings (R02-xx).

// R02-01: a mirrored File has path = ”, so it must not count as a "co-file".
func TestMirroredEpisodeWatchDoesNotPropagate(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "tv")
	ents := []store.MirrorEntity{
		{Type: store.ExportShow, RemoteID: "sh", Data: map[string]any{"title": "S", "identityKey": "s"}},
		{Type: store.ExportSeason, RemoteID: "se", ParentID: "sh", Data: map[string]any{"seasonNumber": 1, "identityKey": "s|1"}},
		{Type: store.ExportEpisode, RemoteID: "e1", ParentID: "se", Data: map[string]any{"title": "E1", "identityKey": "s|1|1", "seasonNumber": 1, "episodeNumber": 1}},
		{Type: store.ExportEpisode, RemoteID: "e2", ParentID: "se", Data: map[string]any{"title": "E2", "identityKey": "s|1|2", "seasonNumber": 1, "episodeNumber": 2}},
		{Type: store.ExportEdition, RemoteID: "ed1", ParentID: "e1", Data: map[string]any{}},
		{Type: store.ExportEdition, RemoteID: "ed2", ParentID: "e2", Data: map[string]any{}},
		{Type: store.ExportFile, RemoteID: "f1", ParentID: "ed1", Data: map[string]any{"durationMs": 1000}},
		{Type: store.ExportFile, RemoteID: "f2", ParentID: "ed2", Data: map[string]any{"durationMs": 1000}},
	}
	if err := db.ApplyMirror(lib.ID, ents, true); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1', 'b', 'admin', 'x')`)
	var e1 string
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id='e1'`).Scan(&e1); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWatchState("u1", e1, 0, true, true); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM watch_state WHERE user_id='u1' AND watched=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("watching ONE mirrored episode marked %d episodes watched, want 1", n)
	}
}

// R02-04: Continue Watching reports the summed duration of a multi-part Edition.
func TestContinueWatchingMultipartDuration(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('L', 'M', 'movie')`)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1', 'b', 'admin', 'x')`)
	if err := db.UpsertTitleTree(store.TitleTree{
		Title: store.Title{ID: "T", LibraryID: "L", Kind: "movie", Title: "Epic", IdentityKey: "epic", SortTitle: "epic"},
		Editions: []store.Edition{{ID: "E", Files: []store.File{
			{ID: "F1", Path: "/m/Epic - part1.mkv", DurationMs: 100000, PartOrdinal: 1},
			{ID: "F2", Path: "/m/Epic - part2.mkv", DurationMs: 100000, PartOrdinal: 2},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveWatchState("u1", "T", 150000, false, true); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ContinueWatching("u1", 10, store.AllAccess())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].DurationMs != 200000 {
		t.Errorf("DurationMs = %d, want the summed parts 200000", rows[0].DurationMs)
	}
}

// R02-12: a data-dir path containing URI metacharacters must open that path and
// keep the pragmas.
func TestOpenPathWithQueryChars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a?b#c")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "obelo.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not created at the requested path: %v", err)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys pragma = %d (err %v), want 1", fk, err)
	}
}

// R02-03 / R02-07: an Admin display-name edit Locks "title"; the next scan must
// not revert it, and the edited sort key strips a leading article like the
// scanner's does.
func TestShowRenameSurvivesRescan(t *testing.T) {
	db := openTemp(t)
	libID, showID, _ := seedShow(t, db)
	name := "The Chernobyl Disaster"
	if err := db.WriteEntityMetadata(store.EntityShow, showID, store.EntityMetadataEdit{Name: &name}); err != nil {
		t.Fatal(err)
	}
	var title, sortT string
	if err := db.QueryRow(`SELECT title, sort_title FROM shows WHERE id=?`, showID).Scan(&title, &sortT); err != nil {
		t.Fatal(err)
	}
	if sortT != "chernobyl disaster" {
		t.Errorf("edited sort_title = %q, want %q", sortT, "chernobyl disaster")
	}
	if err := db.UpsertShowTree(store.ShowTree{Show: store.Show{
		ID: showID, LibraryID: libID, Title: "Chernobyl", Year: 2019,
		IdentityKey: "chernobyl|2019", SortTitle: "chernobyl",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT title, sort_title FROM shows WHERE id=?`, showID).Scan(&title, &sortT); err != nil {
		t.Fatal(err)
	}
	if title != name || sortT != "chernobyl disaster" {
		t.Errorf("after rescan title=%q sort_title=%q, want the locked edit %q", title, sortT, name)
	}
}

func TestArtistAlbumRenameSurvivesRescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libm', 'Music', 'music')`)
	tree := store.ArtistTree{
		Artist: store.Artist{ID: "ar1", LibraryID: "libm", Name: "BoC", IdentityKey: "boc", SortName: "boc"},
		Albums: []store.AlbumTree{{Title: "MHTRTC", Year: 1998, IdentityKey: "boc|mhtrtc", SortTitle: "mhtrtc"}},
	}
	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatal(err)
	}
	var albumID string
	if err := db.QueryRow(`SELECT id FROM albums WHERE artist_id='ar1'`).Scan(&albumID); err != nil {
		t.Fatal(err)
	}
	artistName, albumName := "Boards of Canada", "Music Has the Right to Children"
	if err := db.WriteEntityMetadata(store.EntityArtist, "ar1", store.EntityMetadataEdit{Name: &artistName}); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteEntityMetadata(store.EntityAlbum, albumID, store.EntityMetadataEdit{Name: &albumName}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatal(err)
	}
	var gotArtist, gotAlbum string
	if err := db.QueryRow(`SELECT name FROM artists WHERE id='ar1'`).Scan(&gotArtist); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT title FROM albums WHERE id=?`, albumID).Scan(&gotAlbum); err != nil {
		t.Fatal(err)
	}
	if gotArtist != artistName {
		t.Errorf("artist name after rescan = %q, want locked %q", gotArtist, artistName)
	}
	if gotAlbum != albumName {
		t.Errorf("album title after rescan = %q, want locked %q", gotAlbum, albumName)
	}
}

// R02-02: a recompute that changes nothing must not re-stamp the catalog (the
// *_touch_au triggers would otherwise bump updated_at on every row each scan).
func TestNoopRecomputeHiddenDoesNotBump(t *testing.T) {
	db := openTemp(t)
	libID, showID, _ := seedShow(t, db)
	var ep string
	if err := db.QueryRow(`SELECT id FROM titles WHERE library_id=?`, libID).Scan(&ep); err != nil {
		t.Fatal(err)
	}
	settle()
	wasT := stamp(t, db, "titles", ep)
	wasS := stamp(t, db, "shows", showID)
	if err := db.RecomputeHiddenTitles(libID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecomputeHiddenShows(libID); err != nil {
		t.Fatal(err)
	}
	if now := stamp(t, db, "titles", ep); now != wasT {
		t.Errorf("no-op RecomputeHiddenTitles bumped title %q -> %q", wasT, now)
	}
	if now := stamp(t, db, "shows", showID); now != wasS {
		t.Errorf("no-op RecomputeHiddenShows bumped show %q -> %q", wasS, now)
	}
}
