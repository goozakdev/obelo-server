package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Marker provider Extension point (ADR-0065): an
// Installed Marker provider placed under <dataDir>/plugins/<id>/, a Movie
// Library holding a copy of the Dune fixture, and GET /sessions/{id}/markers —
// the read a player makes when playback starts, and so when the providers are
// first asked.
//
// The guest is the suite's own module, which POSTs each question to its source
// and answers what the source says. The source is an httptest server, so a test
// decides each candidate and the length it claims to be timed for.

// markerQuestion is what a source is asked, as it arrives on the wire.
type markerQuestion struct {
	Kind       string            `json:"kind"`
	Title      string            `json:"title"`
	Year       int               `json:"year"`
	IDs        map[string]string `json:"ids"`
	DurationMs int64             `json:"durationMs"`
}

// markerSource stands in for a remote marker database. answer builds the
// candidates for each question.
type markerSource struct {
	srv    *httptest.Server
	mu     sync.Mutex
	asked  []markerQuestion
	answer func(q markerQuestion) []map[string]any
}

func newMarkerSource(t *testing.T, answer func(q markerQuestion) []map[string]any) *markerSource {
	t.Helper()
	s := &markerSource{answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q markerQuestion
		_ = json.NewDecoder(r.Body).Decode(&q)
		s.mu.Lock()
		s.asked = append(s.asked, q)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"markers": s.answer(q)})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *markerSource) questions() []markerQuestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]markerQuestion(nil), s.asked...)
}

// span is a candidate covering [from, to) of the question's File, as fractions,
// claiming to be timed for a recording offMs longer than it.
func span(q markerQuestion, kind string, from, to float64, offMs int64) map[string]any {
	d := float64(q.DurationMs)
	return map[string]any{
		"kind": kind, "startMs": int64(d * from), "endMs": int64(d * to), "durationMs": q.DurationMs + offMs,
	}
}

// markerProviderServer installs src as a Marker provider, copies the Dune
// fixture into a Movie Library — with edl as its `.edl` when edl is not nil,
// written from the File's duration — scans it, and returns the server, an Admin
// token, the Dune Title id and the File's duration.
func markerProviderServer(t *testing.T, src *markerSource, edl func(secs float64) string) (*testharness.Server, string, string, int64) {
	t.Helper()
	requireFixtures(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MarkerManifest("example-markers", src.srv.URL))
	// The source is the plugin's manifest default on 127.0.0.1, which the fetch
	// policy refuses unless the address is named as exempt.
	srv := testharness.New(t, testharness.WithDataDir(dataDir), testharness.WithPluginFetchesExemptAt(src.srv.Listener.Addr().String()))
	token := adminToken(t, srv)

	root := t.TempDir()
	dir := filepath.Join(root, "Dune (2021) {imdb-tt1160419}")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clip, err := os.ReadFile(filepath.Join(fixtureRoot(t), "Dune (2021)", "Dune (2021).mp4"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dune (2021).mp4"), clip, 0o644); err != nil {
		t.Fatal(err)
	}
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list); status != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", status, body)
	}
	duneID := findTitle(t, list, "Dune")
	dur := titleDuration(t, srv, token, duneID)
	if edl != nil {
		if err := os.WriteFile(filepath.Join(dir, "Dune (2021).edl"), []byte(edl(float64(dur)/1000)), 0o644); err != nil {
			t.Fatal(err)
		}
		scanLib(t, srv, token, libID, "")
	}
	return srv, token, duneID, dur
}

// servedKinds is each served Marker as "kind/source".
func servedKinds(got markersResp) []string {
	out := []string{}
	for _, m := range got.Markers {
		out = append(out, m.Kind+"/"+m.Source)
	}
	return out
}

// TestAFetchedMarkerTimedForAnotherLengthIsNeverServed is the host's judgment:
// the provider's Intro was timed on a recording ten seconds longer than this
// File, so it is not served — not on the first read, and not on a later one,
// which does not ask again.
func TestAFetchedMarkerTimedForAnotherLengthIsNeverServed(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "intro", 0.1, 0.3, 10_000)}
	})
	srv, token, duneID, _ := markerProviderServer(t, src, nil)
	dec := negotiateDune(t, srv, token, duneID)

	for i := 0; i < 2; i++ {
		if got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK); len(got.Markers) != 0 {
			t.Fatalf("read %d: markers = %v, want none: the only candidate was timed for another length", i+1, servedKinds(got))
		}
	}
	if n := len(src.questions()); n != 1 {
		t.Fatalf("the source was asked %d times, want once: the miss is remembered", n)
	}
}

