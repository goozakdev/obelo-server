package catalog_test

import (
	"strings"
	"testing"
)

// tv-episode-editions/01, at the level the bug actually did its damage: a real
// scan into a real store, twice.
//
// The scanner-level tests (scanner/tv_editions_test.go) prove the resolved tree
// is right. These prove the tree SURVIVES the write, which is the half that was
// destroying data: two Episode trees carrying one identity_key both resolved to
// one Title row, and writeTitleSubtree opens by dropping that Title's Editions
// (catalog.go, `DELETE FROM editions WHERE title_id = ?`, Files cascading), so the
// second tree deleted the first's File. Whichever file the walk reached last won,
// and the next scan reversed the coin toss.
//
// Every case therefore scans TWICE and asserts both Files present after the
// SECOND scan. Once is not enough: the loser only disappears when a second tree
// arrives, and a fix that converges on the second pass would still be wrong on the
// first — the one that runs at 4am.

// assertScannedTwiceIsStable runs two more full scans and demands the catalog be
// byte-identical after each. newRescanFixture has already scanned once.
func (f *rescanFixture) assertScannedTwiceIsStable() {
	f.t.Helper()
	first := f.snapshot()
	f.fullScan()
	if diff := diffLines(first, f.snapshot()); diff != "" {
		f.t.Fatalf("a second scan changed the Show:\n%s", diff)
	}
	f.fullScan()
	if diff := diffLines(first, f.snapshot()); diff != "" {
		f.t.Fatalf("a third scan changed the Show:\n%s", diff)
	}
}

// filesUnder returns the paths (relative to the Show folder) of every present
// File the catalog holds for the Show, deduplicated.
func (f *rescanFixture) presentFilePaths() []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT DISTINCT fi.path FROM files fi
		   JOIN editions e ON e.id = fi.edition_id
		   JOIN titles t ON t.id = e.title_id
		  WHERE t.library_id = 'libtv' AND fi.present = 1
		  ORDER BY fi.path`)
	if err != nil {
		f.t.Fatalf("present files: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			f.t.Fatalf("scan path: %v", err)
		}
		out = append(out, f.rel(p))
	}
	return out
}

func (f *rescanFixture) assertHoldsFiles(want ...string) {
	f.t.Helper()
	got := f.presentFilePaths()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("catalogued files =\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestScanMultiPartEpisodeKeepsBothPartsAcrossScans is the headline: `- part1` /
// `- part2` on one SxxExx are ONE Episode with ONE Edition holding both Files in
// part order — what docs/naming-convention.md has promised since it was written
// ("The same applies to a multi-part TV Episode") and what the playback layer's
// TotalDurationMs / PartAt / HLS escalation were already built for. part1 used to
// be deleted from the catalog on every scan, so none of that was reachable.
func TestScanMultiPartEpisodeKeepsBothPartsAcrossScans(t *testing.T) {
	const (
		part1 = "Season 01/The Bear (2022) - S01E01 - System - part1.mkv"
		part2 = "Season 01/The Bear (2022) - S01E01 - System - part2.mkv"
	)
	f := newRescanFixture(t, part1, part2)

	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System - part1.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E01 - System - part2.mkv part=2 present=1
`)
	f.assertHoldsFiles(part1, part2)
	f.assertScannedTwiceIsStable()
	// The point of the whole issue, stated once more after the SECOND scan.
	f.assertHoldsFiles(part1, part2)
}

// TestScanQualityEditionsKeepBothRipsAcrossScans is symptom 2: two rips of one
// Episode distinguished only by a quality token are ONE Episode with TWO
// Editions, auto-labeled by resolution (naming-convention.md
// "Quality-distinguished Edition", ADR-0035) — and line 89's "swapping a 1080p rip
// for a 4K one is a new Edition under the same Title" only means anything if the
// Title can hold two at once. One of them used to be deleted on every scan.
func TestScanQualityEditionsKeepBothRipsAcrossScans(t *testing.T) {
	const (
		hd  = "Season 01/The Bear (2022) - S01E01 - System - 1080p.mkv"
		uhd = "Season 01/The Bear (2022) - S01E01 - System - 2160p.mkv"
	)
	f := newRescanFixture(t, hd, uhd)

	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System - 1080p" sort="system - 1080p" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System - 1080p.mkv part=0 present=1
    edition "2160p"
      file Season 01/The Bear (2022) - S01E01 - System - 2160p.mkv part=0 present=1
`)
	f.assertHoldsFiles(hd, uhd)
	f.assertScannedTwiceIsStable()
	f.assertHoldsFiles(hd, uhd)
}

// TestScanCollidingEpisodeFilesAreAmbiguousAndBothKept is the collision rule
// reaching TV at last: "two files that parse to the same Edition identity and are
// not parts are flagged ambiguous in the web app, never silently guessed"
// (naming-convention.md). It has never fired for an Episode, because the two files
// never met — each built its own tree and the second overwrote the first.
//
// Both Files stay in the catalog. That is what the flag is FOR: an Admin cannot
// settle a collision they cannot see, and a File the scan deleted is invisible to
// the file matcher too (it never reaches unmatched_files either).
func TestScanCollidingEpisodeFilesAreAmbiguousAndBothKept(t *testing.T) {
	const (
		first  = "Season 01/The Bear (2022) - S01E01 - System (repack).mkv"
		second = "Season 01/The Bear (2022) - S01E01 - System.mkv"
	)
	f := newRescanFixture(t, first, second)

	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System (repack)" sort="system (repack)" label="" review=0 ambiguous=1 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System (repack).mkv part=0 present=1
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
`)
	f.assertHoldsFiles(first, second)
	f.assertScannedTwiceIsStable()
	f.assertHoldsFiles(first, second)
}

// TestScanRangeFileOverlappingAStandaloneKeepsBoth is the case the fix has to
// answer for. A range file claims E01 AND E02, so a standalone S01E02 beside it
// puts two Files on Slot E02 which are NOT parts of one another — the range file
// is a superset of the standalone, not its second half. Concatenating them would
// replay E02 after E01-E02.
//
// groupEditions gives them one Edition and flags the Episode ambiguous, and that
// is the right answer rather than a special case: the convention cannot tell which
// File is the Episode, so it refuses to guess and says so, exactly as it does for
// two colliding Movie files. What it must never do — and used to do — is delete
// one of them, which left the Admin with no way to see the conflict at all.
//
// E01, claimed by the range file alone, is untouched.
func TestScanRangeFileOverlappingAStandaloneKeepsBoth(t *testing.T) {
	const (
		rangeFile  = "Season 01/The Bear (2022) - S01E01-E02 - Alpha.mkv"
		standalone = "Season 01/The Bear (2022) - S01E02 - Zulu.mkv"
	)
	f := newRescanFixture(t, rangeFile, standalone)

	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="Alpha (S01E01)" sort="alpha (s01e01)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01-E02 - Alpha.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Alpha (S01E02)" sort="alpha (s01e02)" label="" review=0 ambiguous=1 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01-E02 - Alpha.mkv part=0 present=1
      file Season 01/The Bear (2022) - S01E02 - Zulu.mkv part=0 present=1
`)
	f.assertHoldsFiles(rangeFile, standalone)
	f.assertScannedTwiceIsStable()
	f.assertHoldsFiles(rangeFile, standalone)
}
