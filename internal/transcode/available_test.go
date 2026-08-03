package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestAvailabilityAbsentBinary: a name no host resolves is UNAVAILABLE, and the
// reason names the binary we looked for — that line is the only thing an operator
// gets when their clients quietly stop offering transcode, so it has to say what
// was missing rather than just "no".
func TestAvailabilityAbsentBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	missing := filepath.Join(t.TempDir(), "definitely-not-ffmpeg")
	got := NewAvailabilityProbe(missing).Resolve(ctx)

	if got.Available {
		t.Fatalf("Available = true for %q, want false", missing)
	}
	if got.Binary != missing {
		t.Errorf("Binary = %q, want the configured %q preserved verbatim", got.Binary, missing)
	}
	if got.Reason == "" {
		t.Error("Reason is empty; the startup log line would say nothing")
	}
}

// TestAvailabilityUnrunnableBinary: a file that EXISTS but cannot execute is
// unavailable too. This is the case a bare existence check would get wrong —
// LookPath is satisfied and the first playback is not — and it is why the probe
// actually runs the binary rather than merely finding it.
func TestAvailabilityUnrunnableBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A non-executable regular file: LookPath rejects a mode-0644 file, and even a
	// host whose lookup is laxer fails to run it. Either way the answer is "no".
	notAnExecutable := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(notAnExecutable, []byte("not a binary"), 0o644); err != nil {
		t.Fatalf("writing the stand-in: %v", err)
	}

	if got := NewAvailabilityProbe(notAnExecutable).Resolve(ctx); got.Available {
		t.Fatalf("Available = true for a non-executable file; reason: %s", got.Reason)
	}
}

// TestRealAvailabilityResolvesFFmpeg is the gated real-host check: on a box with
// ffmpeg on PATH the production probe says yes and leaves Binary empty, which is
// FFmpeg.Binary's "ffmpeg on PATH" convention — the Runner must keep invoking
// exactly what it invoked before this seam existed. It self-skips where ffmpeg is
// absent, which is itself the state the tests above cover.
func TestRealAvailabilityResolvesFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; skipping real-host availability check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	got := NewAvailabilityProbe("").Resolve(ctx)
	if !got.Available {
		t.Errorf("Available = false on a host with ffmpeg on PATH; reason: %s", got.Reason)
	}
	if got.Binary != "" {
		t.Errorf("Binary = %q, want \"\" (ffmpeg on PATH, unchanged from before the seam)", got.Binary)
	}
}

// TestStaticAvailabilityIsPassthrough pins the wiring seam the app and harness
// depend on: whatever is pinned is what the server believes, with no probing.
func TestStaticAvailabilityIsPassthrough(t *testing.T) {
	want := Availability{Available: true, Binary: "/opt/ffmpeg", Reason: "pinned"}
	if got := (StaticAvailability{Availability: want}).Resolve(context.Background()); got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}
