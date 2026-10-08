package onlinesource

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The ffmpeg path of an Online play (ADR-0068 decisions 4, 9, 10): a variant the
// client cannot take as is goes to ffmpeg, under the transcode cap, with the
// protocol whitelist, and leaves nothing in the cache when its session ends.

type fakeProvider struct {
	mu       sync.Mutex
	variants []pluginapi.OnlineVariant
	hints    []pluginapi.OnlineHints
}

func (f *fakeProvider) Rows(context.Context, pluginapi.OnlineRowsRequest) (pluginapi.OnlineRowsResponse, error) {
	return pluginapi.OnlineRowsResponse{}, nil
}

func (f *fakeProvider) Row(context.Context, pluginapi.OnlineRowRequest) (pluginapi.OnlineRowResponse, error) {
	return pluginapi.OnlineRowResponse{}, nil
}

func (f *fakeProvider) Resolve(_ context.Context, req pluginapi.OnlineResolveRequest) (pluginapi.OnlineResolveResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hints = append(f.hints, req.Hints)
	return pluginapi.OnlineResolveResponse{Variants: f.variants}, nil
}

// fakeRunner records every ffmpeg run, writes a playlist and a segment into the run's
// output directory the way the real one would, and ends with whatever result the
// test set.
type fakeRunner struct {
	mu      sync.Mutex
	runs    [][]string
	jobs    []*fakeJob
	waitErr error
	startOK error
}

type fakeJob struct {
	killed chan struct{}
	once   sync.Once
	err    error
}

func (j *fakeJob) Wait() error {
	if j.err != nil {
		return j.err
	}
	<-j.killed
	return nil
}

func (j *fakeJob) Kill() error { j.once.Do(func() { close(j.killed) }); return nil }

func (r *fakeRunner) Start(_ context.Context, args []string) (transcode.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startOK != nil {
		return nil, r.startOK
	}
	r.runs = append(r.runs, args)
	out := args[len(args)-1]
	dir := filepath.Dir(out)
	_ = os.WriteFile(out, []byte("#EXTM3U\n#EXT-X-VERSION:3\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "segment000.ts"), []byte("ts"), 0o644)
	j := &fakeJob{killed: make(chan struct{}), err: r.waitErr}
	r.jobs = append(r.jobs, j)
	return j, nil
}

func (r *fakeRunner) argv() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.runs...)
}

type ffmpegRig struct {
	svc      *Service
	provider *fakeProvider
	runner   *fakeRunner
	scratch  string
	slots    *slots
}

// slots stands in for the session Manager's transcode cap.
type slots struct {
	mu   sync.Mutex
	free int
	held int
}

func (s *slots) reserve() (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.free <= 0 {
		return nil, playback.ErrTranscodeCapFull
	}
	s.free--
	s.held++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.free++
			s.held--
		})
	}, nil
}

func (s *slots) heldNow() int { s.mu.Lock(); defer s.mu.Unlock(); return s.held }

func newFFmpegRig(t *testing.T, variants ...pluginapi.OnlineVariant) *ffmpegRig {
	t.Helper()
	rig := &ffmpegRig{provider: &fakeProvider{variants: variants}, runner: &fakeRunner{}, scratch: t.TempDir(), slots: &slots{free: 2}}
	reg := pluginapi.NewRegistry()
	reg.RegisterOnlineSourceProvider(pluginapi.OnlineSourceProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "tube", Name: "Test Tube"},
		New:        func(pluginapi.Settings) (pluginapi.OnlineSourceProvider, error) { return rig.provider, nil },
	})
	rig.svc = New(reg, nil)
	rig.svc.SetMediaHostPolicy(func(_, host string) bool { return host == "cdn.example.test" })
	rig.svc.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	rig.svc.SetTranscoder(Transcoder{Runner: rig.runner, ScratchRoot: rig.scratch, Reserve: rig.slots.reserve})
	return rig
}

func canPlayMP4() playback.DeviceProfile {
	return playback.DeviceProfile{
		Containers:  []string{"mp4"},
		VideoCodecs: []playback.VideoCodecSupport{{Codec: "h264"}},
		AudioCodecs: []string{"aac"},
	}
}