// TestAFetchedMarkerWithinTheToleranceIsServed: with no Local or Detected
// Marker, a Fetched Intro timed on a recording two seconds longer is served —
// and the provider was asked about this film, at this File's length.
func TestAFetchedMarkerWithinTheToleranceIsServed(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "intro", 0.1, 0.3, 2_000)}
	})
	srv, token, duneID, dur := markerProviderServer(t, src, nil)
	dec := negotiateDune(t, srv, token, duneID)

	got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK)
	if len(got.Markers) != 1 || got.Markers[0].Kind != "intro" || got.Markers[0].Source != "fetched" ||
		got.Markers[0].StartMs != int64(float64(dur)*0.1) {
		t.Fatalf("markers = %+v, want the fetched intro", got.Markers)
	}
	q := src.questions()[0]
	if q.Kind != "movie" || q.Title != "Dune" || q.Year != 2021 || q.IDs["imdb"] != "tt1160419" || q.DurationMs != dur {
		t.Fatalf("the source was asked %+v, want Dune (2021), its imdb id and a duration of %d", q, dur)
	}
}

// TestALocalIntroIsServedOverAFetchedIntro: the File's own `.edl` names an
// Intro, so the provider's Intro is not served; its Preview, which nothing else
// covers, is.
func TestALocalIntroIsServedOverAFetchedIntro(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "intro", 0.4, 0.5, 0), span(q, "preview", 0.8, 0.9, 0)}
	})
	srv, token, duneID, _ := markerProviderServer(t, src, func(secs float64) string {
		return fmt.Sprintf("0 %.3f 0 Intro\n", secs*0.2)
	})
	dec := negotiateDune(t, srv, token, duneID)

	got := servedKinds(getMarkers(t, srv, token, dec.SessionID, http.StatusOK))
	if len(got) != 2 || got[0] != "intro/local" || got[1] != "preview/fetched" {
		t.Fatalf("markers = %v, want [intro/local preview/fetched]", got)
	}
}

// TestADetectedRecapIsServedOverAFetchedRecap: Marker detection's Recap is
// measured on this very File, so the provider's Recap — even apart from it — is
// not served.
func TestADetectedRecapIsServedOverAFetchedRecap(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "recap", 0.5, 0.6, 0)}
	})
	srv, token, duneID, dur := markerProviderServer(t, src, nil)
	srv.Exec(`INSERT INTO markers (id, file_path, kind, source, start_ms, end_ms)
		SELECT 'detected-recap', path, 'recap', 'detected', 0, ? FROM files WHERE path LIKE '%Dune (2021).mp4'`, dur/10)
	dec := negotiateDune(t, srv, token, duneID)

	got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK)
	if kinds := servedKinds(got); len(kinds) != 1 || kinds[0] != "recap/detected" || got.Markers[0].EndMs != dur/10 {
		t.Fatalf("markers = %+v, want only the detected recap", got.Markers)
	}
	if n := len(src.questions()); n != 1 {
		t.Fatalf("the source was asked %d times, want once", n)
	}
}

// readMarkers is one GET /sessions/{id}/markers made under ctx, without failing
// the test from the goroutine it may run on: the served "kind/source" list, or
// the error.
func readMarkers(ctx context.Context, srv *testharness.Server, token, sessionID string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL("/api/v1/sessions/"+sessionID+"/markers"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("markers status = %d", resp.StatusCode)
	}
	var got markersResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return nil, err
	}
	return servedKinds(got), nil
}

// awaitMarkers reads the session's Markers until they are want, failing the test
// if they are not within ten seconds.
func awaitMarkers(t *testing.T, srv *testharness.Server, token, sessionID string, want string) {
	t.Helper()
	var got []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		got, _ = readMarkers(context.Background(), srv, token, sessionID)
		if fmt.Sprint(got) == want {
			return
		}
	}
	t.Fatalf("markers = %v, want %s", got, want)
}

// TestLocalMarkersNeverWaitOnAHungProvider: the provider's source never answers,
// yet several first reads at once each answer the File's own Intro within the
// read's wait for its providers (three seconds) and a little slack — none waits
// out a provider call, let alone every call queued ahead of it. The asking goes
// on without them, and a later read serves what it found.
func TestLocalMarkersNeverWaitOnAHungProvider(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		<-release
		return []map[string]any{span(q, "preview", 0.8, 0.9, 0)}
	})
	srv, token, duneID, _ := markerProviderServer(t, src, func(secs float64) string {
		return fmt.Sprintf("0 %.3f 0 Intro\n", secs*0.2)
	})
	t.Cleanup(unblock)
	dec := negotiateDune(t, srv, token, duneID)

	const reads = 4
	const within = 5 * time.Second
	got := make([][]string, reads)
	errs := make([]error, reads)
	elapsed := make([]time.Duration, reads)
	var wg sync.WaitGroup
	for i := 0; i < reads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			got[i], errs[i] = readMarkers(context.Background(), srv, token, dec.SessionID)
			elapsed[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	for i := 0; i < reads; i++ {
		if errs[i] != nil {
			t.Fatalf("read %d: %v", i+1, errs[i])
		}
		if elapsed[i] > within || fmt.Sprint(got[i]) != "[intro/local]" {
			t.Errorf("read %d took %v and served %v, want [intro/local] within %v", i+1, elapsed[i], got[i], within)
		}
	}
	unblock()
	awaitMarkers(t, srv, token, dec.SessionID, "[intro/local preview/fetched]")
}

