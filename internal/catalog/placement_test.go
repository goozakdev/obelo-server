package catalog_test

import (
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Placement Apply (file-matcher/03, ADR-0044): the Admin rearranged which File
// fills which Slot inside an already-identified Show, and Apply makes it true
// immediately — the same structure the Scanner would build from the same stored
// decisions, plus the watch-state folding no scan could do.
//
// The rule these tests exist to pin, and the one a future reader is most likely to
// get backwards: watch state is KEPT here, never reset. The Admin said the file
// was filed in the wrong place, not that it is a different work — the exact
// opposite of CorrectTitleIdentity (ADR-0019).

const (
	showKey  = "the bear|2022"
	showRoot = "/media/TV/The Bear (2022)"
)

func openTemp(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func mustExec(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// episodePath is the canonical on-disk path of one Episode, so SeasonHintForPath
// reads the season off the folder exactly as the Scanner's walk would.
func episodePath(season int, name string) string {
	dir := "Season 01"
	if season != 1 {
		dir = "Season 0" + itoa(season)
	}
	return filepath.Join(showRoot, dir, name)
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func episodeKey(season, episode int) string {
	return showKey + "|s0" + itoa(season) + "e0" + itoa(episode)
}

// fixture is one seeded TV Library holding one Show, plus the catalog service
// under test.
type fixture struct {
	t     *testing.T
	db    *store.DB
	svc   *catalog.Service
	show  string
	lock  *fakeLock
	enrch *fakeReenricher
}

type fakeLock struct {
	held    map[string]bool
	locked  int
	unlockd int
}

func (f *fakeLock) LockLibrary(libraryID string) bool {
	if f.held[libraryID] {
		return false
	}
	f.held[libraryID] = true
	f.locked++
	return true
}

func (f *fakeLock) UnlockLibrary(libraryID string) {
	delete(f.held, libraryID)
	f.unlockd++
}

type fakeReenricher struct{ calls []string }

func (f *fakeReenricher) ReenrichLibrary(libraryID string) { f.calls = append(f.calls, libraryID) }

// seedEpisode builds one Episode the way a scan would leave it: a single unnamed
// Edition holding one File with a known duration (the fold arithmetic is measured
// against it).
func seedEpisode(season, episode int, name string, durationMs int64) store.EpisodeTree {
	path := episodePath(season, name)
	editionID := uuid.NewString()
	return store.EpisodeTree{
		TitleTree: store.TitleTree{
			Title: store.Title{
				ID: uuid.NewString(), LibraryID: "libtv", Kind: "episode",
				Title: name, IdentityKey: episodeKey(season, episode), SortTitle: name,
			},
			Editions: []store.Edition{{
				ID: editionID,
				Files: []store.File{{
					ID: uuid.NewString(), EditionID: editionID, Path: path,
					DurationMs: durationMs, Present: true,
				}},
			}},
		},
		SeasonNumber: season, EpisodeNumber: episode,
	}
}

func newFixture(t *testing.T, episodes ...store.EpisodeTree) *fixture {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u1','u1','member')`)

	bySeason := map[int][]store.EpisodeTree{}
	var order []int
	for _, ep := range episodes {
		if _, ok := bySeason[ep.SeasonNumber]; !ok {
			order = append(order, ep.SeasonNumber)
		}
		bySeason[ep.SeasonNumber] = append(bySeason[ep.SeasonNumber], ep)
	}
	sort.Ints(order)
	tree := store.ShowTree{Show: store.Show{
		ID: uuid.NewString(), LibraryID: "libtv", Title: "The Bear", Year: 2022,
		IdentityKey: showKey, SortTitle: "bear",
	}}
	for _, n := range order {
		tree.Seasons = append(tree.Seasons, store.SeasonTree{
			SeasonNumber: n, IdentityKey: showKey + "|s0" + itoa(n), Episodes: bySeason[n],
		})
	}
	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("seed show: %v", err)
	}

	f := &fixture{t: t, db: db, show: tree.Show.ID,
		lock: &fakeLock{held: map[string]bool{}}, enrch: &fakeReenricher{}}
	f.svc = catalog.NewService(db, t.TempDir())
	f.svc.SetLibraryLock(f.lock)
	f.svc.SetReenricher(f.enrch)
	return f
}

func (f *fixture) apply(decisions ...store.FileDecision) (catalog.PlacementResult, error) {
	f.t.Helper()
	return f.svc.ApplyPlacement(catalog.PlacementInput{ShowID: f.show, Decisions: decisions})
}

func (f *fixture) mustApply(decisions ...store.FileDecision) catalog.PlacementResult {
	f.t.Helper()
	res, err := f.apply(decisions...)
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return res
}

// titleAt reads the Episode Title holding a Slot's identity key.
func (f *fixture) titleAt(season, episode int) (id string, seasonNum, epNum int, hidden bool, ok bool) {
	f.t.Helper()
	var h int
	err := f.db.QueryRow(
		`SELECT id, season_number, episode_number, hidden FROM titles
		   WHERE library_id = 'libtv' AND identity_key = ?`, episodeKey(season, episode)).
		Scan(&id, &seasonNum, &epNum, &h)
	if err != nil {
		return "", 0, 0, false, false
	}
	return id, seasonNum, epNum, h == 1, true
}

func (f *fixture) titleIDForPath(path string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(
		`SELECT t.id FROM titles t JOIN editions e ON e.title_id = t.id
		   JOIN files f ON f.edition_id = e.id WHERE f.path = ?`, path).Scan(&id); err != nil {
		f.t.Fatalf("title for %q: %v", path, err)
	}
	return id
}

func (f *fixture) setWatch(titleID string, resumeMs int64, watched bool) {
	f.t.Helper()
	if err := f.db.SaveWatchState("u1", titleID, resumeMs, watched, true); err != nil {
		f.t.Fatalf("save watch state: %v", err)
	}
}

func (f *fixture) watch(titleID string) store.WatchState {
	f.t.Helper()
	ws, err := f.db.WatchStateFor("u1", titleID)
	if err != nil {
		f.t.Fatalf("read watch state: %v", err)
	}
	return ws
}

func (f *fixture) editionsOf(titleID string) []store.Edition {
	f.t.Helper()
	d, err := f.db.TitleByID(titleID)
	if err != nil {
		f.t.Fatalf("title detail: %v", err)
	}
	return d.Editions
}

func (f *fixture) decisions() map[string]store.FileDecisions {
	f.t.Helper()
	got, err := f.db.FileDecisionsByLibrary("libtv")
	if err != nil {
		f.t.Fatalf("read decisions: %v", err)
	}
	return got
}

func placedOn(path string, season, episode, ordinal int) store.FileDecision {
	return store.FileDecision{
		Path: path, State: store.DecisionPlaced,
		GroupNumber: season, SlotNumber: episode, Ordinal: ordinal,
	}
}

// TestApplyMoveKeepsWatchState is the headline rule: a watched Episode moved to
// another Slot is still watched, by the same User. The row is re-keyed IN PLACE,
// so title_id — and therefore every watch_state row hanging off it — survives.
// This is the deliberate opposite of a Wrong-item correction, which resets.
func TestApplyMoveKeepsWatchState(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	path := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	before := f.titleIDForPath(path)
	f.setWatch(before, 0, true)

	// The Admin says this file is really season 2, episode 5 — a Season with no
	// folder on disk at all.
	res := f.mustApply(placedOn(path, 2, 5, 1))
	if res.Rearranged != 1 {
		t.Fatalf("rearranged = %d, want 1", res.Rearranged)
	}

	after, season, episode, hidden, ok := f.titleAt(2, 5)
	if !ok {
		t.Fatalf("no Title at the assigned Slot S02E05")
	}
	if after != before {
		t.Fatalf("title id moved: %q → %q; the re-key must be an UPDATE", before, after)
	}
	if season != 2 || episode != 5 {
		t.Fatalf("slot = S%02dE%02d, want S02E05", season, episode)
	}
	if hidden {
		t.Fatalf("the moved Episode is hidden")
	}
	if ws := f.watch(after); !ws.Watched {
		t.Fatalf("watch state lost by the move: %+v", ws)
	}
	// The Season was conjured from the assignment, with no folder behind it.
	var seasonRows int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM seasons WHERE show_id = ? AND season_number = 2`, f.show).
		Scan(&seasonRows); err != nil || seasonRows != 1 {
		t.Fatalf("season 2 rows = %d (err %v), want 1", seasonRows, err)
	}
	// Its Enrichment was reset so the re-enrich resolves the new position's record.
	var status string
	if err := f.db.QueryRow(`SELECT enrichment_status FROM titles WHERE id = ?`, after).
		Scan(&status); err != nil || status != "pending" {
		t.Fatalf("enrichment_status = %q (err %v), want pending", status, err)
	}
	if len(f.enrch.calls) != 1 {
		t.Fatalf("re-enrich calls = %v, want exactly one", f.enrch.calls)
	}
	// The decision is stored sparsely: one row, for the one File that moved.
	got := f.decisions()
	if len(got) != 1 || len(got[path]) != 1 || got[path][0].GroupNumber != 2 {
		t.Fatalf("stored decisions = %#v, want one placed row on season 2", got)
	}
}

// TestApplyMergeFoldsOntoJointTimeline: two files on one Slot become one Episode
// with a two-part Edition, and their watch state folds onto the joint timeline.
// One part watched and the other 32% in is NOT a watched Episode — it resumes at
// the earliest unfinished part, mapped onto the combined timeline. Watched-if-any
// is precisely the bug the multi-part duration work exists to prevent.
func TestApplyMergeFoldsOntoJointTimeline(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	first := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	second := episodePath(1, "The Bear (2022) - S01E02 - Hands.mkv")
	firstID, secondID := f.titleIDForPath(first), f.titleIDForPath(second)
	f.setWatch(firstID, 0, true) // part one finished
	f.setWatch(secondID, 320, false)

	f.mustApply(placedOn(first, 1, 1, 1), placedOn(second, 1, 1, 2))

	merged, _, _, _, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("no merged Title at S01E01")
	}
	if merged != firstID {
		t.Fatalf("merged onto %q, want the first part's row %q", merged, firstID)
	}
	eds := f.editionsOf(merged)
	if len(eds) != 1 || !eds[0].IsMultiPart() {
		t.Fatalf("editions = %#v, want one multi-part Edition", eds)
	}
	if eds[0].Files[0].Path != first || eds[0].Files[1].Path != second {
		t.Fatalf("parts out of ordinal order: %q then %q", eds[0].Files[0].Path, eds[0].Files[1].Path)
	}
	ws := f.watch(merged)
	if ws.Watched {
		t.Fatalf("merged Episode marked watched though only one part was")
	}
	// 1000ms of finished part one, then 320ms into part two.
	if ws.ResumePositionMs != 1320 {
		t.Fatalf("resume = %d, want 1320 on the combined timeline", ws.ResumePositionMs)
	}
	// The Episode the second file used to be is emptied and drops out of browse.
	if _, _, _, hidden, ok := f.titleAt(1, 2); ok && !hidden {
		t.Fatalf("the absorbed Episode S01E02 is still visible")
	}
}

// TestApplyMergeOfTwoWatchedIsWatched: watched only if EVERY part was, which the
// all-watched case must actually reach.
func TestApplyMergeOfTwoWatchedIsWatched(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	first := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	second := episodePath(1, "The Bear (2022) - S01E02 - Hands.mkv")
	f.setWatch(f.titleIDForPath(first), 0, true)
	f.setWatch(f.titleIDForPath(second), 0, true)

	f.mustApply(placedOn(first, 1, 1, 1), placedOn(second, 1, 1, 2))

	merged, _, _, _, _ := f.titleAt(1, 1)
	ws := f.watch(merged)
	if !ws.Watched {
		t.Fatalf("two watched parts merged to an unwatched Episode: %+v", ws)
	}
	if ws.ResumePositionMs != 0 {
		t.Fatalf("resume = %d, want 0 on a watched Episode", ws.ResumePositionMs)
	}
}

// TestApplySplitCopiesWatchState: one file across two Slots becomes co-File
// sibling Titles, each inheriting the original's state — which is what
// watch_state.go already maintains between siblings.
func TestApplySplitCopiesWatchState(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
	)
	path := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	original := f.titleIDForPath(path)
	f.setWatch(original, 0, true)

	f.mustApply(placedOn(path, 1, 1, 1), placedOn(path, 1, 2, 1))

	firstID, _, _, _, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("no Title at S01E01 after the split")
	}
	secondID, _, _, _, ok := f.titleAt(1, 2)
	if !ok {
		t.Fatalf("no Title at S01E02 after the split")
	}
	if firstID != original {
		t.Fatalf("the original Slot changed rows: %q → %q", original, firstID)
	}
	if secondID == firstID {
		t.Fatalf("the split produced one Title, not two")
	}
	for _, id := range []string{firstID, secondID} {
		if ws := f.watch(id); !ws.Watched {
			t.Fatalf("sibling %q did not inherit the watched state: %+v", id, ws)
		}
		eds := f.editionsOf(id)
		if len(eds) != 1 || len(eds[0].Files) != 1 || eds[0].Files[0].Path != path {
			t.Fatalf("sibling %q does not share the original File: %#v", id, eds)
		}
	}
}

// TestApplySwapCommits: swapping two files' Slots asks for A→B's key and B→A's
// key at once, which trips UNIQUE (library_id, identity_key) unless the movers
// pass through a temporary key inside the transaction.
func TestApplySwapCommits(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	first := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	second := episodePath(1, "The Bear (2022) - S01E02 - Hands.mkv")
	firstID, secondID := f.titleIDForPath(first), f.titleIDForPath(second)
	f.setWatch(firstID, 0, true)

	f.mustApply(placedOn(first, 1, 2, 1), placedOn(second, 1, 1, 1))

	atOne, _, _, _, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("nothing at S01E01 after the swap")
	}
	atTwo, _, _, _, ok := f.titleAt(1, 2)
	if !ok {
		t.Fatalf("nothing at S01E02 after the swap")
	}
	if atOne != secondID || atTwo != firstID {
		t.Fatalf("swap put %q at E01 and %q at E02; want %q and %q", atOne, atTwo, secondID, firstID)
	}
	// Watch state rode along with the file, not with the Slot.
	if ws := f.watch(firstID); !ws.Watched {
		t.Fatalf("the moved file's watch state was lost: %+v", ws)
	}
	// And no parking key survived the commit.
	rows, err := f.db.Query(`SELECT identity_key FROM titles WHERE library_id = 'libtv'`)
	if err != nil {
		t.Fatalf("read keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		if store.IsTempIdentityKey(key) {
			t.Fatalf("a temporary re-key parking key survived the commit: %q", key)
		}
	}
}

// TestApplyRejectedDuringScan: Apply is a catalog writer like a scan, so it takes
// the same per-Library lock (ADR-0031). A scan holding it means Apply fails
// cleanly with NOTHING written — the same idempotent posture a rejected scan has.
func TestApplyRejectedDuringScan(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
	)
	path := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	before := f.titleIDForPath(path)
	f.lock.held["libtv"] = true // a scan is walking this Library

	if _, err := f.apply(placedOn(path, 4, 1, 1)); !errors.Is(err, catalog.ErrScanRunning) {
		t.Fatalf("apply during a scan: err = %v, want ErrScanRunning", err)
	}
	if got := f.decisions(); len(got) != 0 {
		t.Fatalf("a rejected Apply wrote decisions: %#v", got)
	}
	if _, _, _, _, ok := f.titleAt(4, 1); ok {
		t.Fatalf("a rejected Apply moved a Title")
	}
	if id := f.titleIDForPath(path); id != before {
		t.Fatalf("a rejected Apply disturbed the Show")
	}
	if len(f.enrch.calls) != 0 {
		t.Fatalf("a rejected Apply queued re-enrichment: %v", f.enrch.calls)
	}
}

// TestApplyDisplacesTheParsedFileItPushedOff is the collision Apply is able to
// create and must therefore settle: place file B on S01E01 while file A still
// PARSES to S01E01 and has no decision of its own. Both would resolve to one Slot
// and the second would silently overwrite the first's whole subtree. Displacing a
// parsed file is itself a decision, so Apply writes one.
func TestApplyDisplacesTheParsedFileItPushedOff(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	displacedPath := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	moved := episodePath(1, "The Bear (2022) - S01E02 - Hands.mkv")

	res := f.mustApply(placedOn(moved, 1, 1, 1))

	if len(res.Displaced) != 1 || res.Displaced[0] != displacedPath {
		t.Fatalf("displaced = %v, want exactly %q", res.Displaced, displacedPath)
	}
	got := f.decisions()
	if len(got[displacedPath]) != 1 || got[displacedPath].State() != store.DecisionUnassigned {
		t.Fatalf("displaced file's decision = %#v, want one unassigned row", got[displacedPath])
	}
	// One Title on the Slot, holding the file the Admin put there.
	id, _, _, _, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("no Title at S01E01")
	}
	eds := f.editionsOf(id)
	if len(eds) != 1 || len(eds[0].Files) != 1 || eds[0].Files[0].Path != moved {
		t.Fatalf("S01E01 holds %#v, want only %q", eds, moved)
	}
	// The displaced file is off its Slot: it backs no visible Title. (Its File row
	// went with the Episode that was emptied and superseded — the Slot's key now
	// belongs to the row the Admin's file brought with it — so the assertion is
	// about visibility, not about the row.)
	var visible int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM files f JOIN editions e ON e.id = f.edition_id
		   JOIN titles t ON t.id = e.title_id
		  WHERE f.path = ? AND f.present = 1 AND t.hidden = 0`, displacedPath).
		Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("displaced file still backs a visible Title (%d, err %v)", visible, err)
	}
}

// TestApplyParseCollisionLandsBothFilesOnOneAmbiguousSlot: two Files whose
// FILENAMES claim one Slot are ONE Episode holding BOTH of them, flagged
// ambiguous — the collision rule docs/naming-convention.md has always stated
// ("two files that parse to the same Edition identity and are not parts are
// flagged ambiguous in the web app, never silently guessed").
//
// This test used to assert a refusal (ErrSlotCollision), and that refusal existed
// only because of the data-loss bug beneath it: scanner.ResolveEpisodes emitted one
// Episode tree PER FILE, so two files on one Slot produced two Titles carrying one
// identity_key and the second silently deleted the first's Editions and Files.
// Refusing to commit such a set was the right call while the set was destructive.
// It is not a set any more — the resolver groups a Slot's Files before building it
// (tv-episode-editions/01) — so the honest outcome is the convention's own: keep
// both Files, flag the Episode, and let the Admin settle it in the matcher, which
// can now finally SEE both Files.
func TestApplyParseCollisionLandsBothFilesOnOneAmbiguousSlot(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E01 - System (repack).mkv", 1000),
	)
	system := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	repack := episodePath(1, "The Bear (2022) - S01E01 - System (repack).mkv")

	// As seeded, the repack is parked on S01E02 by a Placement. Take that decision
	// back and it falls to its filename, which claims S01E01 — the Slot the other
	// file's filename claims too.
	f.mustApply(placedOn(repack, 1, 2, 1))
	f.mustApply()

	// The decision is genuinely gone: both files are back on their filenames.
	if got := f.decisions(); len(got[repack]) != 0 {
		t.Fatalf("the standing decision was not taken back: %#v", got)
	}
	id, _, _, _, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("no Title at S01E01")
	}
	// One Episode, holding both claimants — neither file is destroyed.
	var paths []string
	for _, ed := range f.editionsOf(id) {
		for _, file := range ed.Files {
			paths = append(paths, file.Path)
		}
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != repack || paths[1] != system {
		t.Fatalf("S01E01 holds %v, want both %q and %q", paths, repack, system)
	}
	// Flagged, never guessed: the Admin has to say which is which.
	var ambiguous int
	if err := f.db.QueryRow(`SELECT ambiguous FROM titles WHERE id = ?`, id).Scan(&ambiguous); err != nil {
		t.Fatalf("read ambiguous: %v", err)
	}
	if ambiguous != 1 {
		t.Errorf("colliding files must flag the Episode ambiguous")
	}
	// S01E02 kept nothing, so it is emptied out of browse rather than left as a
	// Slot pointing at a file that moved.
	if _, _, _, hidden, ok := f.titleAt(1, 2); ok && !hidden {
		t.Errorf("S01E02 lost its only file and must not stay visible")
	}
}

