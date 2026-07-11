package transcode

// Setup-time hardware-accel detection & validation (ADR-0009: "detection/
// validation is a setup-time concern, not per-stream"). The server resolves the
// configured HardwareAccel preference to a CONCRETE, VALIDATED Accel ONCE at
// startup and hands that already-resolved value to the playback Service; per-
// stream code never re-detects. A misconfigured or absent backend can therefore
// never take down playback — the detector warns and falls back to the always-
// available CPU libx264 path (never fail-fast).
//
// The seam mirrors the scanner's Prober (internal/scanner/ffprobe.go): an
// encodeProbe interface is the fakeable ffmpeg-validation boundary the real
// ffmpegProbe satisfies by shelling out, and that detector unit tests fake so the
// resolution logic (auto-priority, explicit-present, explicit-missing→CPU,
// present-but-no-working-device→CPU, platform gating) is exercised with no GPU.

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// Hardware H.264 encoder names the detector validates. All four backends are now
// wired into the args builder (see videoBackend in ffmpeg.go): VideoToolbox on
// darwin, and NVENC/VAAPI/QSV on linux.
const (
	videoEncoderNVENC = "h264_nvenc"
	videoEncoderVAAPI = "h264_vaapi"
	videoEncoderQSV   = "h264_qsv"
)

// hwEncoders maps each hardware Accel to the ffmpeg `-c:v` encoder the detector
// validates for it. AccelCPU and AccelAuto are absent: CPU needs no validation
// (it is the guaranteed fallback) and auto is resolved through the platform
// candidate list rather than a single encoder.
var hwEncoders = map[Accel]string{
	AccelNVENC:        videoEncoderNVENC,
	AccelVAAPI:        videoEncoderVAAPI,
	AccelQSV:          videoEncoderQSV,
	AccelVideoToolbox: videoEncoderVideoToolbox,
}

// wiredHWBackends are the hardware backends videoBackend can actually emit real
// ffmpeg args for. The detector NEVER resolves to a backend outside this set —
// even if its encoder validates on the host — because the args builder would
// silently fall it back to CPU at encode time. All four named backends are wired,
// so a validated explicit/auto selection routes to the real descriptor.
var wiredHWBackends = map[Accel]bool{
	AccelVideoToolbox: true,
	AccelNVENC:        true,
	AccelVAAPI:        true,
	AccelQSV:          true,
}

// hwCandidate is one hardware backend `auto` may select: the Accel to resolve to
// and the ffmpeg encoder the detector validates for it.
type hwCandidate struct {
	accel   Accel
	encoder string
}

// autoCandidates is the platform-appropriate priority order `auto` probes: the
// first candidate that validates wins, else CPU. Only WIRED backends appear here
// (auto never resolves to a backend the args builder can't emit). On darwin that
// is VideoToolbox; on linux it is NVENC → VAAPI → QSV — discrete NVIDIA first, then
// the VAAPI generic path (Intel/AMD), then Intel Quick Sync — so a box with an
// NVIDIA card uses NVENC while an Intel-only box falls through to VAAPI/QSV. Any
// other OS has no wired HW candidate and resolves to CPU.
func autoCandidates(goos string) []hwCandidate {
	switch goos {
	case "darwin":
		return []hwCandidate{{AccelVideoToolbox, videoEncoderVideoToolbox}}
	case "linux":
		return []hwCandidate{
			{AccelNVENC, videoEncoderNVENC},
			{AccelVAAPI, videoEncoderVAAPI},
			{AccelQSV, videoEncoderQSV},
		}
	default:
		// Every other OS: no wired HW candidate.
		return nil
	}
}

// Resolution is the outcome of setup-time backend detection: the concrete,
// validated Accel the transcode tier will use, plus context for the startup log
// line. Accel is AccelCPU whenever nothing validated — off, an explicit backend
// that failed validation, or auto with no working hardware — which is the always-
// available software path.
type Resolution struct {
	// Accel is the resolved, validated backend the playback Service receives.
	Accel Accel
	// Requested is the operator's configured preference, echoed for the log line.
	Requested Accel
	// Reason is a human-readable explanation of the resolution for the startup log.
	Reason string
	// Warn is true when the operator explicitly asked for hardware acceleration but
	// the server fell back to CPU (an explicit backend that did not validate or is
	// not yet wired). The app logs this LOUDLY — ADR-0009: warn + fall back, never
	// fail-fast — so a HW misconfig is visible without taking down playback. It is
	// false for `off` (CPU by design) and for `auto` falling back to CPU (using CPU
	// when no GPU is present is auto's expected, non-alarming behavior).
	Warn bool
}

