package transcode

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSegmentBoundaries pins the copy-mode cut rule against the exact segment
// durations ffmpeg produced for these keyframe layouts (verified with real ffmpeg:
// `-c copy -hls_time 4`). If this drifts from ffmpeg, the synthesized playlist would
// mislist segments and the player would 404 / stall.
func TestSegmentBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		keyframes  []float64
		duration   float64
		wantExtinf []float64 // ffmpeg's actual per-segment durations
	}{
		{
			name:       "irregular",
			keyframes:  []float64{0, 3, 5, 9, 11, 15, 18},
			duration:   20,
			wantExtinf: []float64{5, 4, 6, 3, 2}, // boundaries 0,5,9,15,18,20
		},
		{
			name:       "sparse long GOP",
			keyframes:  []float64{0, 10, 20},
			duration:   24,
			wantExtinf: []float64{10, 10, 4}, // boundaries 0,10,20,24
		},
		{
			name:       "regular 2s keyframes",
			keyframes:  []float64{0, 2, 4, 6, 8, 10, 12, 14, 16},
			duration:   18,
			wantExtinf: []float64{4, 4, 4, 4, 2},
		},
		{
			name:       "weird",
			keyframes:  []float64{0, 1, 7, 8, 13},
			duration:   15,
			wantExtinf: []float64{7, 1, 5, 2}, // boundaries 0,7,8,13,15
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := SegmentBoundaries(c.keyframes, c.duration, 4)
			if len(b)-1 != len(c.wantExtinf) {
				t.Fatalf("segment count = %d, want %d (boundaries %v)", len(b)-1, len(c.wantExtinf), b)
			}
			for i, want := range c.wantExtinf {
				got := b[i+1] - b[i]
				if math.Abs(got-want) > 1e-6 {
					t.Errorf("segment %d duration = %.3f, want %.3f (boundaries %v)", i, got, want, b)
				}
			}
		})
	}
}

// TestSegmentBoundariesEdges: degenerate inputs never panic and produce a sane
// single-segment playlist.
func TestSegmentBoundariesEdges(t *testing.T) {
	if got := SegmentBoundaries(nil, 10, 4); len(got) != 2 || got[0] != 0 || got[1] != 10 {
		t.Errorf("no keyframes → single segment [0,10], got %v", got)
	}
	if got := SegmentBoundaries([]float64{0}, 0, 4); len(got) != 1 {
		t.Errorf("zero duration → just [0], got %v", got)
	}
	if got := SegmentBoundaries([]float64{0, 4, 8}, 8, 4); len(got) != 3 {
		t.Errorf("keyframes at exactly the boundaries → 2 segments, got %v", got)
	}
}

