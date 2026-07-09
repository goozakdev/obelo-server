package api_test

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// External-metadata-enrichment issue 02 black-box tests: the THREE triggers
// (auto-after-scan, scheduled sweep, manual) feeding background Enrichment, the
// enrichProgress SSE stream, the disabled/zero-interval no-op posture, and
// composition with soft-delete (Missing → return). All run with FAKE seams (zero
// network); reuses the fakes/helpers from enrich_test.go (same package).

// waitFor polls fn until true or the deadline, failing the test on timeout. Used
// because the auto/scheduled passes run in a background goroutine — we assert on
// observable state rather than sleeping a fixed duration.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestAutoEnrichAfterScan: with auto-enrich on, a scan that adds Titles triggers
// a background Enrichment pass — the Title gains metadata with NO manual
// POST /enrich. The scan response itself returns immediately (it is synchronous;
// enrichment is the async follow-on).
func TestAutoEnrichAfterScan(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		testharness.WithAutoEnrich(true),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "") // NO manual enrich — auto-after-scan does it.

	id := titleIDByName(t, srv, token, libID, "Dune")
	waitFor(t, "auto-enrich to mark Dune matched", func() bool {
		return getEnrichedDetail(t, srv, token, id).EnrichmentStatus == "matched"
	})
	if d := getEnrichedDetail(t, srv, token, id); d.Overview == "" {
		t.Errorf("auto-enriched Title has no overview: %+v", d)
	}
	if prov.calls() == 0 {
		t.Errorf("provider was never called by the auto-after-scan pass")
	}
}

// TestScheduledEnrichSweep: with auto-enrich OFF but a short scheduled interval,
// the safety-net sweep enriches still-'pending' Titles on its cadence.
func TestScheduledEnrichSweep(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		// The scheduled cadence is now DB-backed in whole SECONDS
		// (enrichment-runtime-settings), so a sub-second test interval would seed to 0
		// (disabled). Use 1s — the sweep still fires well within waitFor's 5s deadline.
		testharness.WithEnrichInterval(1*time.Second),
		// auto-enrich stays OFF (harness default): only the sweep should enrich.
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Dune")
	waitFor(t, "scheduled sweep to enrich Dune", func() bool {
		return getEnrichedDetail(t, srv, token, id).EnrichmentStatus == "matched"
	})
}

// TestEnrichProgressSSE: a connected client on GET /events observes
// enrichProgress events while a pass runs, including a terminal complete:true.
// Driven via the synchronous manual pass (deterministic) which publishes the
// same events the background worker does.
func TestEnrichProgressSSE(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Open the SSE stream. Cancel before the test returns so the long-lived
	// connection closes (else the harness's ts.Close would block on it).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL("/api/v1/events"), nil)
	if err != nil {
		t.Fatalf("building events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events GET = %d, want 200", resp.StatusCode)
	}

	lines := make(chan string, 128)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	// Wait for the ": connected" handshake so we are subscribed before triggering.
	waitForLine(t, lines, func(s string) bool { return strings.HasPrefix(s, ": connected") })

	// Trigger a manual pass; it publishes enrichProgress (+ a terminal complete).
	enrichLib(t, srv, token, libID, "")

	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, "event: "+enrichProgressEventName) })
	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, `"complete":true`) })
}

const enrichProgressEventName = "enrichProgress"

// waitForLine consumes SSE lines until one satisfies pred (or it times out).
func waitForLine(t *testing.T, ch <-chan string, pred func(string) bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-ch:
			if pred(line) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for a matching SSE line")
		}
	}
}