// Detector resolves a configured HardwareAccel preference to a concrete,
// validated Accel at startup. It is an interface (with a fake in tests, mirroring
// Prober) so the app can be wired against it and the resolution logic verified
// without a GPU.
type Detector interface {
	// Resolve maps the preference (AccelCPU/AccelAuto/an explicit backend) to a
	// validated Resolution. It is called ONCE at startup and must never fail-fast:
	// any unvalidatable preference resolves to AccelCPU.
	Resolve(ctx context.Context, pref Accel) Resolution
}

// StaticDetector is a Detector that always returns a fixed Resolution, ignoring
// the preference and never touching a GPU. It lets the app be wired against a
// pinned backend outcome — the test/harness seam for exercising the observability
// projection (degraded/active/reason) deterministically on a GPU-less CI box,
// mirroring how the fakeProbe pins encode validation.
type StaticDetector struct{ Resolution Resolution }

// Resolve implements Detector, returning the pinned Resolution unchanged.
func (d StaticDetector) Resolve(context.Context, Accel) Resolution { return d.Resolution }

// FFmpegDetector is the production Detector: it validates candidate backends by
// shelling out to ffmpeg through an encodeProbe, and gates `auto`/the candidate
// list on the host OS.
type FFmpegDetector struct {
	probe encodeProbe
	goos  string
}

// NewDetector builds the production detector. ffmpegBinary names the ffmpeg
// executable (empty defaults to "ffmpeg" on PATH, matching FFmpeg/FFprobe), and
// the platform is the running host's GOOS.
func NewDetector(ffmpegBinary string) *FFmpegDetector {
	return &FFmpegDetector{probe: ffmpegProbe{binary: ffmpegBinary}, goos: runtime.GOOS}
}

// Resolve implements Detector. Off short-circuits to CPU without probing (so the
// common path never spawns ffmpeg); auto walks the platform candidate list; an
// explicit backend is validated and used, or warned-and-fallen-back to CPU.
func (d *FFmpegDetector) Resolve(ctx context.Context, pref Accel) Resolution {
	switch pref {
	case AccelCPU:
		return Resolution{
			Accel:     AccelCPU,
			Requested: AccelCPU,
			Reason:    "hardware acceleration off; using CPU libx264",
		}
	case AccelAuto:
		return d.resolveAuto(ctx)
	default:
		return d.resolveExplicit(ctx, pref)
	}
}

// resolveAuto probes the platform candidates in priority order and selects the
// first that validates; if none validate (or the platform has no wired candidate)
// it resolves to CPU. Falling back to CPU under auto is expected, not a misconfig,
// so it never sets Warn.
func (d *FFmpegDetector) resolveAuto(ctx context.Context) Resolution {
	for _, c := range autoCandidates(d.goos) {
		if d.validate(ctx, c.encoder) {
			return Resolution{
				Accel:     c.accel,
				Requested: AccelAuto,
				Reason:    fmt.Sprintf("auto-detected hardware encoder %s (%s validated)", c.accel, c.encoder),
			}
		}
	}
	return Resolution{
		Accel:     AccelCPU,
		Requested: AccelAuto,
		Reason:    "auto found no working hardware encoder; using CPU libx264",
	}
}

