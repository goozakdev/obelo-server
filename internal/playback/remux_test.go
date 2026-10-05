package playback

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// fakeRunner records launches and lets a test observe the kill of the ffmpeg
// job a session owns — no real ffmpeg. It also writes the HLS playlist/segment
// into the output dir so the file-waiting read path can be exercised.
type fakeRunner struct {
	mu        sync.Mutex
	started   int
	outputDir string
}

func (r *fakeRunner) Start(ctx context.Context, args []string) (transcode.Job, error) {
	r.mu.Lock()
	r.started++
	// The last arg is the playlist path; its directory is the scratch dir.
	r.outputDir = filepath.Dir(args[len(args)-1])
	r.mu.Unlock()
	return &fakeJob{}, nil
}

type fakeJob struct {
	mu     sync.Mutex
	killed bool
}

func (j *fakeJob) Wait() error { return nil }
func (j *fakeJob) Kill() error {
	j.mu.Lock()
	j.killed = true
	j.mu.Unlock()
	return nil
}

// TestRemuxSessionStartsLazilyAndTearsDown: a directStream session gets a scratch
// dir + runtime, the remux starts only on the first ensure, and End kills ffmpeg
// and deletes the scratch dir.
func TestRemuxSessionStartsLazilyAndTearsDown(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	m := NewRemuxManager(runner, root)

	dec := Decision{
		Tier:    TierDirectStream,
		Edition: store.Edition{ID: "e1"},
		File:    store.File{ID: "f1", Path: "/movies/x.mkv"},
	}
	s := m.Create(CreateInput{
		UserID:  "u1",
		TitleID: "t1",
		BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: outputDir, Seek: seek})
		},
	}, dec)

	if s.ScratchDir == "" {
		t.Fatal("directStream session has empty ScratchDir")
	}
	if filepath.Dir(s.ScratchDir) != root {
		t.Errorf("scratch %q not under root %q", s.ScratchDir, root)
	}

	rt, ok := m.remuxRuntimeFor(s.ID)
	if !ok {
		t.Fatal("no remux runtime for directStream session")
	}
	// Not started until ensured.
	if runner.started != 0 {
		t.Errorf("remux started %d times before EnsureStarted, want 0", runner.started)
	}

	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	if err := rt.EnsureStarted(); err != nil { // idempotent
		t.Fatalf("second EnsureStarted: %v", err)
	}
	if runner.started != 1 {
		t.Errorf("remux started %d times, want exactly 1 (lazy + once)", runner.started)
	}
	if _, err := os.Stat(s.ScratchDir); err != nil {
		t.Errorf("scratch dir not created on start: %v", err)
	}
	job := rt.job.(*fakeJob)

	// End kills ffmpeg and removes scratch.
	if !m.End(s.ID) {
		t.Fatal("End returned false")
	}
	job.mu.Lock()
	killed := job.killed
	job.mu.Unlock()
	if !killed {
		t.Error("ffmpeg job not killed on End")
	}
	if _, err := os.Stat(s.ScratchDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after End (err=%v)", err)
	}
	if _, ok := m.remuxRuntimeFor(s.ID); ok {
		t.Error("runtime still registered after End")
	}
}

