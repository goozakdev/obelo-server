package markerdetect_test

import (
	"fmt"
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

func ms(secs float64) int64 { return int64(secs*1000 + 0.5) }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
