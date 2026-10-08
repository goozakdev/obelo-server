package onlinesource

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// An Online session re-resolves ONCE, when the media host answers 403 or 410
// (ADR-0068 decision 10): a relay makes a new upstream request, ffmpeg restarts at
// the position, and anything that goes wrong after that ends the session.

// seqProvider answers the nth resolve() with whatever fn says.
type seqProvider struct {
	mu    sync.Mutex
	calls int
	fn    func(n int) ([]pluginapi.OnlineVariant, error)
	// hangAfterFirst makes every resolve after the first wait for its context.
	hangAfterFirst bool
}

func (p *seqProvider) Rows(context.Context, pluginapi.OnlineRowsRequest) (pluginapi.OnlineRowsResponse, error) {
	return pluginapi.OnlineRowsResponse{}, nil
}

func (p *seqProvider) Row(context.Context, pluginapi.OnlineRowRequest) (pluginapi.OnlineRowResponse, error) {
	return pluginapi.OnlineRowResponse{}, nil
}

func (p *seqProvider) Search(context.Context, pluginapi.OnlineSearchRequest) (pluginapi.OnlineSearchResponse, error) {
	return pluginapi.OnlineSearchResponse{}, nil
}

func (p *seqProvider) Resolve(ctx context.Context, _ pluginapi.OnlineResolveRequest) (pluginapi.OnlineResolveResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	if p.hangAfterFirst && n > 1 {
		<-ctx.Done()
		return pluginapi.OnlineResolveResponse{}, ctx.Err()
	}
	v, err := p.fn(n)
	return pluginapi.OnlineResolveResponse{Variants: v}, err
}

func (p *seqProvider) resolves() int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

func newSeqService(t *testing.T, client *http.Client, fn func(n int) ([]pluginapi.OnlineVariant, error)) (*Service, *seqProvider) {
	t.Helper()
	p := &seqProvider{fn: fn}
	reg := pluginapi.NewRegistry()
	reg.RegisterOnlineSourceProvider(pluginapi.OnlineSourceProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "tube", Name: "Test Tube"},
		New:        func(pluginapi.Settings) (pluginapi.OnlineSourceProvider, error) { return p, nil },
	})
	svc := New(reg, client)
	svc.lookup = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	return svc, p
}

func playSeq(t *testing.T, svc *Service) Session {
	t.Helper()
	sess, unsup, err := svc.Play(context.Background(), PlayInput{UserID: "u1", SourceID: "tube", ItemID: "v1", Profile: canPlayMP4()})
	if err != nil || unsup != nil {
		t.Fatalf("Play = %+v, %v, %v", sess, unsup, err)
	}
	return sess
}

const goneMessage = "This video is no longer available from Test Tube"

// mediaHost serves /new.mp4 byte for byte (with Range) and answers every other path
// with status, recording each request's path and Range.
type mediaHost struct {
	srv    *httptest.Server
	body   []byte
	mu     sync.Mutex
	hits   []string
	ranges []string
}

func newMediaHost(t *testing.T, status int) *mediaHost {
	t.Helper()
	m := &mediaHost{body: bytes.Repeat([]byte("0123456789"), 500)}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hits = append(m.hits, r.URL.Path)
		m.ranges = append(m.ranges, r.Header.Get("Range"))
		m.mu.Unlock()
		if r.URL.Path == "/new.mp4" {
			http.ServeContent(w, r, "new.mp4", time.Time{}, bytes.NewReader(m.body))
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mediaHost) hitCount(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range m.hits {
		if h == path {
			n++
		}
	}
	return n
}

func (m *mediaHost) lastRange() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ranges[len(m.ranges)-1]
}

func (m *mediaHost) variant(path string) pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{URL: m.srv.URL + path, Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "720p"}
}

func (m *mediaHost) service(t *testing.T, fn func(n int) ([]pluginapi.OnlineVariant, error)) (*Service, *seqProvider) {
	t.Helper()
	svc, p := newSeqService(t, m.srv.Client(), fn)
	svc.SetMediaHostPolicy(func(_, host string) bool { return host == "127.0.0.1" })
	svc.ExemptMediaAddrs(m.srv.Listener.Addr().String())
	return svc, p
}

