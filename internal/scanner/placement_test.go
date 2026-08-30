package scanner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Replay of the file-anchored Match override during TV resolve (ADR-0044,
// file-matcher/02). These are the Scanner half of the invariant: the Admin's
// Placement / Unassigned / Ignored decisions are an INPUT TO RESOLUTION, so a
// scheduled scan rebuilds the hand-sorted arrangement instead of undoing it.
//
// Everything here goes through a real walk of a real temp folder, because the
// point being pinned is precisely that the folders on disk stop deciding the
// shape of the tree.

// placedAt builds one 'placed' decision row: this File fills this Slot.
func placedAt(id, path string, season, episode, ordinal int) store.FileDecision {
	return store.FileDecision{
		ID: id, LibraryID: "lib1", Path: path, State: store.DecisionPlaced,
		GroupNumber: season, SlotNumber: episode, Ordinal: ordinal,
	}
}

// settledAt builds one settled ('unassigned' / 'ignored') decision row, which
// names no Slot.
func settledAt(id, path, state string) store.FileDecision {
	return store.FileDecision{ID: id, LibraryID: "lib1", Path: path, State: state}
}

// decisionMap collects rows into the per-path shape FileDecisionsByLibrary
// returns.
func decisionMap(rows ...store.FileDecision) map[string]store.FileDecisions {
	out := map[string]store.FileDecisions{}
	for _, r := range rows {
		out[r.Path] = append(out[r.Path], r)
	}
	return out
}

// scanTVWithDecisions walks one temp TV root with the given decisions in force.
func scanTVWithDecisions(t *testing.T, root string, decisions map[string]store.FileDecisions) *captureStore {
	t.Helper()
	cs := &captureStore{
		lib: store.Library{ID: "lib1", Kind: "tv",
			Roots: []store.LibraryRoot{{Path: root}}},
		decisions: decisions,
	}
	svc := NewService(cs, fakeProber{height: 1080})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return cs
}

// seasonOf returns the resolved SeasonTree for a season number, or nil.
func seasonOf(tree store.ShowTree, n int) *store.SeasonTree {
	for i := range tree.Seasons {
		if tree.Seasons[i].SeasonNumber == n {
			return &tree.Seasons[i]
		}
	}
	return nil
}

// episodePaths lists every File path an Episode owns, across its Editions.
func episodePaths(ep store.EpisodeTree) []string {
	var out []string
	for _, ed := range ep.Editions {
		for _, f := range ed.Files {
			out = append(out, f.Path)
		}
	}
	return out
}

// treeHoldsPath reports whether any Episode of the Show owns this path.
func treeHoldsPath(tree store.ShowTree, path string) bool {
	for _, st := range tree.Seasons {
		for _, ep := range st.Episodes {
			for _, p := range episodePaths(ep) {
				if p == path {
					return true
				}
			}
		}
	}
	return false
}

// TestPlacementRefilesRunOfEpisodesIntoAnotherSeason is the motivating case: the
// last five files in a `Season 3/` folder are season 4, and saying so must not
// require moving a single file on disk. Season 4 gets a row with no folder behind
// it; Season 3 shrinks to what is left.
func TestPlacementRefilesRunOfEpisodesIntoAnotherSeason(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "Batman The Animated Series (1992)")
	seasonDir := filepath.Join(show, "Season 03")

	var paths []string
	for e := 1; e <= 8; e++ {
		p := filepath.Join(seasonDir, "Batman The Animated Series (1992) - S03E0"+string(rune('0'+e))+" - Ep.mkv")
		writeFile(t, p)
		paths = append(paths, p)
	}

	// The last five are season 4, episodes 1..5.
	var rows []store.FileDecision
	for i, p := range paths[3:] {
		rows = append(rows, placedAt("d"+p, p, 4, i+1, 1))
	}

	cs := scanTVWithDecisions(t, root, decisionMap(rows...))
	if len(cs.showTrees) != 1 {
		t.Fatalf("show trees = %d, want 1", len(cs.showTrees))
	}
	tree := cs.showTrees[0]

	s3 := seasonOf(tree, 3)
	if s3 == nil || len(s3.Episodes) != 3 {
		t.Fatalf("season 3 = %v episodes, want the 3 files that were not placed", s3)
	}
	s4 := seasonOf(tree, 4)
	if s4 == nil {
		t.Fatal("season 4 has no row: a Season is the set of Episodes claiming its number, folder or no folder")
	}
	if len(s4.Episodes) != 5 {
		t.Fatalf("season 4 episodes = %d, want 5", len(s4.Episodes))
	}

	// The identity key is the SLOT's, not the parse's — this is what makes Apply's
	// in-place re-key and the Scanner's recomputation agree across a rescan.
	for i, ep := range s4.Episodes {
		wantKey := "|s04e0" + string(rune('1'+i))
		if !strings.HasSuffix(ep.Title.IdentityKey, wantKey) {
			t.Errorf("season 4 episode %d identity key = %q, want it to end %q (the assigned Slot, not the S03Exx filename)",
				i+1, ep.Title.IdentityKey, wantKey)
		}
		if ep.SeasonNumber != 4 || ep.EpisodeNumber != i+1 {
			t.Errorf("episode ordering = s%02de%02d, want s04e%02d", ep.SeasonNumber, ep.EpisodeNumber, i+1)
		}
		// The files never moved: each placed Episode still owns the path in the
		// Season 3 folder.
		got := episodePaths(ep)
		if len(got) != 1 || filepath.Dir(got[0]) != seasonDir {
			t.Errorf("episode %d files = %v, want the one file still under %q", i+1, got, seasonDir)
		}
	}
}

