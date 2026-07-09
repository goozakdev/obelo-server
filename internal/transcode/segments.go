package transcode

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Accurate HLS segment boundaries for a COPY (ADR-0024 follow-up). A video the HLS
// muxer stream-COPIES (`-c copy`) cannot be re-cut, so its segments fall on the
// SOURCE's keyframes, at irregular durations — and with -hls_playlist_type vod the
// muxer only writes the media playlist when it FINISHES the whole input (minutes away
// for a feature-length file), so a runtime that serves ffmpeg's own playlist 404s the
// long file. The server therefore SYNTHESIZES the playlist immediately from the
// source's keyframe timestamps, reproducing ffmpeg's cut points EXACTLY so the
// segment names/durations it lists match the ones ffmpeg produces.

// SegmentBoundaries computes the segment boundary times (seconds) that
// `-c copy -hls_time hlsTime` produces from a video's sorted keyframe timestamps,
// matching ffmpeg's rule EXACTLY: the first segment starts at 0, and after a boundary
// at b the next boundary is the FIRST keyframe at/after the next integer multiple of
// hlsTime strictly greater than b; the final boundary is the duration. Segment i spans
// [out[i], out[i+1]), so len(out)-1 segments. Verified against ffmpeg for regular,
// sparse (long-GOP), and irregular keyframe layouts.
//
// keyframes must be sorted ascending and include 0. A small epsilon absorbs the
// floating-point noise in ffprobe timestamps so a keyframe sitting essentially on a
// multiple of hlsTime is treated as reaching it.
func SegmentBoundaries(keyframes []float64, duration, hlsTime float64) []float64 {
	out := []float64{0}
	if duration <= 0 {
		return out
	}
	if hlsTime <= 0 || len(keyframes) == 0 {
		return append(out, duration)
	}
	const eps = 1e-3
	last := 0.0
	ki := 0
	for {
		target := (math.Floor((last+eps)/hlsTime) + 1) * hlsTime
		if target >= duration {
			break
		}
		for ki < len(keyframes) && keyframes[ki] < target-eps {
			ki++
		}
		if ki >= len(keyframes) {
			break
		}
		kf := keyframes[ki]
		if kf >= duration-eps || kf <= last+eps {
			break
		}
		out = append(out, kf)
		last = kf
	}
	return append(out, duration)
}

// KeyframeBoundaries reads the SOURCE file's video keyframe timestamps and returns
// the exact copy-mode segment boundaries (SegmentBoundaries). It reads the
// CONTAINER'S OWN KEYFRAME INDEX — the MP4 moov's stss/stts tables or the Matroska
// Cues — which is a few MB of I/O regardless of file size, so a feature-length
// movie on a slow network mount indexes in well under a second. (The obvious
// alternative, an `ffprobe -skip_frame nokey` frame scan, DEMUXES THE WHOLE FILE:
// minutes over a network mount, which is exactly how the original probe timed out
// and long files 404'd.) The ffprobe scan remains only as the fallback for
// containers without a parsed index (avi, ts, …), bounded by ctx. durationSec, when
// > 0, is the caller's known File duration. A probe error is returned so the caller
// can fall back to serving ffmpeg's own playlist.
func KeyframeBoundaries(ctx context.Context, ffprobeBin, path string, hlsTime, durationSec float64) ([]float64, error) {
	keyframes, err := containerKeyframes(path)
	if err != nil {
		keyframes, err = ffprobeKeyframeScan(ctx, ffprobeBin, path)
		if err != nil {
			return nil, err
		}
	}
	if len(keyframes) == 0 {
		return nil, errNoKeyframes
	}
	sort.Float64s(keyframes)
	// Normalize to FIRST-KEYFRAME-RELATIVE time. ffmpeg's HLS muxer cuts when a
	// keyframe's pts is >= hlsTime past the FIRST packet's pts (verified empirically:
	// a +5s pts offset produces identical EXTINFs) — so any constant timestamp offset
	// (an mp4 edit list, a muxer base offset, a nonzero first cue) cancels, and
	// normalizing here makes the synthesized boundaries immune to those quirks rather
	// than trying to reproduce each one.
	first := keyframes[0]
	for i := range keyframes {
		keyframes[i] -= first
	}
	dur := durationSec
	if dur <= 0 {
		dur = keyframes[len(keyframes)-1]
	}
	return SegmentBoundaries(keyframes, dur, hlsTime), nil
}

// ContainerKeyframes exposes the index parser for diagnostics (the
// debug-hls-boundaries CLI verifies it against an ffprobe scan of the operator's
// real file).
func ContainerKeyframes(path string) ([]float64, error) { return containerKeyframes(path) }

// FfprobeKeyframeScan exposes the whole-file ffprobe fallback scan for the same
// diagnostic comparison.
func FfprobeKeyframeScan(ctx context.Context, ffprobeBin, path string) ([]float64, error) {
	return ffprobeKeyframeScan(ctx, ffprobeBin, path)
}

// containerKeyframes reads the video keyframe timestamps from the container's own
// index: MP4-family (moov > stss/stts/ctts/elst) or Matroska (Cues). Sniffs the
// format from the first bytes; an unsupported container is an error the caller
// falls back from.
func containerKeyframes(path string) ([]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var head [12]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return nil, err
	}
	switch {
	case binary.BigEndian.Uint32(head[:4]) == ebmlIDHeader:
		return mkvKeyframeTimes(f)
	case string(head[4:8]) == "ftyp":
		return mp4KeyframeTimes(f)
	default:
		return nil, errUnknownContainer
	}
}

// ffprobeKeyframeScan is the whole-file fallback: demux every packet, keep the
// keyframes' pts. Correct for any container ffmpeg reads, but I/O-bound on the full
// file size — use only when the container index cannot be parsed.
func ffprobeKeyframeScan(ctx context.Context, ffprobeBin, path string) ([]float64, error) {
	bin := ffprobeBin
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-skip_frame", "nokey",
		"-show_entries", "frame=pts_time",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		return nil, err
	}
	var keyframes []float64
	for _, line := range strings.Split(string(out), "\n") {
		// A csv row can carry trailing empty fields ("0.000000,") when the frame has
		// side data — keep only the pts_time field.
		line, _, _ = strings.Cut(strings.TrimSpace(line), ",")
		if line == "" || line == "N/A" {
			continue
		}
		t, perr := strconv.ParseFloat(line, 64)
		if perr != nil {
			continue
		}
		keyframes = append(keyframes, t)
	}
	if len(keyframes) == 0 {
		return nil, errNoKeyframes
	}
	return keyframes, nil
}

// errUnknownContainer marks a container without a parsed keyframe index, so the
// caller can decide to run the (expensive) ffprobe scan instead.
var errUnknownContainer = &probeError{"transcode: container has no parsed keyframe index"}

// errNoKeyframes signals the probe found no video keyframes (a malformed or
// audio-only input) so the caller falls back rather than synthesizing an empty
// playlist.
var errNoKeyframes = &probeError{"transcode: no video keyframes found"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }
