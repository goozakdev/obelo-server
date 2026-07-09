package playback

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/transcode"
)

// --- pure helpers ---

func TestSegmentCountFor(t *testing.T) {
	tests := []struct {
		name       string
		durationMs int64
		seg        int
		want       int
	}{
		{"exact multiple", 12_000, 4, 3},
		{"rounds up partial last segment", 10_000, 4, 3}, // 4 + 4 + 2
		{"sub-segment duration still one segment", 1_200, 4, 1},
		{"unknown duration → 0", 0, 4, 0},
		{"negative duration → 0", -5, 4, 0},
		{"zero seg length → 0", 12_000, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentCountFor(tc.durationMs, tc.seg); got != tc.want {
				t.Errorf("segmentCountFor(%d,%d) = %d, want %d", tc.durationMs, tc.seg, got, tc.want)
			}
		})
	}
}

func TestParseSegmentIndex(t *testing.T) {
	tests := []struct {
		name   string
		want   int
		wantOK bool
	}{
		{"segment000.ts", 0, true},
		{"segment007.ts", 7, true},
		{"segment123.ts", 123, true},
		{"index.m3u8", 0, false},
		{"segment.ts", 0, false},
		{"segmentABC.ts", 0, false},
		{"../escape.ts", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSegmentIndex(tc.name)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("parseSegmentIndex(%q) = (%d,%v), want (%d,%v)", tc.name, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSynthPlaylistIsStableAndComplete: the server-owned playlist lists exactly
// segmentCount segments by name, is a well-formed VOD playlist (ENDLIST), and is
// identical regardless of where ffmpeg is currently positioned — the invariant
// that keeps it coherent across realignment.
func TestSynthPlaylistIsStableAndComplete(t *testing.T) {
	rt := &hlsRuntime{ownsPlaylist: true, segmentCount: 3, segmentSeconds: 4}
	pl := rt.synthPlaylist()

	if !strings.HasPrefix(pl, "#EXTM3U") {
		t.Fatalf("playlist does not start with #EXTM3U:\n%s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-PLAYLIST-TYPE:VOD") || !strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Errorf("playlist missing VOD/ENDLIST markers:\n%s", pl)
	}
	for _, want := range []string{"segment000.ts", "segment001.ts", "segment002.ts"} {
		if !strings.Contains(pl, want) {
			t.Errorf("playlist missing %q:\n%s", want, pl)
		}
	}
	if strings.Contains(pl, "segment003.ts") {
		t.Errorf("playlist lists a segment past the count:\n%s", pl)
	}
	// Position ffmpeg elsewhere — the playlist must not change.
	rt.startNumber = 2
	if rt.synthPlaylist() != pl {
		t.Error("synthPlaylist changed after repositioning ffmpeg; it must be stable")
	}
}

// --- realignment via the runtime, with a fake runner (no real ffmpeg) ---

// recordingRunner is a fake ffmpeg Runner for the realignment tests: it records
// every launch's args, exposes the jobs it returned (so a test can assert the old
// job was killed), and — to drive the on-demand file-wait path — writes the
// segment file the launch is numbered to produce (its -start_number), simulating
// ffmpeg flushing the first segment of that (re)start.
type recordingRunner struct {
	mu        sync.Mutex
	starts    [][]string
	jobs      []*recordingJob
	outputDir string
	writeSeg  bool   // when true, write segmentNNN.ts for the launch's -start_number
	segFmt    string // segment-name Printf template writeSeg uses ("segment%03d.ts" when empty)
}

func (r *recordingRunner) Start(_ context.Context, args []string) (transcode.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, args)
	// The last arg is the playlist path; its dir is the scratch dir.
	r.outputDir = filepath.Dir(args[len(args)-1])
	j := &recordingJob{}
	r.jobs = append(r.jobs, j)
	if r.writeSeg {
		if n, ok := startNumberOf(args); ok {
			name := segmentName(n)
			if r.segFmt != "" {
				name = fmt.Sprintf(r.segFmt, n)
			}
			_ = os.WriteFile(filepath.Join(r.outputDir, name), []byte("seg-"+name), 0o644)
		}
	}
	return j, nil
}

func (r *recordingRunner) launchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

type recordingJob struct {
	mu     sync.Mutex
	killed bool
}

func (j *recordingJob) Wait() error { return nil }
func (j *recordingJob) Kill() error {
	j.mu.Lock()
	j.killed = true
	j.mu.Unlock()
	return nil
}
func (j *recordingJob) wasKilled() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.killed
}

func startNumberOf(args []string) (int, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-start_number" {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return 0, false
			}
			return n, true
		}
	}
	return 0, false
}

