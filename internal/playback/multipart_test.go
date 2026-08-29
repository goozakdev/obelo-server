package playback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// Multi-part Editions: several files that are ONE work (`- part1`/`- cd1`,
// naming-convention.md), which the playback tiers previously truncated.
//
// The defect these guard against was silent and had two halves. The HLS tiers fed
// ffmpeg only the FIRST File, so the second half of a two-part episode was never
// delivered — it simply ended early, indistinguishable from a corrupt file. And the
// session measured the Watched threshold against that one part, so finishing part 1
// of two crossed the ~90% ceiling and marked the WHOLE work watched with its resume
// cleared, losing the viewer's place and moving the Up Next anchor (ADR-0028).

func partFile(id, path string, durationMs int64) store.File {
	return store.File{
		ID: id, Path: path, DurationMs: durationMs, Present: true,
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080,
	}
}

func twoPartEdition() store.Edition {
	return store.Edition{ID: "e1", Files: []store.File{
		partFile("f1", "/media/Movie/Movie - part1.mkv", 1_500_000),
		partFile("f2", "/media/Movie/Movie - part2.mkv", 1_200_000),
	}}
}

// TestTotalDurationSpansEveryPart: the whole-work duration is the sum, which is what
// the Watched threshold has to be measured against.
func TestTotalDurationSpansEveryPart(t *testing.T) {
	ed := twoPartEdition()
	if got := ed.TotalDurationMs(); got != 2_700_000 {
		t.Errorf("total = %d, want 2700000 (the sum of both parts)", got)
	}
	if !ed.IsMultiPart() {
		t.Error("a two-part Edition does not report as multi-part")
	}
}

// TestMissingPartIsNotAPart: a File absent from disk cannot be streamed (ADR-0008),
// so it neither counts toward the duration nor makes an Edition multi-part.
func TestMissingPartIsNotAPart(t *testing.T) {
	ed := twoPartEdition()
	ed.Files[1].Present = false
	if ed.IsMultiPart() {
		t.Error("one present File plus one Missing reported as multi-part")
	}
	if got := ed.TotalDurationMs(); got != 1_500_000 {
		t.Errorf("total = %d, want only the present part's 1500000", got)
	}
}

// TestSessionMeasuresTheWholeWork is the watch-state half of the bug: a session over
// a two-part Edition must carry the whole duration, or finishing part 1 marks the
// entire work watched at its halfway point.
func TestSessionMeasuresTheWholeWork(t *testing.T) {
	ed := twoPartEdition()
	got := sessionDurationMs(Decision{Edition: ed, File: ed.Files[0]})
	if got != 2_700_000 {
		t.Fatalf("session duration = %d, want the whole Edition's 2700000; "+
			"measuring one part puts the ~90%% watched ceiling at the end of part 1", got)
	}
	// The precise consequence, spelled out: the end of part 1 must be nowhere near
	// the watched ceiling of the whole work.
	if frac := float64(ed.Files[0].DurationMs) / float64(got); frac >= WatchedCeiling {
		t.Errorf("finishing part 1 reaches %.2f of the work, at or past the %.2f ceiling", frac, WatchedCeiling)
	}
}

// TestSessionDurationFallsBackToTheFile: a Decision whose Edition was not populated
// with its Files must not lose its duration — that would silently disable the
// threshold and stop recording resume positions altogether.
func TestSessionDurationFallsBackToTheFile(t *testing.T) {
	got := sessionDurationMs(Decision{
		Edition: store.Edition{ID: "e1"}, // no Files
		File:    store.File{ID: "f1", DurationMs: 10_000, Present: true},
	})
	if got != 10_000 {
		t.Errorf("duration = %d, want the File's 10000", got)
	}
}

