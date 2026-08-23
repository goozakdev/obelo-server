package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/transcode"
)

// debugHLSBoundaries is the `obelo debug-hls-boundaries <file>` diagnostic:
// it runs the container keyframe-index parser against a REAL media file, computes
// the copy-mode segment boundaries the server would synthesize, and verifies the
// parsed keyframes against an interval-bounded ffprobe packet scan of the file's
// first two minutes (cheap even over a network mount). One command, one verdict —
// it exists so a misparsed real-world file (an odd edit list, a sparse cue table)
// can be diagnosed on the operator's machine without shipping the file anywhere.
func debugHLSBoundaries(path string) error {
	fmt.Printf("file: %s\n", path)

	start := time.Now()
	keyframes, err := transcode.ContainerKeyframes(path)
	if err != nil {
		return fmt.Errorf("container index parse FAILED (the server would fall back to the slow ffprobe scan): %w", err)
	}
	fmt.Printf("index parse: %d keyframes in %s\n", len(keyframes), time.Since(start).Round(time.Millisecond))
	if len(keyframes) > 1 {
		var maxGap float64
		for i := 1; i < len(keyframes); i++ {
			if g := keyframes[i] - keyframes[i-1]; g > maxGap {
				maxGap = g
			}
		}
		fmt.Printf("first keyframes: %s\n", formatTimes(keyframes, 12))
		fmt.Printf("max keyframe interval: %.3fs\n", maxGap)
	}

	boundaries, err := transcode.KeyframeBoundaries(context.Background(), "", path, float64(transcode.SegmentSeconds), 0)
	if err != nil {
		return fmt.Errorf("boundary computation failed: %w", err)
	}
	fmt.Printf("synthesized segments: %d (first boundaries: %s)\n", len(boundaries)-1, formatTimes(boundaries, 12))

	// Ground truth: ffprobe's packet keyframes for the first 120s only (interval-
	// bounded, so it reads ~2 minutes of media, not the whole file).
	fmt.Println("\nverifying against ffprobe (first 120s of packets)…")
	truth, err := ffprobeIntervalKeyframes(path, 120)
	if err != nil {
		return fmt.Errorf("ffprobe verification scan failed: %w", err)
	}
	// Normalize both to first-keyframe-relative, mirroring the server.
	rel := func(ts []float64) []float64 {
		if len(ts) == 0 {
			return ts
		}
		out := make([]float64, len(ts))
		for i, t := range ts {
			out[i] = t - ts[0]
		}
		return out
	}
	parsed := rel(keyframes)
	actual := rel(truth)
	n := len(actual)
	if len(parsed) < n {
		n = len(parsed)
	}
	mismatch := -1
	for i := 0; i < n; i++ {
		if math.Abs(parsed[i]-actual[i]) > 0.01 {
			mismatch = i
			break
		}
	}
	switch {
	case n == 0:
		fmt.Println("VERDICT: ffprobe found no keyframes to compare — inconclusive")
	case mismatch >= 0:
		fmt.Printf("VERDICT: MISMATCH at keyframe %d — parser=%.4f ffprobe=%.4f\n", mismatch, parsed[mismatch], actual[mismatch])
		fmt.Printf("parser : %s\n", formatTimes(parsed[max(0, mismatch-2):min(len(parsed), mismatch+4)], 6))
		fmt.Printf("ffprobe: %s\n", formatTimes(actual[max(0, mismatch-2):min(len(actual), mismatch+4)], 6))
	default:
		fmt.Printf("VERDICT: MATCH — first %d keyframes agree within 10ms\n", n)
	}
	return nil
}

// ffprobeIntervalKeyframes lists the video keyframe pts of the file's first
// `seconds` seconds via an interval-bounded packet scan.
func ffprobeIntervalKeyframes(path string, seconds int) ([]float64, error) {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags",
		"-of", "csv=p=0",
		"-read_intervals", "%+"+strconv.Itoa(seconds),
		path,
	).Output()
	if err != nil {
		return nil, err
	}
	var times []float64
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 2 || !strings.Contains(parts[1], "K") {
			continue
		}
		t, perr := strconv.ParseFloat(parts[0], 64)
		if perr != nil {
			continue
		}
		times = append(times, t)
	}
	return times, nil
}

func formatTimes(ts []float64, limit int) string {
	var b strings.Builder
	for i, t := range ts {
		if i >= limit {
			b.WriteString("…")
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%.3f", t)
	}
	return b.String()
}

// maybeRunDebugCommand dispatches `obelo debug-hls-boundaries <file>`;
// returns true when it handled the invocation (main should exit).
func maybeRunDebugCommand() bool {
	if len(os.Args) < 2 || os.Args[1] != "debug-hls-boundaries" {
		return false
	}
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: obelo debug-hls-boundaries <media-file>")
		os.Exit(2)
	}
	if err := debugHLSBoundaries(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return true
}