// segmentName formats a segment index the way the muxer/playlist name it.
func segmentName(n int) string { return fmt.Sprintf("segment%03d.ts", n) }

// argHasPair reports whether args contains flag immediately followed by value.
func argHasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// argHasFlag reports whether args contains the bare flag.
func argHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestRealignSupersedesJobAndRenumbers drives the core realignment behavior
// through the runtime with a fake runner: starting at the top, then requesting a
// segment well ahead, must (1) kill the original job, (2) launch exactly one new
// job seeked to the target with -start_number = target, and (3) serve the segment
// the realigned job produces — all within the same runtime (no new session).
func TestRealignSupersedesJobAndRenumbers(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{writeSeg: true}
	m := NewRemuxManager(runner, root)

	dec := Decision{
		Tier:    TierTranscode,
		Edition: store.Edition{ID: "e1"},
		File:    store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 60_000}, // 15 segments @4s
	}
	job := transcode.TranscodeJob{SourcePath: dec.File.Path, Video: transcode.VideoPlan{}}
	s := m.Create(CreateInput{
		UserID:  "u1",
		TitleID: "t1",
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			job.OutputDir = outputDir
			job.Seek = seek
			return transcode.TranscodeArgs(job)
		},
	}, dec)

	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no runtime for transcode session")
	}
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if runner.launchCount() != 1 {
		t.Fatalf("launches after start = %d, want 1", runner.launchCount())
	}
	orig := runner.jobs[0]

	// Request a segment far ahead of the from-the-top job → realignment.
	const target = 8
	b, err := rt.segment(segmentName(target))
	if err != nil {
		t.Fatalf("segment(%d): %v", target, err)
	}
	if len(b) == 0 {
		t.Fatal("realigned segment body empty")
	}

	// Exactly one extra launch (the realignment), and the original job was killed —
	// no orphaned ffmpeg, no second concurrent job.
	if runner.launchCount() != 2 {
		t.Fatalf("launches after realign = %d, want 2 (initial + 1 realign)", runner.launchCount())
	}
	if !orig.wasKilled() {
		t.Error("original ffmpeg job not killed on realign (would orphan a process)")
	}
	// The realigned launch is seeked + numbered to the target.
	realignArgs := runner.starts[1]
	if sn, ok := startNumberOf(realignArgs); !ok || sn != target {
		t.Errorf("realigned -start_number = %d (ok=%v), want %d; args: %v", sn, ok, target, realignArgs)
	}
	if !argHasPair(realignArgs, "-ss", "32") { // 8 * 4s
		t.Errorf("realigned -ss != 32 (target*segLen); args: %v", realignArgs)
	}

	// Still the same session/runtime — Create was never called again.
	if m.Count() != 1 {
		t.Errorf("session count = %d, want 1 (seek reuses the session)", m.Count())
	}
}

// TestAudioOnlyTranscodeServesFfmpegPlaylistWithoutRealign is the regression for
// the "music 404s on Safari" bug. An audio-only transcode must be delivered like
// the remux tier — ffmpeg's OWN playlist, with NO seek-realignment. Audio segments
// are not keyframe-aligned to the synthesized uniform-4s grid, and a native-HLS
// client (Safari) fetches segments out-of-order / in parallel; the realignment
// machinery would then kill+restart ffmpeg repeatedly, leaving listed segments
// unproduced (404) and hanging the player. A VIDEO transcode keeps the
// server-owned playlist + realignment (re-encoding video is expensive, so seeking
// ahead must not re-encode everything before it).
func TestAudioOnlyTranscodeServesFfmpegPlaylistWithoutRealign(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{writeSeg: true}
	m := NewRemuxManager(runner, root)

	dec := Decision{
		Tier:      TierTranscode,
		AudioOnly: true,
		Edition:   store.Edition{ID: "e1"},
		// A duration that WOULD synthesize 15 uniform 4s segments if this were a
		// server-owned (video) transcode — so the assertions below distinguish the
		// audio path from that.
		File: store.File{ID: "f1", Path: "/music/x.flac", DurationMs: 60_000},
	}
	s := m.Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.TranscodeArgs(transcode.TranscodeJob{
				SourcePath: dec.File.Path, AudioOnly: true, HasAudio: true, OutputDir: outputDir, Seek: seek,
			})
		},
	}, dec)

	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no runtime for audio-only transcode session")
	}
	// The fix: audio-only transcode does NOT own the playlist — no synthesized
	// playlist, no realignment (it is delivered exactly like remux).
	if rt.ownsPlaylist {
		t.Error("audio-only transcode ownsPlaylist=true; want false (serve ffmpeg's own playlist, no realign)")
	}

	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}

	// A far-ahead segment must NOT trigger a realignment — that kill+restart is
	// exactly the thrash that 404s audio segments. Pre-write the segment so the read
	// path returns promptly; the assertion is that NO extra ffmpeg launch happened.
	const target = 8
	if err := os.WriteFile(filepath.Join(rt.scratchDir, segmentName(target)), []byte("ts"), 0o644); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	if _, err := rt.segment(segmentName(target)); err != nil {
		t.Fatalf("segment(%d): %v", target, err)
	}
	if runner.launchCount() != 1 {
		t.Errorf("launches = %d, want 1 (audio-only must NOT realign — realignment thrash is the bug)", runner.launchCount())
	}

	// playlist() serves ffmpeg's OWN written playlist verbatim, not a synthesized,
	// uniform-4s one (whose phantom segment count/durations don't match the audio).
	ffmpegPlaylist := "#EXTM3U\n#EXT-X-VERSION:6\n#EXTINF:4.017056,\nsegment000.ts\n#EXTINF:1.307744,\nsegment001.ts\n#EXT-X-ENDLIST\n"
	if err := os.WriteFile(rt.playlistPath(), []byte(ffmpegPlaylist), 0o644); err != nil {
		t.Fatalf("seed playlist: %v", err)
	}
	pl, err := rt.playlist()
	if err != nil {
		t.Fatalf("playlist(): %v", err)
	}
	if string(pl) != ffmpegPlaylist {
		t.Errorf("playlist = %q,\nwant ffmpeg's own playlist %q (audio-only must not synthesize)", pl, ffmpegPlaylist)
	}
}

