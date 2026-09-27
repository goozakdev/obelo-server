package markerdetect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Analyzer listens to lengthMs of the File at path starting at startMs and
// fingerprints it. FFmpeg is the production one; tests substitute their own to
// observe WHEN detection listens without decoding anything.
type Analyzer interface {
	Analyze(ctx context.Context, path string, startMs, lengthMs int64) (Print, error)
}

// FFmpeg decodes with the `ffmpeg` binary (already a hard requirement of the
// Server) to mono PCM at SampleRate and fingerprints it in Go. Binary names the
// executable; empty means "ffmpeg" on PATH, like transcode.FFmpeg.
type FFmpeg struct {
	Binary string
}

// frontToMono downmixes any layout to mono from its front left, front right and
// centre alone: surrounds and LFE are left out, and the rematrix is normalised
// the same way for every source. A 5.1 rendition of a soundtrack and its stereo
// one then print alike, though the 5.1's surrounds carry sound the stereo's
// front does not. AC-3 and E-AC-3 decoders attach the stream's own downmix
// levels to every frame, which newer ffmpeg's aresample prefers to the ones
// given here, mixing the surrounds back in; those levels are dropped first.
const frontToMono = "asidedata=mode=delete:type=DOWNMIX_INFO," +
	"aresample=ochl=mono:clev=0.707:slev=0:lfe_mix_level=0"

// Analyze runs one ffmpeg over the stretch, on a single thread and at the lowest
// CPU priority the host allows: detection has no viewer waiting on it.
func (f FFmpeg) Analyze(ctx context.Context, path string, startMs, lengthMs int64) (Print, error) {
	bin := f.Binary
	if bin == "" {
		bin = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, bin,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-threads", "1",
		"-ss", seconds(startMs), "-t", seconds(lengthMs), "-i", path,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", frontToMono, "-ar", strconv.Itoa(SampleRate), "-f", "s16le", "pipe:1")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Print{}, fmt.Errorf("markerdetect: starting ffmpeg: %w", err)
	}
	lowerPriority(cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		return Print{}, fmt.Errorf("markerdetect: decoding %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	pcm := make([]int16, out.Len()/2)
	if err := binary.Read(&out, binary.LittleEndian, pcm); err != nil {
		return Print{}, fmt.Errorf("markerdetect: reading decoded audio of %q: %w", path, err)
	}
	return Fingerprint(pcm, startMs), nil
}

func seconds(ms int64) string { return strconv.FormatFloat(float64(ms)/1000, 'f', 3, 64) }