func assertGone(t *testing.T, svc *Service, id string) {
	t.Helper()
	if _, live := svc.Session(id); live {
		t.Fatal("the session is still live, want it ended")
	}
	msg, ok := svc.Gone(id, "u1")
	if !ok || msg != goneMessage {
		t.Fatalf("Gone = %q, %v; want %q", msg, ok, goneMessage)
	}
	if _, ok := svc.Gone(id, "someone-else"); ok {
		t.Fatal("another User can read the ended session's note")
	}
}

// TestARelayedSessionReResolvesOnceOnA403Or410AndContinuesFromTheRange: the media host
// refuses the first URL; the Server resolves again, once, and asks the new URL for
// the same bytes the client asked for.
func TestARelayedSessionReResolvesOnceOnA403Or410AndContinuesFromTheRange(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			m := newMediaHost(t, status)
			svc, p := m.service(t, func(n int) ([]pluginapi.OnlineVariant, error) {
				if n == 1 {
					return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
				}
				return []pluginapi.OnlineVariant{m.variant("/new.mp4")}, nil
			})
			sess := playSeq(t, svc)

			resp, err := svc.OpenMedia(context.Background(), sess, "bytes=1000-", "")
			if err != nil {
				t.Fatalf("OpenMedia = %v, want the re-resolved bytes", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent || !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 1000-") {
				t.Fatalf("answer = %d %q, want a 206 from byte 1000", resp.StatusCode, resp.Header.Get("Content-Range"))
			}
			if got := p.resolves(); got != 2 {
				t.Fatalf("resolve() ran %d times, want 2 (the play and one re-resolve)", got)
			}
			if m.hitCount("/old.mp4") != 1 || m.hitCount("/new.mp4") != 1 || m.lastRange() != "bytes=1000-" {
				t.Fatalf("hits old=%d new=%d range=%q; want 1, 1 and bytes=1000-", m.hitCount("/old.mp4"), m.hitCount("/new.mp4"), m.lastRange())
			}
			now, live := svc.Session(sess.ID)
			if !live || now.Variant.URL != m.srv.URL+"/new.mp4" {
				t.Fatalf("session = %+v, live=%v; want it live on the new URL", now.Variant, live)
			}

			// The next request goes straight to the new URL: no second re-resolve.
			resp2, err := svc.OpenMedia(context.Background(), now, "bytes=5-", "")
			if err != nil {
				t.Fatalf("second OpenMedia = %v", err)
			}
			resp2.Body.Close()
			if p.resolves() != 2 {
				t.Fatalf("resolve() ran %d times after a healthy request, want 2", p.resolves())
			}
		})
	}
}

// TestASecondRefusalAfterRecoveryEndsTheSession: one re-resolve per session. The
// re-resolved URL being refused too, or a refusal after a recovery, ends it.
func TestASecondRefusalAfterRecoveryEndsTheSession(t *testing.T) {
	t.Run("the new URL is refused at once", func(t *testing.T) {
		m := newMediaHost(t, http.StatusGone)
		svc, p := m.service(t, func(n int) ([]pluginapi.OnlineVariant, error) {
			return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
		})
		sess := playSeq(t, svc)
		if _, err := svc.OpenMedia(context.Background(), sess, "", ""); !errors.Is(err, ErrGone) {
			t.Fatalf("OpenMedia = %v, want ErrGone", err)
		}
		if p.resolves() != 2 {
			t.Fatalf("resolve() ran %d times, want 2", p.resolves())
		}
		assertGone(t, svc, sess.ID)
	})
	t.Run("a later refusal", func(t *testing.T) {
		m := newMediaHost(t, http.StatusForbidden)
		svc, p := m.service(t, func(n int) ([]pluginapi.OnlineVariant, error) {
			if n == 1 {
				return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
			}
			return []pluginapi.OnlineVariant{m.variant("/new.mp4")}, nil
		})
		sess := playSeq(t, svc)
		resp, err := svc.OpenMedia(context.Background(), sess, "", "")
		if err != nil {
			t.Fatalf("first OpenMedia = %v", err)
		}
		resp.Body.Close()
		// The host now revokes the new URL as well.
		svc.mu.Lock()
		svc.sessions[sess.ID].Variant.URL = m.srv.URL + "/revoked.mp4"
		svc.mu.Unlock()
		now, _ := svc.Session(sess.ID)
		if _, err := svc.OpenMedia(context.Background(), now, "", ""); !errors.Is(err, ErrGone) {
			t.Fatalf("OpenMedia after recovery = %v, want ErrGone", err)
		}
		if p.resolves() != 2 {
			t.Fatalf("resolve() ran %d times, want still 2", p.resolves())
		}
		assertGone(t, svc, sess.ID)
	})
}

