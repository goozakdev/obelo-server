package transcode

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// fakeProbe is the test seam standing in for ffmpegProbe: it answers the two
// validation questions from in-memory maps (no ffmpeg process, no GPU) and counts
// calls so a test can assert the two-step short-circuit. A missing key is false,
// modelling "encoder not compiled in" / "test-encode failed".
type fakeProbe struct {
	listed      map[string]bool
	encodes     map[string]bool
	listCalls   int
	encodeCalls int
}

func (f *fakeProbe) encoderListed(_ context.Context, encoder string) bool {
	f.listCalls++
	return f.listed[encoder]
}

func (f *fakeProbe) testEncodeSucceeds(_ context.Context, encoder string) bool {
	f.encodeCalls++
	return f.encodes[encoder]
}

// detectorWith builds a detector wired to a fake probe and a chosen host OS, so
// resolution logic (auto-priority, explicit handling, platform gating) is tested
// with no GPU.
func detectorWith(goos string, p encodeProbe) *FFmpegDetector {
	return &FFmpegDetector{probe: p, goos: goos}
}

// validating is a fakeProbe where the named encoders both list AND test-encode
// successfully (a working device present).
func validating(encoders ...string) *fakeProbe {
	f := &fakeProbe{listed: map[string]bool{}, encodes: map[string]bool{}}
	for _, e := range encoders {
		f.listed[e] = true
		f.encodes[e] = true
	}
	return f
}

// TestResolveOffIsCPU: the off preference (the default) short-circuits to CPU
// WITHOUT probing — the common path never spawns ffmpeg.
func TestResolveOffIsCPU(t *testing.T) {
	p := validating()
	d := detectorWith("darwin", p)
	res := d.Resolve(context.Background(), AccelCPU)
	if res.Accel != AccelCPU {
		t.Errorf("off resolved to %q, want CPU", res.Accel)
	}
	if res.Warn {
		t.Errorf("off must not warn (CPU is by design), got Warn=true: %q", res.Reason)
	}
	if p.listCalls != 0 || p.encodeCalls != 0 {
		t.Errorf("off must not probe ffmpeg; got listCalls=%d encodeCalls=%d", p.listCalls, p.encodeCalls)
	}
}

// TestResolveAutoPicksValidatingCandidate: on darwin, auto selects VideoToolbox
// when it validates — the first (and only wired) candidate in priority order.
func TestResolveAutoPicksValidatingCandidate(t *testing.T) {
	d := detectorWith("darwin", validating(videoEncoderVideoToolbox))
	res := d.Resolve(context.Background(), AccelAuto)
	if res.Accel != AccelVideoToolbox {
		t.Errorf("auto resolved to %q, want %q", res.Accel, AccelVideoToolbox)
	}
	if res.Warn {
		t.Errorf("a successful auto resolution must not warn: %q", res.Reason)
	}
}

// TestResolveAutoNoHardwareIsCPU: auto with no validating candidate resolves to
// CPU and does NOT warn (using CPU when no GPU is present is auto's expected
// behavior, not a misconfig).
func TestResolveAutoNoHardwareIsCPU(t *testing.T) {
	// VideoToolbox is listed but the device test-encode fails: not a working device.
	p := &fakeProbe{
		listed:  map[string]bool{videoEncoderVideoToolbox: true},
		encodes: map[string]bool{}, // test-encode fails
	}
	d := detectorWith("darwin", p)
	res := d.Resolve(context.Background(), AccelAuto)
	if res.Accel != AccelCPU {
		t.Errorf("auto with no working HW resolved to %q, want CPU", res.Accel)
	}
	if res.Warn {
		t.Errorf("auto→CPU must not warn: %q", res.Reason)
	}
}

// TestResolveAutoLinuxPicksNVENCFirst: on linux, auto walks NVENC → VAAPI → QSV in
// priority order, so with all three validating it selects NVENC (discrete NVIDIA
// first) and never reaches the lower-priority candidates.
func TestResolveAutoLinuxPicksNVENCFirst(t *testing.T) {
	d := detectorWith("linux", validating(videoEncoderNVENC, videoEncoderVAAPI, videoEncoderQSV))
	res := d.Resolve(context.Background(), AccelAuto)
	if res.Accel != AccelNVENC {
		t.Errorf("linux auto resolved to %q, want %q (NVENC has top priority)", res.Accel, AccelNVENC)
	}
	if res.Warn {
		t.Errorf("a successful auto resolution must not warn: %q", res.Reason)
	}
}

