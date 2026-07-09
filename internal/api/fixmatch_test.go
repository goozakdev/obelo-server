package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

type overrideResp struct {
	ID          string `json:"id"`
	FolderPath  string `json:"folderPath"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
	IdentityKey string `json:"identityKey"`
	Orphaned    bool   `json:"orphaned"`
}

type overridesListResp struct {
	Overrides []overrideResp `json:"overrides"`
}

func listOverrides(t *testing.T, srv *testharness.Server, token, libID string) overridesListResp {
	t.Helper()
	var out overridesListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/overrides", token, &out)
	if status != http.StatusOK {
		t.Fatalf("list overrides = %d, want 200; body: %s", status, body)
	}
	return out
}

// TestFixMatchPersistsAcrossRescan: a fix-match override re-points a folder's
// identity and survives a subsequent rescan (the scan does not undo it).
func TestFixMatchPersistsAcrossRescan(t *testing.T) {
	root := t.TempDir()
	// A yearless folder the convention files as needs-review; the Admin corrects
	// its identity to a precise title+year via fix-match.
	folder := filepath.Join(root, "Mislabelled Folder")
	makeMovie(t, filepath.Join(folder, "Mislabelled Folder.mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	// Record the override, keyed to the folder path.
	var ov overrideResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token, map[string]any{
		"folderPath": folder,
		"title":      "Corrected Title",
		"year":       1999,
	}, &ov)
	if status != http.StatusOK {
		t.Fatalf("fix-match = %d, want 200; body: %s", status, body)
	}
	if ov.IdentityKey == "" {
		t.Fatalf("override identity key empty; body: %s", body)
	}

	// Rescan: the folder now resolves to the corrected identity.
	scanLib(t, srv, token, libID, "")
	list := listAllTitles(t, srv, token, libID)
	found := false
	for _, ts := range list.Titles {
		if ts.Title == "Corrected Title" && ts.Year == 1999 {
			found = true
		}
	}
	if !found {
		t.Errorf("override not applied on rescan; titles = %+v", list.Titles)
	}

	// The override itself persists (still listed, not orphaned).
	ovs := listOverrides(t, srv, token, libID)
	if len(ovs.Overrides) != 1 {
		t.Fatalf("overrides = %d, want 1 (persisted)", len(ovs.Overrides))
	}
	if ovs.Overrides[0].Orphaned {
		t.Errorf("override orphaned while its folder still exists")
	}

	// A full rescan must also keep it.
	scanLib(t, srv, token, libID, "full")
	if got := len(listOverrides(t, srv, token, libID).Overrides); got != 1 {
		t.Errorf("override dropped after full rescan (got %d)", got)
	}
}

// TestFixMatchOrphanedOnFolderRename: renaming the overridden folder drops the
// override's anchor; the next scan flags it orphaned and it appears in the Admin
// attention list (not silently lost).
func TestFixMatchOrphanedOnFolderRename(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Anchor Folder (2010)")
	makeMovie(t, filepath.Join(folder, "Anchor Folder (2010).mp4"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token, map[string]any{
		"folderPath": folder,
		"title":      "Anchored Title",
		"year":       2010,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("fix-match = %d, want 200; body: %s", status, body)
	}

	// Rename (move) the anchor folder, then rescan.
	if err := os.Rename(folder, filepath.Join(root, "Renamed Folder (2010)")); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")

	ovs := listOverrides(t, srv, token, libID)
	if len(ovs.Overrides) != 1 {
		t.Fatalf("overrides = %d, want 1 (surfaced, not lost)", len(ovs.Overrides))
	}
	if !ovs.Overrides[0].Orphaned {
		t.Errorf("override not flagged orphaned after its folder was renamed")
	}
}

// TestFixMatchUnknownLibrary: fix-match on an unknown Library is 404.
func TestFixMatchUnknownLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	status, _ := srv.JSON(http.MethodPost, "/api/v1/libraries/no-such/fix-match", token, map[string]any{
		"folderPath": "/tmp/x", "title": "X",
	}, nil)
	if status != http.StatusNotFound {
		t.Errorf("fix-match unknown lib = %d, want 404", status)
	}
}

// TestFixMatchRequiresAdmin: a Member cannot fix-match or list overrides.
func TestFixMatchRequiresAdmin(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	status, _ := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", mTok, map[string]any{
		"folderPath": "/tmp/x", "title": "X",
	}, nil)
	if status != http.StatusForbidden {
		t.Errorf("member fix-match = %d, want 403", status)
	}
	status, _ = srv.AuthGET("/api/v1/libraries/"+libID+"/overrides", mTok, nil)
	if status != http.StatusForbidden {
		t.Errorf("member list overrides = %d, want 403", status)
	}
}