// TestEnrichSweepDisabledByConfig: background enrichment makes NO progress when
// it is unconfigured (no key) or explicitly disabled (auto off + 0 interval) —
// Titles stay 'pending' and the provider is never called.
func TestEnrichSweepDisabledByConfig(t *testing.T) {
	requireFixtures(t)

	t.Run("no provider key", func(t *testing.T) {
		srv := testharness.New(t, testharness.WithEnrichInterval(20*time.Millisecond))
		token := adminToken(t, srv)
		libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
		scanLib(t, srv, token, libID, "")
		id := titleIDByName(t, srv, token, libID, "Dune")
		// Give any (wrongly-started) sweep ample time to run.
		time.Sleep(200 * time.Millisecond)
		if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "pending" {
			t.Errorf("status = %q, want pending (no provider configured → no background work)", s)
		}
	})

	t.Run("auto off + zero interval", func(t *testing.T) {
		prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
		srv := testharness.New(t,
			testharness.WithEnrichmentKey("test-key"),
			testharness.WithMetadataProvider(prov),
			testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
			testharness.WithEnrichInterval(0),
		)
		token := adminToken(t, srv)
		libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
		scanLib(t, srv, token, libID, "")
		id := titleIDByName(t, srv, token, libID, "Dune")
		time.Sleep(200 * time.Millisecond)
		if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "pending" {
			t.Errorf("status = %q, want pending (auto off + 0 interval → no background work)", s)
		}
		if prov.calls() != 0 {
			t.Errorf("provider called %d times, want 0 (no trigger enabled)", prov.calls())
		}
	})
}

// TestEnrichMissingThenReturn: enrichment composes with soft-delete (ADR-0008).
// A Title whose only File goes Missing is hidden and is not enriched while
// hidden; when the File returns the Title is visible again with its enrichment
// intact (it survived, like watch state), and a fresh full pass re-enriches it
// cleanly.
func TestEnrichMissingThenReturn(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	root := testharness.MutableLibraryDir(t, fixtureRoot(t))
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "") // manual + deterministic

	id := titleIDByName(t, srv, token, libID, "Dune")
	if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "matched" {
		t.Fatalf("precondition: Dune status = %q, want matched", s)
	}

	// Remove Dune's only File → rescan → it is hidden (excluded from browse).
	// Capture the original (probeable) bytes first so the restore re-creates a
	// file ffprobe can read (random bytes would fail the scan).
	duneFile := filepath.Join(root, "Dune (2021)", "Dune (2021).mp4")
	orig, err := os.ReadFile(duneFile)
	if err != nil {
		t.Fatalf("read Dune file: %v", err)
	}
	if err := os.Remove(duneFile); err != nil {
		t.Fatalf("remove Dune file: %v", err)
	}
	scanLib(t, srv, token, libID, "")
	for _, ts := range listAllTitles(t, srv, token, libID).Titles {
		if ts.ID == id {
			t.Errorf("Missing (hidden) Dune is still in the browse list")
		}
	}
	// An enrich pass while Dune is hidden skips it (no error; hidden excluded).
	enrichLib(t, srv, token, libID, "")
	if d := getEnrichedDetail(t, srv, token, id); d.Overview == "" {
		t.Errorf("enrichment lost while Title was Missing (should survive)")
	}

	// Restore the File (original bytes) → rescan → Dune is visible again.
	if err := os.WriteFile(duneFile, orig, 0o644); err != nil {
		t.Fatalf("restore Dune file: %v", err)
	}
	scanLib(t, srv, token, libID, "")
	visible := false
	for _, ts := range listAllTitles(t, srv, token, libID).Titles {
		if ts.ID == id {
			visible = true
		}
	}
	if !visible {
		t.Errorf("returned Dune not back in the browse list")
	}
	if d := getEnrichedDetail(t, srv, token, id); d.Overview == "" {
		t.Errorf("enrichment did not survive the Missing → return cycle")
	}
	// A full pass re-enriches the returned Title cleanly (no error, still matched).
	enrichLib(t, srv, token, libID, "full")
	if s := getEnrichedDetail(t, srv, token, id).EnrichmentStatus; s != "matched" {
		t.Errorf("returned Dune status after full re-enrich = %q, want matched", s)
	}
}

// TestEnrichBackgroundShutsDownCleanly boots BOTH background enrichers (auto +
// scheduled) and drives a scan; the test completing at all proves Close (run in
// t.Cleanup, waiting on the worker + scheduler done-channels) returns promptly —
// a leaked or blocked goroutine would deadlock Close and time the test out.
func TestEnrichBackgroundShutsDownCleanly(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
		testharness.WithAutoEnrich(true),
		testharness.WithEnrichInterval(20*time.Millisecond),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Dune")
	waitFor(t, "background enrich to run", func() bool {
		return getEnrichedDetail(t, srv, token, id).EnrichmentStatus == "matched"
	})
	// t.Cleanup → app.Close must drain the background goroutines without hanging.
}
