package scanner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The TV mirror of scanner_test.go's Movie Edition trio
// (TestScannerMultiPartNotAmbiguous / TestScannerTwoQualityEditions /
// TestScannerAmbiguousCollision). Its absence is why tv-episode-editions/01 went
// unnoticed for so long: the parsed TV branch emitted one Episode tree PER FILE,
// so two files on one SxxExx produced two trees with the same identity_key and
// writeTitleSubtree's `DELETE FROM editions WHERE title_id` made the second
// destroy the first's Edition and Files on every single scan.
//
// docs/naming-convention.md is the specification these assert against: a Movie
// folder and a Season folder sort their Files into Editions by ONE set of rules
// ("Editions, parts, and extras" — and "The same applies to a multi-part TV
// Episode").

// scanTVShowTree scans one temp TV root with the fixed-height fake prober and
// returns the single resolved ShowTree.
func scanTVShowTree(t *testing.T, root string, height int) store.ShowTree {
	t.Helper()
	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, fakeProber{height: height})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cs.showTrees) != 1 {
		t.Fatalf("resolved %d show trees, want 1", len(cs.showTrees))
	}
	return cs.showTrees[0]
}

// episodeAt returns the resolved Episode at a Slot, failing when it is absent or
// when more than one Title claims the Slot (which is the bug: two trees, one key).
func episodeAt(t *testing.T, tree store.ShowTree, season, episode int) store.EpisodeTree {
	t.Helper()
	var found []store.EpisodeTree
	for _, st := range tree.Seasons {
		for _, ep := range st.Episodes {
			if ep.SeasonNumber == season && ep.EpisodeNumber == episode {
				found = append(found, ep)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("S%02dE%02d resolved to %d Episode trees, want exactly 1", season, episode, len(found))
	}
	return found[0]
}

// TestEpisodeMultiPartIsOneEdition is the headline of tv-episode-editions/01 and
// the TV mirror of TestScannerMultiPartNotAmbiguous: `- part1` / `- part2` on one
// SxxExx are ONE Episode with ONE Edition holding both Files in part order, which
// is what docs/naming-convention.md line 27 has always promised ("The same applies
// to a multi-part TV Episode") and what the playback layer's Edition.PartAt /
// TotalDurationMs / HLS escalation were built for. Before the fix part1 never
// reached the catalog at all.
func TestEpisodeMultiPartIsOneEdition(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The Wire (2002)", "Season 01")
	part1 := filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager - part1.mkv")
	part2 := filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager - part2.mkv")
	writeFile(t, part1)
	writeFile(t, part2)

	ep := episodeAt(t, scanTVShowTree(t, root, 1080), 1, 5)
	if ep.Ambiguous {
		t.Errorf("a numbered multi-part Episode must not be ambiguous")
	}
	if len(ep.Editions) != 1 {
		t.Fatalf("editions = %d, want 1 (the parts join)", len(ep.Editions))
	}
	files := ep.Editions[0].Files
	if len(files) != 2 {
		t.Fatalf("files = %d, want both parts", len(files))
	}
	if files[0].Path != part1 || files[0].PartOrdinal != 1 {
		t.Errorf("first file = %q part=%d, want part1", files[0].Path, files[0].PartOrdinal)
	}
	if files[1].Path != part2 || files[1].PartOrdinal != 2 {
		t.Errorf("second file = %q part=%d, want part2", files[1].Path, files[1].PartOrdinal)
	}
	// The part suffix names the Edition's part, never the Episode: an Episode called
	// "The Pager - part1" would be named after half of itself.
	if ep.Title.Title != "The Pager" {
		t.Errorf("episode name = %q, want %q", ep.Title.Title, "The Pager")
	}
}

// TestEpisodeTwoQualityEditions is symptom 2 and the TV mirror of
// TestScannerTwoQualityEditions: distinct quality tokens on one SxxExx are ONE
// Episode with TWO Editions, auto-labeled by resolution (naming-convention.md
// "Quality-distinguished Edition", ADR-0035). Before the fix one rip was deleted
// from the catalog on every scan.
func TestEpisodeTwoQualityEditions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The Wire (2002)", "Season 01")
	writeFile(t, filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager - 1080p.mkv"))
	writeFile(t, filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager - 720p.mkv"))

	ep := episodeAt(t, scanTVShowTree(t, root, 1080), 1, 5)
	if ep.Ambiguous {
		t.Errorf("distinct quality Editions must not be ambiguous")
	}
	if len(ep.Editions) != 2 {
		t.Fatalf("editions = %d, want 2", len(ep.Editions))
	}
	names := map[string]int{}
	for _, ed := range ep.Editions {
		if len(ed.Files) != 1 {
			t.Errorf("edition %q holds %d files, want 1", ed.Name, len(ed.Files))
		}
		names[ed.Name] = len(ed.Files)
	}
	// The resolution-derived labels a Movie's Editions would carry.
	for _, want := range []string{"1080p", "720p"} {
		if _, ok := names[want]; !ok {
			t.Errorf("no Edition labeled %q; got %v", want, names)
		}
	}
}

// TestEpisodeCollisionIsAmbiguous is the collision rule finally reaching TV: two
// files that parse to the same Edition identity and are NOT parts are flagged
// ambiguous, "never silently guessed" (naming-convention.md line 28). It never
// fired for an Episode before, because the two files never met.
func TestEpisodeCollisionIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The Wire (2002)", "Season 01")
	a := filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager.mkv")
	b := filepath.Join(dir, "The Wire (2002) - S01E05 - The Pager (repack).mkv")
	writeFile(t, a)
	writeFile(t, b)

	ep := episodeAt(t, scanTVShowTree(t, root, 1080), 1, 5)
	if !ep.Ambiguous {
		t.Errorf("two colliding files on one Slot must flag the Episode ambiguous")
	}
	if len(ep.Editions) != 1 || len(ep.Editions[0].Files) != 2 {
		t.Fatalf("want 1 Edition holding both files, got %d editions", len(ep.Editions))
	}
	// Neither File is destroyed. That is the data-loss half of the bug: whichever
	// file the walk reached last used to delete the other's row outright.
	got := map[string]bool{}
	for _, f := range ep.Editions[0].Files {
		got[f.Path] = true
	}
	if !got[a] || !got[b] {
		t.Errorf("both files must survive; got %v", got)
	}
}

// TestEpisodeRangeOverlappingStandaloneIsAmbiguous pins the one case the fix has
// to answer for. A range file claims E05 AND E06, so a standalone S01E06 beside it
// puts two Files on Slot E06 that are not parts of one another — the range file is
// a SUPERSET of the standalone, not its second half. groupEditions gives them one
// Edition and flags the Episode ambiguous, which is the honest answer: the
// convention cannot tell which is the Episode, so it refuses to guess and says so.
// What it must NOT do (and used to) is delete one of them.
//
// E05, claimed by the range file alone, is unaffected.
func TestEpisodeRangeOverlappingStandaloneIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "The Wire (2002)", "Season 01")
	rangeFile := filepath.Join(dir, "The Wire (2002) - S01E05-E06 - Alpha.mkv")
	standalone := filepath.Join(dir, "The Wire (2002) - S01E06 - Zulu.mkv")
	writeFile(t, rangeFile)
	writeFile(t, standalone)

	tree := scanTVShowTree(t, root, 1080)

	five := episodeAt(t, tree, 1, 5)
	if five.Ambiguous {
		t.Errorf("E05 is claimed by one file only; it must not be ambiguous")
	}
	if len(five.Editions) != 1 || len(five.Editions[0].Files) != 1 ||
		five.Editions[0].Files[0].Path != rangeFile {
		t.Errorf("E05 should hold only the range file, got %+v", episodePaths(five))
	}

	six := episodeAt(t, tree, 1, 6)
	if !six.Ambiguous {
		t.Errorf("E06 is claimed by two files that are not parts; it must be ambiguous")
	}
	got := map[string]bool{}
	for _, p := range episodePaths(six) {
		got[p] = true
	}
	if !got[rangeFile] || !got[standalone] {
		t.Errorf("E06 must keep BOTH claimants (neither is destroyed); got %v", got)
	}
}
