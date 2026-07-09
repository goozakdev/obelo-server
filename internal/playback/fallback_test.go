package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/transcode"
)

// Per-session hardware→CPU fallback (ADR-0009, issue 03). These exercise the
// session Manager + hlsRuntime directly with a fake Runner (no GPU): the first
// (hardware) ffmpeg job fails to launch, and the runtime must transparently
// restart ONCE on the CPU libx264 path — in the same session, scratch dir, and
// transcode cap slot — and still serve a playable HLS stream.

// fallbackRunner models the two ways a launch can fail. By default the FIRST
// launch (the hardware attempt) fails the realistic way — ffmpeg execs fine (Start
// returns no error) but the process dies immediately with a non-zero exit and no
// output (a hardware-init failure caught by the launch probe) — and the SECOND
// launch (the CPU fallback) succeeds, writing the requested segment so the HLS read
// path returns a real stream. The *StartErr fields instead make a launch fail via a
// Runner.Start error (the other half of the "fails to launch" definition); cpuFails
// makes the CPU fallback die too, to prove the fallback is bounded to one attempt.
type fallbackRunner struct {
	mu         sync.Mutex
	starts     [][]string
	hwStartErr error // when set, the hardware launch fails via a Start error, not a quick death
	cpuFails   bool  // when set, the CPU fallback also fails to launch (a Start error)
}

func (r *fallbackRunner) Start(_ context.Context, args []string) (transcode.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.starts)
	r.starts = append(r.starts, args)
	outputDir := filepath.Dir(args[len(args)-1])
	if n == 0 {
		if r.hwStartErr != nil {
			return nil, r.hwStartErr // ffmpeg could not even be spawned
		}
		// Hardware launch: born dead (immediate non-zero exit, no output).
		return &diedJob{err: errors.New("h264_videotoolbox @ device: init failed")}, nil
	}
	if r.cpuFails {
		// The CPU fallback also fails — a genuine playback error, not another restart.
		return nil, errors.New("libx264: also failed to launch")
	}
	// Healthy CPU fallback: flush the from-the-top segment the player will ask for
	// (transcode synthesizes its own playlist, so no playlist file is needed).
	sn, _ := startNumberOf(args)
	_ = os.WriteFile(filepath.Join(outputDir, segmentName(sn)), []byte("ts-"+segmentName(sn)), 0o644)
	return &liveJob{}, nil
}

func (r *fallbackRunner) launches() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.starts))
	copy(out, r.starts)
	return out
}

// diedJob is a job whose process exited immediately with err — the launch probe
// reads this as a failed launch.
type diedJob struct{ err error }

func (j *diedJob) Wait() error { return j.err }
func (j *diedJob) Kill() error { return nil }

// liveJob is a healthy running job: Wait blocks until Kill (teardown/realign), so
// the launch probe sees it still running and proceeds.
type liveJob struct {
	mu     sync.Mutex
	done   chan struct{}
	killed bool
}

func (j *liveJob) Wait() error {
	j.mu.Lock()
	if j.done == nil {
		j.done = make(chan struct{})
	}
	ch := j.done
	j.mu.Unlock()
	<-ch
	return nil
}

func (j *liveJob) Kill() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.killed {
		j.killed = true
		if j.done == nil {
			j.done = make(chan struct{})
		}
		close(j.done)
	}
	return nil
}

// hwTranscodeDecision is a transcode Decision with a known duration so the runtime
// owns/synthesizes the HLS playlist and the read path is exercised.
func hwTranscodeDecision(id string) Decision {
	return Decision{
		Tier:             TierTranscode,
		Edition:          store.Edition{ID: "e-" + id},
		File:             store.File{ID: "f-" + id, Path: "/movies/" + id + ".mkv", DurationMs: 60_000, Bitrate: 8_000_000},
		EstimatedBitrate: 8_000_000,
	}
}

