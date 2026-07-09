package transcode

import (
	"context"
	"strings"
	"testing"
)

// TestRemuxArgsCopiesIntoHLS asserts the PURE remux command builder: it must use
// `-c copy` (the direct-stream contract — no re-encode) and emit HLS into the
// session scratch dir. The whole tier hinges on these flags, so they are pinned
// here without ever spawning ffmpeg.
func TestRemuxArgsCopiesIntoHLS(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath: "/movies/Blade Runner (1982).mkv",
		OutputDir:  "/data/transcode/sess-1",
	})
	joined := strings.Join(args, " ")

	// The input File is passed with -i.
	if !hasPair(args, "-i", "/movies/Blade Runner (1982).mkv") {
		t.Errorf("missing -i <source>; args: %v", args)
	}
	// -c copy is the load-bearing assertion: a remux NEVER re-encodes.
	if !hasPair(args, "-c", "copy") {
		t.Errorf("missing -c copy (remux must copy, not re-encode); args: %v", args)
	}
	// HLS output format + segmenting.
	if !hasPair(args, "-f", "hls") {
		t.Errorf("missing -f hls; args: %v", args)
	}
	if !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("missing -hls_playlist_type vod; args: %v", args)
	}
	if !hasPair(args, "-hls_time", "4") {
		t.Errorf("missing -hls_time %d; args: %v", SegmentSeconds, args)
	}
	// Segments and playlist are written INTO the session scratch dir.
	if !hasPair(args, "-hls_segment_filename", "/data/transcode/sess-1/"+SegmentPattern) {
		t.Errorf("hls_segment_filename not under scratch dir; args: %v", args)
	}
	if last := args[len(args)-1]; last != "/data/transcode/sess-1/"+PlaylistName {
		t.Errorf("output playlist = %q, want scratch/%s", last, PlaylistName)
	}
	// Defensive: a remux must not carry any encoder/scale flags (slice 2 territory).
	for _, bad := range []string{"libx264", "-vf", "scale=", "-b:v"} {
		if strings.Contains(joined, bad) {
			t.Errorf("remux args unexpectedly contain %q (should be copy-only); args: %v", bad, args)
		}
	}
}

// fakeRunner is the test seam standing in for FFmpeg: it records the args it was
// asked to run and returns a Job that does nothing, so a unit test can assert the
// command without a real process.
type fakeRunner struct {
	gotArgs []string
	started int
}

func (f *fakeRunner) Start(_ context.Context, args []string) (Job, error) {
	f.gotArgs = args
	f.started++
	return fakeJob{}, nil
}

type fakeJob struct{}

func (fakeJob) Wait() error { return nil }
func (fakeJob) Kill() error { return nil }

// TestRunnerSeamReceivesRemuxArgs confirms the Runner interface is the seam: a
// fake Runner receives exactly the args RemuxArgs produced, so higher layers can
// drive the remux without ffmpeg.
func TestRunnerSeamReceivesRemuxArgs(t *testing.T) {
	var r fakeRunner
	want := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/scratch/s"})
	if _, err := r.Start(context.Background(), want); err != nil {
		t.Fatalf("fake Start: %v", err)
	}
	if r.started != 1 {
		t.Fatalf("started = %d, want 1", r.started)
	}
	if strings.Join(r.gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("runner got %v, want %v", r.gotArgs, want)
	}
}

// hasPair reports whether args contains flag immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// hasFlag reports whether args contains a bare flag.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// valueOf returns the value immediately following flag (or "" if absent).
func valueOf(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// --- TranscodeArgs (slice 2) ---

// TestTranscodeArgsReEncodesVideoAndAudio is the core transcode-tier assertion:
// with both tracks re-encoded, the builder emits libx264 video (scaled to the
// max height, capped to the bitrate) + AAC audio (downmixed to the channel cap),
// all into the same HLS container as remux — and NEVER `-c copy`.
func TestTranscodeArgsReEncodesVideoAndAudio(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/movies/Blade Runner (1982).mkv",
		OutputDir:  "/data/transcode/sess-1",
		Video:      VideoPlan{MaxHeight: 720, MaxBitrate: 4_000_000},
		Audio:      AudioPlan{MaxChannels: 2},
		HasAudio:   true,
	})
	joined := strings.Join(args, " ")

	if !hasPair(args, "-i", "/movies/Blade Runner (1982).mkv") {
		t.Errorf("missing -i <source>; args: %v", args)
	}
	// Video: libx264 (CPU path), NOT copy.
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("missing -c:v libx264; args: %v", args)
	}
	if hasPair(args, "-c:v", "copy") {
		t.Errorf("video unexpectedly copied (should re-encode); args: %v", args)
	}
	// Scale-down filter to the max height, with -2 even width (aspect preserved)
	// and a min() guard so a smaller source is never upscaled.
	scale := valueOf(args, "-vf")
	if !strings.Contains(scale, "scale=-2:") || !strings.Contains(scale, "720") || !strings.Contains(scale, "min(") {
		t.Errorf("-vf = %q, want a scale=-2:'min(720,ih)' downscale filter; args: %v", scale, args)
	}
	// Bitrate cap: -b:v + -maxrate at the cap, -bufsize at 2x.
	if !hasPair(args, "-b:v", "4000000") {
		t.Errorf("missing -b:v 4000000; args: %v", args)
	}
	if !hasPair(args, "-maxrate", "4000000") {
		t.Errorf("missing -maxrate 4000000; args: %v", args)
	}
	if !hasPair(args, "-bufsize", "8000000") {
		t.Errorf("missing -bufsize 8000000 (2x maxrate); args: %v", args)
	}
	// Audio: AAC re-encode with a 2-channel downmix.
	if !hasPair(args, "-c:a", audioEncoderAAC) {
		t.Errorf("missing -c:a aac; args: %v", args)
	}
	if !hasPair(args, "-ac", "2") {
		t.Errorf("missing -ac 2 (downmix); args: %v", args)
	}
	// Same HLS delivery as remux.
	if !hasPair(args, "-f", "hls") || !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("missing HLS output flags; args: %v", args)
	}
	if last := args[len(args)-1]; last != "/data/transcode/sess-1/"+PlaylistName {
		t.Errorf("output playlist = %q, want scratch/%s", last, PlaylistName)
	}
	if !hasPair(args, "-hls_segment_filename", "/data/transcode/sess-1/"+SegmentPattern) {
		t.Errorf("segments not under scratch dir; args: %v", args)
	}
	if strings.Contains(joined, "-c copy") {
		t.Errorf("transcode args contain `-c copy`; args: %v", args)
	}
}

// TestTranscodeArgsAccelCPUByDefault: with no Accel set (the zero value) the
// builder emits the CPU libx264 encoder — the always-available software path
// (ADR-0009: HW accel off by default, CPU is the guaranteed fallback).
func TestTranscodeArgsAccelCPUByDefault(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/data/t/s1",
		Video:      VideoPlan{},
	}) // Accel left as the zero value (AccelCPU)
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("default Accel did not select the CPU libx264 encoder; args: %v", args)
	}
}

// TestTranscodeArgsAccelAutoStillCPU: AccelAuto (HardwareAccel on) still resolves
// to the CPU encoder today — no real HW backend is wired, so the knob is honored
// without changing the output, and the CPU path stays the guaranteed fallback.
func TestTranscodeArgsAccelAutoStillCPU(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/data/t/s1",
		Video:      VideoPlan{},
		Accel:      AccelAuto,
	})
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("AccelAuto did not fall back to the CPU libx264 encoder; args: %v", args)
	}
}

