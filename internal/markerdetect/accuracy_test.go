package markerdetect_test

import (
	"context"
	"fmt"
	"math/bits"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/markerdetect"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Accuracy against real decoded audio: short synthetic episodes built with
// ffmpeg's lavfi sources, sharing an identical Intro at a different offset in
// each and an identical closing segment, must come back with Detected spans
// within toleranceMs of where the shared audio really is. Episodes that share
// nothing must come back with none.

const toleranceMs = 1500

// piece is one stretch of an episode's soundtrack: seconds of a lavfi source.
type piece struct {
	src  string
	secs float64
}

// unique is pink noise no other episode has; theme and ending are the shared
// segments — a stepped melody over noise, identical in every episode that uses
// them.
func unique(seed int, secs float64) piece {
	return piece{fmt.Sprintf("anoisesrc=c=pink:a=0.25:r=44100:s=%d", seed), secs}
}

func theme(secs float64) piece {
	return piece{"aevalsrc='0.3*sin(2*PI*t*(330+110*floor(mod(t*3,7))))+0.05*sin(2*PI*t*97)':s=44100", secs}
}

func ending(secs float64) piece {
	return piece{"aevalsrc='0.3*sin(2*PI*t*(520-60*floor(mod(t*2.5,5))))*(0.6+0.4*sin(2*PI*t*0.8))':s=44100", secs}
}

// writeEpisode renders pieces back to back into an audio-only file.
func writeEpisode(t *testing.T, path string, pieces ...piece) int64 {
	t.Helper()
	args := []string{"-y", "-nostdin", "-loglevel", "error"}
	var concat strings.Builder
	var total float64
	for i, p := range pieces {
		args = append(args, "-f", "lavfi", "-t", fmt.Sprintf("%.3f", p.secs), "-i", p.src)
		fmt.Fprintf(&concat, "[%d:a]", i)
		total += p.secs
	}
	fmt.Fprintf(&concat, "concat=n=%d:v=0:a=1[out]", len(pieces))
	args = append(args, "-filter_complex", concat.String(), "-map", "[out]", "-ac", "1", "-c:a", "flac", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	return int64(total * 1000)
}

// writeSurroundEpisode renders pieces back to back as the front of a 5.1 file in
// codec — the same in front left, front right and centre — with surround, as
// long as the whole, in the side and LFE channels. It is what a 5.1 rendition of
// a stereo soundtrack sounds like when its surrounds carry ambience of their own.
func writeSurroundEpisode(t *testing.T, path, codec, surround string, pieces ...piece) int64 {
	t.Helper()
	args := []string{"-y", "-nostdin", "-loglevel", "error"}
	inputs := ""
	var total float64
	for i, p := range pieces {
		args = append(args, "-f", "lavfi", "-t", fmt.Sprintf("%.3f", p.secs), "-i", p.src)
		inputs += fmt.Sprintf("[%d:a]", i)
		total += p.secs
	}
	args = append(args, "-f", "lavfi", "-t", fmt.Sprintf("%.3f", total), "-i", surround)
	graph := fmt.Sprintf("%sconcat=n=%d:v=0:a=1[front];[front][%d:a]amerge=inputs=2,"+
		"pan=5.1(side)|FL=c0|FR=c0|FC=c0|LFE=c1|SL=c1|SR=c1[out]", inputs, len(pieces), len(pieces))
	args = append(args, "-filter_complex", graph, "-map", "[out]", "-c:a", codec, path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	return int64(total * 1000)
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// detectAll runs one forced detection over the Files as a single Season and
// returns what was saved per path.
func detectAll(t *testing.T, files []store.DetectionFile) map[string][]store.Marker {
	t.Helper()
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{"show": {{ID: "s1", ShowID: "show", Files: files}}}}
	d := markerdetect.New(st, markerdetect.FFmpeg{}, markerdetect.Options{})
	d.Start()
	defer d.Close()
	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.saved)
		st.mu.Unlock()
		if n == len(files) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detection saved %d of %d Files", n, len(files))
		}
		time.Sleep(20 * time.Millisecond)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.saved
}

func TestDetectionFindsSharedIntroAndCredits(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	type truth struct {
		introStart, creditsStart float64
	}
	const introSecs, creditsSecs = 20.0, 20.0
	eps := []struct {
		offset, middle float64
	}{
		{2.0, 40.0},
		{7.35, 45.0},
		{12.8, 38.5},
	}
	var files []store.DetectionFile
	truths := map[string]truth{}
	for i, ep := range eps {
		path := filepath.Join(dir, fmt.Sprintf("S01E%02d.mka", i+1))
		dur := writeEpisode(t, path,
			unique(100+i, ep.offset),
			theme(introSecs),
			unique(200+i, ep.middle),
			ending(creditsSecs),
		)
		files = append(files, store.DetectionFile{TitleID: fmt.Sprintf("e%d", i+1), Path: path, DurationMs: dur})
		truths[path] = truth{ep.offset, ep.offset + introSecs + ep.middle}
	}

	saved := detectAll(t, files)
	for _, f := range files {
		tr := truths[f.Path]
		want := []store.Marker{
			{Kind: "intro", Source: "detected", StartMs: ms(tr.introStart), EndMs: ms(tr.introStart + introSecs)},
			{Kind: "credits", Source: "detected", StartMs: ms(tr.creditsStart), EndMs: ms(tr.creditsStart + creditsSecs)},
		}
		got := saved[f.Path]
		if len(got) != len(want) {
			t.Errorf("%s: markers = %+v, want %+v", filepath.Base(f.Path), got, want)
			continue
		}
		for i := range want {
			g, w := got[i], want[i]
			if g.Kind != w.Kind || g.Source != w.Source || abs(g.StartMs-w.StartMs) > toleranceMs || abs(g.EndMs-w.EndMs) > toleranceMs {
				t.Errorf("%s: %s = [%d, %d), want [%d, %d) ±%d ms",
					filepath.Base(f.Path), w.Kind, g.StartMs, g.EndMs, w.StartMs, w.EndMs, toleranceMs)
			}
		}
	}
}

func TestDetectionFindsNothingWhenEpisodesShareNothing(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	var files []store.DetectionFile
	for i := range 3 {
		path := filepath.Join(dir, fmt.Sprintf("S01E%02d.mka", i+1))
		dur := writeEpisode(t, path, unique(300+i, 30), unique(400+i, 30), unique(500+i, 30))
		files = append(files, store.DetectionFile{TitleID: fmt.Sprintf("e%d", i+1), Path: path, DurationMs: dur})
	}
	for path, ms := range detectAll(t, files) {
		if len(ms) != 0 {
			t.Errorf("%s: markers = %+v, want none", filepath.Base(path), ms)
		}
	}
}

// tone is a steady sine: the same sound, frame after frame. Two episodes that
// both hum carry near-identical fingerprints, whatever the pitch, and must not be
// taken to share an Intro.
func tone(hz int, secs float64) piece {
	return piece{fmt.Sprintf("sine=f=%d:r=44100", hz), secs}
}

func TestDetectionIgnoresASharedSteadyTone(t *testing.T) {
	requireFFmpeg(t)
	for name, pitch := range map[string]func(ep int) int{
		"same pitch":        func(int) int { return 440 },
		"different pitches": func(ep int) int { return 440 + 300*ep },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var files []store.DetectionFile
			for i := range 3 {
				path := filepath.Join(dir, fmt.Sprintf("S01E%02d.mka", i+1))
				dur := writeEpisode(t, path,
					unique(600+i, 3+float64(i)),
					tone(pitch(i), 20),
					unique(700+i, 40),
					tone(pitch(i), 20),
				)
				files = append(files, store.DetectionFile{TitleID: fmt.Sprintf("e%d", i+1), Path: path, DurationMs: dur})
			}
			for path, ms := range detectAll(t, files) {
				if len(ms) != 0 {
					t.Errorf("%s: markers = %+v, want none", filepath.Base(path), ms)
				}
			}
		})
	}
}

