package transcode

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// VideoTraits are the delivery-facing traits of a File's video stream that the HLS
// MASTER playlist must declare honestly for Apple's native player (its authoring
// spec, enforced by Safari):
//
//   - VideoRange: "PQ" (HDR10 / smpte2084) or "HLG" (arib-std-b67), "" for SDR.
//     Safari validates the variant's VIDEO-RANGE against the stream's actual
//     transfer characteristics — an HDR stream under an undeclared (implicitly SDR)
//     variant is KILLED with a bare "failed to decode" the moment the init segment
//     arrives, and a declared PQ variant without RESOLUTION/FRAME-RATE is deemed
//     ineligible and silently never loads (the nothing-plays Safari bug on a 4K
//     HDR remux).
//   - FrameRate: the stream's average frame rate (e.g. 23.976), 0 when unknown.
//
// These are not in the scan-time store (older scans predate the need), so they are
// probed per session — a header-only ffprobe, cheap even on a large remote file.
type VideoTraits struct {
	VideoRange string
	FrameRate  float64
}

// ProbeVideoTraits reads the first video stream's color transfer + average frame
// rate. Best-effort: any failure returns zero traits (SDR/unknown) and the error,
// so the caller can degrade to omitting the master attributes rather than failing
// the request.
func ProbeVideoTraits(ctx context.Context, ffprobeBin, path string) (VideoTraits, error) {
	bin := ffprobeBin
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=color_transfer,avg_frame_rate",
		"-of", "default=noprint_wrappers=1",
		path,
	).Output()
	if err != nil {
		return VideoTraits{}, err
	}
	var t VideoTraits
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "color_transfer":
			switch strings.ToLower(val) {
			case "smpte2084":
				t.VideoRange = "PQ"
			case "arib-std-b67":
				t.VideoRange = "HLG"
			}
		case "avg_frame_rate":
			// ffprobe reports a rational ("24000/1001") or "0/0" when unknown.
			if num, den, ok := strings.Cut(val, "/"); ok {
				n, err1 := strconv.ParseFloat(num, 64)
				d, err2 := strconv.ParseFloat(den, 64)
				if err1 == nil && err2 == nil && d > 0 && n > 0 {
					t.FrameRate = n / d
				}
			}
		}
	}
	return t, nil
}