// TestVideoBackendResolution: videoBackend maps each named HW value to its real
// descriptor and the software values — AccelCPU and an un-resolved AccelAuto — back
// to the CPU libx264 descriptor. It pins per-backend expectations for the encoder
// plus the surface-domain shape: ONLY the CPU descriptor has empty initArgs (pure
// software decode); all four HW backends carry a decode -hwaccel (issue 05).
// VideoToolbox and NVENC keep the plain auto-download form (a decode hwaccel but NO
// hwupload — frames return to system memory for the CPU scale + encode), while VAAPI
// and QSV run a full HW-surface pipeline (decode hwaccel WITH -hwaccel_output_format
// + an nv12 hwupload).
func TestVideoBackendResolution(t *testing.T) {
	for _, tc := range []struct {
		accel       Accel
		wantEncoder string
		wantInit    bool // descriptor carries device-init/-hwaccel decode flags before -i
		wantUpload  bool // descriptor moves system-memory frames onto HW surfaces
	}{
		{AccelCPU, videoEncoderLibx264, false, false},
		{AccelAuto, videoEncoderLibx264, false, false},
		{AccelVideoToolbox, videoEncoderVideoToolbox, true, false},
		{AccelNVENC, videoEncoderNVENC, true, false},
		{AccelVAAPI, videoEncoderVAAPI, true, true},
		{AccelQSV, videoEncoderQSV, true, true},
	} {
		be := videoBackend(tc.accel)
		if be.encoder != tc.wantEncoder {
			t.Errorf("videoBackend(%q).encoder = %q, want %q", tc.accel, be.encoder, tc.wantEncoder)
		}
		if gotInit := len(be.initArgs) > 0; gotInit != tc.wantInit {
			t.Errorf("videoBackend(%q) hasInitArgs = %v (%v), want %v", tc.accel, gotInit, be.initArgs, tc.wantInit)
		}
		if gotUpload := be.uploadFilter != ""; gotUpload != tc.wantUpload {
			t.Errorf("videoBackend(%q) hasUploadFilter = %v (%q), want %v", tc.accel, gotUpload, be.uploadFilter, tc.wantUpload)
		}
	}
}

// TestTranscodeArgsAccelVideoToolbox is the core HW-backend assertion: with the
// VideoToolbox knob the builder emits -c:v h264_videotoolbox (NOT libx264/copy),
// honors the scale-down and bitrate cap, and — load-bearing — STILL forces a
// keyframe at every segment boundary so HLS segments stay uniform and seek
// realignment keeps working. The audio branch is unchanged (AAC). No GPU is
// touched — this is the pure arg vector.
func TestTranscodeArgsAccelVideoToolbox(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/movies/Blade Runner (1982).mkv",
		OutputDir:  "/data/transcode/sess-1",
		Video:      VideoPlan{MaxHeight: 720, MaxBitrate: 4_000_000},
		Audio:      AudioPlan{MaxChannels: 2},
		HasAudio:   true,
		Accel:      AccelVideoToolbox,
	})

	// HW DECODE (issue 05): -hwaccel videotoolbox in the plain auto-download form
	// (no -hwaccel_output_format) must appear BEFORE -i so the heavy decode is on the
	// media engine; the rest of the pipeline stays in the CPU domain.
	if !hasPair(args, "-hwaccel", "videotoolbox") {
		t.Errorf("missing -hwaccel videotoolbox (HW decode); args: %v", args)
	}
	if hasFlag(args, "-hwaccel_output_format") {
		t.Errorf("VideoToolbox must use the PLAIN -hwaccel form (auto-download), not -hwaccel_output_format; args: %v", args)
	}
	if i := indexOf(args, "-hwaccel"); i < 0 || i > indexOf(args, "-i") {
		t.Errorf("VideoToolbox -hwaccel must precede -i; args: %v", args)
	}
	// The real HW encoder, not the CPU encoder and not a copy.
	if !hasPair(args, "-c:v", videoEncoderVideoToolbox) {
		t.Errorf("missing -c:v h264_videotoolbox; args: %v", args)
	}
	if hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("VideoToolbox job unexpectedly used libx264; args: %v", args)
	}
	if hasPair(args, "-c:v", "copy") {
		t.Errorf("VideoToolbox job unexpectedly copied video; args: %v", args)
	}
	// VideoToolbox has no libx264-style -preset.
	if hasFlag(args, "-preset") {
		t.Errorf("VideoToolbox job must not carry the libx264 -preset; args: %v", args)
	}
	// CPU-domain scale (system-memory frames), no HW upload filter.
	scale := valueOf(args, "-vf")
	if !strings.Contains(scale, "scale=-2:") || !strings.Contains(scale, "720") || !strings.Contains(scale, "min(") {
		t.Errorf("-vf = %q, want a CPU scale=-2:'min(720,ih)' downscale; args: %v", scale, args)
	}
	if strings.Contains(scale, "hwupload") || strings.Contains(scale, "scale_vaapi") || strings.Contains(scale, "scale_qsv") {
		t.Errorf("VideoToolbox -vf = %q, want no HW upload/scale (system-memory frames); args: %v", scale, args)
	}
	// Bitrate cap honored via -b:v rate control.
	if !hasPair(args, "-b:v", "4000000") {
		t.Errorf("missing -b:v 4000000; args: %v", args)
	}
	if !hasPair(args, "-maxrate", "4000000") || !hasPair(args, "-bufsize", "8000000") {
		t.Errorf("missing -maxrate/-bufsize rate-control; args: %v", args)
	}
	// LOAD-BEARING: segment-boundary keyframe forcing is preserved on the HW path.
	kf := valueOf(args, "-force_key_frames")
	if !strings.Contains(kf, "n_forced") || !strings.Contains(kf, itoa(SegmentSeconds)) {
		t.Errorf("-force_key_frames = %q, want the segment-boundary expr on the HW path; args: %v", kf, args)
	}
	// Audio re-encode unchanged.
	if !hasPair(args, "-c:a", audioEncoderAAC) || !hasPair(args, "-ac", "2") {
		t.Errorf("audio branch changed on the HW path; args: %v", args)
	}
	// Same HLS delivery tail.
	if !hasPair(args, "-f", "hls") || !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("missing HLS output flags; args: %v", args)
	}
}

// assertCapAndKeyframes pins the two cross-backend invariants every HW re-encode
// must keep: the bitrate cap expressed via its native -b:v/-maxrate/-bufsize rate
// control, and — LOAD-BEARING — the segment-boundary keyframe forcing that keeps
// HLS segments uniform so seek realignment stays exact (ADR-0004).
func assertCapAndKeyframes(t *testing.T, args []string, backend string) {
	t.Helper()
	if !hasPair(args, "-b:v", "4000000") || !hasPair(args, "-maxrate", "4000000") || !hasPair(args, "-bufsize", "8000000") {
		t.Errorf("%s: missing -b:v/-maxrate 4000000 + -bufsize 8000000 rate-control; args: %v", backend, args)
	}
	kf := valueOf(args, "-force_key_frames")
	if !strings.Contains(kf, "n_forced") || !strings.Contains(kf, itoa(SegmentSeconds)) {
		t.Errorf("%s: -force_key_frames = %q, want the segment-boundary expr (load-bearing for seek realignment); args: %v", backend, kf, args)
	}
	// Audio + HLS delivery are backend-independent: unchanged on every HW path.
	if !hasPair(args, "-c:a", audioEncoderAAC) || !hasPair(args, "-ac", "2") {
		t.Errorf("%s: audio branch changed on the HW path; args: %v", backend, args)
	}
	if !hasPair(args, "-f", "hls") || !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("%s: missing HLS output flags; args: %v", backend, args)
	}
}

// nvencJob/vaapiJob/qsvJob share the same cap-forcing transcode plan so each
// per-backend test differs ONLY in its expected backend-specific fragments.
func hwTranscodeJob(accel Accel) TranscodeJob {
	return TranscodeJob{
		SourcePath: "/movies/Blade Runner (1982).mkv",
		OutputDir:  "/data/transcode/sess-1",
		Video:      VideoPlan{MaxHeight: 720, MaxBitrate: 4_000_000},
		Audio:      AudioPlan{MaxChannels: 2},
		HasAudio:   true,
		Accel:      accel,
	}
}

// TestTranscodeArgsAccelNVENC: the NVENC knob emits -c:v h264_nvenc with the
// rate-controlled -preset pN, HW DECODE via -hwaccel cuda (issue 05), the CPU-domain
// scale (robust HW-decode → CPU-scale → NVENC baseline — no -init_hw_device, no
// hwupload, no scale_npp), the bitrate cap, and the preserved segment keyframes.
// Pure arg vector — no GPU touched (this Mac has no NVENC; the gated e2e self-skips).
func TestTranscodeArgsAccelNVENC(t *testing.T) {
	args := TranscodeArgs(hwTranscodeJob(AccelNVENC))

	if !hasPair(args, "-c:v", videoEncoderNVENC) {
		t.Errorf("missing -c:v h264_nvenc; args: %v", args)
	}
	if !hasPair(args, "-preset", nvencPreset) {
		t.Errorf("missing NVENC -preset %s; args: %v", nvencPreset, args)
	}
	// HW DECODE: -hwaccel cuda in the PLAIN auto-download form (no
	// -hwaccel_output_format) must precede -i, but the baseline still carries no
	// -init_hw_device, no hwupload, and no scale_npp (decode on GPU, scale on CPU).
	if !hasPair(args, "-hwaccel", "cuda") {
		t.Errorf("missing -hwaccel cuda (HW decode); args: %v", args)
	}
	if i := indexOf(args, "-hwaccel"); i < 0 || i > indexOf(args, "-i") {
		t.Errorf("NVENC -hwaccel must precede -i; args: %v", args)
	}
	if hasFlag(args, "-hwaccel_output_format") || hasFlag(args, "-init_hw_device") {
		t.Errorf("NVENC robust baseline must use plain -hwaccel cuda (no -hwaccel_output_format/-init_hw_device); args: %v", args)
	}
	scale := valueOf(args, "-vf")
	if !strings.Contains(scale, "scale=-2:") || !strings.Contains(scale, "720") {
		t.Errorf("NVENC -vf = %q, want a CPU-domain scale=-2:'min(720,ih)'; args: %v", scale, args)
	}
	if strings.Contains(scale, "hwupload") || strings.Contains(scale, "scale_npp") || strings.Contains(scale, "scale_vaapi") {
		t.Errorf("NVENC -vf = %q, want no upload/HW-scale (CPU-scale baseline); args: %v", scale, args)
	}
	assertCapAndKeyframes(t, args, "NVENC")
}

