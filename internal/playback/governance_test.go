package playback

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// Unit tests for the transcode-governance accounting (ADR-0009): only the
// transcode tier consumes a cap slot, the cap rejects (never queues) at the
// limit, and ending/reaping a transcode frees its slot so a previously-rejected
// transcode can then succeed. These exercise the Manager directly (no ffmpeg);
// the real-ffmpeg end-to-end governance check lives in the api integration test.

func transcodeDecision(id string) Decision {
	return Decision{
		Tier:             TierTranscode,
		Edition:          store.Edition{ID: "e-" + id},
		File:             store.File{ID: "f-" + id, Path: "/movies/" + id + ".mkv", Bitrate: 8_000_000},
		EstimatedBitrate: 8_000_000,
	}
}

// TestTranscodeCapRejectsAtLimit: with a cap of 1, the first transcode takes the
// slot and the second is rejected with ErrTranscodeCapFull WITHOUT creating a
// session — reject-don't-queue.
func TestTranscodeCapRejectsAtLimit(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	s1, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatalf("first transcode: unexpected err %v", err)
	}
	if s1.ID == "" {
		t.Fatal("first transcode created no session")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after 1, want 1", got)
	}

	s2, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b"))
	if !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("second transcode err = %v, want ErrTranscodeCapFull", err)
	}
	if s2.ID != "" {
		t.Error("rejected transcode still created a session")
	}
	// The rejection must not have leaked a slot or a map entry.
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after a rejection, want 1 (no leak)", got)
	}
	if got := m.Count(); got != 1 {
		t.Errorf("session count = %d after a rejection, want 1", got)
	}
}

// TestDirectPlayAndRemuxAreUnmetered: neither direct play nor remux consumes a
// slot or hits the cap, even when the cap is 1 and exhausted by a transcode.
func TestDirectPlayAndRemuxAreUnmetered(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	// Fill the single transcode slot.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}

	// Direct play and remux still succeed at the cap and never increment the count.
	dp := Decision{Tier: TierDirectPlay, Edition: store.Edition{ID: "e2"}, File: store.File{ID: "f2", Path: "/m/dp.mp4"}}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, dp); err != nil {
		t.Errorf("direct play rejected at transcode cap: %v", err)
	}
	rm := Decision{Tier: TierDirectStream, Edition: store.Edition{ID: "e3"}, File: store.File{ID: "f3", Path: "/m/rm.mkv"}}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u3"}, rm); err != nil {
		t.Errorf("remux rejected at transcode cap: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d, want 1 (only the transcode counts)", got)
	}
	if got := m.Count(); got != 3 {
		t.Errorf("session count = %d, want 3 (transcode + directPlay + remux)", got)
	}
}

// TestEndFreesTranscodeSlot: ending the transcode that holds the only slot lets
// a previously-rejected transcode through.
func TestEndFreesTranscodeSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	s1, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatalf("first transcode: %v", err)
	}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("second transcode err = %v, want ErrTranscodeCapFull", err)
	}

	// Free the slot.
	if !m.End(s1.ID) {
		t.Fatal("End returned false for the live transcode")
	}
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after End, want 0 (slot freed)", got)
	}

	// A new transcode now succeeds.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u3"}, transcodeDecision("c")); err != nil {
		t.Errorf("transcode after freed slot rejected: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d, want 1", got)
	}
}

// TestReapFreesTranscodeSlot: reaping an idle transcode frees its slot exactly
// as a clean End would, so an abandoned transcode never permanently holds the cap.
func TestReapFreesTranscodeSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	base := time.Now()
	m.SetNow(func() time.Time { return base })
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}

	// Advance the clock past the idle window and reap.
	m.SetNow(func() time.Time { return base.Add(time.Hour) })
	if n := m.Reap(time.Minute); n != 1 {
		t.Fatalf("Reap swept %d, want 1", n)
	}
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after reap, want 0", got)
	}
	// A new transcode now fits.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b")); err != nil {
		t.Errorf("transcode after reap rejected: %v", err)
	}
}

