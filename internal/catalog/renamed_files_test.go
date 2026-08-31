package catalog_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// renamed_files_test.go pins the answer to "the Admin fixed the filenames
// themselves": a Show whose files were renamed on disk and rescanned is DONE, and
// neither the matcher nor the Needs-Fixing queue may still be talking about the
// old names.
//
// The old names do not go away. Every File is soft-deleted rather than dropped
// (ADR-0008), so a rename leaves the old path behind as a Missing row under the
// Show forever, and the matcher reads its file list off exactly those rows. Left
// unfiltered, the screen offers a file that is not on disk: it cannot be placed
// (resolve refuses to build a Slot from an absent file), so it can never be
// settled, so its Show never leaves the queue however much sorting is done.

// renameOnDisk moves a file within the Show folder, exactly as an Admin
// straightening out their own naming would.
func (f *rescanFixture) renameOnDisk(from, to string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(to)), 0o755); err != nil {
		f.t.Fatalf("mkdir for rename: %v", err)
	}
	if err := os.Rename(f.path(from), f.path(to)); err != nil {
		f.t.Fatalf("rename %q -> %q: %v", from, to, err)
	}
}

// matcherPaths is every File the matcher would list, Show-relative and sorted.
func (f *rescanFixture) matcherPaths() []string {
	f.t.Helper()
	m, err := f.cat.ShowMatcher(context.Background(), f.showID, nil)
	if err != nil {
		f.t.Fatalf("show matcher: %v", err)
	}
	var out []string
	for _, file := range m.Files {
		rel, err := filepath.Rel(f.show, file.Path)
		if err != nil {
			f.t.Fatalf("matcher listed %q, which is not under the Show: %v", file.Path, err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func TestRenamedFilesLeaveNeitherTheMatcherNorTheQueue(t *testing.T) {
	// The shape that started this: episodes numbered "101 - ..." on disk, which the
	// absolute-number parse files at S01E101 rather than S01E01.
	const (
		oldE01 = "Season 01/101 - System.mkv"
		oldE02 = "Season 01/102 - Hands.mkv"
	)
	f := newRescanFixture(t, oldE01, oldE02)
	if got := f.matcherPaths(); len(got) != 2 {
		t.Fatalf("the seeding scan listed %v, want both mis-numbered files", got)
	}

	// The Admin renames them to the convention and rescans, which is the whole fix:
	// the new files number themselves and no decision is ever recorded.
	f.renameOnDisk(oldE01, relE01)
	f.renameOnDisk(oldE02, relE02)
	f.fullScan()

	want := []string{relE01, relE02}
	if got := f.matcherPaths(); !equalStrings(got, want) {
		t.Fatalf("the matcher lists %v, want only the files on disk %v", got, want)
	}

	// And the queue row goes with them. A Show whose every File is placed has no
	// unsettled work, so it must not be listed at all.
	problems, err := f.cat.ShowProblems("libtv")
	if err != nil {
		t.Fatalf("show problems: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("the renamed Show is still queued as needing fixing: %+v", problems)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