// TestAFailedReResolveEndsTheSessionWithTheMessage: a Plugin error, no variants, and a
// first URL the issue-02 checks refuse each end the session.
func TestAFailedReResolveEndsTheSessionWithTheMessage(t *testing.T) {
	for name, second := range map[string]func(m *mediaHost) ([]pluginapi.OnlineVariant, error){
		"plugin error": func(*mediaHost) ([]pluginapi.OnlineVariant, error) { return nil, errors.New("boom") },
		"no variants":  func(*mediaHost) ([]pluginapi.OnlineVariant, error) { return nil, nil },
		"plain http": func(m *mediaHost) ([]pluginapi.OnlineVariant, error) {
			v := m.variant("/new.mp4")
			v.URL = strings.Replace(v.URL, "https://", "http://", 1)
			return []pluginapi.OnlineVariant{v}, nil
		},
		"host off the allowlist": func(m *mediaHost) ([]pluginapi.OnlineVariant, error) {
			v := m.variant("/new.mp4")
			v.URL = strings.Replace(v.URL, "127.0.0.1", "localhost", 1)
			return []pluginapi.OnlineVariant{v}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMediaHost(t, http.StatusGone)
			svc, p := m.service(t, func(n int) ([]pluginapi.OnlineVariant, error) {
				if n == 1 {
					return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
				}
				return second(m)
			})
			sess := playSeq(t, svc)
			if _, err := svc.OpenMedia(context.Background(), sess, "", ""); !errors.Is(err, ErrGone) {
				t.Fatalf("OpenMedia = %v, want ErrGone", err)
			}
			if p.resolves() != 2 || m.hitCount("/new.mp4") != 0 {
				t.Fatalf("resolves=%d new-URL hits=%d; want 2 and 0", p.resolves(), m.hitCount("/new.mp4"))
			}
			assertGone(t, svc, sess.ID)
		})
	}
}

// TestOtherUpstreamFailuresDoNotReResolve: a 404 or 500 is the source failing, not its
// URL expiring.
func TestOtherUpstreamFailuresDoNotReResolve(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		m := newMediaHost(t, status)
		svc, p := m.service(t, func(int) ([]pluginapi.OnlineVariant, error) {
			return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
		})
		sess := playSeq(t, svc)
		if _, err := svc.OpenMedia(context.Background(), sess, "", ""); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("status %d: OpenMedia = %v, want ErrUnavailable", status, err)
		}
		if p.resolves() != 1 {
			t.Fatalf("status %d: resolve() ran %d times, want 1", status, p.resolves())
		}
		if _, live := svc.Session(sess.ID); !live {
			t.Fatalf("status %d: the session ended", status)
		}
	}
}

// TestNoResolveHappensOnATimer: a live session, kept alive and reaped on its clock,
// never calls resolve() again by itself.
func TestNoResolveHappensOnATimer(t *testing.T) {
	m := newMediaHost(t, http.StatusGone)
	svc, p := m.service(t, func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{m.variant("/new.mp4")}, nil
	})
	now := time.Now()
	svc.now = func() time.Time { return now }
	sess := playSeq(t, svc)
	now = now.Add(time.Hour)
	svc.Touch(sess.ID)
	time.Sleep(100 * time.Millisecond)
	if n := svc.Reap(time.Minute); n != 0 {
		t.Fatalf("Reap ended %d sessions, want 0", n)
	}
	if p.resolves() != 1 {
		t.Fatalf("resolve() ran %d times with no request failing, want 1", p.resolves())
	}
}

// ffmpeg-fed sessions.

// runRecorder is an ffmpeg that fails its first run with failFirst (and every run
// with failAll), else runs until killed. Every run writes a playlist and two
// segments into its directory.
type runRecorder struct {
	mu        sync.Mutex
	runs      [][]string
	failFirst error
	failAll   error
	// skipFirst is stderr text of a first run that exits 0 at once, the way ffmpeg
	// does after skipping segments the host refused. endAfter makes every later
	// run exit 0 at once with a clean stderr, as an encode that finished.
	skipFirst string
	endAfter  bool
}

