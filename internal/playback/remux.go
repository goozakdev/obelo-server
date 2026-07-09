package playback

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marioquake/juicebox/internal/transcode"
)

// hlsRuntime owns the on-demand HLS job for one directStream OR transcode
// session: the ffmpeg Runner, the ffmpeg argument builder (remux's `-c copy` args
// or transcode's re-encode args, parameterized by a seek offset for realignment —
// the only thing that differs between the two tiers), the session scratch
// directory, and the running ffmpeg Job once started. It is created (but idle)
// when the session is created and only spins ffmpeg up on the FIRST manifest/
// segment request (EnsureStarted), so a negotiated-but-never-played session costs
// nothing.
//
// Because delivery (on-demand playlist/segment serving), lifecycle, and scratch
// are identical for remux and transcode, this single runtime drives both — the
// caller (Manager.Create) bakes the tier choice into buildArgs and the runtime is
// agnostic to which ffmpeg job it is running.
//
// Seek realignment (ADR-0004): a request for a segment ahead of what the running
// ffmpeg job has produced (a forward seek in a transcode) triggers a REALIGNMENT
// — the runtime kills the current job and restarts ffmpeg at an input-seek offset
// (`-ss`) for the requested segment's timestamp, numbering its first segment at
// that index (`-start_number`) so the produced .ts lands under exactly the name
// the playlist lists. Numbering therefore stays monotonic and coherent across a
// realignment. For transcode the playlist is SERVER-OWNED (synthesized once from
// the File duration) so it never changes as ffmpeg is repositioned; remux keeps
// ffmpeg's own playlist (remux seeking is already cheap — segments map directly to
// the copied stream).
//
// Lifecycle: EnsureStarted is idempotent for the FIRST launch (a sync.Once guards
// scratch-dir creation + the initial ffmpeg start); realign supersedes the current
// job within the SAME session (same scratch, no new negotiation). teardown (called
// by Manager.End/Reap) kills the current ffmpeg job and deletes the scratch dir.
// Every job runs under a context the runtime cancels when it is superseded or torn
// down, so a kill is prompt and no ffmpeg process is orphaned.
type hlsRuntime struct {
	runner transcode.Runner
	// buildArgs returns the ffmpeg argument vector for a given seek offset. The zero
	// SeekOffset is the from-the-top launch (identical to slices 1/2); a non-zero
	// offset is a realignment. The Service supplies it (it holds the profile/
	// constraints + source path needed to plan remux/transcode args).
	buildArgs func(transcode.SeekOffset) []string
	// buildCPUArgs is the CPU (libx264) variant of buildArgs, set ONLY for a
	// hardware-encoded transcode (issue 03). When non-nil the runtime may perform a
	// single hardware→CPU fallback (ADR-0009): if the HW ffmpeg job fails to launch,
	// it restarts ONCE with these args. nil (remux, or an already-CPU transcode)
	// means the session is not fallback-eligible. Once a fallback has fired
	// (fellBack), EVERY subsequent (re)launch — including seek realignment — uses
	// this builder so the whole session stays on CPU.
	buildCPUArgs func(transcode.SeekOffset) []string
	scratchDir   string

	// ownsPlaylist makes the runtime synthesize and serve the media playlist itself
	// rather than serving the one ffmpeg writes. A server-owned playlist appears
	// IMMEDIATELY (ffmpeg's own VOD playlist is written only when the whole input is
	// finished — minutes away for a long file, so serving it 404s the long file) and
	// is stable across realignments. segmentCount/segmentSeconds are the UNIFORM shape
	// (a re-encode forces exact SegmentSeconds keyframes; an audio rendition's frames
	// are ~uniform); boundaries, when set, gives the EXACT per-segment durations of a
	// video COPY (whose segments fall on the source's irregular keyframes — computed by
	// transcode.SegmentBoundaries so the synthesized playlist matches ffmpeg's cuts).
	ownsPlaylist   bool
	segmentCount   int
	segmentSeconds int
	// boundaries, segNameFmt, initSegment shape the synthesized playlist (ownsPlaylist).
	// boundaries nil → uniform segmentCount×segmentSeconds; non-nil → its len-1 exact
	// segments. segNameFmt is the Printf segment-name template ("segment%03d.ts" when
	// empty; a rendition or fMP4 session overrides it). initSegment, when set, is the
	// fMP4 #EXT-X-MAP init segment. realignable marks a runtime that repositions
	// ffmpeg on a seek across unproduced ground: a video re-encode / audio rendition
	// (uniform grid, fixed-lookahead trigger) and a boundaries-based video copy
	// (frontier-aware trigger — see gatedSegment). A remux without probed boundaries
	// serves ffmpeg's own playlist and never realigns.
	boundaries  []float64
	segNameFmt  string
	initSegment string
	realignable bool
	// gateSequential marks a runtime whose ffmpeg job writes segments IN PLACE (the
	// segment muxer used for dictated-cut TS copies has no temp_file atomic rename):
	// a segment is served only once its SUCCESSOR exists or the job has exited, so a
	// read can never return a half-written file.
	gateSequential bool

	// playlistName is the media-playlist file this runtime serves out of the scratch
	// dir. Empty means the standard video playlist (transcode.PlaylistName,
	// index.m3u8); a demuxed AUDIO rendition runtime (audio-streams/03) sets it to its
	// namespaced audio_<streamId>.m3u8 so it serves ffmpeg's own rendition playlist
	// rather than the video variant's.
	playlistName string

	// sharedScratch marks a runtime that does NOT own its scratch dir — a demuxed
	// audio rendition runtime writes its files into the SESSION's scratch dir,
	// alongside the video variant and the other renditions. teardown then kills only
	// its ffmpeg job and leaves the directory for the owning session's teardown to
	// remove, so reaping one rendition can never delete the shared scratch out from
	// under the video job or a sibling rendition.
	sharedScratch bool

	once     sync.Once
	startErr error

	// mu guards the mutable job state below: the running job, its cancel func, and
	// the segment index it was started to produce from (startNumber). Realign and
	// teardown both mutate these and must be serialized.
	mu          sync.Mutex
	cancel      context.CancelFunc
	job         transcode.Job
	startNumber int // segment index the current job began producing from
	startedAt   time.Time
	torndown    bool // set by teardown so a late realign does not resurrect a killed session
	// fellBack records that the single allowed hardware→CPU fallback has already
	// fired (issue 03). It bounds the fallback to ONE attempt — a CPU job that then
	// fails is an honest playback error, never another restart — and it pins every
	// later (re)launch to the CPU builder so a realignment after fallback stays on
	// libx264.
	fellBack bool
	// jobExited is set by the exit watcher when the CURRENT job's process ends (any
	// reason). The sequential gate reads it: once the job is done, the last segment
	// (which has no successor) is complete and servable. Cleared on each (re)launch.
	jobExited bool
	// exited carries the current job's process-exit error so the initial-launch
	// probe can detect a hardware encoder that execs fine but dies immediately
	// (a HW-init failure). It is buffered (cap 1) and replaced on every startLocked;
	// jobs no one probes (CPU, remux, realignment) simply leave it unread.
	exited chan error
}

