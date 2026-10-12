package seekpreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/goozakdev/obelo-server/internal/transcode"
)

// FFmpeg extracts with the `ffmpeg` binary. Binary and Probe name the executables;
// empty means "ffmpeg" / "ffprobe" on PATH.
//
// A frame is the keyframe at or before the asked time (-noaccurate_seek: no
// decoding forward to the exact frame — ADR-0070 §2), decoded on the CPU, at the
// lowest CPU priority the host allows: nobody is waiting for it.
type FFmpeg struct {
	Binary string
	Probe  string
	// HasFilter and IsHDR answer what this ffmpeg can do and what a File is; nil
	// means ask the real ffmpeg / ffprobe. Tests inject them.
	HasFilter func(ctx context.Context, name string) (bool, error)
	IsHDR     func(ctx context.Context, path string) (bool, error)

	mu      sync.Mutex
	lastKey string
	lastHDR bool
	zscale  bool // zscale was found; a "not found" is never kept
}

// keyframeSlackMs is added to the time asked for before seeking. ffmpeg's
// demuxers seek to the keyframe strictly before the target (Matroska by a little
// more), so a keyframe exactly on the asked time would be passed over for the one
// before it. The slack makes "at or before" inclusive. It is several frames at a
// real frame rate (about six at 24 fps) but a small margin against the NEXT
// keyframe, which a real GOP puts seconds away; a keyframe within it is taken a
// fraction of a second late.
const keyframeSlackMs = 250

// scaleFilter makes the frame FrameWidth wide and as tall as the display aspect
// says (the stored aspect times the sample aspect ratio), even-rounded, then
// declares square pixels. The 320 is FrameWidth.
const scaleFilter = "scale=320:trunc(320/dar/2)*2,setsar=1"

// toneMapFilter takes HDR (PQ or HLG) to SDR BT.709 before scaling.
const toneMapFilter = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// frameArgs is the ffmpeg command line for one frame. toneMap says to tone-map
// HDR to SDR first.
func frameArgs(path string, atMs int64, toneMap bool) []string {
	vf := scaleFilter
	if toneMap {
		vf = toneMapFilter + "," + vf
	}
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-noaccurate_seek", "-ss", strconv.FormatFloat(float64(atMs+keyframeSlackMs)/1000, 'f', 3, 64), "-i", path,
		"-map", "0:v:0", "-an", "-sn", "-dn", "-frames:v", "1",
		"-vf", vf, "-c:v", "png", "-f", "image2pipe", "pipe:1",
	}
}

// Frame implements Extractor.
func (f *FFmpeg) Frame(ctx context.Context, path string, atMs int64) (image.Image, error) {
	bin := f.Binary
	if bin == "" {
		bin = "ffmpeg"
	}
	toneMap, err := f.toneMap(ctx, path)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, frameArgs(path, atMs, toneMap)...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	lowerPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("%w at %d ms", ErrNoFrame, atMs)
	}
	img, err := png.Decode(&out)
	if err != nil {
		return nil, fmt.Errorf("decoding frame: %w", err)
	}
	return img, nil
}

// toneMap reports whether frames of path need tone-mapping. An HDR File on an
// ffmpeg without zscale (libzimg, which not every build has) is
// ErrToneMapUnavailable, never a washed-out frame; a failed HDR check or zscale
// probe is an error too. Nothing negative is remembered: a File's HDR-ness is
// kept only for that File as it is on disk now (path, mtime and size), so one
// replaced in place is checked again, and a missing zscale is looked for again on
// every attempt, so an ffmpeg upgraded in place is seen by the next Retry. (A
// File without it fails on its first frame, so that is one probe per attempt.)
func (f *FFmpeg) toneMap(ctx context.Context, path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := ""
	fi, statErr := os.Stat(path)
	if statErr == nil {
		key = fmt.Sprintf("%s|%d|%d", path, fi.ModTime().UnixNano(), fi.Size())
	}
	if errors.Is(statErr, fs.ErrNotExist) && f.IsHDR == nil {
		return false, ErrFileNotFound // the real ffprobe would only say "exit 1"
	}
	if key == "" || key != f.lastKey {
		hdr, err := f.isHDR(ctx, path)
		if err != nil {
			f.lastKey = ""
			return false, fmt.Errorf("checking for HDR: %w", err)
		}
		f.lastKey, f.lastHDR = key, hdr
	}
	if !f.lastHDR {
		return false, nil
	}
	if !f.zscale {
		has, err := f.hasFilter(ctx, "zscale")
		if err != nil {
			return false, fmt.Errorf("probing ffmpeg for zscale: %w", err)
		}
		if !has {
			return false, ErrToneMapUnavailable
		}
		f.zscale = true
	}
	return true, nil
}

func (f *FFmpeg) isHDR(ctx context.Context, path string) (bool, error) {
	if f.IsHDR != nil {
		return f.IsHDR(ctx, path)
	}
	t, err := transcode.ProbeVideoTraits(ctx, f.Probe, path)
	if err != nil {
		return false, fmt.Errorf("ffprobe: %w", err)
	}
	return t.VideoRange != "", nil
}

func (f *FFmpeg) hasFilter(ctx context.Context, name string) (bool, error) {
	if f.HasFilter != nil {
		return f.HasFilter(ctx, name)
	}
	bin := f.Binary
	if bin == "" {
		bin = "ffmpeg"
	}
	out, err := exec.CommandContext(ctx, bin, "-hide_banner", "-filters").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == name {
			return true, nil
		}
	}
	return false, nil
}

// VideoDurationMs is how long the first video stream runs, when ffprobe knows
// (a container often carries no per-stream duration: then false).
func (f *FFmpeg) VideoDurationMs(ctx context.Context, path string) (int64, bool) {
	bin := f.Probe
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := exec.CommandContext(ctx, bin, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=duration", "-of", "default=noprint_wrappers=1:nokey=1", path).Output()
	if err != nil {
		return 0, false
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return int64(sec * 1000), true
}