// TestPartAtMapsAResumeOntoItsPart: a stored resume is a WHOLE-work position, so it
// has to map back to "this part, at this offset" to be usable.
func TestPartAtMapsAResumeOntoItsPart(t *testing.T) {
	ed := twoPartEdition()
	cases := []struct {
		name     string
		position int64
		wantFile string
		wantOff  int64
	}{
		{"start", 0, "f1", 0},
		{"inside part 1", 600_000, "f1", 600_000},
		{"exactly the boundary lands in part 2", 1_500_000, "f2", 0},
		{"inside part 2", 1_800_000, "f2", 300_000},
		{"past the end clamps to the last part", 9_000_000, "f2", 1_200_000},
	}
	for _, c := range cases {
		f, off, ok := ed.PartAt(c.position)
		if !ok || f.ID != c.wantFile || off != c.wantOff {
			t.Errorf("%s: PartAt(%d) = %s@%d (ok %v), want %s@%d",
				c.name, c.position, f.ID, off, ok, c.wantFile, c.wantOff)
		}
	}
}

// TestPartStartMsIsTheRunningOffset: the offset added to a position reported within
// a part to get the whole-work position — the inverse of PartAt.
func TestPartStartMsIsTheRunningOffset(t *testing.T) {
	ed := twoPartEdition()
	if got := ed.PartStartMs(0); got != 0 {
		t.Errorf("part 1 starts at %d, want 0", got)
	}
	if got := ed.PartStartMs(1); got != 1_500_000 {
		t.Errorf("part 2 starts at %d, want 1500000", got)
	}
}

// TestMultiPartNeverDirectPlays is the delivery half of the bug. Direct play hands
// the client ONE File's bytes over one response, so it can only ever deliver part 1
// — silently, with the work just stopping halfway. A multi-part Edition must
// escalate to an HLS tier, which repackages every part into one stream.
func TestMultiPartNeverDirectPlays(t *testing.T) {
	ed := twoPartEdition()
	// A profile that would happily direct-play this File on its own.
	profile := DeviceProfile{
		Containers:       []string{"mp4", "mkv"},
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac"},
		MaxAudioChannels: 8,
	}
	dec, unsup := SelectEdition(profile, Constraints{}, []store.Edition{ed}, "")
	if unsup != nil {
		t.Fatalf("multi-part Edition was refused outright: %+v", unsup)
	}
	if dec.Tier == TierDirectPlay {
		t.Fatal("a multi-part Edition direct-played: that delivers ONLY part 1, " +
			"which is the silent truncation this guards against")
	}

	// The single-part control: the same File alone still direct-plays, so the
	// escalation is scoped to genuinely multi-part Editions.
	single := store.Edition{ID: "e2", Files: []store.File{partFile("f1", "/media/Movie/Movie.mkv", 1_500_000)}}
	sdec, sUnsup := SelectEdition(profile, Constraints{}, []store.Edition{single}, "")
	if sUnsup != nil || sdec.Tier != TierDirectPlay {
		t.Errorf("single-File Edition = tier %v (%+v), want direct play — the escalation leaked", sdec.Tier, sUnsup)
	}
}

// TestConcatListNamesEveryPartInOrder: the list ffmpeg reads must carry every part,
// in play order, or the stream is still truncated (or worse, out of order).
func TestConcatListNamesEveryPartInOrder(t *testing.T) {
	dir := t.TempDir()
	path := concatListFor(twoPartEdition(), dir)
	if path == "" {
		t.Fatal("no concat list written for a two-part Edition")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading concat list: %v", err)
	}
	want := "file '/media/Movie/Movie - part1.mkv'\nfile '/media/Movie/Movie - part2.mkv'\n"
	if string(body) != want {
		t.Errorf("concat list =\n%q\nwant\n%q", body, want)
	}
	if filepath.Base(path) != concatListFileName {
		t.Errorf("list written to %q, want it beside the segments in the scratch dir", path)
	}
}

// TestConcatListSkippedForOneFile: the ordinary Edition must produce no list, so its
// ffmpeg args stay byte-for-byte what they were.
func TestConcatListSkippedForOneFile(t *testing.T) {
	single := store.Edition{ID: "e1", Files: []store.File{partFile("f1", "/m/a.mkv", 1000)}}
	if path := concatListFor(single, t.TempDir()); path != "" {
		t.Errorf("a single-File Edition wrote a concat list at %q", path)
	}
}

