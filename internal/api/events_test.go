package api_test

import (
	"bufio"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/testharness"
)

// TestLibraryUpdatedSSE: a connected client on GET /api/v1/events observes a
// library-scoped `libraryUpdated` event after a scan completes, carrying the
// scanned Library's id. This is the black-box proof of the gating spine end to
// end — it mirrors TestEnrichProgressSSE (same stream, same handshake + cancel
// discipline), but drives the smallest real library-scoped event.
func TestLibraryUpdatedSSE(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "First Movie (2001)", "First Movie (2001).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)

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

	// Trigger a scan; on completion the handler publishes a library-scoped
	// libraryUpdated carrying this Library's id.
	scanLib(t, srv, token, libID, "")

	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, "event: libraryUpdated") })
	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, `"libraryId":"`+libID+`"`) })
}

// openEventStream opens GET /api/v1/events with the given bearer token, asserts a
// 200, drains lines into a channel, and waits for the ": connected" handshake so
// the caller is subscribed before triggering a producing action. The supplied ctx
// must be cancelled by the caller to close the long-lived connection (else the
// harness's ts.Close blocks on it). Mirrors the boilerplate in TestEnrichProgressSSE.
func openEventStream(t *testing.T, ctx context.Context, srv *testharness.Server, token string) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL("/api/v1/events"), nil)
	if err != nil {
		t.Fatalf("building events request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("events GET: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
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
	waitForLine(t, lines, func(s string) bool { return strings.HasPrefix(s, ": connected") })
	return lines
}

// TestScanProgressSSE: a connected client on GET /api/v1/events observes
// scanProgress events while a manual scan runs, terminating with a complete:true
// event whose counts match the scan result. Mirrors TestEnrichProgressSSE.
func TestScanProgressSSE(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "First Movie (2001)", "First Movie (2001).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)

	// Trigger a manual scan; it publishes scanProgress (+ a terminal complete).
	scanLib(t, srv, token, libID, "")

	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, "event: "+scanProgressEventName) })
	// The terminal event carries the final counts (one Title, one File) and is
	// library-scoped to the scanned Library.
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, `"complete":true`) &&
			strings.Contains(s, `"libraryId":"`+libID+`"`) &&
			strings.Contains(s, `"titlesFound":1`) &&
			strings.Contains(s, `"filesFound":1`)
	})
}

const scanProgressEventName = "scanProgress"

// TestScanProgressScheduledSSE: the scheduled safety-net scan path also publishes
// scanProgress (PRD story 4), verifiable via a short WithScanInterval — the same
// way the enrich tests exercise the scheduled enrich path with a short interval.
func TestScanProgressScheduledSSE(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "First Movie (2001)", "First Movie (2001).mp4"))

	srv := testharness.New(t, testharness.WithScanInterval(20*time.Millisecond))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)

	// No manual trigger: the scheduled sweep picks up the Library and scans it,
	// publishing the same library-scoped scanProgress the manual handler does.
	waitForLine(t, lines, func(s string) bool { return strings.Contains(s, "event: "+scanProgressEventName) })
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, `"complete":true`) && strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}
