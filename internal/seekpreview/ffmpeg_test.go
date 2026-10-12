package seekpreview_test

import (
	"context"
	"errors"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/seekpreview"
)

func TestFrameArgsSeekToTheNearestKeyframeAndFixAspect(t *testing.T) {
	args := seekpreview.FrameArgs("/m/a.mkv", 12_500, false)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-noaccurate_seek", "-ss 12.750", "-i /m/a.mkv", "-frames:v 1", "dar", "setsar=1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "tonemap") {
		t.Error("an SDR File's frames were tone-mapped")
	}
	if hdr := strings.Join(seekpreview.FrameArgs("/m/a.mkv", 0, true), " "); !strings.Contains(hdr, "tonemap") {
		t.Error("an HDR File's frames were not tone-mapped")
	}
}

// makeClip writes a synthetic clip (a frame a second, a keyframe every 5 s) with
// the given extra video filter and returns its path.
func makeClip(t *testing.T, ffmpeg string, seconds int, vf string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip.mkv")
	out, err := exec.Command(ffmpeg, "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1:duration="+itoa(seconds),
		"-vf", vf, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "5", path).CombinedOutput()
	if err != nil {
		t.Fatalf("making clip: %v: %s", err, out)
	}
	return path
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestRealFFmpegGeneratesTheSheets runs the production extractor on a 205 s clip
// and a 25 s anamorphic one: sheet and frame counts follow duration/10 s, sheets
// are Columns x FrameWidth by Rows x frame height pixels, unused cells are black,
// and the height follows the display aspect.
func TestRealFFmpegGeneratesTheSheets(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	f := newFixture(t)
	f.addFile(t, "libmov", "wide", 205_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "anamorphic", 25_000, "2024-01-02T00:00:00Z")
	exec1 := func(q string, args ...any) { mustExec(t, f.db, q, args...) }
	exec1(`UPDATE files SET path = ? WHERE id = 'wide'`, makeClip(t, ffmpeg, 205, "null"))
	exec1(`UPDATE files SET path = ? WHERE id = 'anamorphic'`, makeClip(t, ffmpeg, 25, "setsar=2/1"))
	g := seekpreview.New(f.db, &seekpreview.FFmpeg{Binary: ffmpeg}, f.dir, seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "both sets", func() bool { return f.hasSet(t, "wide") && f.hasSet(t, "anamorphic") })

	wide, _ := f.record(t, "wide")
	if wide.FrameCount != 21 || wide.SheetCount != 1 || wide.FrameWidth != 320 || wide.FrameHeight != 180 {
		t.Fatalf("wide record = %+v, want 21 frames (ceil(205/10)), 1 sheet, 320x180", wide)
	}
	sheet := decodeJPEG(t, filepath.Join(f.dir, "wide", "0.jpg"))
	if b := sheet.Bounds(); b.Dx() != 3200 || b.Dy() != 8*180 {
		t.Fatalf("sheet is %dx%d, want 3200x1440", b.Dx(), b.Dy())
	}
	// Frames 0-20 fill cells 0-20; cell 21 (row 2, column 1) is unused and black,
	// and so is the last cell. A decoded frame is not black.
	if v := luma(sheet, 1*320+160, 2*180+90); v > 8 {
		t.Errorf("unused cell 21 luma = %d, want black", v)
	}
	if v := luma(sheet, 3199, 1439); v > 8 {
		t.Errorf("last cell luma = %d, want black", v)
	}
	if v := luma(sheet, 5, 5); v < 10 {
		t.Errorf("cell 0 luma = %d, want a decoded frame", v)
	}

	ana, _ := f.record(t, "anamorphic")
	if ana.FrameCount != 3 || ana.FrameWidth != 320 || ana.FrameHeight != 90 {
		t.Fatalf("anamorphic record = %+v, want 3 frames of 320x90 (2:1 pixels make 32:9)", ana)
	}
	if b := decodeJPEG(t, filepath.Join(f.dir, "anamorphic", "0.jpg")).Bounds(); b.Dx() != 3200 || b.Dy() != 8*90 {
		t.Fatalf("anamorphic sheet is %dx%d, want 3200x720", b.Dx(), b.Dy())
	}
}

// TestFrameAtAnExactKeyframeTimeIsThatKeyframe: with keyframes on 10 s multiples,
// the frame asked for at 20 s is the keyframe at 20 s, not the one before it.
func TestFrameAtAnExactKeyframeTimeIsThatKeyframe(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	// A 32x32 patch at limited-range luma 16+219*T/250 (full range 255*T/250 once
	// decoded), T = source seconds; a frame a second, a keyframe every 10 s.
	path := filepath.Join(t.TempDir(), "kf.mkv")
	if out, err := exec.Command(ffmpeg, "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1:duration=250",
		"-vf", "geq=lum='if(lt(X,32)*lt(Y,32),16+219*T/250,lum(X,Y))':cb='if(lt(X,32)*lt(Y,32),128,cb(X,Y))':cr='if(lt(X,32)*lt(Y,32),128,cr(X,Y))'",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "10", "-keyint_min", "10", "-sc_threshold", "0", path).CombinedOutput(); err != nil {
		t.Fatalf("making clip: %v: %s", err, out)
	}
	x := &seekpreview.FFmpeg{Binary: ffmpeg}
	for _, sec := range []int{10, 20, 120} {
		img, err := x.Frame(context.Background(), path, int64(sec)*1000)
		if err != nil {
			t.Fatalf("frame at %d s: %v", sec, err)
		}
		got := float64(color.GrayModel.Convert(img.At(16, 16)).(color.Gray).Y) * 250 / 255
		if got < float64(sec)-3 || got > float64(sec)+3 {
			t.Errorf("frame asked for at %d s shows source second %.1f, want %d (the keyframe at or before)", sec, got, sec)
		}
	}
}

// TestRealFFmpegVideoShorterThanAudio: a 118 s video under 121 s of audio (format
// duration 121 s) is ready with the 12 frames the video has, not failed.
func TestRealFFmpegVideoShorterThanAudio(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on PATH")
	}
	path := filepath.Join(t.TempDir(), "long-audio.mkv")
	if out, err := exec.Command(ffmpeg, "-y", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=1:duration=118",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=121",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "5", "-c:a", "aac", path).CombinedOutput(); err != nil {
		t.Fatalf("making clip: %v: %s", err, out)
	}
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 121_000, "2024-01-01T00:00:00Z")
	mustExec(t, f.db, `UPDATE files SET path = ? WHERE id = 'm'`, path)
	g := seekpreview.New(f.db, &seekpreview.FFmpeg{Binary: ffmpeg}, f.dir, seekpreview.Options{})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "a record", func() bool { _, ok := f.record(t, "m"); return ok })
	s, _ := f.record(t, "m")
	if s.Status != "ready" || s.FrameCount != 12 {
		t.Fatalf("record = %+v, want ready with 12 frames", s)
	}
}