// TestResolveAutoLinuxPriorityOrder: when a higher-priority backend does not
// validate, auto falls through to the next in NVENC → VAAPI → QSV order.
func TestResolveAutoLinuxPriorityOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid []string
		want  Accel
	}{
		{"no nvenc → vaapi", []string{videoEncoderVAAPI, videoEncoderQSV}, AccelVAAPI},
		{"only qsv → qsv", []string{videoEncoderQSV}, AccelQSV},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := detectorWith("linux", validating(tc.valid...))
			res := d.Resolve(context.Background(), AccelAuto)
			if res.Accel != tc.want {
				t.Errorf("linux auto resolved to %q, want %q", res.Accel, tc.want)
			}
		})
	}
}

// TestResolveAutoLinuxNoHardwareIsCPU: linux auto with no validating candidate
// resolves to CPU and does NOT warn (using CPU when no GPU is present is auto's
// expected behavior). It probes the candidates (unlike a platform with none).
func TestResolveAutoLinuxNoHardwareIsCPU(t *testing.T) {
	p := validating() // nothing lists/validates
	d := detectorWith("linux", p)
	res := d.Resolve(context.Background(), AccelAuto)
	if res.Accel != AccelCPU {
		t.Errorf("linux auto with no working HW resolved to %q, want CPU", res.Accel)
	}
	if res.Warn {
		t.Errorf("auto→CPU must not warn: %q", res.Reason)
	}
	if p.listCalls == 0 {
		t.Errorf("linux auto should probe its candidates; listCalls=%d", p.listCalls)
	}
}

// TestResolveExplicitPresentUsed: an explicitly-configured, wired backend that
// validates is used as-is.
func TestResolveExplicitPresentUsed(t *testing.T) {
	d := detectorWith("darwin", validating(videoEncoderVideoToolbox))
	res := d.Resolve(context.Background(), AccelVideoToolbox)
	if res.Accel != AccelVideoToolbox {
		t.Errorf("explicit videotoolbox resolved to %q, want %q", res.Accel, AccelVideoToolbox)
	}
	if res.Warn {
		t.Errorf("a validated explicit backend must not warn: %q", res.Reason)
	}
}

// TestResolveExplicitMissingFallsBackToCPU: an explicit backend whose encoder is
// absent logs a loud warning and resolves to CPU (server still boots and plays).
func TestResolveExplicitMissingFallsBackToCPU(t *testing.T) {
	d := detectorWith("darwin", validating()) // nothing listed/validates
	res := d.Resolve(context.Background(), AccelVideoToolbox)
	if res.Accel != AccelCPU {
		t.Errorf("explicit missing backend resolved to %q, want CPU", res.Accel)
	}
	if !res.Warn {
		t.Errorf("explicit-but-unvalidated backend must warn loudly; Warn=false: %q", res.Reason)
	}
}

// TestResolveExplicitPresentButTestEncodeFails: the two-step validation in action
// — the encoder is COMPILED IN but the test-encode FAILS (no working device), so
// the backend is treated as unavailable and falls back to CPU with a warning.
func TestResolveExplicitPresentButTestEncodeFails(t *testing.T) {
	p := &fakeProbe{
		listed:  map[string]bool{videoEncoderVideoToolbox: true},
		encodes: map[string]bool{}, // device test-encode fails
	}
	d := detectorWith("darwin", p)
	res := d.Resolve(context.Background(), AccelVideoToolbox)
	if res.Accel != AccelCPU {
		t.Errorf("present-but-no-device resolved to %q, want CPU", res.Accel)
	}
	if !res.Warn {
		t.Errorf("present-but-no-device must warn: %q", res.Reason)
	}
	if p.listCalls != 1 || p.encodeCalls != 1 {
		t.Errorf("two-step validation should list then test-encode once each; got listCalls=%d encodeCalls=%d", p.listCalls, p.encodeCalls)
	}
}