// TestDirectPlaySessionHasNoScratch: a direct-play session under a remux Manager
// still gets no scratch and no runtime (direct play streams bytes, no HLS).
func TestDirectPlaySessionHasNoScratch(t *testing.T) {
	m := NewRemuxManager(&fakeRunner{}, t.TempDir())
	dec := Decision{Tier: TierDirectPlay, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", Path: "/m/x.mp4"}}
	s := m.Create(CreateInput{UserID: "u1"}, dec)
	if s.ScratchDir != "" {
		t.Errorf("direct-play session has scratch %q, want empty", s.ScratchDir)
	}
	if _, ok := m.remuxRuntimeFor(s.ID); ok {
		t.Error("direct-play session has a remux runtime, want none")
	}
}

// demuxedDecision builds a multi-audio directStream Decision (2 audio Streams) for
// the rendition-lifecycle tests.
func demuxedDecision() Decision {
	return Decision{
		Tier:    TierDirectStream,
		Edition: store.Edition{ID: "e1"},
		File: store.File{ID: "f1", Path: "/movies/x.mkv", Streams: []store.Stream{
			{ID: "v1", Kind: "video"},
			{ID: "a1", Kind: "audio", IsDefault: true},
			{ID: "a2", Kind: "audio"},
		}},
		AudioStream: store.Stream{ID: "a1", Kind: "audio", IsDefault: true},
	}
}

// TestAudioRenditionRuntimeLazyAndTearsDownWithSession (audio-streams/03): a demuxed
// session's audio rendition runtime is created + started LAZILY (no ffmpeg until the
// first ensure), shares the SESSION scratch dir under a namespaced playlist name,
// and its ffmpeg job is killed when the session ends — without the rendition teardown
// deleting the shared scratch (the video runtime owns that).
func TestAudioRenditionRuntimeLazyAndTearsDownWithSession(t *testing.T) {
	root := t.TempDir()
	runner := &fakeRunner{}
	m := NewRemuxManager(runner, root)
	dec := demuxedDecision()

	s := m.Create(CreateInput{
		UserID:  "u1",
		TitleID: "t1",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: dir, Seek: seek, VideoOnly: true})
		},
		BuildAudioRenditionArgs: func(streamID, dir string, seek transcode.SeekOffset) []string {
			return transcode.AudioRenditionArgs(transcode.AudioRenditionJob{
				SourcePath:     dec.File.Path,
				OutputDir:      dir,
				PlaylistName:   transcode.AudioRenditionPlaylist(streamID),
				SegmentPattern: transcode.AudioRenditionSegmentPattern(streamID),
			})
		},
	}, dec)

	rt, err := m.EnsureAudioRuntime(s.ID, "a2")
	if err != nil {
		t.Fatalf("EnsureAudioRuntime: %v", err)
	}
	// Lazy: no ffmpeg until EnsureStarted.
	if runner.started != 0 {
		t.Errorf("rendition started %d times before EnsureStarted, want 0 (lazy)", runner.started)
	}
	if err := rt.EnsureStarted(); err != nil {
		t.Fatalf("rendition EnsureStarted: %v", err)
	}
	if err := rt.EnsureStarted(); err != nil { // idempotent
		t.Fatalf("second rendition EnsureStarted: %v", err)
	}
	if runner.started != 1 {
		t.Errorf("rendition started %d times, want exactly 1 (lazy + once)", runner.started)
	}
	// Shares the session scratch dir (namespaced files), does NOT own it.
	if rt.scratchDir != s.ScratchDir {
		t.Errorf("rendition scratch %q != session scratch %q", rt.scratchDir, s.ScratchDir)
	}
	if !rt.sharedScratch {
		t.Error("rendition runtime must be shared-scratch (must not delete the session dir)")
	}
	if rt.playlistName != transcode.AudioRenditionPlaylist("a2") {
		t.Errorf("rendition playlistName = %q, want the namespaced audio_a2.m3u8", rt.playlistName)
	}
	// A second ensure for the same Stream returns the SAME runtime (one job per rendition).
	if rt2, _ := m.EnsureAudioRuntime(s.ID, "a2"); rt2 != rt {
		t.Error("EnsureAudioRuntime minted a second runtime for the same Stream")
	}
	job := rt.job.(*fakeJob)

	// End kills the rendition ffmpeg job too (reaped with the session).
	if !m.End(s.ID) {
		t.Fatal("End returned false")
	}
	job.mu.Lock()
	killed := job.killed
	job.mu.Unlock()
	if !killed {
		t.Error("rendition ffmpeg job not killed on End")
	}
	// The session's scratch is gone (the video runtime removed it once).
	if _, err := os.Stat(s.ScratchDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir still present after End (err=%v)", err)
	}
	// A rendition ensure after End fails — no builder registered anymore.
	if _, err := m.EnsureAudioRuntime(s.ID, "a2"); err != ErrNoAudioRendition {
		t.Errorf("post-End EnsureAudioRuntime err = %v, want ErrNoAudioRendition", err)
	}
}