func TestToneMapProbeIsNotCachedWhenItFailed(t *testing.T) {
	var calls int
	x := &seekpreview.FFmpeg{
		Binary: "/nonexistent/ffmpeg",
		IsHDR:  func(context.Context, string) (bool, error) { return true, nil },
		HasFilter: func(context.Context, string) (bool, error) {
			calls++
			if calls == 1 {
				return false, context.Canceled
			}
			return true, nil
		},
	}
	if _, err := x.Frame(context.Background(), "/m/a.mkv", 0); err == nil || errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatalf("first Frame err = %v, want the probe's failure, not 'lacks zscale'", err)
	}
	_, _ = x.Frame(context.Background(), "/m/a.mkv", 0)
	if calls != 2 {
		t.Fatalf("zscale probed %d times over two Frames, want 2: a failed probe is not an answer", calls)
	}
}

func TestHDRWithoutZscaleIsErrToneMapUnavailable(t *testing.T) {
	x := &seekpreview.FFmpeg{
		Binary:    "/nonexistent/ffmpeg",
		IsHDR:     func(context.Context, string) (bool, error) { return true, nil },
		HasFilter: func(context.Context, string) (bool, error) { return false, nil },
	}
	if _, err := x.Frame(context.Background(), "/m/a.mkv", 0); !errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatalf("Frame err = %v, want ErrToneMapUnavailable", err)
	}
}