// TestApplyIgnoreTakesAFileOutOfBrowse: an Ignored file is settled and silent —
// no Episode, and its Title drops out of browse. Nothing on disk is touched.
func TestApplyIgnoreTakesAFileOutOfBrowse(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E02 - Hands.mkv", 1000),
	)
	sample := episodePath(1, "The Bear (2022) - S01E02 - Hands.mkv")

	f.mustApply(store.FileDecision{Path: sample, State: store.DecisionIgnored})

	_, _, _, hidden, ok := f.titleAt(1, 2)
	if !ok || !hidden {
		t.Fatalf("the ignored file's Episode is still visible (found=%v hidden=%v)", ok, hidden)
	}
	if _, _, _, hidden, ok := f.titleAt(1, 1); !ok || hidden {
		t.Fatalf("ignoring one file disturbed its sibling")
	}
	// Soft-deleted, never removed: the File row survives so re-placing it later
	// reuses it, and nothing on disk was touched (ADR-0008).
	var present int
	if err := f.db.QueryRow(`SELECT present FROM files WHERE path = ?`, sample).
		Scan(&present); err != nil || present != 0 {
		t.Fatalf("ignored file present = %d (err %v), want a surviving row marked absent", present, err)
	}
}

// TestApplyClearingADecisionReturnsTheFileToItsFilename: sparse storage spends
// "no row" on "follow the parse", so an Apply that omits a File's rows must delete
// them — the File goes back to where its filename says, not stay where the
// withdrawn decision put it.
func TestApplyClearingADecisionReturnsTheFileToItsFilename(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
	)
	path := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	original := f.titleIDForPath(path)

	f.mustApply(placedOn(path, 4, 7, 1))
	if _, _, _, _, ok := f.titleAt(4, 7); !ok {
		t.Fatalf("the Placement did not take")
	}

	f.mustApply() // the Admin takes it back

	if got := f.decisions(); len(got) != 0 {
		t.Fatalf("decisions survived their withdrawal: %#v", got)
	}
	id, season, episode, hidden, ok := f.titleAt(1, 1)
	if !ok || hidden {
		t.Fatalf("the File did not return to its parsed Slot (found=%v hidden=%v)", ok, hidden)
	}
	if season != 1 || episode != 1 {
		t.Fatalf("returned to S%02dE%02d, want S01E01", season, episode)
	}
	if id != original {
		t.Fatalf("the round trip minted a new row: %q → %q", original, id)
	}
}

