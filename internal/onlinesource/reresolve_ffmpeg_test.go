package onlinesource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The ffmpeg path against the REAL ffmpeg (skipped when there is none): a media host
// that starts answering 403 mid-stream, and a re-resolve that returns a good URL.
// ffmpeg skips the segments it is refused and exits 0, saying so only on stderr;
// the session must still restart where the playlist ends, number the new segments
// on from it, and never tell the player the stream is over before it is.

// httpRunner is the production FFmpeg runner with the two things a loopback test
// cannot satisfy rewritten: the media host is plain http here (a self-signed
// certificate would fail -tls_verify), so https inputs become http and the protocol
// whitelist gains it. Everything else in the argument vector is the Server's own.
type httpRunner struct {
	mu   sync.Mutex
	runs [][]string
	logs []string
}

type loggedJob struct {
	transcode.StderrJob
	r *httpRunner
}

func (j loggedJob) Wait() error {
	err := j.StderrJob.Wait()
	j.r.mu.Lock()
	j.r.logs = append(j.r.logs, j.StderrJob.Stderr())
	j.r.mu.Unlock()
	return err
}

func (h *httpRunner) Start(ctx context.Context, args []string) (transcode.Job, error) {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-tls_verify":
			i++
		case args[i] == "-protocol_whitelist":
			out = append(out, args[i], "http,tcp,crypto")
			i++
		case args[i] == "-i":
			out = append(out, "-i", strings.Replace(args[i+1], "https://", "http://", 1))
			i++
		default:
			out = append(out, args[i])
		}
	}
	h.mu.Lock()
	h.runs = append(h.runs, out)
	h.mu.Unlock()
	j, err := transcode.FFmpeg{}.Start(ctx, out)
	if err != nil {
		return nil, err
	}
	return loggedJob{StderrJob: j.(transcode.StderrJob), r: h}, nil
}

func (h *httpRunner) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.runs) }

func TestRealFFmpegRestartsAfterAMidStreamRefusalAndKeepsAContinuousPlaylist(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// A 28 s source in seven 4 s segments, keyframes aligned to them.
	src := t.TempDir()
	gen := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440", "-t", "28", "-c:v", "libx264", "-g", "100", "-keyint_min", "100",
		"-sc_threshold", "0", "-c:a", "aac", "-f", "hls", "-hls_time", "4", "-hls_list_size", "0",
		"-hls_playlist_type", "vod", "-hls_segment_filename", filepath.Join(src, "src%d.ts"), filepath.Join(src, "src.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate a test stream (no libx264/aac?): %v\n%s", err, out)
	}

	// phase 1: segments from the fourth on are refused; phase 2 (after the re-resolve): all good.
	var phase atomic.Int32
	phase.Store(1)
	segment := regexp.MustCompile(`src(\d+)\.ts$`)
	files := http.FileServer(http.Dir(src))
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := segment.FindStringSubmatch(r.URL.Path); m != nil {
			if n, _ := strconv.Atoi(m[1]); n >= 3 && phase.Load() == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(host.Close)

	svc, p := newSeqService(t, nil, func(n int) ([]pluginapi.OnlineVariant, error) {
		if n >= 2 {
			phase.Store(2)
		}
		return []pluginapi.OnlineVariant{{
			Kind: pluginapi.OnlineVariantManifest, Container: "hls", Resolution: "720p",
			URL: "https://" + host.Listener.Addr().String() + "/src.m3u8?n=" + strconv.Itoa(n),
		}}, nil
	})
	svc.SetMediaHostPolicy(func(_, h string) bool { return h == "127.0.0.1" })
	svc.ExemptMediaAddrs(host.Listener.Addr().String())
	runner := &httpRunner{}
	scratch := t.TempDir()
	sl := &slots{free: 1}
	svc.SetTranscoder(Transcoder{Runner: runner, ScratchRoot: scratch, Reserve: sl.reserve})

	sess := playSeq(t, svc)
	t.Cleanup(func() { svc.End(sess.ID) })
	playlist := filepath.Join(scratch, sess.ID, "index.m3u8")

	// Watch the playlist the player reads: it must not say the stream is over until the
	// restarted encode has run.
	deadline := time.Now().Add(60 * time.Second)
	var final string
	for {
		b, _ := os.ReadFile(playlist)
		if strings.Contains(string(b), "#EXT-X-ENDLIST") {
			if runner.count() < 2 {
				t.Fatalf("the playlist ended before ffmpeg was restarted:\n%s", b)
			}
			final = string(b)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the playlist never ended; ffmpeg runs: %d; playlist:\n%s", runner.count(), b)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if runner.count() != 2 || p.resolves() != 2 {
		t.Fatalf("ffmpeg ran %d times and resolve() %d; want 2 and 2", runner.count(), p.resolves())
	}
	if _, live := svc.Session(sess.ID); !live {
		t.Fatal("the session ended")
	}
	if strings.Count(final, "#EXT-X-ENDLIST") != 1 || !strings.HasSuffix(strings.TrimSpace(final), "#EXT-X-ENDLIST") {
		t.Fatalf("ENDLIST is not the one last line:\n%s", final)
	}
	if !strings.Contains(final, "#EXT-X-MEDIA-SEQUENCE:0") {
		t.Fatalf("media sequence moved:\n%s", final)
	}

	// Continuous numbering, every listed file present, and the whole 28 s accounted for.
	var names []string
	total := 0.0
	for _, line := range strings.Split(final, "\n") {
		if d, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(d, ","), 64)
			total += v
		}
		if strings.HasSuffix(line, ".ts") {
			names = append(names, line)
		}
	}
	for i, n := range names {
		if want := "segment" + pad3(i) + ".ts"; n != want {
			t.Fatalf("segment %d is %s, want %s (numbering must be continuous):\n%s", i, n, want, final)
		}
		if _, err := os.Stat(filepath.Join(scratch, sess.ID, n)); err != nil {
			t.Fatalf("listed segment %s is missing: %v", n, err)
		}
	}
	// Lines already published must not change: no DISCONTINUITY above the first segment,
	// exactly one at the cut.
	seen, cuts := 0, []int{}
	for _, line := range strings.Split(final, "\n") {
		if line == "#EXT-X-DISCONTINUITY" {
			cuts = append(cuts, seen)
		}
		if strings.HasSuffix(line, ".ts") {
			seen++
		}
	}
	if len(cuts) != 1 || cuts[0] == 0 {
		t.Fatalf("DISCONTINUITY after %v segments; want exactly one, at the cut and not above segment000:\n%s", cuts, final)
	}
	if len(names) < 7 || total < 26 || total > 31 {
		t.Fatalf("playlist lists %d segments, %.1fs; want about the source's 28s in at least 7\n%s", len(names), total, final)
	}
	if runs := func() [][]string { runner.mu.Lock(); defer runner.mu.Unlock(); return runner.runs }(); len(argValues(runs[1], "-ss")) != 1 ||
		len(argValues(runs[1], "-start_number")) != 0 {
		t.Fatalf("restart args: %v", runs[1])
	}

	runner.mu.Lock()
	for i, l := range runner.logs {
		lines := strings.Split(strings.TrimSpace(l), "\n")
		if len(lines) > 12 {
			lines = lines[len(lines)-12:]
		}
		t.Logf("ffmpeg run %d stderr tail:\n%s", i+1, strings.Join(lines, "\n"))
	}
	runner.mu.Unlock()
	t.Logf("final playlist:\n%s", final)
}

func pad3(i int) string {
	s := strconv.Itoa(i)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