// TestTranscodeArgsAccelVAAPI: the VAAPI knob emits the device-init flags BEFORE
// -i (-hwaccel vaapi -hwaccel_device <node> -hwaccel_output_format vaapi), -c:v
// h264_vaapi, and a HW-surface -vf chain (format=nv12,hwupload then scale_vaapi),
// while preserving the cap + segment keyframes. Pure arg vector — no VAAPI device
// on this host (the gated e2e self-skips).
func TestTranscodeArgsAccelVAAPI(t *testing.T) {
	args := TranscodeArgs(hwTranscodeJob(AccelVAAPI))

	if !hasPair(args, "-c:v", videoEncoderVAAPI) {
		t.Errorf("missing -c:v h264_vaapi; args: %v", args)
	}
	// Device-init / decode-hwaccel flags MUST precede -i.
	for _, p := range [][2]string{{"-hwaccel", "vaapi"}, {"-hwaccel_device", vaapiRenderNode}, {"-hwaccel_output_format", "vaapi"}} {
		if !hasPair(args, p[0], p[1]) {
			t.Errorf("missing init pair %s %s; args: %v", p[0], p[1], args)
		}
	}
	if i := indexOf(args, "-hwaccel"); i < 0 || i > indexOf(args, "-i") {
		t.Errorf("VAAPI init flags must precede -i; args: %v", args)
	}
	// VAAPI has no libx264-style -preset.
	if hasFlag(args, "-preset") {
		t.Errorf("VAAPI must not carry a libx264 -preset; args: %v", args)
	}
	// HW-surface filter chain: nv12 hwupload then the VAAPI-domain scale.
	if vf := valueOf(args, "-vf"); vf != "format=nv12,hwupload,scale_vaapi=-2:720" {
		t.Errorf("VAAPI -vf = %q, want \"format=nv12,hwupload,scale_vaapi=-2:720\"; args: %v", vf, args)
	}
	assertCapAndKeyframes(t, args, "VAAPI")
}

// TestTranscodeArgsAccelQSV: the QSV knob emits the QSV device-init flags BEFORE -i
// (-init_hw_device qsv=hw -hwaccel qsv -hwaccel_output_format qsv), -c:v h264_qsv,
// and the HW-surface -vf chain (format=nv12,hwupload then scale_qsv), preserving the
// cap + segment keyframes. Pure arg vector — no QSV device here (gated e2e skips).
func TestTranscodeArgsAccelQSV(t *testing.T) {
	args := TranscodeArgs(hwTranscodeJob(AccelQSV))

	if !hasPair(args, "-c:v", videoEncoderQSV) {
		t.Errorf("missing -c:v h264_qsv; args: %v", args)
	}
	for _, p := range [][2]string{{"-init_hw_device", "qsv=hw"}, {"-hwaccel", "qsv"}, {"-hwaccel_output_format", "qsv"}} {
		if !hasPair(args, p[0], p[1]) {
			t.Errorf("missing init pair %s %s; args: %v", p[0], p[1], args)
		}
	}
	if i := indexOf(args, "-init_hw_device"); i < 0 || i > indexOf(args, "-i") {
		t.Errorf("QSV init flags must precede -i; args: %v", args)
	}
	if hasFlag(args, "-preset") {
		t.Errorf("QSV must not carry a libx264 -preset; args: %v", args)
	}
	if vf := valueOf(args, "-vf"); vf != "format=nv12,hwupload,scale_qsv=-2:720" {
		t.Errorf("QSV -vf = %q, want \"format=nv12,hwupload,scale_qsv=-2:720\"; args: %v", vf, args)
	}
	assertCapAndKeyframes(t, args, "QSV")
}

// TestAllHardwareBackendsDecodeOnDevice pins the issue-05 "all four HW backends
// hardware-DECODE" guarantee: each named HW backend's descriptor carries a decode
// -hwaccel (videotoolbox / cuda / vaapi / qsv) in its initArgs, while the CPU
// descriptor carries none (pure software decode — the guaranteed fallback). This is
// the single assertion that keeps a future backend from silently shipping with HW
// encode but software decode (the motivating 4K-HEVC bug).
func TestAllHardwareBackendsDecodeOnDevice(t *testing.T) {
	for _, tc := range []struct {
		accel    Accel
		wantArg  string // the -hwaccel method this backend decodes with
		wantOutF bool   // true if the backend also keeps frames on HW surfaces (-hwaccel_output_format)
	}{
		{AccelVideoToolbox, "videotoolbox", false},
		{AccelNVENC, "cuda", false},
		{AccelVAAPI, "vaapi", true},
		{AccelQSV, "qsv", true},
	} {
		be := videoBackend(tc.accel)
		if !hasPair(be.initArgs, "-hwaccel", tc.wantArg) {
			t.Errorf("videoBackend(%q).initArgs = %v, want a decode -hwaccel %s", tc.accel, be.initArgs, tc.wantArg)
		}
		if gotOutF := hasFlag(be.initArgs, "-hwaccel_output_format"); gotOutF != tc.wantOutF {
			t.Errorf("videoBackend(%q) -hwaccel_output_format present = %v, want %v (plain auto-download vs HW-surface); initArgs: %v", tc.accel, gotOutF, tc.wantOutF, be.initArgs)
		}
	}
	// The CPU fallback is the one descriptor with NO decode hwaccel — software decode.
	if cpu := videoBackend(AccelCPU); hasFlag(cpu.initArgs, "-hwaccel") {
		t.Errorf("CPU backend must carry no -hwaccel (software decode); initArgs: %v", cpu.initArgs)
	}
}

// TestDecodeHwaccelOnlyOnReencodePath pins the load-bearing invariant: the decode
// -hwaccel is emitted ONLY when re-encoding video. A -c:v copy path and an
// audio-only path — even with a HW backend selected — carry NO -hwaccel at all (the
// copy/remux/audio paths are backend-independent and byte-for-byte unchanged).
func TestDecodeHwaccelOnlyOnReencodePath(t *testing.T) {
	// Copied video with a HW backend selected: no decode hwaccel (nothing to decode-
	// for-re-encode), and the args are identical to the AccelCPU copy path.
	hwCopy := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		Video: VideoPlan{Copy: true}, Audio: AudioPlan{Copy: true}, HasAudio: true,
		Accel: AccelVideoToolbox,
	})
	if hasFlag(hwCopy, "-hwaccel") {
		t.Errorf("copy path must carry no -hwaccel even with a HW backend; args: %v", hwCopy)
	}
	cpuCopy := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		Video: VideoPlan{Copy: true}, Audio: AudioPlan{Copy: true}, HasAudio: true,
	})
	if strings.Join(hwCopy, " ") != strings.Join(cpuCopy, " ") {
		t.Errorf("HW-backend copy path differs from CPU copy path (must be backend-independent):\n hw:  %v\n cpu: %v", hwCopy, cpuCopy)
	}

	// Audio-only with a HW backend selected: -vn, no video decode, no -hwaccel.
	hwAudioOnly := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.m4a", OutputDir: "/s",
		AudioOnly: true, HasAudio: true, Audio: AudioPlan{},
		Accel: AccelNVENC,
	})
	if hasFlag(hwAudioOnly, "-hwaccel") {
		t.Errorf("audio-only path must carry no -hwaccel even with a HW backend; args: %v", hwAudioOnly)
	}
	cpuAudioOnly := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.m4a", OutputDir: "/s",
		AudioOnly: true, HasAudio: true, Audio: AudioPlan{},
	})
	if strings.Join(hwAudioOnly, " ") != strings.Join(cpuAudioOnly, " ") {
		t.Errorf("HW-backend audio-only path differs from CPU (must be backend-independent):\n hw:  %v\n cpu: %v", hwAudioOnly, cpuAudioOnly)
	}
}