// resolveExplicit honors an operator-named backend: it validates the encoder
// (two-step) and uses the backend only when it both validates AND is wired into
// the args builder. Anything else is a loud warning + CPU (never fail-fast): a
// backend that didn't validate (encoder missing or no working device) or — a
// defensive case now that all four backends are wired — one not present in
// wiredHWBackends.
func (d *FFmpegDetector) resolveExplicit(ctx context.Context, pref Accel) Resolution {
	enc, known := hwEncoders[pref]
	if !known {
		// Defensive: config never produces an unrecognized Accel, but a stray value
		// resolves safely to CPU rather than reaching the args builder.
		return Resolution{
			Accel:     AccelCPU,
			Requested: pref,
			Warn:      true,
			Reason:    fmt.Sprintf("unknown hardware backend %q; using CPU libx264", string(pref)),
		}
	}
	// validate-then-fall-back: probe the encoder even for not-yet-wired backends so
	// the log states honestly whether the host could run it.
	validated := d.validate(ctx, enc)
	if validated && wiredHWBackends[pref] {
		return Resolution{
			Accel:     pref,
			Requested: pref,
			Reason:    fmt.Sprintf("using configured hardware backend %s (%s validated)", pref, enc),
		}
	}
	var reason string
	switch {
	case !wiredHWBackends[pref]:
		reason = fmt.Sprintf("configured backend %s is not yet supported on this build (encoder validated=%t); falling back to CPU libx264", pref, validated)
	default:
		reason = fmt.Sprintf("configured backend %s did not validate (encoder missing or no working device); falling back to CPU libx264", pref)
	}
	return Resolution{Accel: AccelCPU, Requested: pref, Warn: true, Reason: reason}
}

// validate is the two-step setup-time check (ADR-0009): the encoder must be
// (1) compiled into ffmpeg AND (2) able to run a tiny real test-encode. Step 2 is
// what separates "ffmpeg was built with this encoder" from "a working device is
// actually present", so a paper-supported backend with no usable hardware is
// treated as unavailable. The list check short-circuits the test-encode.
func (d *FFmpegDetector) validate(ctx context.Context, encoder string) bool {
	return d.probe.encoderListed(ctx, encoder) && d.probe.testEncodeSucceeds(ctx, encoder)
}

// encodeProbe is the fakeable validation boundary the Detector resolves through:
// for one ffmpeg encoder it answers the two questions that together mean "this
// host can really hardware-encode with it". The real ffmpegProbe shells out to
// ffmpeg; detector unit tests fake it so resolution runs with no GPU.
type encodeProbe interface {
	// encoderListed reports whether `encoder` is compiled into ffmpeg (appears in
	// `ffmpeg -hide_banner -encoders`).
	encoderListed(ctx context.Context, encoder string) bool
	// testEncodeSucceeds reports whether a tiny real test-encode with `encoder`
	// completes successfully (a working device is present).
	testEncodeSucceeds(ctx context.Context, encoder string) bool
}

// ffmpegProbe is the production encodeProbe: it invokes the ffmpeg binary. Binary
// names the executable so a deployment/test can point at an absolute path; empty
// defaults to "ffmpeg" on PATH (mirrors FFmpeg/FFprobe).
type ffmpegProbe struct {
	binary string
}

func (p ffmpegProbe) bin() string {
	if p.binary == "" {
		return "ffmpeg"
	}
	return p.binary
}

// encoderListed runs `ffmpeg -hide_banner -encoders` and reports whether the
// encoder name appears as a token in the listing. A non-zero exit / missing
// binary reports false (treated as unavailable, never an error that blocks boot).
func (p ffmpegProbe) encoderListed(ctx context.Context, encoder string) bool {
	out, err := exec.CommandContext(ctx, p.bin(), "-hide_banner", "-encoders").Output()
	if err != nil {
		return false
	}
	// The listing is one encoder per line: " V....D h264_videotoolbox  ...". Match
	// the encoder as a whitespace-delimited field so a name is never a substring of
	// another.
	for _, field := range bytes.Fields(out) {
		if string(field) == encoder {
			return true
		}
	}
	return false
}

// testEncodeSucceeds runs the tiny real test-encode
//
//	ffmpeg -hide_banner -nostdin -f lavfi -i testsrc=... -frames:v 1 -c:v <encoder> -f null -
//
// which exercises the actual device/encoder path and discards the output. A clean
// exit means a working encoder is present; any failure (no device, init error,
// missing binary, context timeout) reports false.
func (p ffmpegProbe) testEncodeSucceeds(ctx context.Context, encoder string) bool {
	cmd := exec.CommandContext(ctx, p.bin(),
		"-hide_banner", "-nostdin",
		"-f", "lavfi", "-i", "testsrc=size=256x144:rate=1:duration=1",
		"-frames:v", "1",
		"-c:v", encoder,
		"-f", "null", "-",
	)
	return cmd.Run() == nil
}
