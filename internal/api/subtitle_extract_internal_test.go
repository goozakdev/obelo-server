package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

func TestSubtitleExtractBudgetScalesWithFileSize(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.mkv")
	if err := os.WriteFile(small, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.mkv")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(60 << 30); err != nil { // sparse 60 GiB
		t.Fatal(err)
	}
	f.Close()

	if got := subtitleExtractBudget(small); got != subtitleExtractTimeout {
		t.Errorf("small file budget = %v, want the %v floor", got, subtitleExtractTimeout)
	}
	if got := subtitleExtractBudget(filepath.Join(dir, "missing")); got != subtitleExtractTimeout {
		t.Errorf("missing file budget = %v, want the floor", got)
	}
	if got := subtitleExtractBudget(big); got < 10*time.Minute {
		t.Errorf("60 GiB budget = %v, want >= 10m (a fixed 30s cap cannot demux it)", got)
	}
}

// TestWholeSubtitleVTTExtractsOnceConcurrently: N simultaneous cache-miss
// requests for one (session, track) run ONE ffmpeg extraction and all get the
// full result.
func TestWholeSubtitleVTTExtractsOnceConcurrently(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	script := filepath.Join(dir, "fakeffmpeg")
	body := "#!/bin/sh\necho x >> " + counter + "\nsleep 0.3\nprintf 'WEBVTT\\n\\n00:00.000 --> 00:01.000\\nhi\\n'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := subtitleFFmpegBinary
	subtitleFFmpegBinary = script
	t.Cleanup(func() { subtitleFFmpegBinary = old })

	media := filepath.Join(dir, "m.mkv")
	if err := os.WriteFile(media, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	sctx := playback.SessionSubtitleContext{
		ScratchDir: scratch,
		Detail: store.TitleDetail{Editions: []store.Edition{{Files: []store.File{{
			Path: media, Present: true,
			Streams: []store.Stream{{ID: "s1", Index: 2, Kind: "subtitle", Codec: "subrip"}},
		}}}}},
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data, err := wholeSubtitleVTT(context.Background(), sctx, "s1")
			results[i], errs[i] = string(data), err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if !strings.Contains(results[i], "hi") {
			t.Errorf("call %d got %q", i, results[i])
		}
	}
	runs, _ := os.ReadFile(counter)
	if got := strings.Count(string(runs), "x"); got != 1 {
		t.Errorf("ffmpeg ran %d times for %d concurrent requests, want 1", got, n)
	}
	if _, err := os.Stat(filepath.Join(scratch, "subfull_s1.vtt")); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(scratch, "*.tmp")); len(m) != 0 {
		t.Errorf("leftover temp files: %v", m)
	}
}