// EnsureStarted lazily launches the ffmpeg job exactly once at the from-the-top
// offset: it creates the scratch directory and starts ffmpeg writing the HLS
// playlist + segments into it. Subsequent calls are no-ops (returning the first
// launch's error, if any). The job runs in the background; the api layer then
// serves the playlist and segments out of the scratch dir as ffmpeg produces them.
// A later forward seek may supersede this job via realign.
func (rt *hlsRuntime) EnsureStarted() error {
	rt.once.Do(func() {
		if err := os.MkdirAll(rt.scratchDir, 0o755); err != nil {
			rt.startErr = err
			return
		}
		rt.startErr = rt.launchInitial()
	})
	return rt.startErr
}

// launchInitial performs the very first ffmpeg launch and applies the single
// per-session hardware→CPU fallback (ADR-0009, issue 03). It starts the configured
// (hardware, when fallback-eligible) job and, if that job FAILS TO LAUNCH, restarts
// once on the CPU libx264 path.
//
// "Fails to launch" here means EITHER of two things — and nothing else (a
// mid-stream failure of an already-running encode and the CPU path itself are out
// of scope):
//
//  1. Runner.Start returns an error (ffmpeg could not even be spawned), or
//  2. the spawned ffmpeg process exits non-zero within hwLaunchProbe, before it
//     has produced any HLS output. This is the realistic case: ffmpeg is bundled
//     so the binary execs fine (Start succeeds), but a hardware encoder that passed
//     startup validation (issue 02) can still fail to initialize the device for a
//     specific encode and die almost immediately. The brief probe catches that.
//
// Since issue 05 a hardware transcode also hardware-DECODES (the backend's initArgs
// carry a decode -hwaccel before -i), so case 2 covers a HW DECODE that cannot init
// for THIS source too — e.g. a 10-bit HEVC profile the device can't decode even
// though it could have encoded the output. Such a decode-init failure surfaces as
// the same early non-zero exit, and falling back to AccelCPU (whose backend has
// empty initArgs) drops BOTH the HW decode and the HW encode → full software
// decode + libx264. Documented limitation (carried forward from issue 03): a decode
// failure AFTER the probe window — the decoder inits fine, then dies on a later
// frame — is NOT caught here and surfaces as a normal manifest/segment timeout; a
// mid-stream decode-fallback is out of scope.
//
// The fallback restart runs in the SAME runtime: same scratch dir, same Runner,
// and — because the runtime never touches the Manager's cap accounting — the SAME
// transcode slot the session already holds. It can therefore never push the server
// past the concurrent-transcode cap. It is bounded to ONE attempt by fellBack.
func (rt *hlsRuntime) launchInitial() error {
	rt.mu.Lock()
	err := rt.startLocked(transcode.SeekOffset{})
	// Eligible only when a CPU builder was provided (a hardware transcode) and no
	// fallback has fired yet. Snapshot the job's exit channel to probe outside the
	// lock so a concurrent teardown/realign is never blocked on the probe window.
	eligible := rt.buildCPUArgs != nil && !rt.fellBack
	exited := rt.exited
	rt.mu.Unlock()

	if err == nil && eligible {
		// Start succeeded; give the hardware job a brief, bounded window to prove it
		// can actually run. An immediate non-zero exit is a HW-init failure → treat it
		// as a launch failure and fall back.
		if exitErr := awaitEarlyExit(exited, hwLaunchProbe); exitErr != nil {
			err = exitErr
		}
	}
	if err == nil || !eligible {
		return err
	}

	// Single bounded fallback: flip THIS session to the CPU libx264 path and restart
	// in the same scratch dir / same held slot. fellBack guarantees at most one
	// fallback, and pins realignment to CPU thereafter (see argsFor).
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.torndown {
		// Ended/reaped while the hardware job was failing — do not resurrect it.
		return os.ErrClosed
	}
	// One line per affected session (guarded by fellBack + the once.Do launch), so
	// an operator can see a validated hardware backend failing at runtime and the
	// session quietly running on CPU — the startup log alone cannot reveal this
	// (ADR-0009). filepath.Base(scratchDir) is the session ID (scratch is keyed by it).
	log.Printf("juicebox: hardware acceleration: session %s — hardware encode failed to launch (%v); falling back to CPU libx264", filepath.Base(rt.scratchDir), err)
	rt.fellBack = true
	rt.killCurrentLocked() // reap the dead/half-started hardware job before restarting
	return rt.startLocked(transcode.SeekOffset{})
}