// TestTranscodeArgsBurnEmbeddedSubtitle: a burn-in job for an EMBEDDED image sub
// overlays the subtitle stream via -filter_complex (the portable, core-ffmpeg path
// — NO libass), -maps the overlay output as the video track, forces a CPU-backend
// re-encode (never -c:v copy, never a HW backend), and keeps the segment-boundary
// keyframe forcing + HLS tail intact.
func TestTranscodeArgsBurnEmbeddedSubtitle(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/movies/Sub Movie (2020).mkv",
		OutputDir:  "/data/transcode/sess-b",
		Video:      VideoPlan{Copy: true}, // would copy — burn-in must override to re-encode
		Audio:      AudioPlan{Copy: true},
		HasAudio:   true,
		Accel:      AccelVideoToolbox, // a HW backend is pinned to CPU for burn-in
		Burn:       &BurnSubtitle{StreamIndex: 1},
	})

	// Re-encode, not copy — you cannot overlay onto a copied stream.
	if hasPair(args, "-c:v", "copy") {
		t.Errorf("burn-in must re-encode video, not copy; args: %v", args)
	}
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("burn-in must use the CPU libx264 encoder (overlay runs in system memory); args: %v", args)
	}
	if hasPair(args, "-c:v", videoEncoderVideoToolbox) {
		t.Errorf("burn-in must pin the CPU backend even with a HW Accel selected; args: %v", args)
	}
	if hasFlag(args, "-hwaccel") {
		t.Errorf("burn-in CPU path must carry no -hwaccel; args: %v", args)
	}
	// The overlay graph reads the embedded track by its subtitle-relative index and
	// its output is mapped as the video track.
	fc := valueOf(args, "-filter_complex")
	if !strings.Contains(fc, "[0:s:1]") || !strings.Contains(fc, "overlay") || !strings.Contains(fc, "[v]") {
		t.Errorf("-filter_complex = %q, want [0:v][0:s:1]overlay[v]; args: %v", fc, args)
	}
	if !hasPair(args, "-map", "[v]") {
		t.Errorf("missing -map [v] for the overlay output; args: %v", args)
	}
	// Audio is mapped explicitly from input 0 (implicit selection is off under
	// -filter_complex/-map).
	if !hasPair(args, "-map", "0:a") {
		t.Errorf("missing -map 0:a for the source audio; args: %v", args)
	}
	// An embedded burn adds NO second input (the sub is in the source container).
	if strings.Count(strings.Join(args, " "), "-i ") != 1 {
		t.Errorf("embedded burn must have exactly one -i; args: %v", args)
	}
	// Load-bearing: keyframe forcing + HLS delivery survive the burn path.
	if kf := valueOf(args, "-force_key_frames"); !strings.Contains(kf, "n_forced") {
		t.Errorf("-force_key_frames = %q, want the segment-boundary expr on the burn path; args: %v", kf, args)
	}
	if !hasPair(args, "-f", "hls") || !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("missing HLS output flags on the burn path; args: %v", args)
	}
}

// TestTranscodeArgsBurnSidecarSubtitle: a burn-in job for a SIDECAR image sub
// (.sup/.idx) adds the sidecar as a SECOND input, overlays [1:s], and scales the
// COMPOSITE down (scale inside the overlay graph, after the overlay).
func TestTranscodeArgsBurnSidecarSubtitle(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/movies/Sub Movie (2020).mkv",
		OutputDir:  "/data/transcode/sess-b",
		Video:      VideoPlan{MaxHeight: 720},
		Burn:       &BurnSubtitle{SidecarPath: "/movies/Sub Movie (2020).de.sup", StreamIndex: -1},
	})
	// The sidecar file is a second -i input.
	if !hasPair(args, "-i", "/movies/Sub Movie (2020).de.sup") {
		t.Errorf("sidecar burn must add the sidecar as a second -i; args: %v", args)
	}
	fc := valueOf(args, "-filter_complex")
	// The sidecar's subtitle stream is input 1 — [1:s], never an embedded si=.
	if !strings.Contains(fc, "[1:s]") || !strings.Contains(fc, "overlay") {
		t.Errorf("-filter_complex = %q, want [0:v][1:s]overlay…; args: %v", fc, args)
	}
	if strings.Contains(fc, "[0:s:") {
		t.Errorf("-filter_complex = %q, sidecar burn must not reference an embedded [0:s:…]; args: %v", fc, args)
	}
	// Overlay precedes the scale so the sub scales WITH the picture and stays aligned.
	overlayIdx := strings.Index(fc, "overlay")
	scaleIdx := strings.Index(fc, "scale=")
	if overlayIdx < 0 || scaleIdx < 0 || overlayIdx > scaleIdx {
		t.Errorf("-filter_complex = %q, want overlay before scale=; args: %v", fc, args)
	}
}

// TestTranscodeArgsNoBurnUnchanged: with no Burn set the args are byte-for-byte the
// non-burn path — the burn extension never perturbs an ordinary transcode.
func TestTranscodeArgsNoBurnUnchanged(t *testing.T) {
	job := TranscodeJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		Video: VideoPlan{MaxHeight: 720}, Audio: AudioPlan{}, HasAudio: true,
	}
	withNilBurn := TranscodeArgs(job)
	job.Burn = nil
	again := TranscodeArgs(job)
	if strings.Join(withNilBurn, " ") != strings.Join(again, " ") {
		t.Errorf("nil-Burn args differ from the plain path; %v vs %v", withNilBurn, again)
	}
	if strings.Contains(strings.Join(withNilBurn, " "), "-filter_complex") {
		t.Errorf("no-burn path must not emit a -filter_complex overlay; args: %v", withNilBurn)
	}
}

// TestCPUFallbackCarriesNoDecodeHwaccel is the decode-failure safety-net assertion
// (issue 05 + issue 03): the per-session fallback flips a failed HW job to AccelCPU,
// and the AccelCPU re-encode path carries NO -hwaccel — so the fallback drops BOTH
// the HW decode and the HW encode and runs fully in software (software decode +
// libx264). A HW DECODE that fails to init surfaces as an early non-zero exit and is
// caught by the playback launch probe (TestHWLaunchFailureFallsBackToCPU); this pins
// that the args it falls back TO are genuinely software end-to-end.
func TestCPUFallbackCarriesNoDecodeHwaccel(t *testing.T) {
	args := TranscodeArgs(hwTranscodeJob(AccelCPU)) // the builder the fallback uses
	if hasFlag(args, "-hwaccel") {
		t.Errorf("CPU fallback re-encode path must carry no -hwaccel (software decode); args: %v", args)
	}
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("CPU fallback must use libx264 (software encode); args: %v", args)
	}
}

// TestTranscodeArgsCPUUnchangedByAccelSeam: the knob-off (CPU) path emits exactly
// the same libx264 arg vector after the descriptor seam as the hand-written
// expectation — no regression. This pins the byte-for-byte CPU output so a future
// backend addition can never silently perturb the always-available software path.
func TestTranscodeArgsCPUUnchangedByAccelSeam(t *testing.T) {
	job := TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Video:      VideoPlan{MaxHeight: 720, MaxBitrate: 4_000_000},
		Audio:      AudioPlan{MaxChannels: 2},
		HasAudio:   true,
	} // Accel zero value = AccelCPU
	want := []string{
		"-nostdin", "-y",
		"-i", "/m/x.mkv",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
		"-force_key_frames", forceKeyFramesExpr,
		"-vf", "scale=-2:'min(720,ih)'",
		"-b:v", "4000000",
		"-maxrate", "4000000",
		"-bufsize", "8000000",
		"-c:a", "aac",
		"-ac", "2",
		"-f", "hls",
		"-hls_time", "4",
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", "/s/" + SegmentPattern,
		"-start_number", "0",
		"/s/" + PlaylistName,
	}
	got := TranscodeArgs(job)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("CPU arg vector changed by the seam:\n got: %v\nwant: %v", got, want)
	}
}

// TestTranscodeArgsNoScaleNoBitrate: a video plan with no caps re-encodes without
// a scale filter or bitrate flags (the source already fits the resolution/bitrate
// — only the codec/container forced the transcode).
func TestTranscodeArgsNoScaleNoBitrate(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Video:      VideoPlan{}, // re-encode, but no down-scale / no bitrate cap
		HasAudio:   false,
	})
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("want libx264; args: %v", args)
	}
	if hasFlag(args, "-vf") {
		t.Errorf("unexpected -vf (no scale needed); args: %v", args)
	}
	if hasFlag(args, "-b:v") {
		t.Errorf("unexpected -b:v (no bitrate cap); args: %v", args)
	}
	// No audio track → no audio codec flags at all.
	if hasFlag(args, "-c:a") {
		t.Errorf("unexpected audio flags for a silent job; args: %v", args)
	}
}