func TestHDRProbeFailureFailsTheFileRatherThanTreatingItAsSDR(t *testing.T) {
	x := &seekpreview.FFmpeg{
		Binary:    "/nonexistent/ffmpeg",
		IsHDR:     func(context.Context, string) (bool, error) { return false, errors.New("ffprobe: boom") },
		HasFilter: func(context.Context, string) (bool, error) { return false, nil },
	}
	_, err := x.Frame(context.Background(), "/m/a.mkv", 0)
	if err == nil || !strings.Contains(err.Error(), "ffprobe: boom") {
		t.Fatalf("Frame err = %v, want the ffprobe failure", err)
	}
	if strings.Contains(err.Error(), "starting ffmpeg") {
		t.Fatal("went on to decode as SDR after the HDR check failed")
	}
}

// TestHDRCheckFollowsAFileReplacedInPlace: the same path, rewritten as HDR, is
// checked again; an SDR answer is not kept for the path.
func TestHDRCheckFollowsAFileReplacedInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.mkv")
	if err := os.WriteFile(path, []byte("sdr"), 0o644); err != nil {
		t.Fatal(err)
	}
	hdr := false
	x := &seekpreview.FFmpeg{
		Binary:    "/nonexistent/ffmpeg",
		IsHDR:     func(context.Context, string) (bool, error) { return hdr, nil },
		HasFilter: func(context.Context, string) (bool, error) { return false, nil },
	}
	if _, err := x.Frame(context.Background(), path, 0); errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatalf("SDR File err = %v", err)
	}
	hdr = true
	if err := os.WriteFile(path, []byte("hdr, and longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Frame(context.Background(), path, 0); !errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatalf("replaced File err = %v, want ErrToneMapUnavailable: the File is HDR now", err)
	}
}

// TestNoZscaleIsProbedAgainOnTheNextAttempt: an ffmpeg upgraded in place is seen
// by the next generation attempt (Retry), not only after a restart.
func TestNoZscaleIsProbedAgainOnTheNextAttempt(t *testing.T) {
	has := false
	x := &seekpreview.FFmpeg{
		Binary:    "/nonexistent/ffmpeg",
		IsHDR:     func(context.Context, string) (bool, error) { return true, nil },
		HasFilter: func(context.Context, string) (bool, error) { return has, nil },
	}
	if _, err := x.Frame(context.Background(), "/m/a.mkv", 0); !errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatalf("first err = %v, want ErrToneMapUnavailable", err)
	}
	has = true // ffmpeg upgraded
	if _, err := x.Frame(context.Background(), "/m/a.mkv", 0); errors.Is(err, seekpreview.ErrToneMapUnavailable) {
		t.Fatal("still 'lacks zscale' after ffmpeg gained it")
	}
}

// TestMissingFileIsErrFileNotFoundWithoutProbing: the real HDR check is not run for
// a path that is not on disk (it would only fail "ffprobe exit 1").
func TestMissingFileIsErrFileNotFoundWithoutProbing(t *testing.T) {
	x := &seekpreview.FFmpeg{Binary: "/nonexistent/ffmpeg", Probe: "/nonexistent/ffprobe"}
	_, err := x.Frame(context.Background(), filepath.Join(t.TempDir(), "gone.mkv"), 0)
	if !errors.Is(err, seekpreview.ErrFileNotFound) {
		t.Fatalf("Frame err = %v, want ErrFileNotFound", err)
	}
}
