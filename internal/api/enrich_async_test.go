package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// ADR-0051's amendment, in tests: **a pass is STARTED, never awaited.**
//
// This file exists because of a real failure, not a review comment. The operator
// pressed "Re-check unmatched items", saw nothing for three minutes, reloaded the
// page, and got a flood of `context canceled`. The pass had been running inside
// their HTTP request — `EnrichLibraryProgress(r.Context(), …)` — so the reload
// aborted it. All 724 flagged rows still carried a blank `enrichment_reason`
// afterwards, which is the proof that not one leaf had been processed: a reason is
// written whenever a leaf settles.
//
// The central test here is TestCancellingTheEnrichRequestDoesNotCancelThePass. A
// future refactor that "simplifies" the handler back to running the pass inline
// will pass every other test in the suite and fail that one.

// enrichGate lets a test hold a pass open inside the provider: the pass signals
// when it has reached the provider (so the test knows it is genuinely running),
// and blocks there until the test releases it.
//
// Holding the pass in the PROVIDER rather than sleeping is what makes these tests
// deterministic — there is a definite instant at which "the pass is in flight and
// has not finished" is true, and the test does its work there.
type enrichGate struct {
	entered     chan struct{}
	release     chan struct{}
	enterOnce   sync.Once
	releaseOnce sync.Once
}

func newEnrichGate() *enrichGate {
	return &enrichGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// hold is the provider body: announce arrival once, then wait to be let go.
func (g *enrichGate) hold() {
	g.enterOnce.Do(func() { close(g.entered) })
	<-g.release
}

// awaitEntry blocks until a pass has reached the provider.
func (g *enrichGate) awaitEntry(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(30 * time.Second):
		t.Fatalf("no enrichment pass reached the provider — nothing was started")
	}
}

func (g *enrichGate) let() { g.releaseOnce.Do(func() { close(g.release) }) }

// gatedEnrichHarness is a scanned Movie Library (three Titles) whose provider
// matches everything but blocks until the gate is released. Releasing is
// registered as a cleanup as well, so a failing assertion can never leave the
// worker parked in the provider — App.Close waits on that goroutine.
func gatedEnrichHarness(t *testing.T) (*testharness.Server, *enrichGate, string, string) {
	t.Helper()
	requireFixtures(t)
	g := newEnrichGate()
	t.Cleanup(g.let)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		g.hold()
		return richMeta(), nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("X")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, g, token, libID
}

// startEnrich POSTs the endpoint and returns the status code and the started-ack.
func startEnrich(t *testing.T, srv *testharness.Server, token, libID, query string) (int, enrichPassResp, []byte) {
	t.Helper()
	var ack enrichPassResp
	status, raw := srv.JSON(http.MethodPost,
		"/api/v1/libraries/"+libID+"/enrich"+query, token, nil, &ack)
	return status, ack, raw
}

// TestEnrichPostReturnsWithoutWaitingForThePass: the endpoint answers 202 while
// the pass is demonstrably still going.
//
// The provider is blocked, so the pass CANNOT have finished; if the handler still
// ran it inline this request would not have come back at all. The assertion is on
// both halves — the reply arrived, and the pass is still in flight — because
// either one alone is satisfiable by a pass that did nothing.
func TestEnrichPostReturnsWithoutWaitingForThePass(t *testing.T) {
	srv, gate, token, libID := gatedEnrichHarness(t)

	status, ack, raw := startEnrich(t, srv, token, libID, "?mode=recheck")
	if status != http.StatusAccepted {
		t.Fatalf("POST /enrich = %d, want 202 Accepted; body: %s", status, raw)
	}
	if !ack.Started || ack.State != "running" {
		t.Fatalf("started-ack = %+v, want started=true state=running; body: %s", ack, raw)
	}
	if ack.Mode != "recheck" {
		t.Errorf("ack names mode %q, want %q — the reply must say which pass it started", ack.Mode, "recheck")
	}

	gate.awaitEntry(t)
	if !srv.EnrichPassRunning(libID) {
		t.Fatalf("the pass was already over when the POST returned — the handler is awaiting it")
	}

	// And it still runs to completion afterwards: a request that returns early is
	// only an improvement if the work actually happens.
	gate.let()
	srv.AwaitEnrichPass(libID)
	if n := len(listEnrichmentAttention(t, srv, token, libID).Titles); n != 0 {
		t.Errorf("%d Titles still unsettled after the pass finished", n)
	}
}