// awaitEarlyExit waits up to `within` for a job's process to exit, returning its
// exit error if it does. A non-nil error means the process died during the probe
// window (a hardware-init failure for the launch probe); a nil result means it
// either exited cleanly (a surprisingly short clip — NOT a failure) or is still
// running after the window (the healthy case). A nil channel (no job) is never a
// failure.
func awaitEarlyExit(exited <-chan error, within time.Duration) error {
	if exited == nil {
		return nil
	}
	select {
	case err := <-exited:
		return err
	case <-time.After(within):
		return nil
	}
}

// startLocked launches a fresh ffmpeg job for the given seek offset and records it
// as the current job. The caller holds rt.mu. It does NOT kill any existing job —
// realign does that first — so it is used both for the initial launch and, after a
// kill, for a realigned restart. A torn-down runtime refuses to start.
func (rt *hlsRuntime) startLocked(seek transcode.SeekOffset) error {
	if rt.torndown {
		return os.ErrClosed
	}
	ctx, cancel := context.WithCancel(context.Background())
	job, err := rt.runner.Start(ctx, rt.argsFor(seek))
	if err != nil {
		cancel()
		return err
	}
	rt.cancel = cancel
	rt.job = job
	rt.startNumber = seek.StartNumber
	rt.startedAt = time.Now()
	// Publish the process exit on a fresh buffered channel so the initial-launch
	// probe (launchInitial) can detect an immediate hardware-init death, while a job
	// no one probes still gets reaped here when it exits on its own (e.g. ffmpeg
	// finishes the clip) so it is not left as a zombie. A deliberate kill via
	// realign/teardown also unblocks this Wait. The buffer (cap 1) means the send
	// never blocks even when the channel is unread.
	rt.jobExited = false
	exited := make(chan error, 1)
	rt.exited = exited
	sess := filepath.Base(rt.scratchDir)
	go func() {
		// Wait() returns nil for a deliberate Kill (teardown/realign) and a non-nil
		// error — carrying the ffmpeg stderr tail — only for a GENUINE failure. Log the
		// latter so a job that dies (a source ffmpeg can't copy, an unreadable/slow
		// mount) is visible in the server log instead of surfacing only as a silent HLS
		// 404 when the playlist/segment never appears.
		err := job.Wait()
		rt.mu.Lock()
		// Superseded (a realign killed us and started a fresh job)? Then we are NOT
		// the current job: don't mark it exited, and don't self-check — our playlist
		// covers only the partial pre-realign run and would false-positive a MISMATCH.
		superseded := rt.exited != exited
		if !superseded {
			rt.jobExited = true
		}
		rt.mu.Unlock()
		if err != nil {
			log.Printf("juicebox: transcode: ffmpeg job for session %s failed: %v", sess, err)
		} else if !superseded && seek.StartNumber == 0 {
			// Self-check only a from-the-top run that completed as the current job: its
			// playlist spans the whole timeline, so the comparison is meaningful. A
			// realigned run covers only [StartNumber..end) and a killed/superseded run is
			// partial — both would report phantom mismatches.
			rt.verifySynthPlaylist(sess)
		}
		exited <- err
	}()
	return nil
}