// TestEndingDirectPlayDoesNotTouchSlot: ending a direct-play session never
// decrements the transcode counter (it never incremented it).
func TestEndingDirectPlayDoesNotTouchSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(2)

	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	dp := Decision{Tier: TierDirectPlay, Edition: store.Edition{ID: "e2"}, File: store.File{ID: "f2", Path: "/m/dp.mp4"}}
	dpSess, err := m.CreateGoverned(CreateInput{UserID: "u2"}, dp)
	if err != nil {
		t.Fatalf("direct play: %v", err)
	}

	if !m.End(dpSess.ID) {
		t.Fatal("End(directPlay) returned false")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after ending a direct-play session, want 1 (unchanged)", got)
	}
}

// TestUnlimitedCapNeverRejects: a cap of 0 means unlimited — transcodes are
// still metered (for observability) but never rejected.
func TestUnlimitedCapNeverRejects(t *testing.T) {
	m := NewManager() // cap defaults to 0 (unlimited)
	for i := 0; i < 5; i++ {
		if _, err := m.CreateGoverned(CreateInput{UserID: "u"}, transcodeDecision(string(rune('a'+i)))); err != nil {
			t.Fatalf("transcode %d rejected under unlimited cap: %v", i, err)
		}
	}
	if got := m.ActiveTranscodes(); got != 5 {
		t.Errorf("activeTranscodes = %d, want 5 (metered even when unlimited)", got)
	}
}

// TestSuggestBusyBitrate exercises the pure suggestedMaxBitrate heuristic
// (ADR-0009 "suggested lower bitrate"): half the estimate, floored, always a
// real step down, with sensible fallbacks when inputs are missing.
func TestSuggestBusyBitrate(t *testing.T) {
	tests := []struct {
		name      string
		estimated int64
		requested int64
		want      int64
	}{
		{"half of estimate", 8_000_000, 0, 4_000_000},
		{"falls back to requested when no estimate", 0, 6_000_000, 3_000_000},
		{"floor when estimate is low", 1_000_000, 0, busyBitrateFloor},
		{"floor when nothing known", 0, 0, busyBitrateFloor},
		{"prefers estimate over requested", 10_000_000, 2_000_000, 5_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestBusyBitrate(tc.estimated, tc.requested)
			if got != tc.want {
				t.Errorf("suggestBusyBitrate(%d, %d) = %d, want %d", tc.estimated, tc.requested, got, tc.want)
			}
			// Invariant: the suggestion must be a genuine step down from the base it
			// derived from (or the floor), so the client does not loop on busy.
			base := tc.estimated
			if base <= 0 {
				base = tc.requested
			}
			if base > 0 && got >= base && got != busyBitrateFloor {
				t.Errorf("suggestion %d is not below base %d", got, base)
			}
		})
	}
}

// TestLocalTranscodesCountsEveryReencodeRunHere: every live Transcode this host
// runs counts — a video encode, a video copy re-encoding only its audio, an
// audio-only encode — whether or not it holds a cap slot; a direct play, a remux
// and a relayed transcode (running on the sharer) do not.
func TestLocalTranscodesCountsEveryReencodeRunHere(t *testing.T) {
	m := NewManager()
	copyVideo := transcodeDecision("copy")
	copyVideo.VideoCopy = true
	audioOnly := transcodeDecision("audio")
	audioOnly.AudioOnly = true
	relayed := transcodeDecision("relay")
	relayed.Relay = &Relayed{LinkID: "l", RemoteSessionID: "r", RemoteTitleID: "t"}

	for _, d := range []Decision{{Tier: TierDirectPlay}, {Tier: TierDirectStream}, relayed} {
		m.Create(CreateInput{UserID: "u"}, d)
	}
	if n := m.LocalTranscodes(); n != 0 {
		t.Fatalf("local transcodes = %d with only direct play, remux and a relay, want 0", n)
	}
	var ids []string
	for i, d := range []Decision{copyVideo, audioOnly, transcodeDecision("video")} {
		s, err := m.CreateGoverned(CreateInput{UserID: "u"}, d)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		if n := m.LocalTranscodes(); n != i+1 {
			t.Fatalf("local transcodes = %d after %d re-encodes, want %d", n, i+1, i+1)
		}
	}
	for i, id := range ids {
		m.End(id)
		if n := m.LocalTranscodes(); n != len(ids)-i-1 {
			t.Fatalf("local transcodes = %d after ending %d, want %d", n, i+1, len(ids)-i-1)
		}
	}
}

// heldRunner starts jobs that run until killed, like a real ffmpeg partway
// through a long File, so a test can observe what is running.
type heldRunner struct{}