// TestPlacementMergesTwoFilesOntoOneSlot: two Files on one Slot are ONE Episode
// with a two-File Edition, in ordinal order and multi-part.
func TestPlacementMergesTwoFilesOntoOneSlot(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	seasonDir := filepath.Join(show, "Season 01")
	// Deliberately named so that path order is the REVERSE of the Admin's part
	// order, and so that a filename-derived Edition name would split them into two
	// Editions of one part each.
	second := filepath.Join(seasonDir, "The Bear (2022) - a-half - 720p.mkv")
	first := filepath.Join(seasonDir, "The Bear (2022) - z-half - 1080p.mkv")
	writeFile(t, first)
	writeFile(t, second)

	cs := scanTVWithDecisions(t, root, decisionMap(
		placedAt("d1", first, 1, 5, 1),
		placedAt("d2", second, 1, 5, 2),
	))
	tree := cs.showTrees[0]
	s1 := seasonOf(tree, 1)
	if s1 == nil || len(s1.Episodes) != 1 {
		t.Fatalf("season 1 = %v, want exactly ONE Episode (the two files merged onto one Slot)", s1)
	}
	ep := s1.Episodes[0]
	if ep.EpisodeNumber != 5 {
		t.Errorf("episode number = %d, want 5", ep.EpisodeNumber)
	}
	if len(ep.Editions) != 1 {
		t.Fatalf("editions = %d, want 1: the Admin said these are one episode, so their filenames' quality tokens must not split them", len(ep.Editions))
	}
	ed := ep.Editions[0]
	if len(ed.Files) != 2 {
		t.Fatalf("files in the Edition = %d, want 2", len(ed.Files))
	}
	if !ed.IsMultiPart() {
		t.Error("Edition is not multi-part; the joint timeline (TotalDurationMs / PartAt) depends on it")
	}
	if ed.Files[0].Path != first || ed.Files[1].Path != second {
		t.Errorf("part order = %v, want the ordinal order (%q then %q), not the filename order",
			[]string{ed.Files[0].Path, ed.Files[1].Path}, first, second)
	}
	if ed.Files[0].PartOrdinal != 1 || ed.Files[1].PartOrdinal != 2 {
		t.Errorf("part ordinals = %d,%d, want 1,2 stored so the order survives the round trip through the database",
			ed.Files[0].PartOrdinal, ed.Files[1].PartOrdinal)
	}
}

// TestPlacementSplitsOneFileOverTwoSlots: one File on two Slots resolves to two
// Episodes sharing the path — the co-File sibling shape a range file (S01E05-E06)
// already produces, and the shape store/multiepisode_test.go pins on the write
// side.
func TestPlacementSplitsOneFileOverTwoSlots(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	combined := filepath.Join(show, "Season 01", "The Bear (2022) - double length finale.mkv")
	writeFile(t, combined)

	cs := scanTVWithDecisions(t, root, decisionMap(
		placedAt("d1", combined, 1, 1, 1),
		placedAt("d2", combined, 1, 2, 1),
	))
	tree := cs.showTrees[0]
	s1 := seasonOf(tree, 1)
	if s1 == nil || len(s1.Episodes) != 2 {
		t.Fatalf("season 1 = %v, want TWO Episodes from the one file", s1)
	}
	for _, ep := range s1.Episodes {
		got := episodePaths(ep)
		if len(got) != 1 || got[0] != combined {
			t.Errorf("episode %d files = %v, want the one shared path", ep.EpisodeNumber, got)
		}
	}
	a, b := s1.Episodes[0], s1.Episodes[1]
	if a.Title.IdentityKey == b.Title.IdentityKey {
		t.Fatalf("both Slots resolved to identity key %q; a split needs two distinct keys", a.Title.IdentityKey)
	}
	if !strings.HasSuffix(a.Title.IdentityKey, "|s01e01") || !strings.HasSuffix(b.Title.IdentityKey, "|s01e02") {
		t.Errorf("identity keys = %q / %q, want them to end |s01e01 and |s01e02", a.Title.IdentityKey, b.Title.IdentityKey)
	}
	// Both Titles must be distinguishable in browse, as the range path already
	// ensures for its two Titles.
	if a.Title.Title == b.Title.Title {
		t.Errorf("both Titles are named %q; the two halves of a split must be tellable apart", a.Title.Title)
	}
}