type recJob struct {
	killed chan struct{}
	once   sync.Once
	err    error
	exit0  bool
	stderr string
}

func (j *recJob) Wait() error {
	if j.err != nil {
		return j.err
	}
	if j.exit0 {
		return nil
	}
	<-j.killed
	return nil
}
func (j *recJob) Kill() error    { j.once.Do(func() { close(j.killed) }); return nil }
func (j *recJob) Stderr() string { return j.stderr }
func (j *recJob) Refused() bool  { return transcode.MediaRefusalPattern.MatchString(j.stderr) }

func (r *runRecorder) Start(_ context.Context, args []string) (transcode.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, args)
	out := args[len(args)-1]
	// Two segments, 4s and a short 3.5s one: the position is the listed 7.5s, not 2*4.
	_ = os.WriteFile(out, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.000000,\nsegment000.ts\n#EXTINF:3.500000,\nsegment001.ts\n"), 0o644)
	_ = os.WriteFile(filepath.Join(filepath.Dir(out), "segment000.ts"), []byte("ts"), 0o644)
	_ = os.WriteFile(filepath.Join(filepath.Dir(out), "segment001.ts"), []byte("ts"), 0o644)
	j := &recJob{killed: make(chan struct{}), err: r.failAll}
	if len(r.runs) == 1 && r.failFirst != nil {
		j.err = r.failFirst
	}
	if len(r.runs) == 1 && r.skipFirst != "" {
		j.exit0, j.stderr = true, r.skipFirst
	}
	if len(r.runs) > 1 && r.endAfter {
		j.exit0 = true
	}
	return j, nil
}

func (r *runRecorder) argv() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.runs...)
}

func argValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func ffmpegDown(code string) error {
	return errors.New("transcode: ffmpeg exited: exit status 8\nffmpeg stderr (tail):\n[https @ 0x1] Server returned " + code + "\nError opening input")
}

type ffmpegSeq struct {
	svc     *Service
	p       *seqProvider
	runner  *runRecorder
	scratch string
	slots   *slots
}

func newFFmpegSeq(t *testing.T, runner *runRecorder, fn func(n int) ([]pluginapi.OnlineVariant, error)) *ffmpegSeq {
	t.Helper()
	svc, p := newSeqService(t, nil, fn)
	svc.SetMediaHostPolicy(func(_, host string) bool { return host == "cdn.example.test" })
	r := &ffmpegSeq{svc: svc, p: p, runner: runner, scratch: t.TempDir(), slots: &slots{free: 2}}
	svc.SetTranscoder(Transcoder{Runner: runner, ScratchRoot: r.scratch, Reserve: r.slots.reserve})
	return r
}

