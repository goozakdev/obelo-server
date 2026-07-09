package playback

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/transcode"
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