// TestKeyframeBoundariesMatchFfmpeg proves the END-TO-END correctness the long-file
// fix depends on: the boundaries KeyframeBoundaries computes — from the CONTAINER
// INDEX (MP4 stss / Matroska Cues), not a whole-file scan — match the segment
// durations ffmpeg ACTUALLY produces for the same `-c copy -hls_time`. So the
// server-synthesized media playlist lists exactly ffmpeg's segments (no mislist, no
// phantom 404) while appearing immediately instead of only when the copy finishes.
// The table covers the shapes that exercise every parser branch: B-frames (ctts +
// the elst edit-list shift — the real-movie mp4 shape), moov-at-end (non-faststart),
// plain m4v, and an ffmpeg-authored mkv (Cues).
func TestKeyframeBoundariesMatchFfmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs ffmpeg; skipped under -short")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	cases := []struct {
		name string
		file string
		args []string // encode args after the lavfi input
	}{
		{
			name: "mp4 bframes faststart", file: "a.mp4",
			args: []string{"-c:v", "libx264", "-bf", "3", "-g", "60", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-movflags", "+faststart"},
		},
		{
			name: "m4v bframes moov-at-end", file: "b.m4v",
			args: []string{"-c:v", "libx264", "-bf", "3", "-g", "48", "-preset", "veryfast", "-pix_fmt", "yuv420p"},
		},
		{
			name: "mkv (ffmpeg cues)", file: "c.mkv",
			args: []string{"-c:v", "libx264", "-g", "60", "-preset", "veryfast", "-pix_fmt", "yuv420p"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, c.file)
			genArgs := append([]string{"-y", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc=duration=40:size=640x360:rate=24"}, c.args...)
			genArgs = append(genArgs, src)
			if err := exec.Command("ffmpeg", genArgs...).Run(); err != nil {
				t.Skipf("fixture gen failed: %v", err)
			}

			// The parser path must succeed on its own — no ffprobe-scan fallback — or the
			// whole point (fast indexing on a slow mount) is lost.
			kfs, err := containerKeyframes(src)
			if err != nil {
				t.Fatalf("containerKeyframes: %v (the index parser must handle this container)", err)
			}
			if len(kfs) == 0 {
				t.Fatal("containerKeyframes returned no keyframes")
			}

			got, err := KeyframeBoundaries(context.Background(), "", src, float64(SegmentSeconds), 40)
			if err != nil {
				t.Fatalf("KeyframeBoundaries: %v", err)
			}

			// ffmpeg's actual cuts for the same copy.
			out := filepath.Join(dir, "hls")
			_ = os.MkdirAll(out, 0o755)
			_ = exec.Command("ffmpeg", "-nostdin", "-y", "-loglevel", "error", "-i", src, "-c:v", "copy", "-an",
				"-f", "hls", "-hls_time", strconv.Itoa(SegmentSeconds), "-hls_playlist_type", "vod",
				"-hls_flags", "independent_segments+temp_file", "-hls_segment_filename", filepath.Join(out, "s%03d.ts"),
				filepath.Join(out, "i.m3u8")).Run()
			pl, _ := os.ReadFile(filepath.Join(out, "i.m3u8"))
			var want []float64
			for _, line := range strings.Split(string(pl), "\n") {
				if strings.HasPrefix(line, "#EXTINF:") {
					d, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
					want = append(want, d)
				}
			}
			if len(want) == 0 {
				t.Skip("ffmpeg produced no reference playlist")
			}
			if len(got)-1 != len(want) {
				t.Fatalf("segment count: synth=%d ffmpeg=%d (boundaries %v)", len(got)-1, len(want), got)
			}
			for i, w := range want {
				if g := got[i+1] - got[i]; g-w > 0.05 || w-g > 0.05 {
					t.Errorf("segment %d duration: synth=%.3f ffmpeg=%.3f", i, g, w)
				}
			}
		})
	}
}

// TestContainerKeyframesMatchFfprobe pins the index parsers against ffprobe's
// whole-file frame scan (the ground truth): the same keyframes, the same times.
func TestContainerKeyframesMatchFfprobe(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs ffmpeg/ffprobe; skipped under -short")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	for _, file := range []string{"x.mp4", "x.mkv"} {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, file)
			args := []string{"-y", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc=duration=30:size=320x240:rate=24",
				"-c:v", "libx264", "-bf", "3", "-g", "50", "-preset", "veryfast", "-pix_fmt", "yuv420p"}
			if strings.HasSuffix(file, ".mp4") {
				args = append(args, "-movflags", "+faststart")
			}
			if err := exec.Command("ffmpeg", append(args, src)...).Run(); err != nil {
				t.Skipf("fixture gen failed: %v", err)
			}
			got, err := containerKeyframes(src)
			if err != nil {
				t.Fatalf("containerKeyframes: %v", err)
			}
			want, err := ffprobeKeyframeScan(context.Background(), "", src)
			if err != nil {
				t.Fatalf("ffprobe scan: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("keyframe count: parser=%d ffprobe=%d\nparser=%v\nffprobe=%v", len(got), len(want), got, want)
			}
			for i := range want {
				if d := got[i] - want[i]; d > 0.005 || d < -0.005 {
					t.Errorf("keyframe %d: parser=%.4f ffprobe=%.4f", i, got[i], want[i])
				}
			}
		})
	}
}