func (r *ffmpegSeq) waitRuns(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(r.runner.argv()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("ffmpeg ran %d times, want %d", len(r.runner.argv()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *ffmpegSeq) waitEnded(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		// Ended means out of the map and, a moment later, its slot and files gone.
		if _, live := r.svc.Session(id); !live && r.slots.heldNow() == 0 && len(scratchEntries(t, r.scratch)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not end and clean up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func manifestAt(path string) pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{Kind: pluginapi.OnlineVariantManifest, URL: "https://cdn.example.test" + path, Container: "hls", Resolution: "720p"}
}

// TestAnFFmpegSessionRestartsAtThePositionOnA403Or410: ffmpeg dies on a refused media
// URL; the Server resolves again, once, and starts ffmpeg again on the new URL at
// the end of what was already cut, in the same directory.
func TestAnFFmpegSessionRestartsAtThePositionOnA403Or410(t *testing.T) {
	for name, mk := range map[string]func() *runRecorder{
		"403 on open": func() *runRecorder { return &runRecorder{failFirst: ffmpegDown("403 Forbidden (access denied)")} },
		"410 on open": func() *runRecorder { return &runRecorder{failFirst: ffmpegDown("410 Gone")} },
		"403 on a skipped segment, exit 0": func() *runRecorder {
			return &runRecorder{skipFirst: "[http @ 0x1] HTTP error 403 Forbidden\nFailed to open segment 4 of playlist 0\n"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := mk()
			rig := newFFmpegSeq(t, runner, func(n int) ([]pluginapi.OnlineVariant, error) {
				return []pluginapi.OnlineVariant{manifestAt("/m" + string(rune('0'+n)) + ".m3u8")}, nil
			})
			sess := playSeq(t, rig.svc)
			rig.waitRuns(t, 2)

			runs := runner.argv()
			if got := argValues(runs[1], "-i"); len(got) != 1 || got[0] != "https://cdn.example.test/m2.m3u8" {
				t.Fatalf("restart reads %v, want the re-resolved URL", got)
			}
			// The playlist lists 4s + 3.5s: the restart is at 7.5, not 2 * SegmentSeconds,
			// and ffmpeg numbers the next segment from the playlist (no -start_number).
			if got := argValues(runs[1], "-ss"); len(got) != 1 || got[0] != "7.5" {
				t.Fatalf("restart -ss = %v, want [7.5]", got)
			}
			if got := argValues(runs[1], "-start_number"); len(got) != 0 {
				t.Fatalf("restart -start_number = %v, want none", got)
			}
			if got := argValues(runs[1], "-hls_flags"); len(got) != 1 || !strings.Contains(got[0], "append_list") {
				t.Fatalf("restart -hls_flags = %v, want append_list", got)
			}
			if got := argValues(runs[0], "-ss"); len(got) != 0 {
				t.Fatalf("the first run seeks: %v", got)
			}
			if whitelistCount(runs[1]) != 1 {
				t.Fatalf("restart lost the protocol whitelist: %v", runs[1])
			}
			if filepath.Dir(runs[0][len(runs[0])-1]) != filepath.Dir(runs[1][len(runs[1])-1]) {
				t.Fatal("the restart writes elsewhere than the first run")
			}
			if rig.p.resolves() != 2 {
				t.Fatalf("resolve() ran %d times, want 2", rig.p.resolves())
			}
			if _, live := rig.svc.Session(sess.ID); !live {
				t.Fatal("the session ended")
			}
			if rig.slots.heldNow() != 1 {
				t.Fatalf("%d cap slots held after the restart, want 1", rig.slots.heldNow())
			}
			rig.svc.End(sess.ID)
			if rig.slots.heldNow() != 0 || len(scratchEntries(t, rig.scratch)) != 0 {
				t.Fatalf("ending left %d slots and %v behind", rig.slots.heldNow(), scratchEntries(t, rig.scratch))
			}
		})
	}
}

// TestASecondFFmpegRefusalEndsTheSession: the restart's own 403 ends the session, with
// no third resolve and nothing left behind.
func TestASecondFFmpegRefusalEndsTheSession(t *testing.T) {
	runner := &runRecorder{failAll: ffmpegDown("403 Forbidden (access denied)")}
	rig := newFFmpegSeq(t, runner, func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{manifestAt("/m.m3u8")}, nil
	})
	sess := playSeq(t, rig.svc)
	rig.waitEnded(t, sess.ID)
	assertGone(t, rig.svc, sess.ID)
	if rig.p.resolves() != 2 || len(runner.argv()) != 2 {
		t.Fatalf("resolves=%d ffmpeg runs=%d; want 2 and 2", rig.p.resolves(), len(runner.argv()))
	}
	if rig.slots.heldNow() != 0 || len(scratchEntries(t, rig.scratch)) != 0 {
		t.Fatalf("left %d slots and %v behind", rig.slots.heldNow(), scratchEntries(t, rig.scratch))
	}
}

// TestAFailedFFmpegReResolveEndsTheSession: a Plugin error, no variants and a URL the
// checks refuse end an ffmpeg session with the message too.
func TestAFailedFFmpegReResolveEndsTheSession(t *testing.T) {
	for name, second := range map[string]func() ([]pluginapi.OnlineVariant, error){
		"plugin error": func() ([]pluginapi.OnlineVariant, error) { return nil, errors.New("boom") },
		"no variants":  func() ([]pluginapi.OnlineVariant, error) { return nil, nil },
		"off allowlist": func() ([]pluginapi.OnlineVariant, error) {
			v := manifestAt("/m.m3u8")
			v.URL = "https://evil.example.test/m.m3u8"
			return []pluginapi.OnlineVariant{v}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &runRecorder{failFirst: ffmpegDown("410 Gone")}
			rig := newFFmpegSeq(t, runner, func(n int) ([]pluginapi.OnlineVariant, error) {
				if n == 1 {
					return []pluginapi.OnlineVariant{manifestAt("/m.m3u8")}, nil
				}
				return second()
			})
			sess := playSeq(t, rig.svc)
			rig.waitEnded(t, sess.ID)
			assertGone(t, rig.svc, sess.ID)
			if len(runner.argv()) != 1 || rig.slots.heldNow() != 0 || len(scratchEntries(t, rig.scratch)) != 0 {
				t.Fatalf("ffmpeg runs=%d slots=%d left=%v; want 1, 0 and none", len(runner.argv()), rig.slots.heldNow(), scratchEntries(t, rig.scratch))
			}
		})
	}
}

// TestAnFFmpegFailureThatIsNotARefusalDoesNotReResolve: a decode error, a 404 and a
// 429 (on open, or on a segment ffmpeg skipped and exited 0) are the source failing,
// not a URL expiring.
func TestAnFFmpegFailureThatIsNotARefusalDoesNotReResolve(t *testing.T) {
	for name, runner := range map[string]*runRecorder{
		"decode error": {failFirst: errors.New("transcode: ffmpeg exited: exit status 1\nInvalid data found when processing input")},
		"404 on open":  {failFirst: ffmpegDown("404 Not Found")},
		"429 on open":  {failFirst: ffmpegDown("429 Too Many Requests")},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newFFmpegSeq(t, runner, func(int) ([]pluginapi.OnlineVariant, error) {
				return []pluginapi.OnlineVariant{manifestAt("/m.m3u8")}, nil
			})
			sess := playSeq(t, rig.svc)
			rig.waitEnded(t, sess.ID)
			if rig.p.resolves() != 1 || len(runner.argv()) != 1 {
				t.Fatalf("resolves=%d runs=%d; want 1 and 1", rig.p.resolves(), len(runner.argv()))
			}
			if _, ok := rig.svc.Gone(sess.ID, "u1"); ok {
				t.Fatal("a plain ffmpeg failure left the no-longer-available note")
			}
		})
	}
	for name, stderr := range map[string]string{
		"404 on a skipped segment": "[http @ 0x1] HTTP error 404 Not Found\nFailed to open segment 4 of playlist 0\n",
		"429 on a skipped segment": "[http @ 0x1] HTTP error 429 Too Many Requests\n",
	} {
		t.Run(name, func(t *testing.T) {
			runner := &runRecorder{skipFirst: stderr}
			rig := newFFmpegSeq(t, runner, func(int) ([]pluginapi.OnlineVariant, error) {
				return []pluginapi.OnlineVariant{manifestAt("/m.m3u8")}, nil
			})
			sess := playSeq(t, rig.svc)
			time.Sleep(100 * time.Millisecond)
			if rig.p.resolves() != 1 || len(runner.argv()) != 1 {
				t.Fatalf("resolves=%d runs=%d; want 1 and 1", rig.p.resolves(), len(runner.argv()))
			}
			if _, live := rig.svc.Session(sess.ID); !live {
				t.Fatal("the session ended")
			}
		})
	}
}