// TestTranscodeArgsCopiesCompatibleTracks: when the plan marks a track as already
// HLS-friendly, the builder copies it (`-c:v copy` / `-c:a copy`) rather than
// re-encoding — the conservative "copy a compatible track" optimization.
func TestTranscodeArgsCopiesCompatibleTracks(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Video:      VideoPlan{Copy: true},
		Audio:      AudioPlan{Copy: true},
		HasAudio:   true,
	})
	if !hasPair(args, "-c:v", "copy") {
		t.Errorf("compatible video not copied; args: %v", args)
	}
	if !hasPair(args, "-c:a", "copy") {
		t.Errorf("compatible audio not copied; args: %v", args)
	}
	// A copied track carries no encoder/scale/downmix flags.
	for _, bad := range []string{"libx264", "-vf", "-b:v", "-ac"} {
		if hasFlag(args, bad) {
			t.Errorf("copied tracks must not carry %q; args: %v", bad, args)
		}
	}
	// Still HLS output.
	if !hasPair(args, "-f", "hls") {
		t.Errorf("missing -f hls; args: %v", args)
	}
}

// TestTranscodeRunnerSeamReceivesArgs confirms the same Runner seam drives a
// transcode job: a fake Runner receives exactly the TranscodeArgs vector.
func TestTranscodeRunnerSeamReceivesArgs(t *testing.T) {
	var r fakeRunner
	want := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		Video: VideoPlan{MaxHeight: 480}, Audio: AudioPlan{MaxChannels: 2}, HasAudio: true,
	})
	if _, err := r.Start(context.Background(), want); err != nil {
		t.Fatalf("fake Start: %v", err)
	}
	if strings.Join(r.gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("runner got %v, want %v", r.gotArgs, want)
	}
}

// --- seek realignment (slice 3): -ss input seek + -start_number ---

// indexOf returns the position of the first occurrence of v in args, or -1.
func indexOf(args []string, v string) int {
	for i, a := range args {
		if a == v {
			return i
		}
	}
	return -1
}

// TestRemuxArgsSeekRealignment: a non-zero SeekOffset injects an INPUT seek
// (`-ss <seconds>` BEFORE -i, for a fast keyframe-accurate seek) and numbers the
// HLS segments from the target index (`-start_number`), so the realigned remux
// produces exactly the sought segment under its playlist name.
func TestRemuxArgsSeekRealignment(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Seek:       SeekOffset{StartNumber: 3, StartSeconds: 12},
	})
	// -ss 12 must appear BEFORE -i (input seek, not output seek).
	ssIdx, iIdx := indexOf(args, "-ss"), indexOf(args, "-i")
	if ssIdx < 0 {
		t.Fatalf("missing -ss for a seeked job; args: %v", args)
	}
	if !hasPair(args, "-ss", "12") {
		t.Errorf("-ss = %q, want 12; args: %v", valueOf(args, "-ss"), args)
	}
	if ssIdx > iIdx {
		t.Errorf("-ss (idx %d) must precede -i (idx %d) for an input seek; args: %v", ssIdx, iIdx, args)
	}
	// First segment numbered at the target index.
	if !hasPair(args, "-start_number", "3") {
		t.Errorf("-start_number = %q, want 3; args: %v", valueOf(args, "-start_number"), args)
	}
	// Still a copy-only remux.
	if !hasPair(args, "-c", "copy") {
		t.Errorf("seeked remux must still copy; args: %v", args)
	}
}

// TestRemuxArgsZeroSeekUnchanged: the zero SeekOffset (the from-the-top launch)
// adds NO -ss and numbers from 0, i.e. it is byte-for-byte the original slice-1
// remux command — realignment must not perturb the initial launch.
func TestRemuxArgsZeroSeekUnchanged(t *testing.T) {
	withZero := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s"})
	if hasFlag(withZero, "-ss") {
		t.Errorf("zero-seek remux must not contain -ss; args: %v", withZero)
	}
	if !hasPair(withZero, "-start_number", "0") {
		t.Errorf("zero-seek remux must number from 0; args: %v", withZero)
	}
}

// TestTranscodeArgsSeekRealignment: a seeked transcode job input-seeks before -i,
// numbers from the target, AND forces a keyframe at every segment boundary — the
// three together make a realigned encode produce exactly the sought segment.
func TestTranscodeArgsSeekRealignment(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Video:      VideoPlan{},
		HasAudio:   true,
		Audio:      AudioPlan{},
		Seek:       SeekOffset{StartNumber: 5, StartSeconds: 20},
	})
	ssIdx, iIdx := indexOf(args, "-ss"), indexOf(args, "-i")
	if ssIdx < 0 || ssIdx > iIdx {
		t.Errorf("-ss must appear before -i for an input seek; args: %v", args)
	}
	if !hasPair(args, "-ss", "20") {
		t.Errorf("-ss = %q, want 20; args: %v", valueOf(args, "-ss"), args)
	}
	if !hasPair(args, "-start_number", "5") {
		t.Errorf("-start_number = %q, want 5; args: %v", valueOf(args, "-start_number"), args)
	}
}

// TestTranscodeArgsForcesSegmentKeyframes: a re-encoding transcode forces a
// keyframe at every SegmentSeconds boundary so segments are uniform and seek-
// aligned (the precondition for precise realignment + the server-owned playlist).
func TestTranscodeArgsForcesSegmentKeyframes(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{SourcePath: "/m/x.mkv", OutputDir: "/s", Video: VideoPlan{}})
	kf := valueOf(args, "-force_key_frames")
	if kf == "" {
		t.Fatalf("missing -force_key_frames on a re-encoding transcode; args: %v", args)
	}
	if !strings.Contains(kf, "n_forced") || !strings.Contains(kf, itoa(SegmentSeconds)) {
		t.Errorf("-force_key_frames = %q, want an expr keyed to %d-second boundaries; args: %v", kf, SegmentSeconds, args)
	}
}

// TestTranscodeArgsCopiedVideoHasNoKeyframeForcing: a COPIED video track cannot be
// keyframe-forced (no re-encode), so the flag must be absent — defensive against a
// stray force on a copy path.
func TestTranscodeArgsCopiedVideoHasNoKeyframeForcing(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{SourcePath: "/m/x.mkv", OutputDir: "/s", Video: VideoPlan{Copy: true}})
	if hasFlag(args, "-force_key_frames") {
		t.Errorf("copied video must not carry -force_key_frames; args: %v", args)
	}
}

// --- PlanVideo / PlanAudio (copy-vs-re-encode policy) ---