// TestAbortedReadsDoNotDisableTheProvider: a viewer who gives up on the read —
// a player closed, a seek, a dropped connection — ends nothing but the read. The
// provider's call runs to its answer, so it is neither a failure counted against
// the Plugin nor asked again; aborts past the failure threshold leave the Plugin
// enabled and its Intro served.
func TestAbortedReadsDoNotDisableTheProvider(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		time.Sleep(time.Second)
		return []map[string]any{span(q, "intro", 0.1, 0.3, 0)}
	})
	srv, token, duneID, _ := markerProviderServer(t, src, nil)
	dec := negotiateDune(t, srv, token, duneID)

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, _ = readMarkers(ctx, srv, token, dec.SessionID)
		cancel()
		time.Sleep(300 * time.Millisecond)
	}
	awaitMarkers(t, srv, token, dec.SessionID, "[intro/fetched]")
	if p := pluginNamed(t, readPlugins(t, srv, token), "example-markers"); p.DisabledByFailure || !p.Enabled {
		t.Fatalf("plugin = %+v, want it enabled: a viewer aborting a read is not the Plugin failing", p)
	}
	if n := len(src.questions()); n != 1 {
		t.Fatalf("the source was asked %d times, want once: an aborted read does not end the call", n)
	}
}

// TestFetchedMarkersAreServedOnlyWhileAProviderIsEnabled: with the only Marker
// provider disabled, its Fetched Markers are not served and its Credits no
// longer set the Watched ceiling; enabled again, they are served as they were,
// without asking it again — the rows were kept.
func TestFetchedMarkersAreServedOnlyWhileAProviderIsEnabled(t *testing.T) {
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		return []map[string]any{span(q, "intro", 0.1, 0.2, 0), span(q, "credits", 0.6, 0.95, 0)}
	})
	srv, token, duneID, _ := markerProviderServer(t, src, nil)
	dec := negotiateDune(t, srv, token, duneID)
	got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK)
	if kinds := fmt.Sprint(servedKinds(got)); kinds != "[intro/fetched credits/fetched]" {
		t.Fatalf("markers = %s, want the fetched intro and credits", kinds)
	}
	credits := got.Markers[1]

	if status, body := srv.JSON(http.MethodPost, "/api/v1/settings/plugins/example-markers/disable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("disable = %d; body: %s", status, body)
	}
	if got := getMarkers(t, srv, token, dec.SessionID, http.StatusOK); len(got.Markers) != 0 {
		t.Fatalf("markers with the provider disabled = %v, want none", servedKinds(got))
	}
	if out := postProgress(t, srv, token, dec.SessionID, credits.StartMs, http.StatusOK); out.Watched {
		t.Fatalf("at the Fetched Credits start with the provider disabled: %+v, want unwatched", out)
	}

	if status, body := srv.JSON(http.MethodPost, "/api/v1/settings/plugins/example-markers/enable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("enable = %d; body: %s", status, body)
	}
	if kinds := fmt.Sprint(servedKinds(getMarkers(t, srv, token, dec.SessionID, http.StatusOK))); kinds != "[intro/fetched credits/fetched]" {
		t.Fatalf("markers with the provider enabled again = %s, want both back", kinds)
	}
	if out := postProgress(t, srv, token, dec.SessionID, credits.StartMs, http.StatusOK); !out.Watched {
		t.Errorf("at the Fetched Credits start with the provider enabled: %+v, want watched", out)
	}
	if n := len(src.questions()); n != 1 {
		t.Errorf("the source was asked %d times, want once: the kept rows are served again", n)
	}
}

// TestAppCloseEndsAMarkerFetchInFlight: a provider that never answers leaves
// the asking running when the Server shuts down. Close ends it: once Close
// returns no asking is running, and nothing is logged about it afterwards (it
// used to fail into the closed database, "sql: database is closed").
func TestAppCloseEndsAMarkerFetchInFlight(t *testing.T) {
	release := make(chan struct{})
	src := newMarkerSource(t, func(q markerQuestion) []map[string]any {
		<-release
		return nil
	})
	t.Cleanup(func() { close(release) })
	srv, token, duneID, _ := markerProviderServer(t, src, nil)
	dec := negotiateDune(t, srv, token, duneID)
	getMarkers(t, srv, token, dec.SessionID, http.StatusOK) // answers after its wait; the asking goes on
	if len(src.questions()) != 1 {
		t.Fatalf("the source was asked %d times, want once before Close", len(src.questions()))
	}

	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	srv.Close()
	if n := goroutinesIn("internal/markerfetch."); n != 0 {
		t.Errorf("%d marker fetch goroutines outlived Close", n)
	}
	time.Sleep(500 * time.Millisecond)
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "marker") {
			t.Errorf("logged after Close: %s", line)
		}
	}
}

// goroutinesIn counts the goroutines with a frame in pkg.
func goroutinesIn(pkg string) int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, pkg) {
			n++
		}
	}
	return n
}