// TestIgnoredFileContributesNothing: an Ignored path produces no Episode, no
// Unmatched row and no Extra — even though its filename parses perfectly well.
func TestIgnoredFileContributesNothing(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	seasonDir := filepath.Join(show, "Season 01")
	keep := filepath.Join(seasonDir, "The Bear (2022) - S01E01 - System.mkv")
	stray := filepath.Join(seasonDir, "The Bear (2022) - S01E02 - Hands.mkv")
	writeFile(t, keep)
	writeFile(t, stray)

	cs := scanTVWithDecisions(t, root, decisionMap(settledAt("d1", stray, store.DecisionIgnored)))
	tree := cs.showTrees[0]
	if treeHoldsPath(tree, stray) {
		t.Error("an Ignored file is still in the tree; it must contribute no Episode at all")
	}
	if !treeHoldsPath(tree, keep) {
		t.Error("ignoring one file dropped its sibling too")
	}
	for _, u := range cs.unmatched {
		if u.Path == stray {
			t.Error("an Ignored file became an Unmatched row: a settled decision is not work")
		}
	}
	for _, st := range tree.Seasons {
		for _, ep := range st.Episodes {
			if len(ep.Extras) != 0 {
				t.Errorf("episode %d gained %d Extras; an Ignored file must not become one", ep.EpisodeNumber, len(ep.Extras))
			}
		}
	}
}

// TestUnassignedFileProducesNoEpisodeAndNoUnmatchedRow: the state the issue
// predates. An Unassigned File is a RECORDED DECISION, not a parse failure, so it
// leaves the tree entirely and must NOT be re-listed as Unmatched — the matcher
// finds it through file_decisions, and an Unmatched row would double-count it as
// work. The unrelated unparseable file in the same folder proves the Unmatched
// path itself is untouched.
func TestUnassignedFileProducesNoEpisodeAndNoUnmatchedRow(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	seasonDir := filepath.Join(show, "Season 01")
	keep := filepath.Join(seasonDir, "The Bear (2022) - S01E01 - System.mkv")
	pulled := filepath.Join(seasonDir, "The Bear (2022) - S01E02 - Hands.mkv")
	nameless := filepath.Join(seasonDir, "some random rip.mkv")
	writeFile(t, keep)
	writeFile(t, pulled)
	writeFile(t, nameless)

	cs := scanTVWithDecisions(t, root, decisionMap(settledAt("d1", pulled, store.DecisionUnassigned)))
	tree := cs.showTrees[0]
	if treeHoldsPath(tree, pulled) {
		t.Error("an Unassigned file still resolved to an Episode; the whole point of the state is that it comes off its Slot")
	}
	if !treeHoldsPath(tree, keep) {
		t.Error("unassigning one file dropped its sibling too")
	}
	var unmatchedPaths []string
	for _, u := range cs.unmatched {
		unmatchedPaths = append(unmatchedPaths, u.Path)
		if u.Path == pulled {
			t.Error("an Unassigned file became an Unmatched row; that double-counts one file as two problems")
		}
	}
	if len(unmatchedPaths) != 1 || unmatchedPaths[0] != nameless {
		t.Errorf("unmatched = %v, want only the genuinely unparseable file", unmatchedPaths)
	}
}

// TestPlacementRescuesFileTheParseCouldNotNumber: a file with no episode token at
// all is resolvable purely by Placement, so it becomes an Episode rather than an
// Unmatched row.
func TestPlacementRescuesFileTheParseCouldNotNumber(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	blob := filepath.Join(show, "Season 02", "untitled recording.mkv")
	writeFile(t, blob)

	cs := scanTVWithDecisions(t, root, decisionMap(placedAt("d1", blob, 2, 3, 1)))
	tree := cs.showTrees[0]
	s2 := seasonOf(tree, 2)
	if s2 == nil || len(s2.Episodes) != 1 {
		t.Fatalf("season 2 = %v, want one Episode built purely from the Placement", s2)
	}
	if s2.Episodes[0].EpisodeNumber != 3 {
		t.Errorf("episode number = %d, want 3", s2.Episodes[0].EpisodeNumber)
	}
	if s2.Episodes[0].Title.NeedsReview {
		t.Error("a placed Episode is flagged needs-review; the Admin already decided where it goes")
	}
	for _, u := range cs.unmatched {
		if u.Path == blob {
			t.Error("a file the Admin placed is still listed as Unmatched")
		}
	}
}