// hasExited reports whether the CURRENT ffmpeg job's process has ended.
func (rt *hlsRuntime) hasExited() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.jobExited
}

// verifySynthPlaylist is a SELF-CHECK for the keyframe-synthesized playlist (the
// video-copy path): once the copy job exits cleanly, ffmpeg's own playlist finally
// exists in the scratch dir — compare its segments against the boundaries the
// server synthesized and predicted. A divergence means the player was handed a
// playlist that mislists the real segments (drift, stalls, phantom 404s), which is
// otherwise nearly impossible to diagnose from the outside — so it is logged
// loudly with the first point of divergence. It runs only for a boundaries-based
// runtime (uniform re-encode/audio synth intentionally differs from ffmpeg's
// variable audio cuts, and a realigned job's playlist covers only part of the
// timeline).
func (rt *hlsRuntime) verifySynthPlaylist(sess string) {
	rt.mu.Lock()
	boundaries := rt.boundaries
	torndown := rt.torndown
	rt.mu.Unlock()
	if torndown || len(boundaries) < 2 {
		return
	}
	own, err := os.ReadFile(rt.playlistPath())
	if err != nil {
		return // killed mid-write / already reaped — nothing to compare
	}
	var actual []float64
	for _, line := range strings.Split(string(own), "\n") {
		if strings.HasPrefix(line, "#EXTINF:") {
			v := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			if d, perr := strconv.ParseFloat(strings.TrimSpace(v), 64); perr == nil {
				actual = append(actual, d)
			}
		}
	}
	if len(actual) == 0 {
		return
	}
	if len(actual) != len(boundaries)-1 {
		log.Printf("juicebox: playback: session %s synthesized playlist MISMATCH: predicted %d segments, ffmpeg produced %d — the keyframe index disagrees with ffmpeg's cuts for this file",
			sess, len(boundaries)-1, len(actual))
		return
	}
	for i, d := range actual {
		if p := boundaries[i+1] - boundaries[i]; p-d > 0.1 || d-p > 0.1 {
			log.Printf("juicebox: playback: session %s synthesized playlist MISMATCH at segment %d: predicted %.3fs, ffmpeg produced %.3fs",
				sess, i, p, d)
			return
		}
	}
}

// argsFor selects the argument builder for a (re)launch at the given seek offset.
// After the single hardware→CPU fallback has fired (fellBack), every launch —
// including a seek realignment — uses the CPU builder so the whole session stays on
// libx264; otherwise it uses the configured builder (hardware when so configured).
// The caller holds rt.mu.
func (rt *hlsRuntime) argsFor(seek transcode.SeekOffset) []string {
	if rt.fellBack && rt.buildCPUArgs != nil {
		return rt.buildCPUArgs(seek)
	}
	return rt.buildArgs(seek)
}