// TestACleanFFmpegExitMarksThePlaylistComplete: ffmpeg is told not to write ENDLIST,
// so the host adds it when an encode ends with nothing refused, and not before.
func TestACleanFFmpegExitMarksThePlaylistComplete(t *testing.T) {
	runner := &runRecorder{skipFirst: "all done\n"}
	rig := newFFmpegSeq(t, runner, func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{manifestAt("/m.m3u8")}, nil
	})
	sess := playSeq(t, rig.svc)
	path := filepath.Join(rig.scratch, sess.ID, "index.m3u8")
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if strings.HasSuffix(string(b), "#EXT-X-ENDLIST\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("playlist never ended: %q", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, live := rig.svc.Session(sess.ID); !live {
		t.Fatal("a finished encode ended the session")
	}
	if rig.p.resolves() != 1 {
		t.Fatalf("resolves=%d, want 1", rig.p.resolves())
	}
}

// TestTwoRequestsRefusedTogetherCostOneReResolveEvenWhenTheURLIsUnchanged: the
// re-resolve may name the very same address (a refreshed token inside the Plugin,
// say). Both refused requests then succeed, and the session lives.
func TestTwoRequestsRefusedTogetherCostOneReResolveEvenWhenTheURLIsUnchanged(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	bothHere := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		if n == 2 {
			close(bothHere)
		}
		mu.Unlock()
		if n <= 2 {
			// Hold both refusals until both requests are in, so neither can recover first.
			select {
			case <-bothHere:
			case <-time.After(3 * time.Second):
			}
			w.WriteHeader(http.StatusForbidden)
			return
		}
		http.ServeContent(w, r, "v.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
	}))
	t.Cleanup(srv.Close)
	svc, p := newSeqService(t, srv.Client(), func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{{URL: srv.URL + "/v.mp4", Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "720p"}}, nil
	})
	svc.SetMediaHostPolicy(func(_, host string) bool { return host == "127.0.0.1" })
	svc.ExemptMediaAddrs(srv.Listener.Addr().String())
	sess := playSeq(t, svc)

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, err := svc.OpenMedia(context.Background(), sess, "", "")
			if err == nil {
				resp.Body.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent request = %v, want the recovered bytes", err)
		}
	}
	if p.resolves() != 2 {
		t.Fatalf("resolve() ran %d times, want 2", p.resolves())
	}
	if _, live := svc.Session(sess.ID); !live {
		t.Fatal("the session ended")
	}
}

