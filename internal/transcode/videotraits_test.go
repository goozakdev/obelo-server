package transcode

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ProbeVideoTraits must report the HEVC profile + level_idc so the master's CODECS can
// name the real stream (Main 10 → hvc1.2.4), not a generic Main string.
func TestProbeVideoTraitsReportsHEVCProfileAndLevel(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg has no libx265")
	}
	path := filepath.Join(t.TempDir(), "hdr.mkv")
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=24",
		"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:log-level=none", path).CombinedOutput(); err != nil {
		t.Skipf("cannot build fixture: %v: %s", err, out)
	}
	tr, err := ProbeVideoTraits(context.Background(), "", path)
	if err != nil {
		t.Fatal(err)
	}
	if tr.VideoRange != "PQ" {
		t.Errorf("VideoRange = %q, want PQ", tr.VideoRange)
	}
	if tr.Profile != "Main 10" {
		t.Errorf("Profile = %q, want Main 10", tr.Profile)
	}
	if tr.Level != 60 {
		t.Errorf("Level = %d, want 60 (320x240 → level 2.0)", tr.Level)
	}
}