func TestPlanVideo(t *testing.T) {
	const cap1080 = 1080
	const cap2160 = 2160
	tests := []struct {
		name                 string
		codec                string
		height               int
		bitrate              int64
		clientSupportsSource bool
		copyMaxHeight        int
		reencodeMaxHeight    int
		maxBitrate           int64
		wantCopy             bool
		wantCodec            string
		wantScaleToHeight    int
		wantBitrate          int64
	}{
		{
			name:  "h264 within limits → copy (mpegts, unchanged)",
			codec: "h264", height: 720, bitrate: 3_000_000, clientSupportsSource: true,
			copyMaxHeight: cap1080, reencodeMaxHeight: cap1080, maxBitrate: 8_000_000,
			wantCopy: true, wantCodec: "h264",
		},
		{
			name:  "mpeg4, client can't decode it → re-encode to h264",
			codec: "mpeg4", height: 720, bitrate: 3_000_000, clientSupportsSource: false,
			copyMaxHeight: cap1080, reencodeMaxHeight: cap1080, maxBitrate: 8_000_000,
			wantCopy: false, wantCodec: "h264",
		},
		{
			name:  "h264 over the resolution cap → re-encode + scale",
			codec: "h264", height: 2160, bitrate: 3_000_000, clientSupportsSource: true,
			copyMaxHeight: cap1080, reencodeMaxHeight: cap1080, maxBitrate: 8_000_000,
			wantCopy: false, wantCodec: "h264", wantScaleToHeight: cap1080,
		},
		{
			name:  "h264 over the bitrate cap → re-encode + cap",
			codec: "h264", height: 720, bitrate: 20_000_000, clientSupportsSource: true,
			copyMaxHeight: cap1080, reencodeMaxHeight: cap1080, maxBitrate: 8_000_000,
			wantCopy: false, wantCodec: "h264", wantBitrate: 8_000_000,
		},
		{
			// The BttF case: 4K HEVC, a HEVC-capable client (hevc@2160p), generous
			// bitrate → COPY the HEVC untouched (fMP4), NOT a 4K→1080p h264 re-encode.
			name:  "hevc 4K, client decodes hevc@2160p → copy hevc",
			codec: "hevc", height: 2160, bitrate: 60_000_000, clientSupportsSource: true,
			copyMaxHeight: cap2160, reencodeMaxHeight: cap1080, maxBitrate: 100_000_000,
			wantCopy: true, wantCodec: "hevc",
		},
		{
			// A HEVC file on a client that lacks HEVC → re-encode to h264, downscaled to
			// the h264 ceiling (today's behavior, unchanged).
			name:  "hevc 4K, client lacks hevc → re-encode to h264@1080p",
			codec: "hevc", height: 2160, bitrate: 60_000_000, clientSupportsSource: false,
			copyMaxHeight: cap2160, reencodeMaxHeight: cap1080, maxBitrate: 100_000_000,
			wantCopy: false, wantCodec: "h264", wantScaleToHeight: cap1080,
		},
		{
			// HEVC the client decodes but ABOVE its HEVC ceiling → can't copy (no
			// downscale on a copy) → re-encode to h264.
			name:  "hevc above the client's hevc ceiling → re-encode",
			codec: "hevc", height: 2160, bitrate: 60_000_000, clientSupportsSource: true,
			copyMaxHeight: cap1080, reencodeMaxHeight: cap1080, maxBitrate: 100_000_000,
			wantCopy: false, wantCodec: "h264", wantScaleToHeight: cap1080,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanVideo(tc.codec, tc.height, tc.bitrate, tc.clientSupportsSource, tc.copyMaxHeight, tc.reencodeMaxHeight, tc.maxBitrate)
			if p.Copy != tc.wantCopy {
				t.Errorf("Copy = %v, want %v", p.Copy, tc.wantCopy)
			}
			if p.Codec != tc.wantCodec {
				t.Errorf("Codec = %q, want %q", p.Codec, tc.wantCodec)
			}
			if p.MaxHeight != tc.wantScaleToHeight {
				t.Errorf("MaxHeight = %d, want %d", p.MaxHeight, tc.wantScaleToHeight)
			}
			if p.MaxBitrate != tc.wantBitrate {
				t.Errorf("MaxBitrate = %d, want %d", p.MaxBitrate, tc.wantBitrate)
			}
		})
	}
}

// TestFMP4OutputArgs: the fMP4 flag flips the HLS muxer to fragmented-MP4 (init
// segment + .m4s) for a copied-HEVC video variant, and leaves the MPEG-TS output
// byte-for-byte unchanged when off (ADR-0024).
func TestFMP4OutputArgs(t *testing.T) {
	// Remux of a HEVC mkv (copy everything) → fMP4.
	rem := RemuxArgs(RemuxJob{SourcePath: "/in.mkv", OutputDir: "/s", FMP4: true})
	if !hasPair(rem, "-hls_segment_type", "fmp4") {
		t.Errorf("remux fMP4 args missing -hls_segment_type fmp4: %v", rem)
	}
	if !hasPair(rem, "-hls_fmp4_init_filename", InitSegmentName) {
		t.Errorf("remux fMP4 args missing init filename: %v", rem)
	}
	if !hasPair(rem, "-hls_segment_filename", "/s/"+SegmentPatternFMP4) {
		t.Errorf("remux fMP4 args missing .m4s segment pattern: %v", rem)
	}
	// The MPEG-TS remux is unchanged (no init filename, mpegts, .ts).
	ts := RemuxArgs(RemuxJob{SourcePath: "/in.mkv", OutputDir: "/s"})
	if !hasPair(ts, "-hls_segment_type", "mpegts") || hasFlag(ts, "-hls_fmp4_init_filename") {
		t.Errorf("mpegts remux args changed: %v", ts)
	}

	// A copy-video + transcode-audio transcode (Video.Copy + FMP4) copies the video
	// and re-encodes audio to AAC, delivered as fMP4.
	tc := TranscodeArgs(TranscodeJob{
		SourcePath: "/in.mkv", OutputDir: "/s",
		Video: VideoPlan{Copy: true, Codec: "hevc"},
		Audio: AudioPlan{}, HasAudio: true, FMP4: true,
	})
	if !hasPair(tc, "-c:v", "copy") {
		t.Errorf("copy-video transcode did not copy the video: %v", tc)
	}
	if !hasPair(tc, "-c:a", audioEncoderAAC) {
		t.Errorf("copy-video transcode did not re-encode audio to aac: %v", tc)
	}
	if !hasPair(tc, "-hls_segment_type", "fmp4") || !hasPair(tc, "-hls_fmp4_init_filename", InitSegmentName) {
		t.Errorf("copy-video transcode is not fMP4: %v", tc)
	}

	// A demuxed audio rendition carrying InitName is fMP4 too (matches the variant).
	ar := AudioRenditionArgs(AudioRenditionJob{
		SourcePath: "/in.mkv", OutputDir: "/s", AudioStreamIndex: 1,
		PlaylistName: AudioRenditionPlaylist("abc"), SegmentPattern: AudioRenditionSegmentPatternFMP4("abc"),
		InitName: AudioRenditionInit("abc"),
	})
	if !hasPair(ar, "-hls_segment_type", "fmp4") || !hasPair(ar, "-hls_fmp4_init_filename", "audio_abc_init.mp4") {
		t.Errorf("audio rendition is not fMP4: %v", ar)
	}
	if !hasPair(ar, "-hls_segment_filename", "/s/audio_abc_%03d.m4s") {
		t.Errorf("audio rendition missing .m4s pattern: %v", ar)
	}
}

func TestPlanAudio(t *testing.T) {
	tests := []struct {
		name            string
		codec           string
		channels        int
		clientAAC       bool
		maxChannels     int
		wantCopy        bool
		wantDownmixToCh int
	}{
		{name: "aac within channel cap → copy", codec: "aac", channels: 2, clientAAC: true, maxChannels: 6, wantCopy: true},
		{name: "mp3 → re-encode to aac", codec: "mp3", channels: 2, clientAAC: true, maxChannels: 6, wantCopy: false},
		{name: "aac but client lacks aac → re-encode", codec: "aac", channels: 2, clientAAC: false, maxChannels: 6, wantCopy: false},
		{name: "aac over channel cap → re-encode + downmix", codec: "aac", channels: 6, clientAAC: true, maxChannels: 2, wantCopy: false, wantDownmixToCh: 2},
		{name: "no channel cap → copy aac", codec: "aac", channels: 8, clientAAC: true, maxChannels: 0, wantCopy: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanAudio(tc.codec, tc.channels, tc.clientAAC, tc.maxChannels)
			if p.Copy != tc.wantCopy {
				t.Errorf("Copy = %v, want %v", p.Copy, tc.wantCopy)
			}
			if p.MaxChannels != tc.wantDownmixToCh {
				t.Errorf("MaxChannels = %d, want %d", p.MaxChannels, tc.wantDownmixToCh)
			}
		})
	}
}

// --- explicit audio -map (audio-streams/02) ---------------------------------

// ptrInt is a tiny helper for the optional AudioStreamIndex pointer field.
func ptrInt(n int) *int { return &n }

// countMap counts how many times "-map <spec>" appears in args.
func countMap(args []string, spec string) int {
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-map" && args[i+1] == spec {
			n++
		}
	}
	return n
}

// TestRemuxArgsMapsNegotiatedAudio: a multi-audio remux maps the negotiated audio
// Stream by its audio-relative index AND the video (any -map disables ffmpeg's
// implicit selection) — closing the reported-vs-audible divergence. A single-audio
// remux (nil index) maps nothing and stays byte-for-byte the copy-everything args.
func TestRemuxArgsMapsNegotiatedAudio(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath:       "/movies/Audio Movie (2021).mkv",
		OutputDir:        "/data/transcode/sess-a",
		AudioStreamIndex: ptrInt(1), // the 2nd audio Stream
	})
	if !hasPair(args, "-map", "0:v:0") {
		t.Errorf("missing -map 0:v:0 (video must be mapped once audio is); args: %v", args)
	}
	if !hasPair(args, "-map", "0:a:1") {
		t.Errorf("missing -map 0:a:1 (the negotiated audio Stream); args: %v", args)
	}
	if !hasPair(args, "-c", "copy") {
		t.Errorf("remux must still copy; args: %v", args)
	}

	// Single-audio (nil index): no -map at all — byte-for-byte the original args.
	single := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s"})
	if hasFlag(single, "-map") {
		t.Errorf("single-audio remux must emit no -map; args: %v", single)
	}
	baseline := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s"})
	if strings.Join(single, " ") != strings.Join(baseline, " ") {
		t.Errorf("nil-index remux args drifted from the baseline; %v", single)
	}
}