func (r *ffmpegRig) play(t *testing.T, c playback.Constraints) (Session, *playback.Unsupported, error) {
	t.Helper()
	return r.svc.Play(context.Background(), PlayInput{UserID: "u1", SourceID: "tube", ItemID: "v1", Profile: canPlayMP4(), Constraints: c})
}

func scratchEntries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && p != dir {
			out = append(out, strings.TrimPrefix(p, dir))
		}
		return nil
	})
	return out
}

const whitelist = "https,tls,tcp,crypto"

func whitelistCount(args []string) int {
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-protocol_whitelist" && args[i+1] == whitelist {
			n++
		}
	}
	return n
}

func splitV() pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{
		Kind: pluginapi.OnlineVariantSplit, VideoURL: "https://cdn.example.test/v.mp4", AudioURL: "https://cdn.example.test/a.m4a",
		Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "1080p",
		Headers: map[string]string{"Referer": "https://tube.example/", "User-Agent": "Tube/1", "Authorization": "Bearer secret"},
	}
}

func manifestV() pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{Kind: pluginapi.OnlineVariantManifest, URL: "https://cdn.example.test/master.m3u8", Container: "hls", Resolution: "720p"}
}

func muxedV(res string) pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{URL: "https://cdn.example.test/" + res + ".mp4", Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: res}
}

// TestEveryOnlineFFmpegPathCarriesTheProtocolWhitelist: a split variant, a manifest
// and a muxed variant above the ceiling each reach ffmpeg with the whitelist on
// every input, and a muxed variant the client can play never reaches ffmpeg at all.
func TestEveryOnlineFFmpegPathCarriesTheProtocolWhitelist(t *testing.T) {
	for name, tc := range map[string]struct {
		variant    pluginapi.OnlineVariant
		constraint playback.Constraints
		inputs     int
	}{
		"split":        {splitV(), playback.Constraints{}, 2},
		"manifest":     {manifestV(), playback.Constraints{}, 1},
		"over-ceiling": {muxedV("1080p"), playback.Constraints{MaxResolution: "480p"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newFFmpegRig(t, tc.variant)
			sess, unsup, err := rig.play(t, tc.constraint)
			if err != nil || unsup != nil || !sess.Transcoded {
				t.Fatalf("Play = %+v, %v, %v; want a transcoded session", sess, unsup, err)
			}
			runs := rig.runner.argv()
			if len(runs) != 1 {
				t.Fatalf("ffmpeg ran %d times, want once", len(runs))
			}
			if n := whitelistCount(runs[0]); n != tc.inputs {
				t.Fatalf("%d inputs carry the whitelist, want %d; args: %v", n, tc.inputs, runs[0])
			}
		})
	}

	rig := newFFmpegRig(t, muxedV("720p"))
	sess, _, err := rig.play(t, playback.Constraints{})
	if err != nil || sess.Transcoded || len(rig.runner.argv()) != 0 {
		t.Fatalf("a muxed variant the client can play = %+v, %v, %d ffmpeg runs; want a relay and none", sess, err, len(rig.runner.argv()))
	}
}

