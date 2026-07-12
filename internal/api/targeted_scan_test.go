package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for the Targeted scan (ADR-0030): POST /titles/{id}/scan
// re-walks just that Movie's folder — picking up a file added to it while leaving
// a sibling Movie's folder untouched — and refuses a Movie with no files on disk.
// These MUTATE the library between scans, so each builds its own throwaway dir.

func titleDetail(t *testing.T, srv *testharness.Server, token, id string) titleDetailResp {
	t.Helper()
	var d titleDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title %s = %d; body: %s", id, status, body)
	}
	return d
}

// TestTargetedScanMovieAddsWithinScopeOnly: a Targeted scan of one Movie picks up
// a second Edition dropped into its folder, and does NOT catalogue a stray file
// added to a sibling Movie's out-of-scope folder.
func TestTargetedScanMovieAddsWithinScopeOnly(t *testing.T) {
	root := t.TempDir()
	aDir := filepath.Join(root, "Alpha (2001)")
	bDir := filepath.Join(root, "Bravo (2002)")
	makeMovie(t, filepath.Join(aDir, "Alpha (2001).mp4"))
	makeMovie(t, filepath.Join(bDir, "Bravo (2002).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	alphaID := titleIDByName(t, srv, token, libID, "Alpha")
	bravoID := titleIDByName(t, srv, token, libID, "Bravo")

	// Drop a second Edition into Alpha's folder AND a stray file into Bravo's.
	makeMovie(t, filepath.Join(aDir, "Alpha (2001) - 1080p.mp4"))
	makeMovie(t, filepath.Join(bDir, "Bravo (2002) - 1080p.mp4"))

	// Targeted scan of Alpha only.
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+alphaID+"/scan", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("targeted scan status = %d, want 202; body: %s", status, body)
	}
	waitScanSettled(t, srv, token, libID)

	// Alpha gained the new Edition file (2 files across its editions now).
	if got := countFiles(titleDetail(t, srv, token, alphaID)); got != 2 {
		t.Errorf("Alpha files after targeted scan = %d, want 2", got)
	}
	// Bravo's stray file was outside the scope, so Bravo is unchanged (1 file).
	if got := countFiles(titleDetail(t, srv, token, bravoID)); got != 1 {
		t.Errorf("Bravo files = %d, want 1 (its folder was out of scope)", got)
	}
}

// TestTargetedScanRefusesMovieWithNoFiles: a Movie whose only file was removed is
// hidden (Missing); a Targeted scan of it has nothing on disk to walk, so it 409s
// (hidden-entity resurrection is out of scope for v1 — ADR-0030).
func TestTargetedScanRefusesMovieWithNoFiles(t *testing.T) {
	root := t.TempDir()
	moviePath := filepath.Join(root, "Vanish (2003)", "Vanish (2003).mp4")
	makeMovie(t, moviePath)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Vanish")

	// Remove the only file and full-scan: the Title goes Missing/hidden.
	if err := os.Remove(moviePath); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")

	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/scan", token, nil, nil)
	if status != http.StatusConflict {
		t.Fatalf("targeted scan of all-Missing Movie = %d, want 409; body: %s", status, body)
	}
}

// TestTargetedScanMemberForbidden: a Member cannot trigger a Targeted scan
// (Admin-only, mirroring the full-Library scan).
func TestTargetedScanMemberForbidden(t *testing.T) {
	root := t.TempDir()
	makeMovie(t, filepath.Join(root, "Charlie (2004)", "Charlie (2004).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := titleIDByName(t, srv, token, libID, "Charlie")

	srv.CreateMember("member", "memberpass123")
	memberToken := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	status, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/scan", memberToken, nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("member targeted scan = %d, want 403", status)
	}
}

// countFiles totals the Files across a Title's Editions.
func countFiles(d titleDetailResp) int {
	n := 0
	for _, e := range d.Editions {
		n += len(e.Files)
	}
	return n
}
