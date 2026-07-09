package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue-06 black-box tests: drive incremental rescan, soft-delete of Missing,
// identity stability across renames, full-mode re-derivation, and the fix-match
// Match override through the HTTP API. These MUTATE the library between scans,
// so each builds its own throwaway library dir (never the checked-in fixtures).

// makeMovie generates one ~1s clip at dst (parent dirs created). Skips the test
// if ffmpeg is unavailable.
func makeMovie(t *testing.T, dst string) {
	t.Helper()
	if !namingFixturesAvailable {
		t.Skip("ffmpeg not on PATH")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if !generateNamingClip(namingFixtureClip{relPath: dst, size: "160x120"}, dst) {
		t.Fatalf("ffmpeg failed to generate %q", dst)
	}
}

// scanLib triggers a scan (mode optional), waits for it to finish, and returns
// the settled status. The POST is asynchronous — it returns 202 with state
// "running" and the scan runs in the background (observed via the pollable GET)
// — so the helper polls until the Library leaves "running" before returning, so
// callers can then assert on the resulting catalog deterministically.
func scanLib(t *testing.T, srv *testharness.Server, token, libID, mode string) scanStatusResp {
	t.Helper()
	path := "/api/v1/libraries/" + libID + "/scan"
	if mode != "" {
		path += "?mode=" + mode
	}
	status, body := srv.JSON(http.MethodPost, path, token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("scan trigger status = %d, want 202; body: %s", status, body)
	}
	return waitScanSettled(t, srv, token, libID)
}

// waitScanSettled polls the pollable scan status until the Library leaves the
// "running" state (or the test times out), returning the settled status.
func waitScanSettled(t *testing.T, srv *testharness.Server, token, libID string) scanStatusResp {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var st scanStatusResp
		status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/scan", token, &st)
		if status != http.StatusOK {
			t.Fatalf("scan status read = %d; body: %s", status, body)
		}
		if st.State != "running" {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not settle within timeout; last state %q", st.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRescanAddOneFile: adding a movie folder between scans surfaces exactly one
// new Title; the existing one is unaffected (incremental add).
func TestRescanAddOneFile(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "First Movie (2001)", "First Movie (2001).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 1 {
		t.Fatalf("after first scan titles = %d, want 1", got)
	}

	makeMovie(t, filepath.Join(root, "Second Movie (2002)", "Second Movie (2002).mp4"))
	scanLib(t, srv, token, libID, "")
	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 2 {
		t.Errorf("after adding one movie titles = %d, want 2", got)
	}
}

// TestRescanSoftDeleteAndRecover: removing a Title's only file hides it from
// browse (Missing) without deleting it; re-adding restores it. While hidden it
// is still fetchable by id with its hidden/missing state visible.
func TestRescanSoftDeleteAndRecover(t *testing.T) {
	root := t.TempDir()
	moviePath := filepath.Join(root, "Gone Movie (2005)", "Gone Movie (2005).mp4")
	makeMovie(t, moviePath)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	if len(list.Titles) != 1 {
		t.Fatalf("titles = %d, want 1", len(list.Titles))
	}
	titleID := list.Titles[0].ID

	// Remove the only file and rescan: Title drops out of the browse list.
	if err := os.Remove(moviePath); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")
	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 0 {
		t.Errorf("after removal list titles = %d, want 0 (hidden)", got)
	}

	// But the Title is still fetchable by id (state recoverable), flagged hidden
	// with its File marked missing.
	var d struct {
		ID       string `json:"id"`
		Hidden   bool   `json:"hidden"`
		Editions []struct {
			Files []struct {
				Missing bool `json:"missing"`
			} `json:"files"`
		} `json:"editions"`
	}
	status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get hidden title = %d, want 200; body: %s", status, body)
	}
	if !d.Hidden {
		t.Errorf("all-missing title not flagged hidden")
	}
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 || !d.Editions[0].Files[0].Missing {
		t.Errorf("file not flagged missing; body: %s", body)
	}

	// Re-add the file and rescan: Title returns to the browse list (same id).
	makeMovie(t, moviePath)
	scanLib(t, srv, token, libID, "")
	restored := listAllTitles(t, srv, token, libID)
	if len(restored.Titles) != 1 {
		t.Fatalf("after re-add list titles = %d, want 1", len(restored.Titles))
	}
	if restored.Titles[0].ID != titleID {
		t.Errorf("restored Title id = %q, want same %q (identity stable)", restored.Titles[0].ID, titleID)
	}
}

// TestRescanRenameSameTitle: renaming a movie's file (same parsed identity)
// re-resolves to the SAME Title without creating a duplicate. The old path goes
// Missing; the new path is the live File under the unchanged Title id.
func TestRescanRenameSameTitle(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Stable Movie (2006)")
	makeMovie(t, filepath.Join(folder, "Stable Movie (2006).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	if len(list.Titles) != 1 {
		t.Fatalf("titles = %d, want 1", len(list.Titles))
	}
	origID := list.Titles[0].ID

	// Rename the file within the same folder (identity is folder-derived, so it
	// is unchanged). A scanner sees missing-old + new file → same Title.
	if err := os.Rename(
		filepath.Join(folder, "Stable Movie (2006).mp4"),
		filepath.Join(folder, "Stable Movie (2006) - 1080p.mkv"),
	); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")

	after := listAllTitles(t, srv, token, libID)
	if len(after.Titles) != 1 {
		t.Fatalf("after rename titles = %d, want 1 (no duplicate)", len(after.Titles))
	}
	if after.Titles[0].ID != origID {
		t.Errorf("Title id changed across rename: %q -> %q", origID, after.Titles[0].ID)
	}
}

// TestRescanFullModeRederives: a full-mode scan re-derives the catalog and the
// library remains consistent (no duplicates, correct count).
func TestRescanFullModeRederives(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "Full A (2007)", "Full A (2007).mp4"))
	makeMovie(t, filepath.Join(root, "Full B (2008)", "Full B (2008).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 2 {
		t.Fatalf("incremental titles = %d, want 2", got)
	}

	st := scanLib(t, srv, token, libID, "full")
	if st.TitlesFound != 2 {
		t.Errorf("full-mode titlesFound = %d, want 2", st.TitlesFound)
	}
	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 2 {
		t.Errorf("after full rescan titles = %d, want 2 (no duplicates)", got)
	}
}

// TestScanSurvivesClientDisconnect: a scan must run to completion server-side
// even when the triggering client is already gone — navigating away from the
// admin page must not cancel an in-flight scan. We drive the fully wired handler
// in-process with a request whose context is ALREADY cancelled (standing in for
// a dropped connection). Before the fix the handler passed r.Context() straight
// to the scanner, so the walk aborted at its first cancellation checkpoint and
// the Library was left marked errored ("context canceled"); now the scan is
// detached from the request, so it finishes and the Library settles to idle.
func TestScanSurvivesClientDisconnect(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// An already-cancelled request context models a client that navigated away:
	// the connection (and thus r.Context()) is dead before the scan walks the
	// Library. ServeHTTP runs the handler in-process against exactly this context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/libraries/"+libID+"/scan", nil,
	).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// The trigger is accepted (202) without waiting for the walk; the scan runs in
	// the background on a request-independent context, so the dead request context
	// must not affect it.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scan trigger = %d, want 202 (accepted despite the dead request context); body: %s",
			rec.Code, rec.Body)
	}
	var accepted scanStatusResp
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decoding scan response: %v; body: %s", err, rec.Body)
	}
	if accepted.State != "running" {
		t.Errorf("accepted state = %q, want running", accepted.State)
	}

	// The background scan must run to completion regardless of the dead client:
	// it settles to idle with the full count, never errored ("context canceled").
	st := waitScanSettled(t, srv, token, libID)
	if st.State != "idle" {
		t.Fatalf("post-scan state = %q (err %q), want idle — the scan must not abort on client disconnect",
			st.State, st.ErrorMessage)
	}
	// The checked-in fixture root holds three movies; a completed scan finds all.
	if st.TitlesFound != 3 {
		t.Errorf("titlesFound = %d, want 3 (scan ran to completion)", st.TitlesFound)
	}
}

// TestScheduledScanRunsOnInterval: with a short configured interval, the always-
// on scheduled scan picks up a new movie without any manual scan trigger.
func TestScheduledScanRunsOnInterval(t *testing.T) {
	if !namingFixturesAvailable {
		t.Skip("ffmpeg not on PATH")
	}
	root := t.TempDir()

	// Boot with a fast scheduled-scan cadence so the test doesn't wait long.
	srv := testharness.New(t, testharness.WithScanInterval(150*time.Millisecond))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)

	// Drop a movie in AFTER boot and never trigger a manual scan: the scheduler
	// must discover it on a later tick.
	makeMovie(t, filepath.Join(root, "Scheduled Movie (2020)", "Scheduled Movie (2020).mp4"))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(listAllTitles(t, srv, token, libID).Titles) == 1 {
			return // the scheduled scan found it
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled scan did not pick up the new movie within the deadline")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