// TestDetectionPrefersTheIntroEveryEpisodeShares: two of four episodes open with
// the same 30 s recap straight before the theme, so that pair shares 50 s of
// continuous sound; the 20 s theme is all the others share. The Intro of every
// episode is the theme, which every pair agrees on — not the longer run only
// one pair has.
func TestDetectionPrefersTheIntroEveryEpisodeShares(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	const introSecs = 20.0
	eps := []struct {
		offset float64
		recap  bool
	}{
		{2.0, false},
		{3.5, true},
		{5.2, true},
		{7.1, false},
	}
	var files []store.DetectionFile
	introAt := map[string]float64{}
	for i, ep := range eps {
		path := filepath.Join(dir, fmt.Sprintf("S01E%02d.mka", i+1))
		pieces := []piece{unique(800+i, ep.offset)}
		at := ep.offset
		if ep.recap {
			pieces = append(pieces, unique(777, 30))
			at += 30
		}
		pieces = append(pieces, theme(introSecs), unique(900+i, 80), ending(20))
		dur := writeEpisode(t, path, pieces...)
		files = append(files, store.DetectionFile{TitleID: fmt.Sprintf("e%d", i+1), Path: path, DurationMs: dur})
		introAt[path] = at
	}

	saved := detectAll(t, files)
	for _, f := range files {
		want := store.Marker{Kind: "intro", Source: "detected", StartMs: ms(introAt[f.Path]), EndMs: ms(introAt[f.Path] + introSecs)}
		got := saved[f.Path]
		if len(got) == 0 || got[0].Kind != want.Kind || abs(got[0].StartMs-want.StartMs) > toleranceMs || abs(got[0].EndMs-want.EndMs) > toleranceMs {
			t.Errorf("%s: markers = %+v, want intro [%d, %d) ±%d ms", filepath.Base(f.Path), got, want.StartMs, want.EndMs, toleranceMs)
		}
	}
}