// TestApplyDefersAFileTheCatalogHasNeverProbed: the Admin can place a file the
// scan never catalogued (an Unmatched one). Apply reuses stored File rows and
// never probes, so it cannot build that Episode — the DECISION is stored, the
// caller is told, and the Episode arrives on the next scan.
func TestApplyDefersAFileTheCatalogHasNeverProbed(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
	)
	stray := episodePath(1, "bear.s01.e09.WEBRip.mkv")

	res := f.mustApply(placedOn(stray, 1, 9, 1))

	if len(res.Deferred) != 1 || res.Deferred[0] != stray {
		t.Fatalf("deferred = %v, want %q", res.Deferred, stray)
	}
	if got := f.decisions(); len(got[stray]) != 1 || got[stray][0].SlotNumber != 9 {
		t.Fatalf("the decision was not stored: %#v", got)
	}
	if _, _, _, _, ok := f.titleAt(1, 9); ok {
		t.Fatalf("Apply invented a Title for a File it has never probed")
	}
	// The Show it was placed into is otherwise untouched.
	if _, _, _, hidden, ok := f.titleAt(1, 1); !ok || hidden {
		t.Fatalf("the rest of the Show was disturbed")
	}
}

// TestApplyRefusesAFileFromAnotherShow: a decision is keyed on (library, path)
// with no Show on it, so a foreign path accepted here would be replayed against
// whichever Show its folder resolves to — silently rearranging one the Admin was
// not even looking at.
func TestApplyRefusesAFileFromAnotherShow(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
	)
	foreign := "/media/TV/Severance (2022)/Season 01/Severance (2022) - S01E01 - Good News.mkv"

	if _, err := f.apply(placedOn(foreign, 1, 2, 1)); !errors.Is(err, catalog.ErrOutsideShow) {
		t.Fatalf("err = %v, want ErrOutsideShow", err)
	}
	if got := f.decisions(); len(got) != 0 {
		t.Fatalf("a refused Apply wrote decisions: %#v", got)
	}
	// The Library lock is released even on the refusal, so the next Apply is not
	// locked out by the one that failed.
	if f.lock.held["libtv"] {
		t.Fatalf("the library lock was not released after a refused Apply")
	}
	if f.lock.locked != 1 || f.lock.unlockd != 1 {
		t.Fatalf("lock/unlock = %d/%d, want 1/1", f.lock.locked, f.lock.unlockd)
	}
}