// TestAudioStreamEndIsTheAudiosOwnLength: with audio shorter than the video (so the
// container outlasts it), AudioStreamEnd reports the audio's own end — not the
// container's.
func TestAudioStreamEndIsTheAudiosOwnLength(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs ffmpeg/ffprobe; skipped under -short")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	src := filepath.Join(t.TempDir(), "x.mkv")
	if err := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=16.5:size=160x120:rate=24",
		"-f", "lavfi", "-i", "sine=duration=15.9",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", src).Run(); err != nil {
		t.Skipf("fixture gen failed: %v", err)
	}
	got, err := AudioStreamEnd(context.Background(), "", src, 1, 16.5)
	if err != nil {
		t.Fatalf("AudioStreamEnd: %v", err)
	}
	if got < 15.8 || got > 16.1 {
		t.Errorf("audio end = %.3f, want ~15.9 (the container is 16.5)", got)
	}
}

// TestAudioStreamEndCountsSegmentsLikeFfmpeg: the length AudioStreamEnd reports sizes
// a rendition playlist (ceil(length / SegmentSeconds) segments), so it must give the
// count ffmpeg's hls muxer actually writes — which counts from the rendition's FIRST
// packet. A source whose timestamps do not start at 0 (MPEG-TS's ~1.4 s start, audio
// that begins late in an mkv) must not list one segment too many.
func TestAudioStreamEndCountsSegmentsLikeFfmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs ffmpeg/ffprobe; skipped under -short")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	cases := []struct {
		name string
		file string
		args []string // after the two lavfi inputs
		in   []string // input args: video, then audio
	}{
		{
			name: "mpegts start offset", file: "x.ts",
			in: []string{"-f", "lavfi", "-i", "testsrc=duration=15:size=160x120:rate=24",
				"-f", "lavfi", "-i", "sine=duration=14.9"},
			args: []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-f", "mpegts"},
		},
		{
			// The harness's Multitrack recipe: -shortest ends the audio just before the
			// subtitle-extended container, and the AAC priming packet has a NEGATIVE pts.
			name: "mkv aac priming and a longer subtitle", file: "z.mkv",
			in: []string{"-f", "lavfi", "-i", "testsrc=duration=30:size=160x120:rate=24",
				"-f", "lavfi", "-i", "sine=duration=30", "-i", "SUBS", "-map", "0:v", "-map", "1:a", "-map", "2:s"},
			args: []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-c:s", "srt", "-t", "16.001", "-shortest"},
		},
		{
			name: "mkv audio starts late", file: "y.mkv",
			in: []string{"-f", "lavfi", "-i", "testsrc=duration=16:size=160x120:rate=24",
				"-itsoffset", "0.5", "-f", "lavfi", "-i", "sine=duration=11.7"},
			args: []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, c.file)
			subs := filepath.Join(dir, "s.srt")
			if err := os.WriteFile(subs, []byte("1\n00:00:10,000 --> 00:00:16,000\nhi\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			in := append([]string(nil), c.in...)
			for i, a := range in {
				if a == "SUBS" {
					in[i] = subs
				}
			}
			gen := append(append([]string{"-y", "-loglevel", "error"}, in...), c.args...)
			if err := exec.Command("ffmpeg", append(gen, src)...).Run(); err != nil {
				t.Skipf("fixture gen failed: %v", err)
			}
			container := probeFormatDuration(t, src)
			got, err := AudioStreamEnd(context.Background(), "", src, 1, container)
			if err != nil {
				t.Fatalf("AudioStreamEnd: %v", err)
			}
			out := t.TempDir()
			args := AudioRenditionArgs(AudioRenditionJob{
				SourcePath:     src,
				OutputDir:      out,
				PlaylistName:   AudioRenditionPlaylist("a"),
				SegmentPattern: AudioRenditionSegmentPattern("a"),
			})
			if b, err := exec.Command("ffmpeg", append([]string{"-loglevel", "error"}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("rendition ffmpeg: %v\n%s", err, b)
			}
			written, _ := filepath.Glob(filepath.Join(out, "audio_a_*.ts"))
			want := int(math.Ceil((got - 1e-3) / float64(SegmentSeconds)))
			if len(written) != want {
				t.Errorf("ffmpeg wrote %d segments, AudioStreamEnd=%.3f lists %d (container %.3f)", len(written), got, want, container)
			}
		})
	}
}

func probeFormatDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe duration: %v", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parse duration %q: %v", out, err)
	}
	return d
}