// TestCancellingTheEnrichRequestDoesNotCancelThePass is THE regression.
//
// This is the operator's reload, reproduced exactly: start the pass, let it reach
// the provider, then abandon the request the way a browser does when the page goes
// away. The pass must finish anyway and every leaf must settle.
//
// It bites because the enrich Service checks ctx.Err() before EVERY leaf
// (service.go's pass loop). Wire the pass back to r.Context() and this library's
// first leaf is processed, the cancellation lands, and the remaining two are left
// exactly as the operator found their 724 rows: untouched, with nothing written.
func TestCancellingTheEnrichRequestDoesNotCancelThePass(t *testing.T) {
	srv, gate, token, libID := gatedEnrichHarness(t)

	// Issue the POST with a context WE control, so we can pull it out from under
	// the server mid-pass. (srv.JSON cannot do this: its request is not cancellable,
	// which is precisely the condition this test needs to create.)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL("/api/v1/libraries/"+libID+"/enrich?mode=recheck"), nil)
	if err != nil {
		t.Fatalf("building the enrich request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /enrich: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		resp.Body.Close()
		cancel()
		t.Fatalf("POST /enrich = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// The pass is inside the provider. Now the operator reloads: the in-flight
	// request is abandoned and its context cancelled.
	gate.awaitEntry(t)
	cancel()
	// Give the cancellation every chance to reach a handler that was still holding
	// the request — if the pass were tied to it, this is when it would die.
	time.Sleep(50 * time.Millisecond)

	gate.let()
	srv.AwaitEnrichPass(libID)

	// The proof the operator did not have: every leaf settled.
	if n := len(listEnrichmentAttention(t, srv, token, libID).Titles); n != 0 {
		t.Fatalf("%d Titles are still unsettled — cancelling the request killed the pass, "+
			"which is the exact bug ADR-0051's amendment was written about", n)
	}
	for _, name := range []string{"Dune", "Blade Runner", "Sample Movie"} {
		id := titleIDByName(t, srv, token, libID, name)
		if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "matched" {
			t.Errorf("%s enrichmentStatus = %q, want matched — the pass did not reach it", name, s)
		}
	}
	// And the status route agrees a whole pass finished, not a truncated one.
	st := enrichStatus(t, srv, token, libID)
	if st.State != "idle" || st.LastPass == nil {
		t.Fatalf("status after the pass = %+v, want idle with a finished summary", st)
	}
	if st.LastPass.Total != 3 || st.LastPass.Matched != 3 {
		t.Errorf("finished summary %+v, want total=matched=3 — a cancelled pass would report fewer",
			*st.LastPass)
	}
}

// A second press while a pass is running reports the pass that IS running rather
// than queueing a duplicate behind the same per-Library lock.
func TestSecondEnrichWhileOneIsRunningReportsAlreadyRunning(t *testing.T) {
	srv, gate, token, libID := gatedEnrichHarness(t)

	if status, ack, raw := startEnrich(t, srv, token, libID, "?mode=recheck"); status != http.StatusAccepted || !ack.Started {
		t.Fatalf("first POST = %d %+v, want 202 started=true; body: %s", status, ack, raw)
	}
	gate.awaitEntry(t)

	status, ack, raw := startEnrich(t, srv, token, libID, "?mode=recheck")
	if status != http.StatusAccepted {
		t.Fatalf("second POST = %d, want 202; body: %s", status, raw)
	}
	if ack.Started {
		t.Errorf("the second POST claims it started a pass; it must report the running one instead")
	}
	if ack.State != "running" || ack.Mode != "recheck" {
		t.Errorf("second POST = %+v, want the in-flight pass's state and mode", ack)
	}

	// One pass, not two: releasing the gate settles the Library once.
	gate.let()
	srv.AwaitEnrichPass(libID)
	if srv.EnrichPassRunning(libID) {
		t.Errorf("a second pass was queued behind the first after all")
	}
}

// GET reports idle before a pass, running (with its mode) during one, and the
// finished summary after.
func TestEnrichStatusReportsIdleRunningAndFinished(t *testing.T) {
	srv, gate, token, libID := gatedEnrichHarness(t)

	before := enrichStatus(t, srv, token, libID)
	if before.State != "idle" || before.LastPass != nil {
		t.Fatalf("status before any pass = %+v, want idle with no lastPass", before)
	}

	startEnrich(t, srv, token, libID, "?mode=recheck")
	gate.awaitEntry(t)

	during := enrichStatus(t, srv, token, libID)
	if during.State != "running" || during.Mode != "recheck" {
		t.Fatalf("status during a pass = %+v, want running in mode recheck", during)
	}
	if during.Progress == nil || during.Progress.Total != 3 {
		t.Errorf("status during a pass carries progress %+v, want a total of 3 — a page that "+
			"joins mid-pass has to be able to see how big the job is", during.Progress)
	}
	if during.StartedAt == "" {
		t.Errorf("a running pass reports no startedAt")
	}

	gate.let()
	srv.AwaitEnrichPass(libID)

	after := enrichStatus(t, srv, token, libID)
	if after.State != "idle" {
		t.Fatalf("status after the pass = %q, want idle", after.State)
	}
	if after.LastPass == nil {
		t.Fatalf("no finished summary after the pass")
	}
	if after.LastPass.Total != 3 || after.LastPass.Matched != 3 || after.LastPass.Mode != "recheck" {
		t.Errorf("finished summary %+v, want 3/3 matched in mode recheck", *after.LastPass)
	}
	if after.LastPass.FinishedAt == "" {
		t.Errorf("the finished summary does not say when it finished")
	}
}

// Unknown Library → 404 on both verbs (hide-existence), and validated BEFORE
// anything is queued so a typo never starts a pass somewhere.
func TestEnrichUnknownLibraryIsNotFound(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)

	if status, _ := srv.AuthGET("/api/v1/libraries/nope/enrich", token, nil); status != http.StatusNotFound {
		t.Errorf("GET /libraries/nope/enrich = %d, want 404", status)
	}
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/libraries/nope/enrich", token, nil, nil); status != http.StatusNotFound {
		t.Errorf("POST /libraries/nope/enrich = %d, want 404", status)
	}
}

// The status route is Admin-only, like the pass it reports on.
func TestEnrichStatusRequiresAdmin(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	if status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/enrich", mTok, nil); status != http.StatusForbidden {
		t.Errorf("member GET enrich status = %d, want 403", status)
	}
}