// TestSingleAudioSessionExposesNoRenditions (audio-streams/03 regression pin): a
// session created without an audio-rendition builder (single-audio / muxed) exposes
// no renditions — EnsureAudioRuntime is ErrNoAudioRendition, so the muxed pipeline is
// untouched by construction.
func TestSingleAudioSessionExposesNoRenditions(t *testing.T) {
	m := NewRemuxManager(&fakeRunner{}, t.TempDir())
	dec := Decision{
		Tier:    TierDirectStream,
		Edition: store.Edition{ID: "e1"},
		File:    store.File{ID: "f1", Path: "/movies/x.mkv", Streams: []store.Stream{{ID: "a1", Kind: "audio", IsDefault: true}}},
	}
	s := m.Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string { return nil },
		// BuildAudioRenditionArgs deliberately nil — single-audio.
	}, dec)
	if _, err := m.EnsureAudioRuntime(s.ID, "a1"); err != ErrNoAudioRendition {
		t.Errorf("single-audio EnsureAudioRuntime err = %v, want ErrNoAudioRendition", err)
	}
}

// TestAudioRenditionPlaylistSizedFromAudioStream: a demuxed rendition's playlist is
// sized from its own audio Stream, not the (longer) container. Container 16.021 s with
// audio ending at 15.998 s lists 4 segments — 4, 4, 4 and the true 3.998 remainder —
// and every listed segment is served, instead of a fifth that ffmpeg never writes and
// the player stalls on.
func TestAudioRenditionPlaylistSizedFromAudioStream(t *testing.T) {
	m := NewRemuxManager(&fakeRunner{}, t.TempDir())
	dec := demuxedDecision()
	dec.File.DurationMs = 16021
	s := m.Create(CreateInput{
		UserID:  "u1",
		TitleID: "t1",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: dir, Seek: seek, VideoOnly: true})
		},
		BuildAudioRenditionArgs: func(streamID, dir string, seek transcode.SeekOffset) []string {
			return transcode.AudioRenditionArgs(transcode.AudioRenditionJob{
				SourcePath:     dec.File.Path,
				OutputDir:      dir,
				PlaylistName:   transcode.AudioRenditionPlaylist(streamID),
				SegmentPattern: transcode.AudioRenditionSegmentPattern(streamID),
			})
		},
		AudioDurationsSec: map[string]float64{"a2": 15.998},
	}, dec)
	if s.DurationMs != 16021 {
		t.Fatalf("session DurationMs = %d, want 16021 (the container)", s.DurationMs)
	}

	rt, err := m.EnsureAudioRuntime(s.ID, "a2")
	if err != nil {
		t.Fatalf("EnsureAudioRuntime: %v", err)
	}
	body, err := rt.playlist()
	if err != nil {
		t.Fatalf("playlist: %v", err)
	}
	var extinfs, names []string
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			extinfs = append(extinfs, strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","))
		case strings.HasPrefix(line, "audio_a2_"):
			names = append(names, line)
		}
	}
	wantExtinf := []string{"4.000000", "4.000000", "4.000000", "3.998000"}
	if !reflect.DeepEqual(extinfs, wantExtinf) {
		t.Fatalf("EXTINFs = %v, want %v\n%s", extinfs, wantExtinf, body)
	}
	if !strings.Contains(string(body), "#EXT-X-TARGETDURATION:4\n") {
		t.Errorf("TARGETDURATION is not 4:\n%s", body)
	}
	// Every listed segment is served: ffmpeg would write exactly these.
	if err := os.MkdirAll(s.ScratchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(s.ScratchDir, n), []byte("seg"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		if _, err := rt.segment(n); err != nil {
			t.Errorf("listed segment %s not served: %v", n, err)
		}
	}
	// A stream with no probed duration keeps the container-sized playlist.
	rt1, err := m.EnsureAudioRuntime(s.ID, "a1")
	if err != nil {
		t.Fatalf("EnsureAudioRuntime a1: %v", err)
	}
	if rt1.segmentCount != 5 {
		t.Errorf("unprobed rendition segmentCount = %d, want 5 (container fallback)", rt1.segmentCount)
	}
}

