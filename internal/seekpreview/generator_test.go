package seekpreview_test

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/bglane"
	"github.com/goozakdev/obelo-server/internal/seekpreview"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Seek preview generation (ADR-0070). These tests drive a Generator over a real
// store with a fake Extractor, so the order, staleness, lifecycle, failure and
// yield rules are observable without decoding anything.

// fakeExtractor returns a 320x180 frame whose colour is its index (atMs/10 s) and
// records every call, so a test can see what was extracted, in what order.
type fakeExtractor struct {
	mu    sync.Mutex
	calls []string // path of every call
	fail  map[string]error
	onHit func(path string, atMs int64)
	// endMs, when set, is where the video ends: later frames are ErrNoFrame.
	endMs     int64
	endByPath map[string]int64
	// streamMs, when set, is the video stream's own duration, as a prober knows it.
	streamMs int64
}

func (x *fakeExtractor) VideoDurationMs(context.Context, string) (int64, bool) {
	return x.streamMs, x.streamMs > 0
}

func (x *fakeExtractor) Frame(ctx context.Context, path string, atMs int64) (image.Image, error) {
	x.mu.Lock()
	x.calls = append(x.calls, path)
	err := x.fail[path]
	if x.endMs != 0 && atMs >= x.endMs {
		err = seekpreview.ErrNoFrame
	}
	if end, ok := x.endByPath[path]; ok && atMs >= end {
		err = seekpreview.ErrNoFrame
	}
	hit := x.onHit
	x.mu.Unlock()
	if hit != nil {
		hit(path, atMs)
	}
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	i := uint8(atMs/10_000 + 1)
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: i, G: i, B: i, A: 255}), image.Point{}, draw.Src)
	return img, nil
}

func (x *fakeExtractor) paths() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.calls...)
}

