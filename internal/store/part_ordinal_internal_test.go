package store

import "testing"

// TestNumberedPartsTreatsANegativeOrdinalLikeZero: numberedParts (Parts' rule)
// reads `PartOrdinal <= 0` as unnumbered, not `== 0` — no writer this server ships
// ever produces a negative part_ordinal, but the two tests are NOT the same
// function: `== 0` would let a File carrying -1 pass as numbered, silently
// joining a File nothing actually numbered into a multi-part Edition. Pinned
// directly against numberedParts, since no reachable input tells the two apart
// through Parts/IsMultiPart alone.
func TestNumberedPartsTreatsANegativeOrdinalLikeZero(t *testing.T) {
	present := []File{
		{ID: "f1", PartOrdinal: 1},
		{ID: "f2", PartOrdinal: -1},
	}
	if numberedParts(present) {
		t.Error("a negative part_ordinal counted as numbered, want it treated like the unnumbered 0")
	}
}