// TestTheVariantsHeadersAndTheCeilingReachFFmpeg: the media host's Referer and
// User-Agent are passed to ffmpeg, a header outside the allowed set is not, and the
// ceiling bounds the encode and reaches resolve() as a hint.
func TestTheVariantsHeadersAndTheCeilingReachFFmpeg(t *testing.T) {
	rig := newFFmpegRig(t, splitV())
	if _, _, err := rig.play(t, playback.Constraints{MaxResolution: "480p", MaxBitrate: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(rig.runner.argv()[0], "\x00")
	for _, want := range []string{"-user_agent\x00Tube/1", "Referer: https://tube.example/\r\n", "min(480,ih)", "-maxrate\x001000000"} {
		if !strings.Contains(args, want) {
			t.Errorf("ffmpeg args lack %q: %q", want, args)
		}
	}
	if strings.Contains(args, "Bearer secret") || strings.Contains(args, "Authorization") {
		t.Errorf("a header outside the allowed set reached ffmpeg: %q", args)
	}
	if len(rig.provider.hints) != 1 || rig.provider.hints[0].MaxHeight != 480 {
		t.Errorf("resolve() hints = %+v, want maxHeight 480", rig.provider.hints)
	}
}

// TestAVariantWhoseFirstURLFailsTheCheckNeverReachesFFmpeg: the first-URL judgment
// runs before the choice, on both paths; a host off the allowlist leaves the source
// not responding and starts nothing.
func TestAVariantWhoseFirstURLFailsTheCheckNeverReachesFFmpeg(t *testing.T) {
	bad := splitV()
	bad.AudioURL = "https://elsewhere.example.test/a.m4a"
	for name, v := range map[string]pluginapi.OnlineVariant{
		"split audio off the allowlist": bad,
		"plain http manifest":           {Kind: pluginapi.OnlineVariantManifest, URL: "http://cdn.example.test/m.m3u8"},
		"relayable muxed off the list":  {URL: "https://elsewhere.example.test/v.mp4", Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "720p"},
	} {
		rig := newFFmpegRig(t, v)
		if _, _, err := rig.play(t, playback.Constraints{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: Play = %v, want ErrUnavailable", name, err)
		}
		if len(rig.runner.argv()) != 0 || rig.svc.Count() != 0 || rig.slots.heldNow() != 0 {
			t.Errorf("%s: ffmpeg ran %d times, %d sessions, %d slots held; want none", name, len(rig.runner.argv()), rig.svc.Count(), rig.slots.heldNow())
		}
	}
}

// TestTheTranscodeCapRefusesAnOnlineEncodeAsItDoesATitle: at the cap Play reports
// ErrBusy, creates no session and no scratch; a relay is not metered; ending an
// encode frees its slot.
func TestTheTranscodeCapRefusesAnOnlineEncodeAsItDoesATitle(t *testing.T) {
	rig := newFFmpegRig(t, splitV())
	rig.slots.free = 1
	first, _, err := rig.play(t, playback.Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = rig.play(t, playback.Constraints{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second encode at a cap of 1 = %v, want ErrBusy", err)
	}
	if rig.svc.Count() != 1 || len(rig.runner.argv()) != 1 || len(scratchEntries(t, rig.scratch)) == 0 {
		t.Fatalf("a refused encode left a session, a run or nothing of the first: %d sessions, %d runs", rig.svc.Count(), len(rig.runner.argv()))
	}
	rig.svc.End(first.ID)
	if rig.slots.heldNow() != 0 {
		t.Fatalf("ending the encode left %d slots held", rig.slots.heldNow())
	}
	if _, _, err = rig.play(t, playback.Constraints{}); err != nil {
		t.Fatalf("after the slot was freed: %v", err)
	}

	relay := newFFmpegRig(t, muxedV("720p"))
	relay.slots.free = 0
	if _, _, err = relay.play(t, playback.Constraints{}); err != nil {
		t.Fatalf("a relay at a full cap = %v, want it unmetered", err)
	}
}

// TestFFmpegFailingToStartLeavesNothing: a run that cannot start is a source not
// responding, with the slot back and no scratch behind.
func TestFFmpegFailingToStartLeavesNothing(t *testing.T) {
	rig := newFFmpegRig(t, splitV())
	rig.runner.startOK = errors.New("no ffmpeg")
	if _, _, err := rig.play(t, playback.Constraints{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Play = %v, want ErrUnavailable", err)
	}
	if rig.slots.heldNow() != 0 || rig.svc.Count() != 0 || len(scratchEntries(t, rig.scratch)) != 0 {
		t.Fatalf("a failed start left %d slots, %d sessions, %v", rig.slots.heldNow(), rig.svc.Count(), scratchEntries(t, rig.scratch))
	}
}

// TestAnFFmpegSessionLeavesNoSegmentsWhenItEnds: Q2. The segments are in the cache
// while the session lives, and gone — directory and all — after the client stops it,
// after the idle reaper ends it, after shutdown, and after ffmpeg dies on its own.
func TestAnFFmpegSessionLeavesNoSegmentsWhenItEnds(t *testing.T) {
	for name, end := range map[string]func(rig *ffmpegRig, s Session){
		"client stop": func(rig *ffmpegRig, s Session) { rig.svc.End(s.ID) },
		"idle reap": func(rig *ffmpegRig, s Session) {
			rig.svc.now = func() time.Time { return time.Now().Add(time.Hour) }
			rig.svc.Reap(time.Minute)
		},
		"shutdown": func(rig *ffmpegRig, s Session) { rig.svc.EndAll() },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newFFmpegRig(t, splitV())
			sess, _, err := rig.play(t, playback.Constraints{})
			if err != nil {
				t.Fatal(err)
			}
			if got := scratchEntries(t, rig.scratch); len(got) == 0 {
				t.Fatal("the encode left no segments in the cache while the session lived")
			}
			end(rig, sess)
			if got := scratchEntries(t, rig.scratch); len(got) != 0 {
				t.Fatalf("segments left in the cache after the session ended: %v", got)
			}
			if rig.slots.heldNow() != 0 {
				t.Fatalf("%d transcode slots still held", rig.slots.heldNow())
			}
			select {
			case <-rig.runner.jobs[0].killed:
			default:
				t.Fatal("ffmpeg was not stopped")
			}
		})
	}

	t.Run("ffmpeg error", func(t *testing.T) {
		rig := newFFmpegRig(t, splitV())
		rig.runner.waitErr = errors.New("ffmpeg exited: signal: segmentation fault")
		var endedMu sync.Mutex
		var ended []string
		rig.svc.SetOnEnd(func(id string) { endedMu.Lock(); ended = append(ended, id); endedMu.Unlock() })
		sess, _, err := rig.play(t, playback.Constraints{})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			endedMu.Lock()
			done := len(ended) > 0
			endedMu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		endedMu.Lock()
		defer endedMu.Unlock()
		if rig.svc.Count() != 0 || len(ended) != 1 || ended[0] != sess.ID {
			t.Fatalf("a dead ffmpeg left %d sessions, end callbacks %v; want the session ended", rig.svc.Count(), ended)
		}
		if got := scratchEntries(t, rig.scratch); len(got) != 0 {
			t.Fatalf("segments left in the cache after ffmpeg failed: %v", got)
		}
		if rig.slots.heldNow() != 0 {
			t.Fatalf("%d transcode slots still held", rig.slots.heldNow())
		}
	})
}

// TestTheEncodedFilesAreServedOnlyByTheirOwnNames: the playlist and segments of a
// live session open; any other name, a path, or a session without an encode does not.
func TestTheEncodedFilesAreServedOnlyByTheirOwnNames(t *testing.T) {
	rig := newFFmpegRig(t, splitV())
	sess, _, err := rig.play(t, playback.Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.m3u8", "segment000.ts"} {
		f, err := rig.svc.OpenEncoded(context.Background(), sess.ID, name)
		if err != nil {
			t.Fatalf("OpenEncoded(%s): %v", name, err)
		}
		f.Close()
	}
	for _, name := range []string{"../index.m3u8", "segment000.ts/../../x", "secret.txt", "index.m3u8.tmp", "segment1.ts", "", "/etc/passwd"} {
		if f, err := rig.svc.OpenEncoded(context.Background(), sess.ID, name); err == nil {
			f.Close()
			t.Errorf("OpenEncoded(%q) opened a file", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if f, err := rig.svc.OpenEncoded(ctx, sess.ID, "segment099.ts"); err == nil {
		f.Close()
		t.Error("a segment ffmpeg has not written opened")
	}
	if _, err := rig.svc.OpenEncoded(context.Background(), "nope", "index.m3u8"); !errors.Is(err, ErrNoSession) {
		t.Errorf("unknown session = %v, want ErrNoSession", err)
	}
	relay := newFFmpegRig(t, muxedV("720p"))
	rs, _, _ := relay.play(t, playback.Constraints{})
	if _, err := relay.svc.OpenEncoded(context.Background(), rs.ID, "index.m3u8"); !errors.Is(err, ErrNoSession) {
		t.Errorf("a relayed session = %v, want ErrNoSession", err)
	}
}