// TestRealignNotTriggeredForNearbySegment: a segment within the lookahead window
// of the current job is left to arrive on its own — no needless restart while
// ffmpeg is catching up.
func TestRealignNotTriggeredForNearbySegment(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{writeSeg: true}
	m := NewRemuxManager(runner, root)
	dec := Decision{Tier: TierTranscode, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 60_000}}
	s := m.Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.TranscodeArgs(transcode.TranscodeJob{SourcePath: dec.File.Path, OutputDir: outputDir, Seek: seek})
		},
	}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	// segment 1 is within realignLookahead of start 0 — the initial job already
	// wrote segment000; segment001 should be served without a realign. The fake
	// runner only wrote segment000 on the initial launch, so write segment001 to
	// stand in for the running job producing it.
	_ = os.WriteFile(filepath.Join(rt.scratchDir, segmentName(1)), []byte("ts"), 0o644)
	if _, err := rt.segment(segmentName(1)); err != nil {
		t.Fatalf("segment(1): %v", err)
	}
	if runner.launchCount() != 1 {
		t.Errorf("launches = %d, want 1 (no realign for a nearby segment)", runner.launchCount())
	}
}

// TestFMP4CopyRealignsOnJump: an fMP4 (copied-HEVC) session must realign on a seek
// across unproduced ground — before this, the fMP4 runtime was not realignable at
// all, so a RESUME at a saved position (or any deep seek) waited on the linear
// ~realtime copy of a huge file and the player timed out and never started
// (Chrome: fragLoadTimeOut loop; Safari native: a silent stall). And it must NOT
// realign for a request at the production frontier (a copy outruns playback, so
// ordinary prefetch would thrash the job) — including while the predecessor is
// still an hls-muxer temp_file (.tmp) awaiting its atomic rename.
func TestFMP4CopyRealignsOnJump(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{writeSeg: true, segFmt: transcode.SegmentPatternFMP4}
	m := NewRemuxManager(runner, root)

	// A copied 4K HEVC with a TrueHD default → TierTranscode + VideoCopy + fMP4
	// (ADR-0024), with probed keyframe boundaries (irregular, like a real remux).
	boundaries := []float64{0, 4.5, 8, 12.4, 17.1, 20.5, 24.4, 28.2, 32.5, 36.5, 40, 44.4, 48, 52.1, 56.4, 60.2, 64, 68.3, 72.1, 76, 80}
	dec := Decision{
		Tier:      TierTranscode,
		VideoCopy: true,
		Edition:   store.Edition{ID: "e1"},
		File:      store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 80_000, VideoCodec: "hevc"},
		VideoStream: store.Stream{Codec: "hevc"},
	}
	if !dec.UsesFMP4() {
		t.Fatal("test decision must be an fMP4 (copied HEVC) session")
	}
	s := m.Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		SegmentBoundaries: boundaries,
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{
				SourcePath: dec.File.Path, OutputDir: outputDir,
				Seek: seek, VideoOnly: true, FMP4: true,
			})
		},
	}, dec)
	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no runtime for fMP4 copy session")
	}
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	orig := runner.jobs[0]
	segName := func(n int) string { return fmt.Sprintf(transcode.SegmentPatternFMP4, n) }

	// A COLD-START BURST must not realign: Safari's native player fetches several
	// segments in parallel the moment a session starts, before ffmpeg has written
	// anything (not even a temp file) — treating that as a seek kills the job the
	// sibling requests are waiting on, they starve and 404, and nothing ever plays.
	_ = os.WriteFile(filepath.Join(rt.scratchDir, segName(3)), []byte("m4s"), 0o644)
	if _, err := rt.segment(segName(3)); err != nil {
		t.Fatalf("segment(3) cold-start burst: %v", err)
	}
	if runner.launchCount() != 1 {
		t.Fatalf("launches after cold-start burst = %d, want 1 (a near-start request must wait, not realign)", runner.launchCount())
	}

	// A resume/seek far past the frontier realigns to the target's exact keyframe
	// boundary and serves the segment the realigned job produces.
	const target = 12
	b, err := rt.segment(segName(target))
	if err != nil {
		t.Fatalf("segment(%d): %v", target, err)
	}
	if len(b) == 0 {
		t.Fatal("realigned segment body empty")
	}
	if runner.launchCount() != 2 {
		t.Fatalf("launches after jump = %d, want 2 (initial + 1 realign)", runner.launchCount())
	}
	if !orig.wasKilled() {
		t.Error("original ffmpeg job not killed on realign (would orphan a process)")
	}
	realignArgs := runner.starts[1]
	if sn, ok := startNumberOf(realignArgs); !ok || sn != target {
		t.Errorf("realigned -start_number = %d (ok=%v), want %d; args: %v", sn, ok, target, realignArgs)
	}
	// -ss must be the target segment's keyframe boundary (+ the rounding epsilon),
	// NOT the uniform target*segmentSeconds grid.
	ss := argValueFloat(t, realignArgs, "-ss")
	if diff := ss - boundaries[target]; diff < 0 || diff > 0.01 {
		t.Errorf("realigned -ss = %v, want boundaries[%d]=%v (+ ~2ms)", ss, target, boundaries[target])
	}
	// Timestamp continuity across the realign (Chrome's MSE rejects a backward jump).
	if !argHasFlag(realignArgs, "-output_ts_offset") {
		t.Errorf("realigned job missing -output_ts_offset; args: %v", realignArgs)
	}

	// Prefetch at the frontier: the successor of a segment that exists (final name
	// or the hls muxer's in-flight .tmp) must be WAITED for, never realigned to.
	_ = os.WriteFile(filepath.Join(rt.scratchDir, segName(target+1)), []byte("m4s"), 0o644)
	if _, err := rt.segment(segName(target + 1)); err != nil {
		t.Fatalf("segment(%d): %v", target+1, err)
	}
	_ = os.WriteFile(filepath.Join(rt.scratchDir, segName(target+2)+".tmp"), []byte("tmp"), 0o644)
	_ = os.WriteFile(filepath.Join(rt.scratchDir, segName(target+3)), []byte("m4s"), 0o644)
	if _, err := rt.segment(segName(target + 3)); err != nil {
		t.Fatalf("segment(%d): %v", target+3, err)
	}
	if runner.launchCount() != 2 {
		t.Errorf("launches after frontier prefetch = %d, want 2 (no realign thrash)", runner.launchCount())
	}
}

// argValueFloat finds flag's value in args and parses it as a float.
func argValueFloat(t *testing.T, args []string, flag string) float64 {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			f, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil {
				t.Fatalf("%s value %q not a float: %v", flag, args[i+1], err)
			}
			return f
		}
	}
	t.Fatalf("args missing %s: %v", flag, args)
	return 0
}

// TestRealignAfterTeardownDoesNotResurrect: a realign attempt after teardown must
// not start ffmpeg again — a torn-down (ended/reaped) session stays dead.
func TestRealignAfterTeardownDoesNotResurrect(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{}
	m := NewRemuxManager(runner, root)
	dec := Decision{Tier: TierTranscode, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 60_000}}
	s := m.Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.TranscodeArgs(transcode.TranscodeJob{SourcePath: dec.File.Path, OutputDir: outputDir, Seek: seek})
		},
	}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	rt.teardown()
	before := runner.launchCount()
	if err := rt.realign(5); err == nil {
		t.Error("realign after teardown returned nil error, want a refusal")
	}
	if runner.launchCount() != before {
		t.Errorf("launches changed after teardown realign: %d → %d", before, runner.launchCount())
	}
}