// TestTranscodeArgsMapsNegotiatedAudioReencode: a multi-audio transcode that
// re-encodes both tracks maps 0:v:0 + 0:a:N so the encoded output carries the
// negotiated audio, not ffmpeg's most-channels pick.
func TestTranscodeArgsMapsNegotiatedAudioReencode(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/Audio Movie.mkv",
		OutputDir:        "/s",
		Video:            VideoPlan{MaxHeight: 720},
		Audio:            AudioPlan{},
		HasAudio:         true,
		AudioStreamIndex: ptrInt(2),
	})
	if !hasPair(args, "-map", "0:v:0") {
		t.Errorf("missing -map 0:v:0; args: %v", args)
	}
	if !hasPair(args, "-map", "0:a:2") {
		t.Errorf("missing -map 0:a:2 (negotiated audio); args: %v", args)
	}
	if !hasPair(args, "-c:v", videoEncoderLibx264) || !hasPair(args, "-c:a", audioEncoderAAC) {
		t.Errorf("re-encode both tracks expected; args: %v", args)
	}
}

// TestTranscodeArgsMapsNegotiatedAudioCopy: the copy path (both tracks already
// HLS-friendly) still maps the negotiated audio + video on a multi-audio File.
func TestTranscodeArgsMapsNegotiatedAudioCopy(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/Audio Movie.mkv",
		OutputDir:        "/s",
		Video:            VideoPlan{Copy: true},
		Audio:            AudioPlan{Copy: true},
		HasAudio:         true,
		AudioStreamIndex: ptrInt(1),
	})
	if !hasPair(args, "-c:v", "copy") {
		t.Errorf("expected -c:v copy; args: %v", args)
	}
	if !hasPair(args, "-map", "0:v:0") || !hasPair(args, "-map", "0:a:1") {
		t.Errorf("copy path must still map 0:v:0 + 0:a:1; args: %v", args)
	}
}

// TestTranscodeArgsAudioOnlyMapsAudio: an audio-only (Music) transcode with a
// selected Stream maps 0:a:N but NO video (there is none; -vn drops it).
func TestTranscodeArgsAudioOnlyMapsAudio(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/album.flac",
		OutputDir:        "/s",
		AudioOnly:        true,
		HasAudio:         true,
		Audio:            AudioPlan{},
		AudioStreamIndex: ptrInt(0),
	})
	if !hasFlag(args, "-vn") {
		t.Errorf("audio-only must drop video with -vn; args: %v", args)
	}
	if hasPair(args, "-map", "0:v:0") {
		t.Errorf("audio-only must not map a video track; args: %v", args)
	}
	if !hasPair(args, "-map", "0:a:0") {
		t.Errorf("missing -map 0:a:0; args: %v", args)
	}
}

// TestTranscodeArgsBurnMapsSelectedAudio: audio selection composes with a subtitle
// burn-in — the burn path maps the CHOSEN audio (0:a:N), not all source audio
// (0:a), while still mapping the overlay output [v] (PRD user story 14).
func TestTranscodeArgsBurnMapsSelectedAudio(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/Audio Movie.mkv",
		OutputDir:        "/s",
		Video:            VideoPlan{Copy: true},
		Audio:            AudioPlan{Copy: true},
		HasAudio:         true,
		Burn:             &BurnSubtitle{StreamIndex: 0},
		AudioStreamIndex: ptrInt(1),
	})
	if !hasPair(args, "-map", "[v]") {
		t.Errorf("burn must map the overlay output [v]; args: %v", args)
	}
	if !hasPair(args, "-map", "0:a:1") {
		t.Errorf("burn+selection must map the chosen audio 0:a:1; args: %v", args)
	}
	if countMap(args, "0:a") != 0 {
		t.Errorf("burn+selection must NOT map all audio (0:a) when a Stream is chosen; args: %v", args)
	}
}

// TestTranscodeArgsNoAudioIndexUnchanged: a nil AudioStreamIndex leaves the args
// byte-for-byte the pre-audio-map path — single-audio Files never regress.
func TestTranscodeArgsNoAudioIndexUnchanged(t *testing.T) {
	job := TranscodeJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		Video: VideoPlan{MaxHeight: 720}, Audio: AudioPlan{}, HasAudio: true,
	}
	withField := TranscodeArgs(job) // AudioStreamIndex nil
	if hasFlag(withField, "-map") {
		t.Errorf("single-audio transcode must emit no -map; args: %v", withField)
	}
}

// --- demuxed multi-audio layout (audio-streams/03) ---

// TestRemuxArgsVideoOnly: the demuxed video variant copies ONLY the video
// (-map 0:v:0 -an -c copy) — no audio in the variant, since each audio Stream
// rides as its own in-band rendition. VideoOnly wins over any AudioStreamIndex.
func TestRemuxArgsVideoOnly(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath: "/movies/Audio Movie (2021).mkv",
		OutputDir:  "/s",
		VideoOnly:  true,
	})
	if !hasPair(args, "-map", "0:v:0") {
		t.Errorf("video-only remux must map the video 0:v:0; args: %v", args)
	}
	if !hasFlag(args, "-an") {
		t.Errorf("video-only remux must drop audio with -an; args: %v", args)
	}
	if countMap(args, "0:a:0") != 0 {
		t.Errorf("video-only remux must map NO audio; args: %v", args)
	}
	if !hasPair(args, "-c", "copy") {
		t.Errorf("video variant is still a copy (-c copy); args: %v", args)
	}
	if last := args[len(args)-1]; last != "/s/"+PlaylistName {
		t.Errorf("video variant still writes the standard playlist; got %q", last)
	}
}

// TestTranscodeArgsVideoOnly: a demuxed video transcode re-encodes video but drops
// audio (-an) and emits no audio codec/map — the audio is delivered as renditions.
func TestTranscodeArgsVideoOnly(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath: "/m/x.mkv",
		OutputDir:  "/s",
		Video:      VideoPlan{MaxHeight: 720},
		Audio:      AudioPlan{MaxChannels: 2},
		HasAudio:   true,
		VideoOnly:  true,
	})
	if !hasPair(args, "-c:v", videoEncoderLibx264) {
		t.Errorf("video-only transcode still re-encodes video; args: %v", args)
	}
	if !hasFlag(args, "-an") {
		t.Errorf("video-only transcode must drop audio with -an; args: %v", args)
	}
	if hasFlag(args, "-c:a") {
		t.Errorf("video-only transcode must emit no audio codec; args: %v", args)
	}
	if countMap(args, "0:a") != 0 || countMap(args, "0:a:0") != 0 {
		t.Errorf("video-only transcode must map no audio; args: %v", args)
	}
}

// TestAudioRenditionArgsCopy: a codec-compatible rendition stream-copies the chosen
// audio Stream (-map 0:a:N -vn -c:a copy) into its own namespaced HLS playlist +
// segments within the shared scratch dir.
func TestAudioRenditionArgsCopy(t *testing.T) {
	args := AudioRenditionArgs(AudioRenditionJob{
		SourcePath:       "/movies/Audio Movie (2021).mkv",
		OutputDir:        "/data/sess-1",
		AudioStreamIndex: 1,
		Copy:             true,
		PlaylistName:     AudioRenditionPlaylist("abc"),
		SegmentPattern:   AudioRenditionSegmentPattern("abc"),
	})
	if !hasPair(args, "-i", "/movies/Audio Movie (2021).mkv") {
		t.Errorf("missing -i <source>; args: %v", args)
	}
	if !hasFlag(args, "-vn") {
		t.Errorf("audio rendition must drop video with -vn; args: %v", args)
	}
	if !hasPair(args, "-map", "0:a:1") {
		t.Errorf("audio rendition must map the chosen Stream 0:a:1; args: %v", args)
	}
	if !hasPair(args, "-c:a", "copy") {
		t.Errorf("compatible-codec rendition must stream-copy; args: %v", args)
	}
	if hasFlag(args, "-ac") {
		t.Errorf("a copy must not downmix; args: %v", args)
	}
	if last := args[len(args)-1]; last != "/data/sess-1/audio_abc.m3u8" {
		t.Errorf("rendition playlist = %q, want the namespaced audio_abc.m3u8", last)
	}
	if !hasPair(args, "-hls_segment_filename", "/data/sess-1/audio_abc_%03d.ts") {
		t.Errorf("rendition segments not namespaced under scratch; args: %v", args)
	}
	if !hasPair(args, "-f", "hls") || !hasPair(args, "-hls_playlist_type", "vod") {
		t.Errorf("rendition must emit the same HLS VOD delivery; args: %v", args)
	}
}

