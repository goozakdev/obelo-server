package catalog_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/scanner"
	"github.com/goozakdev/obelo-server/internal/store"
)

// unreadable_file_test.go pins what happens to a file that is NAMED correctly and
// CORRUPT — a truncated download, a bad remux — which is a different problem from
// every other row in the Needs-Fixing queue and used to be indistinguishable from
// one of them.
//
// The trap it exists to prevent: the file's name numbers it, so the file matcher
// resolves it onto its Slot and the screen reports the Show as perfectly sorted,
// while ffprobe refuses the bytes and no Episode is ever built. Told it was
// "not recognized as a title", the Admin searches a provider and presses "Use
// this" — an identity correction for a file whose identity was never wrong. It
// cannot work, and the row comes back on the next scan, forever.

// corruptProber probes everything except the one file it is told is corrupt —
// named by basename, which the fixture's files carry uniquely, so the prober can
// be built before the temp directory it will be asked about exists. It fails the
// way the real ffprobe fails on a bad EBML header.
type corruptProber struct {
	rescanProber
	corrupt string // basename
}

func (p *corruptProber) Probe(ctx context.Context, path string) (scanner.MediaInfo, error) {
	if filepath.Base(path) == p.corrupt {
		return scanner.MediaInfo{}, &scanner.ProbeError{
			Path:   path,
			Detail: "Invalid data found when processing input",
		}
	}
	return p.rescanProber.Probe(ctx, path)
}

// newCorruptFixture is newRescanFixture with one file ffprobe cannot read, corrupt
// from the very first scan — the shape that matters, since an incremental rescan
// never re-probes a file that has not changed.
func newCorruptFixture(t *testing.T, corrupt string, files ...string) *rescanFixture {
	t.Helper()
	return newRescanFixtureWith(t, &corruptProber{corrupt: filepath.Base(corrupt)}, files...)
}

func (f *rescanFixture) unmatched() []store.UnmatchedFile {
	f.t.Helper()
	list, err := f.db.ListUnmatched("libtv")
	if err != nil {
		f.t.Fatalf("list unmatched: %v", err)
	}
	return list
}

func TestCorruptFileIsUnreadableNotUnidentified(t *testing.T) {
	f := newCorruptFixture(t, relE01, oneSeason...)

	list := f.unmatched()
	if len(list) != 1 {
		t.Fatalf("unmatched = %+v, want just the corrupt file", list)
	}
	u := list[0]
	if u.Path != f.path(relE01) {
		t.Fatalf("unmatched names %q, want the corrupt file", u.Path)
	}
	if !u.Unreadable() {
		t.Fatalf("the corrupt file is kind %q, want %q", u.Kind, store.UnmatchedUnreadable)
	}
	// The reason has to carry ffprobe's own verdict: "could not probe" sends an
	// Admin looking for a naming mistake that is not there.
	if !strings.Contains(u.Reason, "Invalid data found when processing input") {
		t.Fatalf("reason %q does not say what ffprobe said", u.Reason)
	}

	// No identity anchor is offered, so the inert "Use this" cannot even be built.
	items, err := f.cat.ListUnmatched("libtv")
	if err != nil {
		t.Fatalf("catalog list unmatched: %v", err)
	}
	if len(items) != 1 || items[0].Anchor != "" {
		t.Fatalf("unreadable file carries fix-match anchor %q, want none", items[0].Anchor)
	}
}

