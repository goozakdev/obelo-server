package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The other side of multipart_test.go: an Edition holding more than one File that
// is NOT a multi-part work.
//
// The convention's collision rule says two files that parse to the same Edition
// identity and are not parts are "flagged ambiguous in the web app, never silently
// guessed" (naming-convention.md). The scanner does flag them — and then put them
// in one Edition, where "more than one present File" was the whole test for
// multi-part. So the HLS tiers concatenated them: a `S01E05-E06` range file
// followed by the standalone `S01E06` again, or two equal-quality rips playing the
// same episode twice, presented to the viewer as one continuous work with a
// double-length duration and a resume that lands in the wrong copy.
//
// The rule that separates the two is NUMBERING (store.Edition.Parts). These pin
// both directions of it, and — most importantly — the degradation for rows written
// before migration 0049 added part_ordinal with no backfill.

// ambiguousEdition is the range-vs-standalone collision: one Edition, two present
// Files, neither of them numbered.
func ambiguousEdition() store.Edition {
	return store.Edition{ID: "e1", Files: []store.File{
		partFile("f1", "/media/The Bear/Season 1/The Bear - S01E05-E06.mkv", 2_600_000),
		partFile("f2", "/media/The Bear/Season 1/The Bear - S01E06.mkv", 1_300_000),
	}}
}

// TestAmbiguousEditionPlaysOnlyItsFirstFile is the bug itself. Nothing about these
// two files says they are halves of one work; joining them plays episode 6 twice.
func TestAmbiguousEditionPlaysOnlyItsFirstFile(t *testing.T) {
	ed := ambiguousEdition()
	if ed.IsMultiPart() {
		t.Error("an ambiguous Edition reports as multi-part, so its Files get concatenated")
	}
	if got := len(ed.Parts()); got != 1 {
		t.Errorf("Parts() = %d files, want only the first", got)
	}
	if got := ed.TotalDurationMs(); got != 2_600_000 {
		t.Errorf("total = %d, want only the first File's 2600000 — summing an unrelated "+
			"second file puts the watched threshold past the end of what actually plays", got)
	}
	if path := concatListFor(ed, t.TempDir()); path != "" {
		t.Errorf("a concat list was written at %q, splicing two unrelated files into one timeline", path)
	}
	// PartAt has one part to map onto, so a resume can only ever land in the file
	// that actually plays.
	if f, off, ok := ed.PartAt(2_400_000); !ok || f.ID != "f1" || off != 2_400_000 {
		t.Errorf("PartAt(2400000) = %s@%d (ok %v), want f1@2400000", f.ID, off, ok)
	}
}

// TestAmbiguousEditionStillDirectPlays: the HLS escalation exists ONLY to join
// parts. With nothing to join, an ambiguous Edition is an ordinary single-File one
// and must not be pushed through ffmpeg for no reason.
func TestAmbiguousEditionStillDirectPlays(t *testing.T) {
	profile := DeviceProfile{
		Containers:       []string{"mp4", "mkv"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac"},
		MaxAudioChannels: 8,
	}
	dec, unsup := SelectEdition(profile, Constraints{}, []store.Edition{ambiguousEdition()}, "")
	if unsup != nil {
		t.Fatalf("ambiguous Edition was refused outright: %+v", unsup)
	}
	if dec.Tier != TierDirectPlay {
		t.Errorf("tier = %v, want direct play — there are no parts to repackage", dec.Tier)
	}
	if dec.File.ID != "f1" {
		t.Errorf("playing %s, want the first File", dec.File.ID)
	}
}

// TestUnbackfilledOrdinalsFallBackToTheFilenames is the regression this rule is
// most at risk of causing. Migration 0049 added part_ordinal with `DEFAULT 0` and
// no backfill, so on an install that has not rescanned since, EVERY File row sits
// at 0 — including both halves of a legitimate two-part movie. Reading only the
// column would truncate every one of them to part 1 and put the ~90% Watched
// threshold back at the end of that part, which is precisely the defect
// multipart_test.go exists to prevent.
//
// Before 0049 the only way to get a multi-part Edition was to NAME the files for
// it, so for exactly the rows the column cannot answer, the names still can.
func TestUnbackfilledOrdinalsFallBackToTheFilenames(t *testing.T) {
	ed := twoPartEdition() // part1/part2 names, part_ordinal 0 on both — a pre-0049 row
	for _, f := range ed.Files {
		if f.PartOrdinal != 0 {
			t.Fatalf("fixture %s carries ordinal %d; this test only means something at 0", f.ID, f.PartOrdinal)
		}
	}
	if !ed.IsMultiPart() {
		t.Fatal("a pre-0049 two-part Edition stopped being multi-part: every un-rescanned " +
			"multi-part movie and episode now plays only its first half")
	}
	if got := ed.TotalDurationMs(); got != 2_700_000 {
		t.Errorf("total = %d, want the summed 2700000", got)
	}
	if got := sessionDurationMs(Decision{Edition: ed, File: ed.Files[0]}); got != 2_700_000 {
		t.Errorf("session duration = %d, want 2700000 — the watched threshold moved back onto part 1", got)
	}
}

// TestStoredOrdinalsMakeAMultiPartEdition: the Placement case (ADR-0044). The
// Admin dropped two files on one Slot and said which plays first; neither filename
// mentions a part, and no amount of re-reading them could recover the decision —
// the ordinals are the only record of it.
func TestStoredOrdinalsMakeAMultiPartEdition(t *testing.T) {
	first := partFile("f1", "/media/The Bear/Season 1/Finale (b).mkv", 1_000_000)
	second := partFile("f2", "/media/The Bear/Season 1/Finale (a).mkv", 800_000)
	first.PartOrdinal, second.PartOrdinal = 1, 2
	ed := store.Edition{ID: "e1", Files: []store.File{first, second}}

	if !ed.IsMultiPart() {
		t.Fatal("a placed two-part Edition is not multi-part; the Admin's arrangement was discarded")
	}
	if got := ed.TotalDurationMs(); got != 1_800_000 {
		t.Errorf("total = %d, want 1800000", got)
	}
	if f, off, ok := ed.PartAt(1_200_000); !ok || f.ID != "f2" || off != 200_000 {
		t.Errorf("PartAt(1200000) = %s@%d (ok %v), want f2@200000", f.ID, off, ok)
	}
}

// TestSomeNumberedIsACollisionNotAPartSet: `Movie - part1.mkv` beside a bare
// `Movie.mkv` is not "one part plus a part 0" — it is two files claiming the same
// Edition, one of which happens to say "part1". Joining them would play the first
// half and then the whole film.
func TestSomeNumberedIsACollisionNotAPartSet(t *testing.T) {
	numbered := partFile("f1", "/media/Movie/Movie - part1.mkv", 1_500_000)
	numbered.PartOrdinal = 1
	bare := partFile("f2", "/media/Movie/Movie.mkv", 3_000_000)
	ed := store.Edition{ID: "e1", Files: []store.File{numbered, bare}}

	if ed.IsMultiPart() {
		t.Error("a numbered File beside an unnumbered one reported as a part set")
	}
	if got := ed.TotalDurationMs(); got != 1_500_000 {
		t.Errorf("total = %d, want only the first File's 1500000", got)
	}
}