// realign supersedes the current ffmpeg job with one repositioned to begin
// producing at segment index target: it kills the running job (prompt, via its
// context cancel + Kill) and starts a fresh job seeked to target*segmentSeconds and
// numbered from target. The previous job's scratch segments stay on disk; the new
// job overwrites/extends from the seek point under the same names, so numbering is
// monotonic and the (server-owned) playlist remains coherent. A no-op when the
// current job already starts at or before target AND is still the right job (a
// backward seek to within the current run needs no restart — the earlier segments
// already exist); callers only realign when a wanted segment is genuinely ahead.
func (rt *hlsRuntime) realign(target int) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.torndown {
		return os.ErrClosed
	}
	// The input-seek time for the target segment: its exact keyframe boundary for a
	// boundaries-based video COPY (whose segments fall on the source's irregular
	// keyframes — the tiny epsilon keeps the demuxer's keyframe-at-or-before seek ON
	// the intended keyframe against float rounding), else the uniform grid position.
	seconds := float64(target * rt.segmentSeconds)
	if len(rt.boundaries) > target {
		seconds = rt.boundaries[target] + 0.002
	}
	// Kill the current job before restarting so there is never more than one ffmpeg
	// process per session (no orphan, no two jobs racing to write the same names).
	rt.killCurrentLocked()
	return rt.startLocked(transcode.SeekOffset{
		StartNumber:  target,
		StartSeconds: seconds,
	})
}

// killCurrentLocked cancels + kills the current job (if any) and clears the
// handles. Caller holds rt.mu. Used by both realign (before a restart) and
// teardown (final cleanup).
func (rt *hlsRuntime) killCurrentLocked() {
	if rt.cancel != nil {
		rt.cancel()
		rt.cancel = nil
	}
	if rt.job != nil {
		_ = rt.job.Kill()
		rt.job = nil
	}
}

// teardown kills the current ffmpeg job and removes the scratch directory. It is
// safe to call even if no job ever started and is the single cleanup path shared by
// Manager.End and Manager.Reap. It marks the runtime torn down so a concurrent
// realign cannot resurrect a killed session's ffmpeg.
func (rt *hlsRuntime) teardown() {
	rt.mu.Lock()
	rt.torndown = true
	rt.killCurrentLocked()
	rt.mu.Unlock()
	// A shared-scratch runtime (a demuxed audio rendition) does NOT own the directory
	// — the session's video runtime removes it. Only kill the job here so reaping one
	// rendition never deletes the video variant / sibling renditions' files.
	if rt.scratchDir != "" && !rt.sharedScratch {
		_ = os.RemoveAll(rt.scratchDir)
	}
}

// playlistPath / segmentPath resolve a file within the session scratch dir. A
// rendition runtime (playlistName set) serves its namespaced audio_<id>.m3u8; the
// video runtime leaves playlistName empty and serves the standard index.m3u8.
func (rt *hlsRuntime) playlistPath() string {
	name := rt.playlistName
	if name == "" {
		name = transcode.PlaylistName
	}
	return filepath.Join(rt.scratchDir, name)
}

func (rt *hlsRuntime) segmentPath(name string) string {
	return filepath.Join(rt.scratchDir, name)
}

// playlist returns the media-playlist bytes for the session. For a server-owned
// (transcode) runtime it synthesizes a stable VOD playlist from the File duration —
// the same set of segment names regardless of how ffmpeg is currently positioned —
// so a realignment never changes what the player sees. For a remux runtime
// (ownsPlaylist false) it waits for and returns ffmpeg's own playlist, unchanged
// from slice 1.
func (rt *hlsRuntime) playlist() ([]byte, error) {
	// Own the playlist only when we know how many segments to list (a known File
	// duration). Without that we cannot synthesize a complete VOD playlist, so fall
	// back to ffmpeg's own — the realignment path still works for any segment ffmpeg
	// names, it just isn't index-bounded.
	if rt.ownsPlaylist && (rt.segmentCount > 0 || len(rt.boundaries) > 1) {
		return []byte(rt.synthPlaylist()), nil
	}
	return waitReadFile(rt.playlistPath(), hlsManifestTimeout)
}