// scrolledJob is an exited ffmpeg whose refusal line has long scrolled out of the
// stderr tail, but which remembers having seen it.
type scrolledJob struct{ refused bool }

func (scrolledJob) Wait() error     { return nil }
func (scrolledJob) Kill() error     { return nil }
func (scrolledJob) Stderr() string  { return "frame= 900 later output only" }
func (j scrolledJob) Refused() bool { return j.refused }

func TestARefusalThatScrolledOutOfTheStderrTailIsStillARefusal(t *testing.T) {
	if !mediaRefused(nil, scrolledJob{refused: true}) {
		t.Fatal("a remembered refusal was lost")
	}
	if mediaRefused(nil, scrolledJob{}) {
		t.Fatal("a clean exit counted as a refusal")
	}
}

// TestRepeatedAbortsCannotBuyMoreThanOneExtraReResolve: a client that gives up
// mid-way gets the session its re-resolve back once; a client that aborts every time
// cannot turn that into a resolve() per request.
func TestRepeatedAbortsCannotBuyMoreThanOneExtraReResolve(t *testing.T) {
	m := newMediaHost(t, http.StatusGone)
	svc, p := m.service(t, func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
	})
	p.hangAfterFirst = true
	sess := playSeq(t, svc)
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, err := svc.OpenMedia(ctx, sess, "", "")
		cancel()
		if err == nil {
			t.Fatal("OpenMedia succeeded")
		}
	}
	// The play, one aborted re-resolve handed back, one aborted that was not.
	if got := p.resolves(); got != 3 {
		t.Fatalf("resolve() ran %d times over 8 aborted requests, want 3", got)
	}
}

// TestASessionEndedDuringTheReResolveLeavesNoNote: the User stopped it while resolve()
// ran, so "no longer available" would be a lie.
func TestASessionEndedDuringTheReResolveLeavesNoNote(t *testing.T) {
	m := newMediaHost(t, http.StatusGone)
	started := make(chan struct{})
	release := make(chan struct{})
	var svc *Service
	svc, _ = m.service(t, func(n int) ([]pluginapi.OnlineVariant, error) {
		if n == 1 {
			return []pluginapi.OnlineVariant{m.variant("/old.mp4")}, nil
		}
		close(started)
		<-release
		return nil, errors.New("cancelled")
	})
	sess := playSeq(t, svc)
	done := make(chan error, 1)
	go func() {
		_, err := svc.OpenMedia(context.Background(), sess, "", "")
		done <- err
	}()
	<-started
	svc.End(sess.ID)
	close(release)
	<-done
	if _, ok := svc.Gone(sess.ID, "u1"); ok || svc.HasGone(sess.ID) {
		t.Fatal("a session the User ended left the no-longer-available note")
	}
}