// TestAudioRenditionArgsAAC: an incompatible-codec rendition (DTS, TrueHD)
// re-encodes to AAC with the channel-cap downmix, NOT a copy.
func TestAudioRenditionArgsAAC(t *testing.T) {
	args := AudioRenditionArgs(AudioRenditionJob{
		SourcePath:       "/m/x.mkv",
		OutputDir:        "/s",
		AudioStreamIndex: 2,
		Copy:             false,
		MaxChannels:      2,
		PlaylistName:     AudioRenditionPlaylist("dts"),
		SegmentPattern:   AudioRenditionSegmentPattern("dts"),
	})
	if !hasPair(args, "-map", "0:a:2") {
		t.Errorf("must map the chosen Stream 0:a:2; args: %v", args)
	}
	if !hasPair(args, "-c:a", audioEncoderAAC) {
		t.Errorf("incompatible-codec rendition must re-encode to AAC; args: %v", args)
	}
	if hasPair(args, "-c:a", "copy") {
		t.Errorf("incompatible-codec rendition must not copy; args: %v", args)
	}
	if !hasPair(args, "-ac", "2") {
		t.Errorf("missing -ac 2 downmix; args: %v", args)
	}
}

// --- explicit video -map (selectable-video/01) ------------------------------

// argValue returns the argument immediately following flag (the flag's value), or ""
// when the flag is absent — used to inspect the -filter_complex graph string.
func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// TestRemuxArgsMapsSelectedVideo: a multi-video remux that shares ONE audio maps the
// negotiated video Stream (0:v:N) AND all source audio (0:a) — any -map disables
// ffmpeg's implicit selection, so the shared audio must be mapped or it is dropped.
func TestRemuxArgsMapsSelectedVideo(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath:       "/movies/Spider Noir.mkv",
		OutputDir:        "/s",
		VideoStreamIndex: ptrInt(1), // the 2nd video Stream (the B&W cut)
	})
	if !hasPair(args, "-map", "0:v:1") {
		t.Errorf("missing -map 0:v:1 (the negotiated video Stream); args: %v", args)
	}
	if !hasPair(args, "-map", "0:a") {
		t.Errorf("multi-video remux must map the shared audio (0:a); args: %v", args)
	}
	if !hasPair(args, "-c", "copy") {
		t.Errorf("remux must still copy; args: %v", args)
	}
}

// TestRemuxArgsMapsSelectedVideoAndAudio: a multi-video AND multi-audio muxed remux
// pins both — 0:v:N + 0:a:M — not the bare all-audio map.
func TestRemuxArgsMapsSelectedVideoAndAudio(t *testing.T) {
	args := RemuxArgs(RemuxJob{
		SourcePath: "/m/x.mkv", OutputDir: "/s",
		VideoStreamIndex: ptrInt(1), AudioStreamIndex: ptrInt(2),
	})
	if !hasPair(args, "-map", "0:v:1") || !hasPair(args, "-map", "0:a:2") {
		t.Errorf("must map 0:v:1 + 0:a:2; args: %v", args)
	}
	if countMap(args, "0:a") != 0 {
		t.Errorf("a chosen audio must map 0:a:2, never the bare all-audio 0:a; args: %v", args)
	}
}

// TestRemuxArgsVideoOnlyMapsSelectedVideo: the demuxed video variant maps the chosen
// video (0:v:N), not ffmpeg's implicit first, and still drops audio.
func TestRemuxArgsVideoOnlyMapsSelectedVideo(t *testing.T) {
	args := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s", VideoOnly: true, VideoStreamIndex: ptrInt(1)})
	if !hasPair(args, "-map", "0:v:1") {
		t.Errorf("video-only remux must map the chosen video 0:v:1; args: %v", args)
	}
	if !hasFlag(args, "-an") {
		t.Errorf("video-only remux must drop audio with -an; args: %v", args)
	}
}

// TestTranscodeArgsMapsSelectedVideo: a multi-video transcode that shares one audio
// maps the chosen video (0:v:N) and, because that turns off implicit selection, all
// source audio (0:a) — the re-encoded output carries the negotiated cut.
func TestTranscodeArgsMapsSelectedVideo(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/Spider Noir.mkv",
		OutputDir:        "/s",
		Video:            VideoPlan{MaxHeight: 1080},
		Audio:            AudioPlan{},
		HasAudio:         true,
		VideoStreamIndex: ptrInt(1),
	})
	if !hasPair(args, "-map", "0:v:1") {
		t.Errorf("missing -map 0:v:1 (the negotiated video Stream); args: %v", args)
	}
	if !hasPair(args, "-map", "0:a") {
		t.Errorf("a pinned video must map the shared audio (0:a) or it is dropped; args: %v", args)
	}
}

// TestTranscodeArgsMapsSelectedVideoCopy: the video-copy path (a decodable chosen cut)
// still maps 0:v:N + the shared audio.
func TestTranscodeArgsMapsSelectedVideoCopy(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/x.mkv", OutputDir: "/s",
		Video:            VideoPlan{Copy: true},
		Audio:            AudioPlan{Copy: true},
		HasAudio:         true,
		VideoStreamIndex: ptrInt(2),
	})
	if !hasPair(args, "-c:v", "copy") {
		t.Errorf("expected -c:v copy; args: %v", args)
	}
	if !hasPair(args, "-map", "0:v:2") || !hasPair(args, "-map", "0:a") {
		t.Errorf("copy path must map 0:v:2 + shared audio 0:a; args: %v", args)
	}
}

// TestTranscodeArgsBurnOverlaysSelectedVideo: a burn on a multi-video File overlays
// onto the CHOSEN video's [0:v:N] input, so the reported and burned video agree.
func TestTranscodeArgsBurnOverlaysSelectedVideo(t *testing.T) {
	args := TranscodeArgs(TranscodeJob{
		SourcePath:       "/m/x.mkv", OutputDir: "/s",
		Video:            VideoPlan{Copy: true},
		Audio:            AudioPlan{Copy: true},
		HasAudio:         true,
		Burn:             &BurnSubtitle{StreamIndex: 0},
		VideoStreamIndex: ptrInt(1),
	})
	fc := argValue(args, "-filter_complex")
	if !strings.Contains(fc, "[0:v:1]") {
		t.Errorf("burn on a multi-video File must overlay onto [0:v:1]; graph: %q", fc)
	}
}

// TestVideoMapByteForByteSingleVideo: a nil VideoStreamIndex leaves every path
// byte-for-byte the pre-selectable args — single-video Files never regress. The
// overlay graph in particular keeps its bare [0:v] input (not [0:v:0]).
func TestVideoMapByteForByteSingleVideo(t *testing.T) {
	// Plain remux: no -map at all.
	if base := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s"}); hasFlag(base, "-map") {
		t.Errorf("single-video remux must emit no -map; args: %v", base)
	}
	// Video-only remux with nil index: the implicit first video, 0:v:0.
	if vo := RemuxArgs(RemuxJob{SourcePath: "/m/x.mkv", OutputDir: "/s", VideoOnly: true}); !hasPair(vo, "-map", "0:v:0") {
		t.Errorf("nil-index video-only remux must map 0:v:0; args: %v", vo)
	}
	// Single-video single-audio transcode: no -map.
	tj := TranscodeArgs(TranscodeJob{SourcePath: "/m/x.mkv", OutputDir: "/s", Video: VideoPlan{MaxHeight: 720}, Audio: AudioPlan{}, HasAudio: true})
	if hasFlag(tj, "-map") {
		t.Errorf("single-video single-audio transcode must emit no -map; args: %v", tj)
	}
	// Nil-index burn: bare [0:v], never a numbered [0:v:N].
	burn := TranscodeArgs(TranscodeJob{SourcePath: "/m/x.mkv", OutputDir: "/s", Video: VideoPlan{Copy: true}, Audio: AudioPlan{Copy: true}, HasAudio: true, Burn: &BurnSubtitle{StreamIndex: 0}})
	fc := argValue(burn, "-filter_complex")
	if !strings.Contains(fc, "[0:v]") || strings.Contains(fc, "[0:v:") {
		t.Errorf("nil-index burn must overlay onto the bare [0:v]; graph: %q", fc)
	}
}