// synthPlaylist builds the server-owned VOD media playlist: segmentCount uniform
// segments of segmentSeconds each, named by SegmentPattern, ending with
// EXT-X-ENDLIST. Because the transcode encode forces a keyframe at every segment
// boundary (transcode.forceKeyFramesExpr), every segment is exactly segmentSeconds
// long, so the uniform EXTINF is accurate and segment N corresponds to exactly
// N*segmentSeconds — the invariant seek realignment relies on.
func (rt *hlsRuntime) synthPlaylist() string {
	nameFmt := rt.segNameFmt
	if nameFmt == "" {
		nameFmt = segmentNameFmt
	}
	// Per-segment durations: the EXACT boundaries of a video copy, else uniform.
	var durs []float64
	if len(rt.boundaries) > 1 {
		for i := 0; i+1 < len(rt.boundaries); i++ {
			durs = append(durs, rt.boundaries[i+1]-rt.boundaries[i])
		}
	} else {
		for i := 0; i < rt.segmentCount; i++ {
			durs = append(durs, float64(rt.segmentSeconds))
		}
	}
	// TARGETDURATION must be >= every EXTINF (rounded up); a copy's segments can exceed
	// SegmentSeconds when keyframes are sparse.
	target := rt.segmentSeconds
	for _, d := range durs {
		if c := int(math.Ceil(d - 1e-6)); c > target {
			target = c
		}
	}
	version := 3
	if rt.initSegment != "" {
		version = 6 // fMP4 / #EXT-X-MAP requires >= 6
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	fmt.Fprintf(&b, "#EXT-X-VERSION:%d\n", version)
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", target)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	if rt.initSegment != "" {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=%q\n", rt.initSegment)
	}
	for i, d := range durs {
		fmt.Fprintf(&b, "#EXTINF:%.6f,\n", d)
		fmt.Fprintf(&b, nameFmt, i)
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// segment returns the bytes of a named HLS segment, realigning ffmpeg if the
// segment is ahead of what the current job has produced. The flow:
//
//  1. Try to read it (brief wait) — the current job may be about to flush it.
//  2. If it is still absent AND its index is outside what the current job will
//     produce soon (a forward seek), realign ffmpeg to that index and wait again.
//
// A name that is not a recognized segment, or an index past the playlist, returns
// an os.IsNotExist error so the api layer 404s it. Only the server-owned
// (transcode) runtime realigns; a remux runtime serves its segments directly (its
// seek is already cheap) and just waits.
func (rt *hlsRuntime) segment(name string) ([]byte, error) {
	path := rt.segmentPath(name)

	// The fMP4 EXT-X-MAP init segment is NOT an indexed media segment: it is
	// written right at (re)launch, so it gets a plain existence wait — on EVERY
	// path. Without this, an audio rendition's init request racing its just-spawned
	// ffmpeg fell into the realign block's "not a segment we will ever produce"
	// 404 after only the short settle wait; Safari retries such a rendition init
	// exactly once and then kills the whole presentation with a decode error (the
	// nothing-ever-plays Safari bug on a slow-to-open source).
	if rt.initSegment != "" && name == rt.initSegment {
		return waitReadFile(path, hlsSegmentTimeout)
	}

	// A video COPY with probed boundaries (TS segment-muxer or fMP4 hls-muxer) uses
	// the FRONTIER-AWARE path: it realigns only on a genuine jump across unproduced
	// ground (a copy produces at I/O speed, so the fixed lookahead below would thrash
	// it on ordinary prefetch), and — when the muxer writes in place (gateSequential)
	// — serves a segment only once its successor exists. A seek to a segment far from
	// the current run restarts ffmpeg at that segment's exact boundary.
	if rt.gateSequential || (rt.realignable && len(rt.boundaries) > 1) {
		return rt.gatedSegment(name, path)
	}

	// Fast path: the segment is already on disk (or appears within a short wait
	// because the running job is producing it right now).
	if b, err := waitReadFile(path, hlsSegmentSettleTimeout); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// Not present yet. For a REALIGNABLE runtime, a forward seek to a segment the
	// current job will not reach soon triggers a realignment so the segment is
	// produced near its timestamp rather than after producing everything before it.
	if rt.ownsPlaylist && rt.realignable {
		idx, ok := rt.segmentIndex(name)
		if !ok || idx < 0 || (rt.segmentCount > 0 && idx >= rt.segmentCount) {
			// Not a segment we will ever produce: a genuine 404.
			return nil, os.ErrNotExist
		}
		if rt.shouldRealign(idx) {
			if err := rt.realign(idx); err != nil {
				return nil, err
			}
		}
	}

	// Wait the full segment window for the (possibly realigned) job to flush it.
	return waitReadFile(path, hlsSegmentTimeout)
}

// segmentIndex parses a segment name to its zero-based index using this runtime's
// segment-name template (an audio rendition's audio_<id>_%03d.ts, the fMP4 .m4s
// form, or the default segment%03d.ts).
func (rt *hlsRuntime) segmentIndex(name string) (int, bool) {
	if rt.segNameFmt == "" {
		return parseSegmentIndex(name)
	}
	var n int
	if _, err := fmt.Sscanf(name, rt.segNameFmt, &n); err != nil {
		return 0, false
	}
	return n, true
}

// gatedSegment serves one segment of a boundaries-based video-copy runtime:
// realign when the wanted index is far from the current run, then wait for the
// segment. A sequential-gated (segment-muxer TS) runtime additionally waits until
// the SUCCESSOR segment exists (the predecessor is then complete — the muxer
// writes strictly in order) or the job has exited (the final segment, or a
// finished copy, is complete by definition); an fMP4 (hls-muxer) runtime's
// temp_file renames are atomic, so a plain existence wait is a complete read.
func (rt *hlsRuntime) gatedSegment(name, path string) ([]byte, error) {
	idx, ok := rt.segmentIndex(name)
	if !ok || idx < 0 || (rt.segmentCount > 0 && idx >= rt.segmentCount) {
		return nil, os.ErrNotExist
	}
	nameFmt := rt.segNameFmt
	if nameFmt == "" {
		nameFmt = segmentNameFmt
	}
	// Realign only on a GENUINE jump. A copy produces at I/O speed — usually far
	// faster than playback — so a sequential prefetch just ahead of the production
	// frontier must WAIT, not restart ffmpeg (the fixed small lookahead of the
	// re-encode path realigns on ordinary prefetch here, thrashing the job). The
	// frontier test is on disk: if the PREDECESSOR segment exists we are at/near
	// the frontier (the muxer writes strictly in order) — wait; if it does not,
	// the request is a real seek across unproduced ground — realign to it. A
	// request BEFORE the current run's start is always a jump (those names were
	// skipped by the last realign).
	if rt.realignable {
		rt.mu.Lock()
		start := rt.startNumber
		rt.mu.Unlock()
		// A request BEFORE the current run's start is always a jump (those names were
		// skipped by the last realign). A request AHEAD is a jump only when it is
		// (a) beyond the near-start grace window — Safari's native player fetches a
		// BURST of segments in parallel when a session (re)starts, and treating an
		// early out-of-order request as a seek would kill the job its siblings are
		// waiting on (they then starve and 404, and nothing ever plays) — and
		// (b) not near the production frontier: a recent predecessor existing under
		// its final name (the segment muxer writes in place) or as the hls muxer's
		// in-flight .tmp means production is approaching this segment — wait, don't
		// restart. Only a request across genuinely unproduced ground realigns.
		jumpBack := idx < start
		jumpAhead := idx > start && !rt.nearFrontier(start, idx, nameFmt)
		if jumpBack || jumpAhead {
			if err := rt.realign(idx); err != nil {
				return nil, err
			}
		}
	}
	if !rt.gateSequential {
		// Atomic writes (the fMP4 hls muxer's temp_file): once the segment exists
		// under its final name it is complete — just wait for it.
		return waitReadFile(path, hlsSegmentTimeout)
	}
	next := rt.segmentPath(fmt.Sprintf(nameFmt, idx+1))
	deadline := time.Now().Add(hlsSegmentTimeout)
	for {
		if rt.hasExited() || fileExists(next) {
			return waitReadFile(path, hlsSegmentSettleTimeout)
		}
		if time.Now().After(deadline) {
			return nil, os.ErrNotExist
		}
		time.Sleep(hlsPollInterval)
	}
}

// fileExists reports whether path exists (any stat success).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// nearFrontier reports whether segment idx sits near the current job's production
// frontier, meaning the request should WAIT rather than realign. Two forms count:
//   - idx is within a few segments of the run's START — a job that just (re)started
//     may have written nothing yet (not even a temp file), and a native player's
//     cold-start burst must never read as a seek;
//   - a recent predecessor exists, under its final name (the segment muxer writes
//     in place) or as the hls muxer's in-flight .tmp — production is approaching.
// The window is a handful of segments: wide enough that parallel prefetch several
// segments ahead of disk never restarts the job, narrow enough that a genuine jump
// (a resume/seek minutes ahead) still realigns instantly.
func (rt *hlsRuntime) nearFrontier(start, idx int, nameFmt string) bool {
	const window = 8
	if idx-start <= window {
		return true
	}
	for i := idx - 1; i >= 0 && i >= idx-window; i-- {
		p := rt.segmentPath(fmt.Sprintf(nameFmt, i))
		if fileExists(p) || fileExists(p+".tmp") {
			return true
		}
	}
	return false
}

// shouldRealign reports whether a wanted segment index justifies repositioning
// ffmpeg. It does when the index is ahead of the current job's start by more than a
// small lookahead — i.e. the player has seeked forward past what this job will
// produce soon. A segment at/just after the current start is left to arrive on its
// own (no needless restart while ffmpeg is catching up). A wanted index BEFORE the
// current start also realigns (a backward seek into a region a later realignment
// jumped past, whose segments may not exist) — those earlier segments are produced
// by restarting there.
func (rt *hlsRuntime) shouldRealign(idx int) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if idx < rt.startNumber {
		return true
	}
	return idx > rt.startNumber+realignLookahead
}

