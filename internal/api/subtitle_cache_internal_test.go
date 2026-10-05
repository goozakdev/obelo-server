package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// fakeSubtitleSession installs a fake ffmpeg running script (a /bin/sh body) and
// returns a session context for one embedded track "s1" plus the fake's dir.
func fakeSubtitleSession(t *testing.T, script string) (playback.SessionSubtitleContext, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakeffmpeg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := subtitleFFmpegBinary
	subtitleFFmpegBinary = bin
	t.Cleanup(func() { subtitleFFmpegBinary = old })

	media := filepath.Join(dir, "m.mkv")
	if err := os.WriteFile(media, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	return playback.SessionSubtitleContext{
		ScratchDir: scratch,
		Detail: store.TitleDetail{Editions: []store.Edition{{Files: []store.File{{
			Path: media, Present: true,
			Streams: []store.Stream{{ID: "s1", Index: 2, Kind: "subtitle", Codec: "subrip"}},
		}}}}},
	}, dir
}

const fakeCueOutput = "printf 'WEBVTT\\n\\n00:00.000 --> 00:01.000\\nhi\\n'\n"

// TestWholeSubtitleVTTWaiterHonoursOwnContext: a waiter whose context ends
// returns promptly with that error while the extraction (still wanted by
// another waiter) carries on and serves it.
func TestWholeSubtitleVTTWaiterHonoursOwnContext(t *testing.T) {
	sctx, _ := fakeSubtitleSession(t, "sleep 1\n"+fakeCueOutput)

	var wg sync.WaitGroup
	var leaderData []byte
	var leaderErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		leaderData, leaderErr = wholeSubtitleVTT(context.Background(), sctx, "s1")
	}()
	time.Sleep(200 * time.Millisecond) // let the first caller start the extraction

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := wholeSubtitleVTT(ctx, sctx, "s1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("follower err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 600*time.Millisecond {
		t.Errorf("follower blocked %v past its own deadline", d)
	}
	wg.Wait()
	if leaderErr != nil || !strings.Contains(string(leaderData), "hi") {
		t.Errorf("remaining waiter got %q, %v", leaderData, leaderErr)
	}
}

// TestWholeSubtitleVTTCancelsWhenNoWaiterRemains: once every waiter has gone the
// ffmpeg extraction is killed rather than reading the whole file for nobody.
func TestWholeSubtitleVTTCancelsWhenNoWaiterRemains(t *testing.T) {
	sctx, dir := fakeSubtitleSession(t, "echo $$ > \"$(dirname \"$0\")/pid\"\nexec sleep 30\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := wholeSubtitleVTT(ctx, sctx, "s1")
		done <- err
	}()
	var pid int
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			if p, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				pid = p
				break
			}
		}
	}
	if pid == 0 {
		t.Fatal("fake ffmpeg never started")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want Canceled", err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("extraction still running after its last waiter left")
}

// TestSubtitleFlightPanicReachesAllWaiters: a panic in the shared run becomes an
// error for every waiter and leaves the key reusable.
func TestSubtitleFlightPanicReachesAllWaiters(t *testing.T) {
	f := &subtitleFlight{calls: map[string]*subtitleCall{}}
	release := make(chan struct{})
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.do(context.Background(), "k", func(context.Context) ([]byte, error) {
				<-release
				panic("boom")
			})
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Errorf("waiter %d got a nil error from a panicked run", i)
		}
	}
	data, err := f.do(context.Background(), "k", func(context.Context) ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(data) != "ok" {
		t.Errorf("key not reusable after panic: %q, %v", data, err)
	}
}

// TestSubtitleSegmentCuesParsesOncePerSubtitle: repeated segment requests parse
// the whole WebVTT once; a changed cache file (a new extraction) re-parses.
func TestSubtitleSegmentCuesParsesOncePerSubtitle(t *testing.T) {
	sctx, _ := fakeSubtitleSession(t, fakeCueOutput)
	var parses atomic.Int32
	old := parseSegmentCues
	parseSegmentCues = func(b []byte) subtitleCues {
		parses.Add(1)
		return old(b)
	}
	t.Cleanup(func() { parseSegmentCues = old })

	for i := 0; i < 5; i++ {
		sc, err := subtitleSegmentCues(context.Background(), sctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if got := string(sc.Segment(0, 6)); !strings.Contains(got, "hi") {
			t.Fatalf("segment 0 = %q", got)
		}
	}
	if got := parses.Load(); got != 1 {
		t.Errorf("parsed %d times for 5 requests, want 1", got)
	}

	cachePath := subtitleCachePath(sctx.ScratchDir, "s1")
	if err := os.WriteFile(cachePath, []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nchanged!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := subtitleSegmentCues(context.Background(), sctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(sc.Segment(0, 6)); !strings.Contains(got, "changed!") {
		t.Errorf("stale cues served after the cache file changed: %q", got)
	}
	if got := parses.Load(); got != 2 {
		t.Errorf("parses = %d, want 2", got)
	}
}

// TestSubtitleCueCacheIsBounded: the parsed-cue cache never holds more than its cap.
func TestSubtitleCueCacheIsBounded(t *testing.T) {
	c := newSubtitleCueCache(3)
	for i := 0; i < 10; i++ {
		c.put("k"+strconv.Itoa(i), subtitleCueStamp{size: int64(i)}, subtitleCues{})
	}
	if n := c.len(); n != 3 {
		t.Errorf("cache holds %d entries, want 3", n)
	}
}