// order is the distinct paths in call order (a File's frames are consecutive).
func (x *fakeExtractor) order() []string {
	var out []string
	for _, p := range x.paths() {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}

func (x *fakeExtractor) failPath(path string, err error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fail == nil {
		x.fail = map[string]error{}
	}
	if err == nil {
		delete(x.fail, path)
		return
	}
	x.fail[path] = err
}

type fixture struct {
	db  *store.DB
	dir string
	ex  *fakeExtractor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmov', 'Movies', 'movie')`)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus', 'Music', 'music')`)
	return &fixture{db: db, dir: filepath.Join(t.TempDir(), "seekpreviews"), ex: &fakeExtractor{}}
}

func mustExec(t *testing.T, db *store.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// addFile puts one video File (durationMs long) in the Library as its own Title,
// added at addedAt, and returns its id. Its path is "/m/<id>.mkv".
func (f *fixture) addFile(t *testing.T, lib, id string, durationMs int64, addedAt string) string {
	t.Helper()
	tree := store.TitleTree{
		Title: store.Title{ID: "t-" + id, LibraryID: lib, Kind: "movie", Title: id, IdentityKey: id, SortTitle: id},
		Editions: []store.Edition{{ID: "ed-" + id, Files: []store.File{{
			ID: id, Path: "/m/" + id + ".mkv", Container: "matroska", DurationMs: durationMs,
			Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000, Present: true,
			Streams: []store.Stream{{ID: "s-" + id, Index: 0, Kind: "video", Codec: "h264"}},
		}}}},
	}
	if err := f.db.UpsertTitleTree(tree); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f.db, `UPDATE files SET added_at = ? WHERE id = ?`, addedAt, id)
	return id
}

func (f *fixture) generator(opts seekpreview.Options) *seekpreview.Generator {
	if opts.YieldPoll == 0 {
		opts.YieldPoll = 5 * time.Millisecond
	}
	return seekpreview.New(f.db, f.ex, f.dir, opts)
}

func start(t *testing.T, g *seekpreview.Generator) {
	t.Helper()
	g.Start()
	t.Cleanup(g.Close)
}

// waitFor polls cond (a store or disk read, not a timer on the code under test).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fixture) record(t *testing.T, id string) (store.SeekPreviewSet, bool) {
	t.Helper()
	recs, err := f.db.SeekPreviewRecords()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.FileID == id {
			return r.SeekPreviewSet, true
		}
	}
	return store.SeekPreviewSet{}, false
}

func (f *fixture) hasSet(t *testing.T, id string) bool {
	t.Helper()
	s, ok := f.record(t, id)
	return ok && s.Status == store.SeekPreviewReady
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestGenerationOrderBumpThenMostRecentlyAdded(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "old", 10_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "mid", 10_000, "2024-02-01T00:00:00Z")
	f.addFile(t, "libmov", "new", 10_000, "2024-03-01T00:00:00Z")
	var hold atomic.Bool // busy until every request is in, so nothing starts before
	hold.Store(true)
	g := f.generator(seekpreview.Options{Busy: hold.Load})
	start(t, g)
	g.AfterScan("libmov")
	g.Bump("old")
	hold.Store(false)
	waitFor(t, "all three sets", func() bool { return f.hasSet(t, "old") && f.hasSet(t, "mid") && f.hasSet(t, "new") })
	want := []string{"/m/old.mkv", "/m/new.mkv", "/m/mid.mkv"}
	got := f.ex.order()
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("generation order = %v, want %v (bumped first, then most recently added)", got, want)
	}
}

func TestNothingStartsWhileATranscodeRuns(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "a", 10_000, "2024-01-01T00:00:00Z")
	var mu sync.Mutex
	busy := true
	g := f.generator(seekpreview.Options{Busy: func() bool { mu.Lock(); defer mu.Unlock(); return busy }})
	start(t, g)
	g.AfterScan("libmov")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted while a Transcode was running", n)
	}
	mu.Lock()
	busy = false
	mu.Unlock()
	waitFor(t, "the set once idle", func() bool { return f.hasSet(t, "a") })
}

func TestYieldsBetweenSheetsWhenATranscodeStarts(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "long", 1_600_000, "2024-01-01T00:00:00Z") // 160 frames = 2 sheets
	var mu sync.Mutex
	busy := false
	f.ex.onHit = func(string, int64) {
		// A Transcode starts while the first sheet is being built.
		mu.Lock()
		defer mu.Unlock()
		if len(f.ex.calls) == 40 {
			busy = true
		}
	}
	g := f.generator(seekpreview.Options{Busy: func() bool { mu.Lock(); defer mu.Unlock(); return busy }})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "sheet 0", func() bool { return exists(filepath.Join(f.dir, "long", "0.jpg")) })
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 80 {
		t.Fatalf("%d frames extracted while a Transcode ran, want the 80 of sheet 0 only", n)
	}
	if exists(filepath.Join(f.dir, "long", "1.jpg")) || f.hasSet(t, "long") {
		t.Fatal("generation went on past the safe point while a Transcode was running")
	}
	mu.Lock()
	busy = false
	mu.Unlock()
	waitFor(t, "the set once idle", func() bool { return f.hasSet(t, "long") })
	if s, _ := f.record(t, "long"); s.SheetCount != 2 || s.FrameCount != 160 {
		t.Fatalf("record = %+v, want 2 sheets and 160 frames", s)
	}
}

func TestSheetLayoutAndRecord(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 955_000, "2024-01-01T00:00:00Z") // 96 frames: sheet 1 holds 16
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the set", func() bool { return f.hasSet(t, "m") })
	s, _ := f.record(t, "m")
	want := store.SeekPreviewSet{
		FileID: "m", Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000, Status: "ready",
		IntervalMs: 10_000, FrameWidth: 320, FrameHeight: 180, Columns: 10, Rows: 8, FrameCount: 96, SheetCount: 2,
	}
	if s != want {
		t.Fatalf("record = %+v, want %+v", s, want)
	}
	sheet := decodeJPEG(t, filepath.Join(f.dir, "m", "1.jpg"))
	if b := sheet.Bounds(); b.Dx() != 3200 || b.Dy() != 1440 {
		t.Fatalf("sheet 1 is %dx%d, want 3200x1440", b.Dx(), b.Dy())
	}
	// Frame 90 is cell 10 of sheet 1: row 1, column 0 (row-major). Its colour is
	// its index; JPEG is lossy, so compare within a tolerance.
	if v := luma(sheet, 160, 180+90); v < 85 || v > 97 {
		t.Errorf("cell 10 luma = %d, want about 91 (frame 90)", v)
	}
	// Cells 0-15 hold frames 80-95; cell 16 (row 1, column 6) onward is unused: black.
	if v := luma(sheet, 6*320+160, 180+90); v > 8 {
		t.Errorf("unused cell 16 luma = %d, want black", v)
	}
	if v := luma(sheet, 3199, 1439); v > 8 {
		t.Errorf("last cell luma = %d, want black", v)
	}
}

func TestStaleSetIsDeletedAndRegenerated(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "first set", func() bool { return f.hasSet(t, "m") })
	// The File changes on disk; the Scanner updates its row in place.
	mustExec(t, f.db, `UPDATE files SET mtime = '2025-05-05T00:00:00Z', size_bytes = 2222 WHERE id = 'm'`)
	g.AfterScan("libmov")
	waitFor(t, "regenerated set", func() bool {
		s, ok := f.record(t, "m")
		return ok && s.Mtime == "2025-05-05T00:00:00Z" && s.SizeBytes == 2222 && s.Status == "ready"
	})
	if n := len(f.ex.paths()); n != 2 {
		t.Fatalf("%d frames extracted over two generations of a one-frame File, want 2", n)
	}
}

func TestStaleSetIsDeletedEvenWhenTheToggleIsOff(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "first set", func() bool { return f.hasSet(t, "m") })
	if err := f.db.SetSeekPreviewsEnabled("libmov", false); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f.db, `UPDATE files SET size_bytes = 9 WHERE id = 'm'`)
	g.AfterScan("libmov")
	waitFor(t, "the stale set gone", func() bool { _, ok := f.record(t, "m"); return !ok })
	if exists(filepath.Join(f.dir, "m")) {
		t.Fatal("stale set still on disk")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(f.ex.paths()); n != 1 {
		t.Fatalf("%d frames extracted, want 1: the toggle is off so no new set is made", n)
	}
}

func TestLifecycleMissingKeptPurgedDeletedToggleKept(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "missing", 10_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "purged", 10_000, "2024-01-02T00:00:00Z")
	f.addFile(t, "libmov", "toggled", 10_000, "2024-01-03T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "three sets", func() bool { return f.hasSet(t, "missing") && f.hasSet(t, "purged") && f.hasSet(t, "toggled") })

	mustExec(t, f.db, `UPDATE files SET present = 0 WHERE id = 'missing'`)
	mustExec(t, f.db, `DELETE FROM files WHERE id = 'purged'`)
	if err := f.db.SetSeekPreviewsEnabled("libmov", false); err != nil {
		t.Fatal(err)
	}
	g.AfterScan("libmov")
	waitFor(t, "the purged set gone", func() bool { _, ok := f.record(t, "purged"); return !ok })
	if exists(filepath.Join(f.dir, "purged")) {
		t.Error("purged File's set still on disk")
	}
	for _, id := range []string{"missing", "toggled"} {
		if !f.hasSet(t, id) || !exists(filepath.Join(f.dir, id, "0.jpg")) {
			t.Errorf("%s: set was not kept", id)
		}
	}
}

func TestToggleOffStopsGenerationAndMusicIsNeverEligible(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmus", "song", 10_000, "2024-01-01T00:00:00Z")
	if err := f.db.SetSeekPreviewsEnabled("libmov", false); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetSeekPreviewsEnabled("libmus", true); !errors.Is(err, store.ErrNoSeekPreviews) {
		t.Fatalf("music toggle err = %v, want ErrNoSeekPreviews", err)
	}
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	g.AfterScan("libmus")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted for a disabled Library and a music Library, want 0", n)
	}
}

func TestLinkedLibraryFilesAreNeverGenerated(t *testing.T) {
	f := newFixture(t)
	mustExec(t, f.db, `INSERT INTO links (id, server_id, server_name, origins, token, link_protocol_version, created_at)
		VALUES ('lnk', 'srv', 'Other', '[]', 'x', 1, '2024-01-01T00:00:00Z')`)
	mustExec(t, f.db, `INSERT INTO libraries (id, name, kind, source, link_id, remote_library_id)
		VALUES ('liblnk', 'Dave', 'movie', 'linked', 'lnk', 'r1')`)
	f.addFile(t, "liblnk", "mirror", 10_000, "2024-01-01T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("liblnk")
	g.GenerateTitle("t-mirror")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted for a Linked Library mirror, want 0", n)
	}
}

func TestFailureIsRecordedAndNotRetriedUntilTheFileChangesOrRetry(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "bad", 10_000, "2024-01-01T00:00:00Z")
	f.ex.failPath("/m/bad.mkv", errors.New("moov atom not found\nsecond line"))
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the failure record", func() bool { s, ok := f.record(t, "bad"); return ok && s.Status == "failed" })
	s, _ := f.record(t, "bad")
	if s.Reason != "frame 0: moov atom not found" {
		t.Errorf("reason = %q, want the one-line reason", s.Reason)
	}
	if exists(filepath.Join(f.dir, "bad")) {
		t.Error("a failed File left a directory behind")
	}

	g.AfterScan("libmov")
	g.AfterScan("libmov")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 1 {
		t.Fatalf("%d extractions after the failure and two more scans, want no automatic retry (1 total)", n)
	}

	// The File changes: it is tried again, and succeeds.
	f.ex.failPath("/m/bad.mkv", nil)
	mustExec(t, f.db, `UPDATE files SET size_bytes = 5 WHERE id = 'bad'`)
	g.AfterScan("libmov")
	waitFor(t, "the set after the File changed", func() bool { return f.hasSet(t, "bad") })

	// A failure at the new size is retried by an explicit Retry.
	f.ex.failPath("/m/bad.mkv", errors.New("boom"))
	if err := g.Retry("bad"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failure after Retry", func() bool { s, _ := f.record(t, "bad"); return s.Status == "failed" })
	f.ex.failPath("/m/bad.mkv", nil)
	if err := g.Retry("bad"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the set after a second Retry", func() bool { return f.hasSet(t, "bad") })
}

func TestGenerateNowRegeneratesAReadySetOfATitle(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "first set", func() bool { return f.hasSet(t, "m") })
	if err := g.GenerateTitle("t-m"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second extraction", func() bool { return len(f.ex.paths()) == 2 })
	if err := g.GenerateTitle("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown Title err = %v, want ErrNotFound", err)
	}
	if err := g.GenerateShow("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown Show err = %v, want ErrNotFound", err)
	}
}

func TestDeleteLibraryRemovesSetsAndRecordsAndKeepsOthers(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "a", 10_000, "2024-01-01T00:00:00Z")
	mustExec(t, f.db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	f.addFile(t, "libtv", "b", 10_000, "2024-01-01T00:00:00Z")
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	g.AfterScan("libtv")
	waitFor(t, "both sets", func() bool { return f.hasSet(t, "a") && f.hasSet(t, "b") })
	if err := f.db.SetSeekPreviewsEnabled("libmov", false); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteLibrary("libmov"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.record(t, "a"); ok || exists(filepath.Join(f.dir, "a")) {
		t.Error("deleted Library's set remains")
	}
	if !f.hasSet(t, "b") || !exists(filepath.Join(f.dir, "b", "0.jpg")) {
		t.Error("another Library's set was deleted")
	}
}

func TestGenerationSharesTheLaneWithAHolder(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	var lane bglane.Lane
	rel, err := lane.Acquire(context.Background(), bglane.PriorityMarkers)
	if err != nil {
		t.Fatal(err)
	}
	g := f.generator(seekpreview.Options{Lane: &lane})
	start(t, g)
	g.AfterScan("libmov")
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted while another job held the lane", n)
	}
	rel()
	waitFor(t, "the set once the lane is free", func() bool { return f.hasSet(t, "m") })
}

func TestVideoShorterThanTheContainerEndsAtItsLastFrame(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 121_000, "2024-01-01T00:00:00Z") // format duration, from the audio
	f.ex.endMs = 118_000                                         // the video stops here
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the set", func() bool { s, ok := f.record(t, "m"); return ok && s.Status != "" })
	s, _ := f.record(t, "m")
	if s.Status != "ready" || s.FrameCount != 12 || s.SheetCount != 1 {
		t.Fatalf("record = %+v, want ready with the 12 frames the video has (0-110 s)", s)
	}
}

func TestFileThatYieldsNoFrameStillFails(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 121_000, "2024-01-01T00:00:00Z")
	f.ex.endMs = -1 // no frame at 0 ms either
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the failure", func() bool { s, ok := f.record(t, "m"); return ok && s.Status == "failed" })
}

func TestStreamDurationEndsTheVideoWhenItIsShorterThanTheStoredOne(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 135_000, "2024-01-01T00:00:00Z")
	f.ex.streamMs = 118_000
	f.ex.endMs = 118_000
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the set", func() bool { return f.hasSet(t, "m") })
	if s, _ := f.record(t, "m"); s.FrameCount != 12 {
		t.Fatalf("frame count = %d, want 12 from the 118 s stream", s.FrameCount)
	}
}

// TestStoredDurationShorterThanTheStreamDoesNotUnderCoverTheFile: the catalog
// says 60 s but the video runs 118 s; the set covers the video.
func TestStoredDurationShorterThanTheStreamDoesNotUnderCoverTheFile(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 60_000, "2024-01-01T00:00:00Z")
	f.ex.streamMs = 118_000
	f.ex.endMs = 118_000
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the set", func() bool { return f.hasSet(t, "m") })
	if s, _ := f.record(t, "m"); s.FrameCount != 12 {
		t.Fatalf("frame count = %d, want 12 (the video's 118 s, not the stored 60 s)", s.FrameCount)
	}
}

// TestFileChangedWhileItIsBeingGeneratedIsNotKeptAsReady: the Scanner updates the
// row mid-generation; the set made from the old bytes is discarded and the File
// is made again, with no further scan.
func TestFileChangedWhileItIsBeingGeneratedIsNotKeptAsReady(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	var once sync.Once
	f.ex.onHit = func(string, int64) {
		once.Do(func() { mustExec(t, f.db, `UPDATE files SET size_bytes = 9999 WHERE id = 'm'`) })
	}
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "a set that matches the changed File", func() bool {
		s, ok := f.record(t, "m")
		return ok && s.Status == "ready" && s.SizeBytes == 9999
	})
	if n := len(f.ex.paths()); n != 2 {
		t.Fatalf("%d extractions, want 2: the first set was discarded", n)
	}
}

// TestAfterScanQueuesFilesWhoseSetIsStaleNotOnlyThoseWithNone: a record that differs
// from the catalog is made again after AfterScan.
func TestAfterScanQueuesFilesWhoseSetIsStaleNotOnlyThoseWithNone(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	if err := f.db.SaveSeekPreview(store.SeekPreviewSet{FileID: "m", Mtime: "2020-01-01T00:00:00Z", SizeBytes: 1000, Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "a set matching the catalog", func() bool {
		s, ok := f.record(t, "m")
		return ok && s.Status == "ready" && s.Mtime == "2024-01-01T00:00:00Z"
	})
}

func TestStaleSetIsDeletedWhileATranscodeRuns(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	var busy atomic.Bool
	g := f.generator(seekpreview.Options{Busy: busy.Load})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "first set", func() bool { return f.hasSet(t, "m") })
	busy.Store(true)
	mustExec(t, f.db, `UPDATE files SET size_bytes = 77 WHERE id = 'm'`)
	g.AfterScan("libmov")
	waitFor(t, "the stale set gone during a Transcode", func() bool {
		_, ok := f.record(t, "m")
		return !ok && !exists(filepath.Join(f.dir, "m"))
	})
	if n := len(f.ex.paths()); n != 1 {
		t.Fatalf("%d frames extracted, want 1: only deleting is exempt from waiting", n)
	}
}

func TestBackfillQueuesEveryFileWithoutARecordNewestFirstAndIdleOnly(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "old", 10_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "new", 10_000, "2024-03-01T00:00:00Z")
	f.addFile(t, "libmov", "done", 10_000, "2024-04-01T00:00:00Z")
	f.addFile(t, "libmov", "bad", 10_000, "2024-05-01T00:00:00Z")
	f.addFile(t, "libmus", "song", 10_000, "2024-06-01T00:00:00Z")
	mustExec(t, f.db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	f.addFile(t, "libtv", "ep", 10_000, "2024-02-01T00:00:00Z")
	for id, status := range map[string]string{"done": "ready", "bad": "failed"} {
		if err := f.db.SaveSeekPreview(store.SeekPreviewSet{FileID: id, Mtime: "2024-01-01T00:00:00Z", SizeBytes: 1000, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	var busy atomic.Bool
	busy.Store(true)
	g := f.generator(seekpreview.Options{Busy: busy.Load})
	start(t, g)
	g.Backfill()
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted while a Transcode ran", n)
	}
	busy.Store(false)
	waitFor(t, "three sets", func() bool { return f.hasSet(t, "old") && f.hasSet(t, "new") && f.hasSet(t, "ep") })
	want := []string{"/m/new.mkv", "/m/ep.mkv", "/m/old.mkv"}
	got := f.ex.order()
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("backfill order = %v, want %v (newest first; recorded and music Files left out)", got, want)
	}
}

func TestHDRFileThatCannotBeToneMappedFailsWithTheDecidedReasonAndRetryRecovers(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "hdr", 10_000, "2024-01-01T00:00:00Z")
	f.ex.failPath("/m/hdr.mkv", seekpreview.ErrToneMapUnavailable)
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "the failure", func() bool { s, ok := f.record(t, "hdr"); return ok && s.Status == "failed" })
	if s, _ := f.record(t, "hdr"); s.Reason != "ffmpeg lacks zscale; cannot tone-map HDR" {
		t.Fatalf("reason = %q, want the decided one", s.Reason)
	}
	if exists(filepath.Join(f.dir, "hdr")) {
		t.Fatal("a washed-out set was left behind")
	}
	f.ex.failPath("/m/hdr.mkv", nil) // ffmpeg upgraded
	if err := g.Retry("hdr"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the set after Retry", func() bool { return f.hasSet(t, "hdr") })
}

func TestEmptyFrameMidFileFailsTheFileButNearTheEndEndsIt(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "mid", 200_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "tail", 200_000, "2024-01-02T00:00:00Z")
	f.ex.mu.Lock()
	f.ex.endByPath = map[string]int64{"/m/mid.mkv": 50_000, "/m/tail.mkv": 185_000}
	f.ex.mu.Unlock()
	g := f.generator(seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "both records", func() bool { _, a := f.record(t, "mid"); _, b := f.record(t, "tail"); return a && b })
	if s, _ := f.record(t, "mid"); s.Status != "failed" || !strings.Contains(s.Reason, "frame 5") {
		t.Errorf("mid-file record = %+v, want failed at frame 5", s)
	}
	if s, _ := f.record(t, "tail"); s.Status != "ready" || s.FrameCount != 19 {
		t.Errorf("near-the-end record = %+v, want ready with 19 frames (frame 19 is empty, in the last interval)", s)
	}
}