func (heldRunner) Start(_ context.Context, _ []string) (transcode.Job, error) {
	return &heldJob{done: make(chan struct{})}, nil
}

type heldJob struct {
	once sync.Once
	done chan struct{}
}

func (j *heldJob) Wait() error { <-j.done; return nil }
func (j *heldJob) Kill() error { j.once.Do(func() { close(j.done) }); return nil }

// TestLocalTranscodesCountsRemuxReencodingAudioAfterSeek: a remux copies every
// stream from the top, but once a seek realigns it the job re-encodes the audio
// (transcode.SeekOffset.mustEncodeCopiedAudio) — from then on it counts.
func TestLocalTranscodesCountsRemuxReencodingAudioAfterSeek(t *testing.T) {
	m := NewRemuxManager(heldRunner{}, t.TempDir())
	dec := Decision{Tier: TierDirectStream, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 60_000}}
	s := m.Create(CreateInput{
		UserID: "u",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: dir, Seek: seek})
		},
	}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)
	if err := rt.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	if n := m.LocalTranscodes(); n != 0 {
		t.Fatalf("local transcodes = %d with a pure copy remux, want 0", n)
	}
	if err := rt.realign(5); err != nil {
		t.Fatal(err)
	}
	if n := m.LocalTranscodes(); n != 1 {
		t.Fatalf("local transcodes = %d with a remux re-encoding its audio after a seek, want 1", n)
	}
	m.End(s.ID)
	if n := m.LocalTranscodes(); n != 0 {
		t.Fatalf("local transcodes = %d after the remux ended, want 0", n)
	}
}

// TestLocalTranscodesCountsRemuxEncodingAnAudioRendition: a demuxed remux copies
// its video, but an audio rendition the client cannot decode (TrueHD, DTS) is
// encoded to AAC (ADR-0022) — the session counts while that rendition runs, and
// not for a rendition that is copied.
func TestLocalTranscodesCountsRemuxEncodingAnAudioRendition(t *testing.T) {
	m := NewRemuxManager(heldRunner{}, t.TempDir())
	dec := demuxedDecision()
	s := m.Create(CreateInput{
		UserID: "u",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: dir, Seek: seek, VideoOnly: true})
		},
		BuildAudioRenditionArgs: func(streamID, dir string, seek transcode.SeekOffset) []string {
			return transcode.AudioRenditionArgs(transcode.AudioRenditionJob{
				SourcePath:     dec.File.Path,
				OutputDir:      dir,
				Copy:           streamID == "a1",
				PlaylistName:   transcode.AudioRenditionPlaylist(streamID),
				SegmentPattern: transcode.AudioRenditionSegmentPattern(streamID),
				Seek:           seek,
			})
		},
	}, dec)
	rt, _ := m.remuxRuntimeFor(s.ID)
	if err := rt.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	copied, err := m.EnsureAudioRuntime(s.ID, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if err := copied.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	if n := m.LocalTranscodes(); n != 0 {
		t.Fatalf("local transcodes = %d with a video copy and a copied rendition, want 0", n)
	}
	encoded, err := m.EnsureAudioRuntime(s.ID, "a2")
	if err != nil {
		t.Fatal(err)
	}
	if err := encoded.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	if n := m.LocalTranscodes(); n != 1 {
		t.Fatalf("local transcodes = %d with a rendition encoding to AAC, want 1", n)
	}
	m.End(s.ID)
	if n := m.LocalTranscodes(); n != 0 {
		t.Fatalf("local transcodes = %d after the session ended, want 0", n)
	}
}

// scriptedRunner starts jobs a test finishes by hand, and refuses to start once
// refuse is set. A job ignores Kill when stubborn is set, like a process that
// takes its time to die, so only the kill's own bookkeeping can clear the flag.
type scriptedRunner struct {
	mu       sync.Mutex
	jobs     []*heldJob
	refuse   bool
	stubborn bool
}

func (r *scriptedRunner) Start(_ context.Context, _ []string) (transcode.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refuse {
		return nil, errors.New("ffmpeg would not start")
	}
	j := &heldJob{done: make(chan struct{})}
	r.jobs = append(r.jobs, j)
	if r.stubborn {
		return stubbornJob{j}, nil
	}
	return j, nil
}

func (r *scriptedRunner) last() *heldJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[len(r.jobs)-1]
}