// hwArgsBuilders returns the hardware + CPU-fallback ffmpeg-args builders for a
// transcode, mirroring what Service.hlsArgsBuilders produces when the configured
// backend is hardware: identical plans differing only in the encode backend, so
// the hardware launch emits -c:v h264_videotoolbox and the fallback emits
// -c:v libx264.
func hwArgsBuilders(src string) (hw, cpu func(string, transcode.SeekOffset) []string) {
	build := func(accel transcode.Accel) func(string, transcode.SeekOffset) []string {
		return func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.TranscodeArgs(transcode.TranscodeJob{
				SourcePath: src,
				OutputDir:  outputDir,
				Video:      transcode.VideoPlan{}, // re-encode (not copy) so -c:v appears
				Accel:      accel,
				Seek:       seek,
			})
		}
	}
	return build(transcode.AccelVideoToolbox), build(transcode.AccelCPU)
}

// TestHWLaunchFailureFallsBackToCPU is the headline test: a hardware transcode
// whose ffmpeg job fails to launch transparently restarts ONCE on libx264, reuses
// the slot it already holds (a concurrently-capped second transcode stays
// rejected), and still serves a valid HLS playlist + segment.
func TestHWLaunchFailureFallsBackToCPU(t *testing.T) {
	root := t.TempDir()
	runner := &fallbackRunner{}
	m := NewRemuxManager(runner, root)
	m.SetTranscodeCap(1) // exactly one transcode slot

	dec := hwTranscodeDecision("a")
	hw, cpu := hwArgsBuilders(dec.File.Path)
	s, err := m.CreateGoverned(CreateInput{
		UserID:          "u1",
		TitleID:         "t1",
		BuildHLSArgs:    hw,
		BuildHLSArgsCPU: cpu,
	}, dec)
	if err != nil {
		t.Fatalf("create transcode session: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Fatalf("activeTranscodes = %d after create, want 1", got)
	}

	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no runtime for transcode session")
	}

	// First manifest/segment request launches ffmpeg. The hardware job dies on
	// launch, so the runtime must fall back to CPU — EnsureStarted still succeeds.
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted after HW launch failure: %v (fallback should have rescued it)", err)
	}

	launches := runner.launches()
	if len(launches) != 2 {
		t.Fatalf("launch count = %d, want exactly 2 (hardware attempt + one CPU fallback)", len(launches))
	}
	// The first launch is the hardware encoder; the second (fallback) is libx264.
	if !argHasPair(launches[0], "-c:v", "h264_videotoolbox") {
		t.Errorf("first launch is not the hardware encoder; args: %v", launches[0])
	}
	if !argHasPair(launches[1], "-c:v", "libx264") {
		t.Errorf("fallback launch did not restart on AccelCPU (libx264); args: %v", launches[1])
	}
	if argHasPair(launches[1], "-c:v", "h264_videotoolbox") {
		t.Errorf("fallback launch still used the hardware encoder; args: %v", launches[1])
	}
	// Decode-failure safety net (issue 05): the HW launch carries a decode -hwaccel
	// (videotoolbox), so a HW DECODE that fails to init dies early and is caught by the
	// same launch probe as a HW-encode failure. The CPU fallback must drop BOTH the HW
	// decode and the HW encode — its args carry NO -hwaccel — so a device that cannot
	// decode the source (e.g. a 10-bit HEVC profile) recovers fully in software.
	if !argHasFlag(launches[0], "-hwaccel") {
		t.Errorf("HW launch should carry a decode -hwaccel; args: %v", launches[0])
	}
	if argHasFlag(launches[1], "-hwaccel") {
		t.Errorf("CPU fallback must carry no -hwaccel (software decode + libx264); args: %v", launches[1])
	}

	// Slot reuse: the fallback restarted in the SAME slot — it did not acquire a
	// second one — so a concurrently-capped second transcode is still rejected.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2", TitleID: "t2"}, hwTranscodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("second transcode err = %v, want ErrTranscodeCapFull (fallback must not free or double-take the slot)", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after fallback, want 1 (still exactly one slot held)", got)
	}

	// Still a playable HLS stream: server-owned playlist + the CPU job's segment.
	pl, err := rt.playlist()
	if err != nil {
		t.Fatalf("playlist after fallback: %v", err)
	}
	if !strings.Contains(string(pl), "#EXTM3U") || !strings.Contains(string(pl), "segment000.ts") {
		t.Errorf("synthesized playlist looks invalid after fallback:\n%s", pl)
	}
	seg, err := rt.segment(segmentName(0))
	if err != nil {
		t.Fatalf("segment(0) after fallback: %v", err)
	}
	if len(seg) == 0 {
		t.Error("segment body empty after fallback")
	}

	// Cleanup is unchanged: End frees the slot so the previously-rejected transcode
	// can now take it.
	if !m.End(s.ID) {
		t.Fatal("End returned false")
	}
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after End, want 0", got)
	}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u3", TitleID: "t3"}, hwTranscodeDecision("c")); err != nil {
		t.Errorf("transcode after freed slot err = %v, want nil", err)
	}
}