// TestSeasonEmptiedByPlacementIsNotCreated: reassigning every file out of a
// Season leaves it with no Episodes, and a Season with no Episodes gets no row —
// even though its folder is still sitting on disk.
func TestSeasonEmptiedByPlacementIsNotCreated(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	seasonDir := filepath.Join(show, "Season 03")
	a := filepath.Join(seasonDir, "The Bear (2022) - S03E01 - One.mkv")
	b := filepath.Join(seasonDir, "The Bear (2022) - S03E02 - Two.mkv")
	writeFile(t, a)
	writeFile(t, b)

	cs := scanTVWithDecisions(t, root, decisionMap(
		placedAt("d1", a, 4, 1, 1),
		placedAt("d2", b, 4, 2, 1),
	))
	tree := cs.showTrees[0]
	if s3 := seasonOf(tree, 3); s3 != nil {
		t.Errorf("season 3 still has a row with %d episodes; a Season emptied by reassignment must disappear", len(s3.Episodes))
	}
	if s4 := seasonOf(tree, 4); s4 == nil || len(s4.Episodes) != 2 {
		t.Fatalf("season 4 = %v, want both Episodes", s4)
	}
}

// TestPlacementWhoseFileIsGoneIsOrphanedNotDropped: a correction pointing at
// nothing is broken rather than done, so the scan flags it (surfaced in the
// Needs-Fixing queue) exactly as it already does for a folder anchor.
func TestPlacementWhoseFileIsGoneIsOrphanedNotDropped(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	present := filepath.Join(show, "Season 01", "The Bear (2022) - S01E01 - System.mkv")
	writeFile(t, present)
	renamed := filepath.Join(show, "Season 01", "The Bear (2022) - S01E02 - Hands.mkv") // never written

	cs := scanTVWithDecisions(t, root, decisionMap(
		placedAt("keep", present, 1, 1, 1),
		placedAt("gone", renamed, 1, 2, 1),
	))
	if got, ok := cs.orphanedPlacements["gone"]; !ok || !got {
		t.Errorf("orphan flags = %v, want the Placement whose file is absent flagged orphaned", cs.orphanedPlacements)
	}
	if flagged, ok := cs.orphanedPlacements["keep"]; ok && flagged {
		t.Error("a Placement whose file is on disk was flagged orphaned")
	}
}

// TestTargetedScanReplaysPlacement: the Targeted scan runs the same resolvers, so
// it must load the same decisions. Without this a targeted scan — which is
// exactly what runs right after Apply — would rebuild the Show from its filenames
// and silently undo the sorting.
func TestTargetedScanReplaysPlacement(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Bear (2022)")
	f := filepath.Join(show, "Season 03", "The Bear (2022) - S03E08 - Braciole.mkv")
	writeFile(t, f)

	cs := &captureStore{
		lib: store.Library{ID: "lib1", Kind: "tv",
			Roots: []store.LibraryRoot{{Path: root}}},
		decisions: decisionMap(placedAt("d1", f, 4, 1, 1)),
	}
	svc := NewService(cs, fakeProber{height: 1080})
	if _, err := svc.TargetedScan(context.Background(), "lib1", TargetedScope{
		Folders: []string{show}, Label: "The Bear",
	}); err != nil {
		t.Fatalf("targeted scan: %v", err)
	}
	if len(cs.showTrees) != 1 {
		t.Fatalf("show trees = %d, want 1", len(cs.showTrees))
	}
	tree := cs.showTrees[0]
	if seasonOf(tree, 3) != nil {
		t.Error("targeted scan rebuilt season 3 from the filename, undoing the Placement")
	}
	s4 := seasonOf(tree, 4)
	if s4 == nil || len(s4.Episodes) != 1 {
		t.Fatalf("season 4 = %v, want the placed Episode", s4)
	}
	if !strings.HasSuffix(s4.Episodes[0].Title.IdentityKey, "|s04e01") {
		t.Errorf("identity key = %q, want it to end |s04e01", s4.Episodes[0].Title.IdentityKey)
	}
}
