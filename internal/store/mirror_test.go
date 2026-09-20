package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Store-level tests for the mirror (.scratch/linked-servers issue 07,
// ADR-0056 §3). The end-to-end round trip lives in internal/api, where two real
// Servers can disagree; these are the three things that are easier to state here
// than there: a mirrored File names no disk, a re-apply is idempotent, and a full
// pull prunes what the sharer no longer has.

// mirrorLibrary makes a Link and a linked Library to hang mirrored rows under.
func mirrorLibrary(t *testing.T, db *store.DB, kind string) (linkID string, lib store.Library) {
	t.Helper()
	linkID = "link-1"
	if err := db.InsertLink(store.Link{
		ID: linkID, ServerID: "peer-1", ServerName: "Dave's server",
		Origins: []string{"http://dave.example"}, ActiveOrigin: "http://dave.example",
		Token: "t", State: store.LinkStateConnected, CreatedAt: "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("inserting link: %v", err)
	}
	lib, err := db.UpsertLinkedLibrary("lib-1", "Dave's films", kind, linkID, "their-lib")
	if err != nil {
		t.Fatalf("creating linked library: %v", err)
	}
	if !lib.Linked() {
		t.Fatalf("a linked Library reported source %q", lib.Source)
	}
	return linkID, lib
}

// movieFeed is one Movie with one Edition, TWO Files (a two-part film) and one
// Stream on each — the shape that proves the empty path is allowed.
func movieFeed() []store.MirrorEntity {
	return []store.MirrorEntity{
		{Type: store.ExportTitle, RemoteID: "t1", Data: map[string]any{
			"title": "Lawrence of Arabia", "year": 1962, "identityKey": "lawrence|1962",
			"sortTitle": "lawrence of arabia",
		}},
		{Type: store.ExportEdition, RemoteID: "e1", ParentID: "t1", Data: map[string]any{"name": "1080p"}},
		{Type: store.ExportFile, RemoteID: "f1", ParentID: "e1", Data: map[string]any{
			"container": "mkv", "videoCodec": "h264", "partOrdinal": 1, "durationMs": 100000,
		}},
		{Type: store.ExportFile, RemoteID: "f2", ParentID: "e1", Data: map[string]any{
			"container": "mkv", "videoCodec": "h264", "partOrdinal": 2, "durationMs": 100000,
		}},
		{Type: store.ExportStream, RemoteID: "s1", ParentID: "f1", Data: map[string]any{
			"index": 0, "kind": "video", "codec": "h264",
		}},
		{Type: store.ExportStream, RemoteID: "s2", ParentID: "f2", Data: map[string]any{
			"index": 0, "kind": "video", "codec": "h264",
		}},
	}
}

func count(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}

// TestAMirroredFileNamesNoDisk pins the schema's partial UNIQUE index on
// (edition_id, path), scoped to a non-empty path. Two Files under ONE Edition — a
// two-part film, which is exactly what part_ordinal is for — both carrying the
// empty path (a mirrored file has no local path to invent) must NOT collide on
// that constraint.
func TestAMirroredFileNamesNoDisk(t *testing.T) {
	db := openTemp(t)
	linkID, lib := mirrorLibrary(t, db, "movie")
	_ = linkID

	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("applying the feed: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM files WHERE path = ''`); n != 2 {
		t.Fatalf("%d mirrored Files carry an empty path, want 2", n)
	}
	if n := count(t, db, `SELECT COUNT(DISTINCT edition_id) FROM files`); n != 1 {
		t.Fatalf("the two parts landed under %d Editions, want 1", n)
	}
	// The constraint is relaxed, not removed: a LOCAL Library still cannot hold two
	// Files at the same real path under one Edition.
	if _, err := db.Exec(
		`INSERT INTO files (id, edition_id, path) SELECT 'x1', id, '/films/a.mkv' FROM editions LIMIT 1`,
	); err != nil {
		t.Fatalf("inserting a first real path: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO files (id, edition_id, path) SELECT 'x2', id, '/films/a.mkv' FROM editions LIMIT 1`,
	); err == nil {
		t.Error("two Files at the same path under one Edition were accepted; the partial unique index is not doing its job")
	}
}

// TestReapplyingAFeedIsIdempotent: the local ids are this household's forever —
// Watch state, Playlists and Collections point at them — so a second apply must
// find the same rows and not mint new ones.
func TestReapplyingAFeedIsIdempotent(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")

	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	var firstTitle string
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id = 't1'`).Scan(&firstTitle); err != nil {
		t.Fatalf("reading the mirrored Title: %v", err)
	}

	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, probe := range []struct {
		table string
		want  int
	}{{"titles", 1}, {"editions", 1}, {"files", 2}, {"streams", 2}} {
		if n := count(t, db, `SELECT COUNT(*) FROM `+probe.table); n != probe.want {
			t.Errorf("after a re-apply, %s holds %d rows, want %d", probe.table, n, probe.want)
		}
	}
	var secondTitle string
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id = 't1'`).Scan(&secondTitle); err != nil {
		t.Fatalf("reading the mirrored Title again: %v", err)
	}
	if secondTitle != firstTitle {
		t.Errorf("a re-apply re-keyed the Title: %q → %q", firstTitle, secondTitle)
	}
}

// TestAFullPullPrunesWhatTheSharerNoLongerHas closes issue 05's known gap:
// Editions and Streams have no soft-delete and the sharer's scanner rebuilds them
// with fresh ids, so a removed one can carry no tombstone. A full pull is the
// whole truth, so absence there means gone.
func TestAFullPullPrunesWhatTheSharerNoLongerHas(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")

	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// The sharer re-probed the file and the second Stream is gone — no tombstone,
	// just an absence.
	shorter := movieFeed()
	shorter = shorter[:len(shorter)-1]
	if err := db.ApplyMirror(lib.ID, shorter, true); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM streams`); n != 1 {
		t.Errorf("a full pull left %d Streams, want 1", n)
	}

	// An INCREMENTAL pull says nothing about what it did not carry, so it must
	// prune nothing — otherwise every change would empty the Library.
	if err := db.ApplyMirror(lib.ID, movieFeed()[:1], false); err != nil {
		t.Fatalf("incremental apply: %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM files`); n != 2 {
		t.Errorf("an incremental pull pruned Files down to %d; it must prune nothing", n)
	}
}

// TestATombstoneHidesRatherThanDeletes: the feed's Title tombstone IS `hidden`
// on the sharer's side, so it round-trips to `hidden` here. Deleting the row
// instead would cascade this household's Watch state away — the "dropping
// mirrored rows" ADR-0056 rejects by name.
func TestATombstoneHidesRatherThanDeletes(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")
	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	feed := movieFeed()
	feed[0].Deleted = true // the Title
	feed[2].Deleted = true // one File
	if err := db.ApplyMirror(lib.ID, feed, true); err != nil {
		t.Fatalf("tombstone apply: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM titles WHERE remote_id = 't1'`); n != 1 {
		t.Fatalf("a tombstoned Title was deleted; %d rows remain", n)
	}
	if n := count(t, db, `SELECT hidden FROM titles WHERE remote_id = 't1'`); n != 1 {
		t.Error("a tombstoned Title is not hidden")
	}
	if n := count(t, db, `SELECT present FROM files WHERE remote_id = 'f1'`); n != 0 {
		t.Error("a tombstoned File is still present")
	}
	if n := count(t, db, `SELECT present FROM files WHERE remote_id = 'f2'`); n != 1 {
		t.Error("a File with no tombstone was marked Missing anyway")
	}
}

// TestTombstoningAMirrorKeepsEverything is what happens when the sharer stops
// granting a Library this Server had mirrored (.scratch/linked-servers issue 08).
//
// It is the opposite of an unlink, and the difference is the whole point: the
// rows stay, this household's Watch state stays, and the shelf goes quiet by
// being HIDDEN. It also clears the checkpoint, which is what makes coming back
// work — the next pull is a full one, and a full pull rewrites `hidden` from the
// feed, so a re-granted Library returns with the same local ids and the same
// resume position.
func TestTombstoningAMirrorKeepsEverything(t *testing.T) {
	db := openTemp(t)
	_, lib := mirrorLibrary(t, db, "movie")
	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := db.SetLibraryCheckpoint(lib.ID, "cursor-42"); err != nil {
		t.Fatalf("recording the checkpoint: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES ('u1', 'brandon', 'admin', 'x')`); err != nil {
		t.Fatalf("seeding a user: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO watch_state (user_id, title_id, resume_position_ms)
		 SELECT 'u1', id, 42 FROM titles WHERE remote_id = 't1'`); err != nil {
		t.Fatalf("seeding watch state: %v", err)
	}
	var titleID string
	if err := db.QueryRow(`SELECT id FROM titles WHERE remote_id = 't1'`).Scan(&titleID); err != nil {
		t.Fatalf("reading the mirrored title: %v", err)
	}

	if err := db.TombstoneMirror(lib.ID); err != nil {
		t.Fatalf("tombstoning: %v", err)
	}

	if n := count(t, db, `SELECT hidden FROM titles WHERE remote_id = 't1'`); n != 1 {
		t.Error("the mirrored Title is not hidden after the grant went away")
	}
	for _, table := range []string{"titles", "editions", "files", "streams", "watch_state"} {
		if got := count(t, db, `SELECT COUNT(*) FROM `+table); got == 0 {
			t.Errorf("tombstoning emptied %s; nothing may be deleted", table)
		}
	}
	after, err := db.LibraryByID(lib.ID)
	if err != nil {
		t.Fatalf("reading the library back: %v", err)
	}
	if after.RemoteCheckpoint != "" {
		t.Errorf("checkpoint = %q after a tombstone, want it cleared so the next pull is full",
			after.RemoteCheckpoint)
	}

	// She shares it again: one full pull, and the shelf is itself again — same
	// local id, same resume position.
	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if n := count(t, db, `SELECT hidden FROM titles WHERE remote_id = 't1'`); n != 0 {
		t.Error("the re-granted Title is still hidden")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM titles WHERE id = ?`, titleID); n != 1 {
		t.Error("the re-granted Title came back with a different local id")
	}
	if n := count(t, db, `SELECT resume_position_ms FROM watch_state WHERE title_id = ?`, titleID); n != 42 {
		t.Errorf("resume position = %d after a tombstone and a re-grant, want 42", n)
	}
}

// TestUnlinkingDeletesTheWholeMirror is ADR-0056 §6 at the store grain: one
// DELETE and the schema's cascades take the catalog and the Watch state with it.
func TestUnlinkingDeletesTheWholeMirror(t *testing.T) {
	db := openTemp(t)
	linkID, lib := mirrorLibrary(t, db, "movie")
	if err := db.ApplyMirror(lib.ID, movieFeed(), true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (id, username, role, password_hash) VALUES ('u1', 'brandon', 'admin', 'x')`); err != nil {
		t.Fatalf("seeding a user: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO watch_state (user_id, title_id, resume_position_ms)
		 SELECT 'u1', id, 42 FROM titles WHERE remote_id = 't1'`); err != nil {
		t.Fatalf("seeding watch state: %v", err)
	}

	n, err := db.DeleteLibrariesForLink(linkID)
	if err != nil {
		t.Fatalf("deleting the mirror: %v", err)
	}
	if n != 1 {
		t.Fatalf("unlink removed %d libraries, want 1", n)
	}
	for _, table := range []string{"titles", "editions", "files", "streams", "watch_state"} {
		if got := count(t, db, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("unlinking left %d rows in %s", got, table)
		}
	}
}

// TestLinkedLibraryStatesFollowTheLink: `available` is "the Link is connected"
// today, `serverName` is the name the Link was recorded under, and a local
// Library is not in the map at all — which is what keeps the wire fields off
// every Server that has never linked.
func TestLinkedLibraryStatesFollowTheLink(t *testing.T) {
	db := openTemp(t)
	linkID, lib := mirrorLibrary(t, db, "movie")
	local, err := db.CreateLibrary("local-1", "Ours", "movie", []store.LibraryRootInput{{ID: "r1", Path: "/films"}})
	if err != nil {
		t.Fatalf("creating a local library: %v", err)
	}

	states, err := db.LinkedLibraryStates()
	if err != nil {
		t.Fatalf("reading states: %v", err)
	}
	st, ok := states[lib.ID]
	if !ok || !st.Available {
		t.Errorf("a mirror on a connected Link reported %+v/%v", st, ok)
	}
	if st.ServerName != "Dave's server" {
		t.Errorf("a mirror carried server name %q, want %q", st.ServerName, "Dave's server")
	}
	if _, ok := states[local.ID]; ok {
		t.Error("a local Library appeared in the linked-library states")
	}

	if err := db.SetLinkState(linkID, store.LinkStateUnreachable, ""); err != nil {
		t.Fatalf("moving the link state: %v", err)
	}
	states, err = db.LinkedLibraryStates()
	if err != nil {
		t.Fatalf("re-reading states: %v", err)
	}
	if states[lib.ID].Available {
		t.Error("a mirror on an unreachable Link still reported available")
	}
	if states[lib.ID].ServerName != "Dave's server" {
		t.Errorf("an unreachable mirror lost its server name: %q", states[lib.ID].ServerName)
	}
}

// TestReapingRemovesALinkedLibraryWhoseLinkIsGone is the cleanup half of
// .scratch/linked-servers issue 17: a partial unlink can delete a Link and leave
// its Libraries behind — a row whose link_id names no Link, which shows twice in
// the browse menus (the second copy empty) and cannot be reached to remove. The
// boot-time reaper deletes exactly those, cascading their mirrored rows and Watch
// state, and leaves a Library whose Link still lives untouched.
func TestReapingRemovesALinkedLibraryWhoseLinkIsGone(t *testing.T) {
	db := openTemp(t)
	// A healthy Link with a mirror.
	linkID, live := mirrorLibrary(t, db, "movie")
	if err := db.ApplyMirror(live.ID, movieFeed(), true); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// An orphan: a linked Library pointing at a Link that no longer exists, with
	// its own mirrored content so the cascade has something to remove.
	if _, err := db.Exec(
		`INSERT INTO libraries (id, name, kind, source, link_id, remote_library_id)
		 VALUES ('orphan-lib', 'Ghost films', 'movie', ?, 'dead-link', 'their-lib')`,
		store.LibrarySourceLinked); err != nil {
		t.Fatalf("seeding the orphan: %v", err)
	}
	orphanFeed := []store.MirrorEntity{
		{Type: store.ExportTitle, RemoteID: "og1", Data: map[string]any{
			"title": "Ghost", "year": 1990, "identityKey": "ghost|1990", "sortTitle": "ghost",
		}},
		{Type: store.ExportEdition, RemoteID: "oe1", ParentID: "og1", Data: map[string]any{"name": "1080p"}},
		{Type: store.ExportFile, RemoteID: "of1", ParentID: "oe1", Data: map[string]any{
			"container": "mkv", "videoCodec": "h264", "partOrdinal": 1, "durationMs": 100000,
		}},
	}
	if err := db.ApplyMirror("orphan-lib", orphanFeed, true); err != nil {
		t.Fatalf("seeding the orphan's content: %v", err)
	}

	n, err := db.DeleteOrphanLinkedLibraries()
	if err != nil {
		t.Fatalf("reaping: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaper removed %d libraries, want 1 (the orphan only)", n)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM libraries WHERE id = 'orphan-lib'`); got != 0 {
		t.Errorf("the orphan library survived the reaper")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM titles WHERE library_id = 'orphan-lib'`); got != 0 {
		t.Errorf("the orphan's %d mirrored titles were not cascaded away", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM libraries WHERE id = ?`, live.ID); got != 1 {
		t.Errorf("the reaper took a Library whose Link is still alive")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM titles WHERE library_id = ?`, live.ID); got == 0 {
		t.Errorf("the live Library lost its mirrored titles")
	}
	_ = linkID
}

// TestReapingSparesLocalLibraries: a reaper keyed on source must never touch a
// local Library, however the links table looks.
func TestReapingSparesLocalLibraries(t *testing.T) {
	db := openTemp(t)
	if _, err := db.CreateLibrary("local-1", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "r1", Path: t.TempDir()}}); err != nil {
		t.Fatalf("creating a local library: %v", err)
	}
	n, err := db.DeleteOrphanLinkedLibraries()
	if err != nil {
		t.Fatalf("reaping: %v", err)
	}
	if n != 0 {
		t.Fatalf("reaper removed %d rows against a local-only server, want 0", n)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM libraries WHERE source = 'local'`); got != 1 {
		t.Errorf("the local library count is %d, want 1", got)
	}
}