// With NO background worker there is nothing to start a pass on, and the endpoint
// says so. This used to be silence: `enqueue` returned early when no worker was
// running, so the button would have done nothing, forever, with no message.
func TestEnrichWithNoWorkerRunningReportsIt(t *testing.T) {
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithoutEnrichWorker(),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	var envelope errorEnvelope
	status, raw := srv.JSON(http.MethodPost,
		"/api/v1/libraries/"+libID+"/enrich?mode=recheck", token, nil, &envelope)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("POST /enrich with no worker = %d, want 503; body: %s", status, raw)
	}
	if envelope.Error.Code != "ENRICH_UNAVAILABLE" {
		t.Errorf("error code = %q, want ENRICH_UNAVAILABLE; body: %s", envelope.Error.Code, raw)
	}
	if envelope.Error.Message == "" {
		t.Errorf("a refusal with no message is the silence this exists to end; body: %s", raw)
	}
	if srv.EnrichPassRunning(libID) {
		t.Errorf("a pass is somehow running on a server with no worker")
	}
}

// A FULL queue is refused out loud too. It used to log a line the operator never
// saw and drop the request on the floor.
func TestEnrichWithAFullQueueReportsIt(t *testing.T) {
	requireFixtures(t)
	gate := newEnrichGate()
	t.Cleanup(gate.let)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		gate.hold()
		return richMeta(), nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("X")}),
		// One waiting slot, so three requests reach the overflow instead of sixty-six.
		testharness.WithEnrichQueueCapacity(1),
	)
	token := adminToken(t, srv)
	busy := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, busy, "")
	queued := createLibraryNamed(t, srv, token, "Queued", t.TempDir())
	refused := createLibraryNamed(t, srv, token, "Refused", t.TempDir())

	// Occupy the worker, then fill the single queue slot.
	startEnrich(t, srv, token, busy, "")
	gate.awaitEntry(t)
	if status, _, raw := startEnrich(t, srv, token, queued, ""); status != http.StatusAccepted {
		t.Fatalf("filling the queue = %d, want 202; body: %s", status, raw)
	}

	var envelope errorEnvelope
	status, raw := srv.JSON(http.MethodPost,
		"/api/v1/libraries/"+refused+"/enrich", token, nil, &envelope)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("POST /enrich with a full queue = %d, want 503; body: %s", status, raw)
	}
	if envelope.Error.Code != "ENRICH_BUSY" {
		t.Errorf("error code = %q, want ENRICH_BUSY; body: %s", envelope.Error.Code, raw)
	}
	// A refused start left no phantom pass behind: the Library reads idle.
	if srv.EnrichPassRunning(refused) {
		t.Errorf("the refused Library is reported as running a pass")
	}

	gate.let()
	srv.AwaitEnrichPass(busy)
	srv.AwaitEnrichPass(queued)
}

// createLibraryNamed makes an empty Movie Library with a distinct name, for the
// queue-capacity test (which needs several Libraries and cares about none of
// their contents).
func createLibraryNamed(t *testing.T, srv *testharness.Server, token, name, root string) string {
	t.Helper()
	_, lib, raw := createLibrary(t, srv, token, map[string]any{
		"name":        name,
		"kind":        "movie",
		"rootFolders": []string{root},
	})
	if lib.ID == "" {
		t.Fatalf("library %q not created; body: %s", name, raw)
	}
	return lib.ID
}