// TestConcatListEscapesQuotes: the concat demuxer's `file '...'` syntax breaks on a
// literal quote in a path, and film titles contain apostrophes constantly
// ("Ocean's Eleven"). An unescaped one would make ffmpeg read a garbage path.
func TestConcatListEscapesQuotes(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{
		partFile("f1", "/media/Ocean's Eleven/cd1.mkv", 1000),
		partFile("f2", "/media/Ocean's Eleven/cd2.mkv", 1000),
	}}
	body, err := os.ReadFile(concatListFor(ed, t.TempDir()))
	if err != nil {
		t.Fatalf("reading concat list: %v", err)
	}
	if !strings.Contains(string(body), `Ocean'\''s Eleven`) {
		t.Errorf("apostrophe not escaped for the concat demuxer:\n%s", body)
	}
}

// TestRemuxArgsUseTheConcatInput: the args must actually name the list, with -safe 0
// (the paths are absolute), or ffmpeg reads only the single source path.
func TestRemuxArgsUseTheConcatInput(t *testing.T) {
	args := transcode.RemuxArgs(transcode.RemuxJob{
		SourcePath:     "/media/Movie/part1.mkv",
		ConcatListPath: "/scratch/s1/parts.concat",
		OutputDir:      "/scratch/s1",
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-f concat -safe 0 -i /scratch/s1/parts.concat") {
		t.Errorf("remux args do not read the concat list:\n%s", joined)
	}
	if strings.Contains(joined, "-i /media/Movie/part1.mkv") {
		t.Error("remux args still read the single part as the input")
	}

	// The ordinary job is untouched.
	plain := strings.Join(transcode.RemuxArgs(transcode.RemuxJob{
		SourcePath: "/media/Movie/Movie.mkv", OutputDir: "/scratch/s1",
	}), " ")
	if !strings.Contains(plain, "-i /media/Movie/Movie.mkv") || strings.Contains(plain, "concat") {
		t.Errorf("single-File remux args changed:\n%s", plain)
	}
}

// TestTranscodeArgsUseTheConcatInput: the transcode tier must span the parts too, or
// a work that needs re-encoding is still truncated.
func TestTranscodeArgsUseTheConcatInput(t *testing.T) {
	joined := strings.Join(transcode.TranscodeArgs(transcode.TranscodeJob{
		SourcePath:     "/media/Movie/part1.mkv",
		ConcatListPath: "/scratch/s1/parts.concat",
		OutputDir:      "/scratch/s1",
		Video:          transcode.VideoPlan{Copy: true},
	}), " ")
	if !strings.Contains(joined, "-f concat -safe 0 -i /scratch/s1/parts.concat") {
		t.Errorf("transcode args do not read the concat list:\n%s", joined)
	}
}

// TestJoinPartBoundariesHasNoZeroLengthSeam is the seam bug, caught on a real
// two-part stream before it shipped: each part's cut points run 0..itsDuration, so
// naively offsetting every one of them repeats the join instant — the previous
// part's closing boundary and the next part's opening 0. A repeated cut point is a
// ZERO-LENGTH segment in the synthesized playlist, which players read as a broken
// stream. The observed playlist was `4.022, 0.000, 4.022`.
func TestJoinPartBoundariesHasNoZeroLengthSeam(t *testing.T) {
	// Two 4.022s parts, each probed as [start, end].
	got := joinPartBoundaries([][]float64{{0, 4.022}, {0, 4.022}}, []float64{4.022, 4.022})
	want := []float64{0, 4.022, 8.044}
	if len(got) != len(want) {
		t.Fatalf("boundaries = %v, want %v", got, want)
	}
	for i := range want {
		if diff := got[i] - want[i]; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("boundaries = %v, want %v", got, want)
		}
	}
	// Stated as the property that actually matters: no two cuts coincide.
	for i := 1; i < len(got); i++ {
		if got[i]-got[i-1] < 0.01 {
			t.Errorf("zero-length segment between cut %d and %d in %v", i-1, i, got)
		}
	}
}

// TestJoinPartBoundariesSpansEveryPart: three parts lay end to end on one timeline.
func TestJoinPartBoundariesSpansEveryPart(t *testing.T) {
	got := joinPartBoundaries(
		[][]float64{{0, 10}, {0, 5, 10}, {0, 10}},
		[]float64{10, 10, 10},
	)
	want := []float64{0, 10, 15, 20, 30}
	if len(got) != len(want) {
		t.Fatalf("boundaries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("boundaries = %v, want %v", got, want)
		}
	}
}