// TestNegotiateSizesDemuxedRenditionsFromTheAudio: the Negotiate → probe → session
// wiring. A real multi-audio mkv whose audio ends before its video is negotiated as a
// demuxed session; each rendition lists the segments the AUDIO fills (4 for ~15.9 s),
// not the container's (5 for 16.5 s). Remove the probe call and this fails.
func TestNegotiateSizesDemuxedRenditionsFromTheAudio(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs ffmpeg; skipped under -short")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	src := filepath.Join(t.TempDir(), "m.mkv")
	if err := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=16.5:size=160x120:rate=24",
		"-f", "lavfi", "-i", "sine=duration=15.9",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=15.9",
		"-map", "0", "-map", "1", "-map", "2",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", src).Run(); err != nil {
		t.Skipf("fixture gen failed: %v", err)
	}
	f := multiAudioMKVFile()
	f.Path = src
	f.DurationMs = 16500
	f.Present = true
	f.Streams[0].Index, f.Streams[1].Index, f.Streams[2].Index = 0, 1, 2
	detail := store.TitleDetail{Editions: []store.Edition{{ID: "e1", Name: "1080p", Files: []store.File{f}}}}
	detail.Title.ID = "t1"
	svc := NewService(remuxSelStore{detail: detail}, &fakeRunner{}, t.TempDir(), Governance{})

	dec, sess, unsup, busy, err := svc.Negotiate(Request{
		UserID: "u1", TitleID: "t1", Profile: h264Profile(),
		Constraints:   Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"},
		AudioStreamID: "a-ja",
		Scope:         access.Scope{AllLibraries: true},
	})
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if !IsDemuxed(dec) {
		t.Fatalf("decision is not demuxed (tier %q); the fixture no longer exercises renditions", dec.Tier)
	}
	for _, id := range []string{"a-en", "a-ja"} {
		rt, err := svc.Sessions().EnsureAudioRuntime(sess.ID, id)
		if err != nil {
			t.Fatalf("EnsureAudioRuntime %s: %v", id, err)
		}
		if rt.segmentCount != 4 {
			t.Errorf("%s rendition segmentCount = %d, want 4 (audio ~15.9 s; the 16.5 s container would list 5)", id, rt.segmentCount)
		}
	}
}

// deadStore fails every Title read, standing in for a catalog the per-segment audio
// path must not touch.
type deadStore struct{ ceilingStore }

func (deadStore) TitleByID(string) (store.TitleDetail, error) {
	return store.TitleDetail{}, errors.New("catalog read on the audio segment path")
}

// TestAudioRenditionValidationNeedsNoCatalogRead: every 4 s audio segment resolves
// its rendition, so that must be answered from the Manager's own per-session state —
// a Title-tree read plus an ffprobe per segment stalls playback on a slow mount.
// Ownership, the direct-play gate and the stream-id check all still hold.
func TestAudioRenditionValidationNeedsNoCatalogRead(t *testing.T) {
	svc := NewService(deadStore{}, &fakeRunner{}, t.TempDir(), Governance{})
	dec := demuxedDecision()
	s := svc.Sessions().Create(CreateInput{
		UserID: "u1", TitleID: "t1",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string { return nil },
		BuildAudioRenditionArgs: func(streamID, dir string, seek transcode.SeekOffset) []string {
			return nil
		},
	}, dec)
	if _, err := svc.audioRuntimeFor("u1", s.ID, "a2"); err != nil {
		t.Fatalf("audioRuntimeFor: %v (it must not read the catalog)", err)
	}
	if _, err := svc.audioRuntimeFor("u1", s.ID, "nope"); !errors.Is(err, ErrNoAudioRendition) {
		t.Errorf("unknown stream id err = %v, want ErrNoAudioRendition", err)
	}
	if _, err := svc.audioRuntimeFor("u1", s.ID, "v1"); !errors.Is(err, ErrNoAudioRendition) {
		t.Errorf("a video stream id err = %v, want ErrNoAudioRendition", err)
	}
	if _, err := svc.audioRuntimeFor("u2", s.ID, "a2"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("foreign user err = %v, want ErrSessionNotFound", err)
	}
	direct := svc.Sessions().Create(CreateInput{UserID: "u1", TitleID: "t1"}, Decision{Tier: TierDirectPlay, Edition: dec.Edition, File: dec.File})
	if _, err := svc.audioRuntimeFor("u1", direct.ID, "a2"); !errors.Is(err, ErrNotHLS) {
		t.Errorf("direct-play session err = %v, want ErrNotHLS", err)
	}
}