func (r *scriptedRunner) set(refuse, stubborn bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse, r.stubborn = refuse, stubborn
}

// stubbornJob is a heldJob whose Kill does not end it.
type stubbornJob struct{ *heldJob }

func (stubbornJob) Kill() error { return nil }

// realignedRemux is a remux session whose job re-encodes its audio after a seek
// realigned it (so LocalTranscodes counts it), on runner r.
func realignedRemux(t *testing.T, r transcode.Runner) (*Manager, *hlsRuntime) {
	t.Helper()
	m := NewRemuxManager(r, t.TempDir())
	dec := Decision{Tier: TierDirectStream, Edition: store.Edition{ID: "e1"}, File: store.File{ID: "f1", Path: "/m/x.mkv", DurationMs: 60_000}}
	s := m.Create(CreateInput{
		UserID: "u",
		BuildHLSArgs: func(dir string, seek transcode.SeekOffset) []string {
			return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: dir, Seek: seek})
		},
	}, dec)
	t.Cleanup(func() { m.End(s.ID) })
	rt, _ := m.remuxRuntimeFor(s.ID)
	if err := rt.EnsureStarted(); err != nil {
		t.Fatal(err)
	}
	if err := rt.realign(5); err != nil {
		t.Fatal(err)
	}
	if n := m.LocalTranscodes(); n != 1 {
		t.Fatalf("local transcodes = %d with a remux re-encoding its audio after a seek, want 1", n)
	}
	return m, rt
}

// TestAReencodeThatFinishesStopsCounting: a re-encoding job that runs to its end
// on its own is no longer re-encoding, though the session lives on. Were it
// still counted, Marker detection would wait on it for the rest of the session.
func TestAReencodeThatFinishesStopsCounting(t *testing.T) {
	r := &scriptedRunner{}
	m, rt := realignedRemux(t, r)
	r.last().Kill() // ffmpeg reaches the end of the File
	deadline := time.Now().Add(5 * time.Second)
	for !rt.hasExited() {
		if time.Now().After(deadline) {
			t.Fatal("the job's exit was never observed")
		}
		time.Sleep(time.Millisecond)
	}
	if n := m.LocalTranscodes(); n != 0 {
		t.Errorf("local transcodes = %d once the re-encoding job finished, want 0", n)
	}
}

// TestAKilledReencodeStopsCountingAtOnce: a seek kills the re-encoding job and
// the restart fails. The killed job is not re-encoding any more from the moment
// it is killed, whenever its process gets round to exiting.
func TestAKilledReencodeStopsCountingAtOnce(t *testing.T) {
	r := &scriptedRunner{}
	r.set(false, true)
	m, rt := realignedRemux(t, r)
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, j := range r.jobs {
			j.Kill() // let the stubborn processes exit before the session ends
		}
	})
	r.set(true, true)
	if err := rt.realign(9); err == nil {
		t.Fatal("realign succeeded with a runner that refuses to start")
	}
	if n := m.LocalTranscodes(); n != 0 {
		t.Errorf("local transcodes = %d once the re-encoding job was killed, want 0", n)
	}
}

// TestReserveTranscodeSharesTheCapWithTitles: an Online ffmpeg play takes a slot of
// the same cap a Title transcode does, is rejected at the limit without queuing, and
// frees the slot on release (once, however often release is called).
func TestReserveTranscodeSharesTheCapWithTitles(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(2)
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatal(err)
	}
	release, err := m.ReserveTranscode()
	if err != nil {
		t.Fatalf("reserve with a free slot: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 2 {
		t.Fatalf("active = %d, want 2", got)
	}
	if _, err := m.ReserveTranscode(); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("reserve at the cap = %v, want ErrTranscodeCapFull", err)
	}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("a Title transcode beside a reserved slot = %v, want ErrTranscodeCapFull", err)
	}
	release()
	release()
	if got := m.ActiveTranscodes(); got != 1 {
		t.Fatalf("active after releasing twice = %d, want 1", got)
	}
	if _, err := m.ReserveTranscode(); err != nil {
		t.Fatalf("reserve after a release: %v", err)
	}

	unlimited := NewManager()
	for i := 0; i < 5; i++ {
		if _, err := unlimited.ReserveTranscode(); err != nil {
			t.Fatalf("an uncapped Manager refused reservation %d: %v", i, err)
		}
	}
}