// TestValidationShortCircuitsTestEncode: when the encoder is not even listed, the
// (more expensive) test-encode is never run — the two steps short-circuit.
func TestValidationShortCircuitsTestEncode(t *testing.T) {
	p := &fakeProbe{listed: map[string]bool{}, encodes: map[string]bool{}}
	d := detectorWith("darwin", p)
	d.Resolve(context.Background(), AccelVideoToolbox)
	if p.encodeCalls != 0 {
		t.Errorf("test-encode must not run when the encoder is unlisted; encodeCalls=%d", p.encodeCalls)
	}
}

// TestResolveExplicitWiredHWBackendsUsed: each of the three Linux GPU backends,
// now wired into the args builder, is used as-is when its encoder validates — the
// detector routes an explicit, validated selection straight to the real backend.
func TestResolveExplicitWiredHWBackendsUsed(t *testing.T) {
	for _, a := range []Accel{AccelNVENC, AccelVAAPI, AccelQSV} {
		d := detectorWith("linux", validating(hwEncoders[a]))
		res := d.Resolve(context.Background(), a)
		if res.Accel != a {
			t.Errorf("explicit %q resolved to %q, want %q", a, res.Accel, a)
		}
		if res.Warn {
			t.Errorf("a validated explicit %q must not warn: %q", a, res.Reason)
		}
	}
}

// TestResolveExplicitHWBackendUnvalidatedFallsBack: each GPU backend whose encoder
// does NOT validate (absent or no working device) warns loudly and falls back to
// CPU, so a misconfigured/absent GPU never takes down playback.
func TestResolveExplicitHWBackendUnvalidatedFallsBack(t *testing.T) {
	for _, a := range []Accel{AccelNVENC, AccelVAAPI, AccelQSV} {
		d := detectorWith("linux", validating()) // nothing validates
		res := d.Resolve(context.Background(), a)
		if res.Accel != AccelCPU {
			t.Errorf("unvalidated explicit %q resolved to %q, want CPU", a, res.Accel)
		}
		if !res.Warn {
			t.Errorf("unvalidated explicit %q must warn: %q", a, res.Reason)
		}
	}
}

// TestRealDetectorValidatesVideoToolbox is the gated real-host check: it drives
// the production detector (real ffmpegProbe shelling out to ffmpeg) and confirms
// that on a macOS box with VideoToolbox, `auto` resolves to AccelVideoToolbox.
// It self-skips where the backend is absent (non-darwin, no ffmpeg on PATH, or a
// host whose VideoToolbox device doesn't actually encode) so CI without the
// hardware stays green — the unit tests above carry the resolution logic.
func TestRealDetectorValidatesVideoToolbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("VideoToolbox is macOS-only; skipping real-host detection on " + runtime.GOOS)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping real-host detection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	d := NewDetector("")
	// Validate directly first so an absent device produces a clear skip rather than
	// a confusing auto→CPU failure.
	if !d.validate(ctx, videoEncoderVideoToolbox) {
		t.Skip("h264_videotoolbox did not validate on this host (no working device); skipping")
	}
	res := d.Resolve(ctx, AccelAuto)
	if res.Accel != AccelVideoToolbox {
		t.Errorf("auto on a VideoToolbox host resolved to %q, want %q (reason: %s)", res.Accel, AccelVideoToolbox, res.Reason)
	}
	if res.Warn {
		t.Errorf("a validated auto resolution must not warn: %q", res.Reason)
	}
	// And an explicit request for it is honored.
	if got := d.Resolve(ctx, AccelVideoToolbox); got.Accel != AccelVideoToolbox {
		t.Errorf("explicit videotoolbox resolved to %q, want %q", got.Accel, AccelVideoToolbox)
	}
}

// TestResolutionRequestedEchoed: the Resolution echoes the operator's preference
// for the startup log line, regardless of the resolved backend.
func TestResolutionRequestedEchoed(t *testing.T) {
	d := detectorWith("darwin", validating())
	res := d.Resolve(context.Background(), AccelVideoToolbox)
	if res.Requested != AccelVideoToolbox {
		t.Errorf("Requested = %q, want %q", res.Requested, AccelVideoToolbox)
	}
	if res.Reason == "" {
		t.Error("Resolution must carry a non-empty Reason for the startup log line")
	}
}