func TestUnreadableFileKeepsItsOwnRowInTheQueue(t *testing.T) {
	// A Show with OTHER unsettled work, so the fold has something to fold into:
	// without the exclusion the corrupt file is absorbed into the Show row and
	// disappears behind a matcher screen that says the Show is already correct.
	f := newCorruptFixture(t, relE01, oneSeason...)
	f.apply(f.settle(relE04, store.DecisionUnassigned))

	problems, err := f.cat.ShowProblems("libtv")
	if err != nil {
		t.Fatalf("show problems: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("show problems = %+v, want the one Show", problems)
	}
	for _, p := range problems[0].UnmatchedPaths {
		if p == f.path(relE01) {
			t.Fatalf("the unreadable file was folded into the Show row, hiding it from the queue")
		}
	}
	if problems[0].Unassigned != 1 {
		t.Fatalf("unassigned = %d, want the one file the Admin took off its Slot", problems[0].Unassigned)
	}
	// Attributed all the same, so the flat row can name the Show whose matcher can
	// ignore it — the one gesture that settles a file nobody intends to replace.
	if got := problems[0].UnreadablePaths; len(got) != 1 || got[0] != f.path(relE01) {
		t.Fatalf("unreadable paths = %v, want the corrupt file attributed to its Show", got)
	}
}

// TestShowWithOnlyAnUnreadableFileIsReportedButCountsNothing: the attribution has
// to survive a Show that is otherwise perfect, which is the common case — one bad
// file in an otherwise clean season. It reports zero work, so no client builds a
// Show row from it, and it still says which Show the file is under.
func TestShowWithOnlyAnUnreadableFileIsReportedButCountsNothing(t *testing.T) {
	f := newCorruptFixture(t, relE01, oneSeason...)

	problems, err := f.cat.ShowProblems("libtv")
	if err != nil {
		t.Fatalf("show problems: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("show problems = %+v, want the Show carrying the unreadable file", problems)
	}
	p := problems[0]
	if !p.Empty() {
		t.Fatalf("the Show reports work to do (%+v); an unreadable file is not matcher work", p)
	}
	if len(p.UnreadablePaths) != 1 || p.UnreadablePaths[0] != f.path(relE01) {
		t.Fatalf("unreadable paths = %v, want the corrupt file", p.UnreadablePaths)
	}
}

func TestMatcherMarksThePlacedFileItCannotBuild(t *testing.T) {
	f := newCorruptFixture(t, relE01, oneSeason...)

	m, err := f.cat.ShowMatcher(context.Background(), f.showID, nil)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	var found bool
	for _, file := range m.Files {
		if file.Path != f.path(relE01) {
			if file.Unreadable {
				t.Fatalf("%q is marked unreadable and is not", filepath.Base(file.Path))
			}
			continue
		}
		found = true
		// It really is placed — the filename numbers it — which is exactly why the
		// screen has to say the other thing as well.
		if file.State != store.DecisionPlaced {
			t.Fatalf("the corrupt file is %q, want it placed by its filename", file.State)
		}
		if file.TitleID != "" {
			t.Fatalf("the corrupt file has Title %q, want none: it never assembled", file.TitleID)
		}
		if !file.Unreadable {
			t.Fatalf("the matcher shows the corrupt file as an ordinary placed file")
		}
		if !strings.Contains(file.Reason, "Invalid data found") {
			t.Fatalf("placed file kept no reason: %q", file.Reason)
		}
	}
	if !found {
		t.Fatalf("the matcher does not list the corrupt file at all")
	}
}

func TestIgnoringAnUnreadableFileClearsIt(t *testing.T) {
	// The one action that does work today, pinned so it keeps working: an Ignored
	// File is never probed, so the row it was generating stops being generated.
	f := newCorruptFixture(t, relE01, oneSeason...)
	if len(f.unmatched()) != 1 {
		t.Fatalf("expected the corrupt file to be listed before it is ignored")
	}

	f.apply(f.settle(relE01, store.DecisionIgnored))
	f.fullScan()

	if list := f.unmatched(); len(list) != 0 {
		t.Fatalf("ignoring the unreadable file left %+v", list)
	}
}

// TestProbeDetailIsFFprobesOwnVerdict: the reason an Admin reads comes from
// ffprobe's last line, not from its first — the first carries a memory address
// and describes a demuxer internal, the last says what to do about the file.
func TestProbeDetailIsFFprobesOwnVerdict(t *testing.T) {
	stderr := "[matroska,webm @ 0x148e04000] 0x00 at pos 0 (0x0) invalid as first byte of an EBML number\n" +
		"[matroska,webm @ 0x148e04000] EBML header parsing failed\n" +
		"/Volumes/media/TV/Show/S01E01.mkv: Invalid data found when processing input\n"
	err := &exec.ExitError{ProcessState: nil, Stderr: []byte(stderr)}
	var probe error = err
	got := scanner.ProbeDetailForTest(probe)
	if got != "Invalid data found when processing input" {
		t.Fatalf("probe detail = %q, want ffprobe's verdict with the path stripped", got)
	}
	if errors.Is(probe, exec.ErrNotFound) {
		t.Fatal("unreachable; keeps errors imported")
	}
}

// TestARottedFileDoesNotStrandItsOldShow is the second half of the same bug, and
// the one an Admin cannot work around: the library listing the same series twice.
//
// A File ffprobe refuses builds no Title, so an identity correction — which files
// the work under a NEW Show — cannot claim it. Its old File row therefore survived
// every scan (the walk counted the path as seen, because it is on disk), which
// kept its old Episode visible, which kept the ORIGINAL Show row visible: one
// folder on disk, two Shows in the grid, and nothing in the UI that could remove
// the second. The corrupt file was holding the door open for a Show that no longer
// existed.
func TestARottedFileDoesNotStrandItsOldShow(t *testing.T) {
	// Everything readable at first, so the file gets a real catalogued row.
	prober := &corruptProber{}
	f := newRescanFixtureWith(t, prober, oneSeason...)

	// The file rots: its bytes change (so the incremental scan re-probes it rather
	// than reusing the stored row) and ffprobe now refuses it.
	prober.corrupt = filepath.Base(relE01)
	if err := os.WriteFile(f.path(relE01), make([]byte, 200*1024), 0o644); err != nil {
		t.Fatalf("rewriting the file as corrupt: %v", err)
	}

	// The Admin corrects the Show's identity from the Needs-Fixing queue, which is
	// what moves every Episode onto a new identity key — and what used to leave the
	// old Show behind.
	if _, err := f.db.UpsertMatchOverride(store.MatchOverride{
		LibraryID: "libtv", FolderPath: f.show,
		Title: "The Bear", Year: 2022, TMDBID: "228079", IdentityKey: "tmdb:228079",
	}); err != nil {
		t.Fatalf("match override: %v", err)
	}
	f.fullScan()

	visible := f.visibleShows()
	if len(visible) != 1 || visible[0] != "tmdb:228079" {
		t.Fatalf("the library lists %v, want only the corrected Show", visible)
	}

	// The file is still ON DISK and still reported — it is simply not holding a
	// Title open any more.
	list := f.unmatched()
	if len(list) != 1 || !list[0].Unreadable() {
		t.Fatalf("unmatched = %+v, want the corrupt file listed as unreadable", list)
	}

	// And it is still offered in the matcher, so ignoring it there remains possible:
	// a file the Admin cannot see is a file they cannot settle.
	m, err := f.cat.ShowMatcher(context.Background(), f.showIDForKey("tmdb:228079"), nil)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	var listed bool
	for _, file := range m.Files {
		if file.Path == f.path(relE01) {
			listed = true
			if !file.Unreadable {
				t.Fatalf("the corrupt file is listed without its mark")
			}
		}
	}
	if !listed {
		t.Fatalf("the matcher no longer lists the corrupt file, so it cannot be ignored")
	}
}

// visibleShows is every Show identity key the grid would render, ordered.
func (f *rescanFixture) visibleShows() []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT identity_key FROM shows WHERE library_id = 'libtv' AND hidden = 0 ORDER BY identity_key`)
	if err != nil {
		f.t.Fatalf("listing shows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			f.t.Fatalf("scanning show: %v", err)
		}
		out = append(out, key)
	}
	return out
}

func (f *rescanFixture) showIDForKey(key string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(
		`SELECT id FROM shows WHERE library_id = 'libtv' AND identity_key = ?`, key).Scan(&id); err != nil {
		f.t.Fatalf("no Show with key %q: %v", key, err)
	}
	return id
}
