package catalog_test

import (
	"sort"
	"testing"
)

// The Episode half of the collision rule reaching the Admin.
//
// TestApplyParseCollisionLandsBothFilesOnOneAmbiguousSlot (placement_test.go) pins
// what the resolver does with two files claiming one Slot: keep both, flag the
// Episode, guess nothing. This pins the consequence the Admin actually sees — the
// flagged Episode is on the identity attention list the Needs-Fixing queue is built
// from, it names its Show (so the queue can fold it into that Show's one row and its
// action can be the matcher), and it names the two files that collide.
//
// Before this, the flag stopped at the database: nothing selected it, so an Admin's
// only route to the conflict was to open the matcher for a Show that never announced
// it had one.
func TestAmbiguousEpisodeReachesTheAttentionList(t *testing.T) {
	f := newFixture(t,
		seedEpisode(1, 1, "The Bear (2022) - S01E01 - System.mkv", 1000),
		seedEpisode(1, 2, "The Bear (2022) - S01E01 - System (repack).mkv", 1000),
	)
	system := episodePath(1, "The Bear (2022) - S01E01 - System.mkv")
	repack := episodePath(1, "The Bear (2022) - S01E01 - System (repack).mkv")

	// Take back the Placement parking the repack on S01E02, so both filenames claim
	// S01E01 — the collision.
	f.mustApply(placedOn(repack, 1, 2, 1))
	f.mustApply()

	items, err := f.svc.NeedsReview("libtv")
	if err != nil {
		t.Fatalf("NeedsReview: %v", err)
	}
	var found int
	for _, it := range items {
		if !it.Ambiguous {
			continue
		}
		found++
		if it.Kind != "episode" {
			t.Errorf("kind = %q, want episode", it.Kind)
		}
		// The Show is what lets the queue collapse this into one Show row whose
		// action is the matcher — the only screen that can settle the collision.
		if it.Context.ShowTitle != "The Bear" || it.Context.ShowID == "" {
			t.Errorf("item names no Show (%q / %q), so the queue cannot route it to the matcher",
				it.Context.ShowTitle, it.Context.ShowID)
		}
		paths := append([]string(nil), it.CollidingPaths...)
		sort.Strings(paths)
		if len(paths) != 2 || paths[0] != repack || paths[1] != system {
			t.Errorf("collidingPaths = %v, want both %q and %q — a row that cannot name "+
				"the conflicting files is not actionable", paths, repack, system)
		}
	}
	if found != 1 {
		t.Fatalf("ambiguous items = %d, want exactly the one collided Episode; list: %+v", found, items)
	}
}