// TestDetectionMatchesA51AndAStereoRenditionOfTheSameTheme: one episode is 5.1
// whose surrounds carry loud ambience the front does not, the other stereo; both
// carry the same theme and ending in front. Their Intro and Credits are the
// front's, so each File must come back with both, where the shared audio is.
func TestDetectionMatchesA51AndAStereoRenditionOfTheSameTheme(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	const introSecs, creditsSecs = 20.0, 20.0
	type truth struct{ introStart, creditsStart float64 }
	var files []store.DetectionFile
	truths := map[string]truth{}
	for i, ep := range []struct {
		offset, middle float64
		surround       bool
	}{
		{2.0, 40.0, true},
		{6.4, 44.0, false},
	} {
		path := filepath.Join(dir, fmt.Sprintf("S01E%02d.mka", i+1))
		pieces := []piece{unique(1000+i, ep.offset), theme(introSecs), unique(1100+i, ep.middle), ending(creditsSecs)}
		var dur int64
		if ep.surround {
			dur = writeSurroundEpisode(t, path, "flac", "anoisesrc=c=white:a=0.6:r=44100:s=1200", pieces...)
		} else {
			dur = writeEpisode(t, path, pieces...)
		}
		files = append(files, store.DetectionFile{TitleID: fmt.Sprintf("e%d", i+1), Path: path, DurationMs: dur})
		truths[path] = truth{ep.offset, ep.offset + introSecs + ep.middle}
	}

	saved := detectAll(t, files)
	for _, f := range files {
		tr := truths[f.Path]
		want := []store.Marker{
			{Kind: "intro", Source: "detected", StartMs: ms(tr.introStart), EndMs: ms(tr.introStart + introSecs)},
			{Kind: "credits", Source: "detected", StartMs: ms(tr.creditsStart), EndMs: ms(tr.creditsStart + creditsSecs)},
		}
		got := saved[f.Path]
		if len(got) != len(want) {
			t.Errorf("%s: markers = %+v, want %+v", filepath.Base(f.Path), got, want)
			continue
		}
		for i := range want {
			g, w := got[i], want[i]
			if g.Kind != w.Kind || abs(g.StartMs-w.StartMs) > toleranceMs || abs(g.EndMs-w.EndMs) > toleranceMs {
				t.Errorf("%s: %s = [%d, %d), want [%d, %d) ±%d ms",
					filepath.Base(f.Path), w.Kind, g.StartMs, g.EndMs, w.StartMs, w.EndMs, toleranceMs)
			}
		}
	}
}

// TestFFmpegLeavesTheSurroundsOfAC3AndEAC3Out: a 5.1 file whose surrounds and
// LFE carry loud noise the front does not must print like its front alone. AC-3
// and E-AC-3 decoders hand every frame the stream's own downmix levels, which
// some ffmpeg versions apply over the ones asked for, mixing the surrounds back
// in. Lossy coding alone costs some frames, so most, not all, must match: with
// the surrounds mixed in, few do.
func TestFFmpegLeavesTheSurroundsOfAC3AndEAC3Out(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	pieces := []piece{theme(20), ending(10)}
	front := filepath.Join(dir, "front.mka")
	dur := writeEpisode(t, front, pieces...)
	ff := markerdetect.FFmpeg{}
	want, err := ff.Analyze(context.Background(), front, 0, dur)
	if err != nil {
		t.Fatal(err)
	}
	for _, codec := range []string{"ac3", "eac3"} {
		path := filepath.Join(dir, codec+".mka")
		writeSurroundEpisode(t, path, codec, "anoisesrc=c=white:a=0.6:r=44100:s=1500", pieces...)
		got, err := ff.Analyze(context.Background(), path, 0, dur)
		if err != nil {
			t.Fatal(err)
		}
		n := min(len(got.Words), len(want.Words))
		alike := 0
		for i := range n {
			if bits.OnesCount32(got.Words[i]^want.Words[i]) <= markerdetect.DefaultParams.MaxBitErrors {
				alike++
			}
		}
		if n == 0 || alike*2 < n {
			t.Errorf("%s 5.1: %d of %d frames print like the front alone, want at least half", codec, alike, n)
		}
	}
}

func ms(secs float64) int64 { return int64(secs*1000 + 0.5) }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