// segmentNameFmt / parseSegmentIndex bridge the transcode.SegmentPattern
// (segment%03d.ts) the muxer writes and the index the runtime reasons about.
// segmentNameFmt is the Printf form; parseSegmentIndex is its inverse for an
// incoming request name.
const segmentNameFmt = "segment%03d.ts"

func parseSegmentIndex(name string) (int, bool) {
	if !strings.HasPrefix(name, "segment") {
		return 0, false
	}
	s := name[len("segment"):]
	switch {
	case strings.HasSuffix(s, ".ts"):
		s = strings.TrimSuffix(s, ".ts")
	case strings.HasSuffix(s, ".m4s"): // fMP4 (ADR-0024) media segments
		s = strings.TrimSuffix(s, ".m4s")
	default:
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// HLS on-demand timeouts: how long a manifest/segment request waits for ffmpeg to
// flush the file before giving up. hlsSegmentSettleTimeout is a SHORT first wait —
// long enough for the running job to flush a segment it is actively producing, but
// short enough that a forward seek does not stall before realigning. hlsSegmentTimeout
// is the longer wait after a (possible) realignment, bounding how long we wait for
// the repositioned job's first segment. The manifest is allowed longer (ffmpeg must
// decode+mux the first segment before any playlist exists, for the remux path).
// These bound a single request, not the whole job.
const (
	// hlsManifestTimeout bounds how long a manifest request waits for ffmpeg to write
	// the first playlist. It is generous because a REMUX / video-COPY of a large file
	// on SLOW STORAGE (an external / network mount) must open the file and read/copy
	// its first GOP before the muxer emits the playlist — which can take many seconds,
	// far longer than the sub-second local case. Too short a wait 404s a playlist that
	// was merely slow to appear (the reported "index.m3u8 404" on a network-mounted
	// movie); 30s covers a slow mount while still bounding a genuinely dead job (whose
	// failure the runtime now also logs with the ffmpeg stderr).
	hlsManifestTimeout      = 30 * time.Second
	hlsSegmentTimeout       = 15 * time.Second
	hlsSegmentSettleTimeout = 750 * time.Millisecond
	hlsPollInterval         = 25 * time.Millisecond

	// hwLaunchProbe is how long launchInitial waits for a just-started HARDWARE
	// ffmpeg job to prove it can run before declaring the launch successful (issue
	// 03). A hardware-init failure surfaces as an immediate non-zero exit, well
	// inside this window; a healthy job is still running when the window elapses and
	// proceeds normally. It is a one-time cost on the first manifest of a hardware
	// transcode only (remux and CPU transcodes are not probed), kept short so a
	// working stream is not noticeably delayed yet long enough to catch the common
	// fast device-init death. A HW death slower than this misses the fallback and
	// surfaces as a normal manifest/segment timeout — the documented boundary.
	hwLaunchProbe = 250 * time.Millisecond

	// realignLookahead is how many segments ahead of the current job's start a
	// wanted segment may be before we realign rather than wait. A small window
	// absorbs the normal case (the player asks for the next segment or two while
	// ffmpeg is still catching up) without restarting; a request well beyond it is a
	// genuine forward seek that warrants repositioning ffmpeg.
	realignLookahead = 2
)

// waitReadFile reads path, retrying on os.IsNotExist until the file appears or
// timeout elapses. ffmpeg's HLS muxer writes each segment and the playlist via a
// temp file and an atomic rename (the temp_file flag), so once a path exists it is
// complete — a successful os.ReadFile returns the whole file, never a partial. A
// path that never appears within timeout returns the last not-exist error so the
// caller can 404 a segment that genuinely will not be produced. Errors other than
// not-exist (e.g. a permission fault) return immediately.
func waitReadFile(path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error = os.ErrNotExist
	for {
		b, err := os.ReadFile(path)
		// A successful read of a NON-EMPTY file is complete. A 0-byte read is treated
		// as "not ready": ffmpeg creates some artifacts (notably the fMP4 init segment)
		// by opening the file and filling it a moment later WITHOUT the temp-file atomic
		// rename, so a read that races that window sees an empty file. No valid HLS
		// artifact (playlist, .ts/.m4s segment, init.mp4) is ever legitimately empty, so
		// waiting for content is always correct and never masks a real empty result.
		if err == nil {
			if len(b) > 0 {
				return b, nil
			}
			lastErr = os.ErrNotExist // keep waiting for ffmpeg to fill it
		} else if !os.IsNotExist(err) {
			return nil, err
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(hlsPollInterval)
	}
}