// TestHWStartErrorFallsBackToCPU covers the other half of "fails to launch": the
// hardware Runner.Start itself errors (ffmpeg could not be spawned). The runtime
// must still fall back to CPU exactly once and serve the stream.
func TestHWStartErrorFallsBackToCPU(t *testing.T) {
	root := t.TempDir()
	runner := &fallbackRunner{hwStartErr: errors.New("exec: hardware ffmpeg failed to start")}
	m := NewRemuxManager(runner, root)

	dec := hwTranscodeDecision("a")
	hw, cpu := hwArgsBuilders(dec.File.Path)
	s := m.Create(CreateInput{UserID: "u1", TitleID: "t1", BuildHLSArgs: hw, BuildHLSArgsCPU: cpu}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)

	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted after HW Start error: %v (fallback should have rescued it)", err)
	}
	launches := runner.launches()
	if len(launches) != 2 {
		t.Fatalf("launch count = %d, want 2 (hardware Start error + CPU fallback)", len(launches))
	}
	if !argHasPair(launches[1], "-c:v", "libx264") {
		t.Errorf("fallback launch did not restart on libx264; args: %v", launches[1])
	}
	if _, err := rt.segment(segmentName(0)); err != nil {
		t.Fatalf("segment(0) after Start-error fallback: %v", err)
	}
}

// TestHWFallbackBoundedToSingleAttempt: when the CPU fallback ALSO fails to launch,
// the runtime surfaces an honest error and does NOT thrash — exactly two launches
// (hardware + one CPU), no third.
func TestHWFallbackBoundedToSingleAttempt(t *testing.T) {
	root := t.TempDir()
	runner := &fallbackRunner{cpuFails: true}
	m := NewRemuxManager(runner, root)

	dec := hwTranscodeDecision("a")
	hw, cpu := hwArgsBuilders(dec.File.Path)
	s := m.Create(CreateInput{UserID: "u1", TitleID: "t1", BuildHLSArgs: hw, BuildHLSArgsCPU: cpu}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)

	// The hardware job dies → fall back to CPU; the CPU launch also fails → honest
	// error surfaced to the caller, no further restart.
	if err := rt.EnsureStarted(); err == nil {
		t.Fatal("EnsureStarted returned nil, want an error when the CPU fallback also fails")
	}
	if got := len(runner.launches()); got != 2 {
		t.Fatalf("launch count = %d, want exactly 2 (no restart loop after the bounded fallback)", got)
	}
}

// TestNonHardwareTranscodeDoesNotFallBack: a non-hardware session (no CPU-fallback
// builder, as for a CPU transcode or remux) whose launch fails is an honest error —
// there is nothing to fall back FROM, so the runtime must not retry.
func TestNonHardwareTranscodeDoesNotFallBack(t *testing.T) {
	root := t.TempDir()
	// A Start error (not a quick death) so the failure surfaces even though a
	// non-eligible session is never probed — proving the absence of a retry, not the
	// probe.
	runner := &fallbackRunner{hwStartErr: errors.New("exec: ffmpeg failed to start")}
	m := NewRemuxManager(runner, root)

	dec := hwTranscodeDecision("a")
	hw, _ := hwArgsBuilders(dec.File.Path)
	// BuildHLSArgsCPU omitted → not fallback-eligible.
	s := m.Create(CreateInput{UserID: "u1", TitleID: "t1", BuildHLSArgs: hw}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)

	if err := rt.EnsureStarted(); err == nil {
		t.Fatal("EnsureStarted returned nil, want the launch error (no fallback for a non-eligible session)")
	}
	if got := len(runner.launches()); got != 1 {
		t.Errorf("launch count = %d, want exactly 1 (a non-eligible session must not retry)", got)
	}
}
