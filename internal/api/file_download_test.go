package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for the sessionless direct-file download (GET
// /api/v1/files/{id}/download) behind the "Open in VLC" affordance: it serves
// the original bytes addressed by the stable File id, with bearer OR ?token=
// auth (an external player can send neither a header nor the media cookie), and
// hides unknown ids / bad tokens.

// duneFile scans the fixtures and returns (token, fileID, sizeBytes) for Dune's
// single File — the shared setup for the download tests.
func duneFile(t *testing.T, srv *testharness.Server) (token, fileID string, size int64) {
	t.Helper()
	requireFixtures(t)
	token = adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var detail titleDetailResp
	if status, body := srv.AuthGET("/api/v1/titles/"+duneID, token, &detail); status != http.StatusOK {
		t.Fatalf("title detail status = %d; body: %s", status, body)
	}
	if len(detail.Editions) == 0 || len(detail.Editions[0].Files) == 0 {
		t.Fatal("Dune has no editions/files")
	}
	f := detail.Editions[0].Files[0]
	return token, f.ID, f.SizeBytes
}

// TestFileDownloadBearer: the bearer header streams the whole File; the body is
// the complete on-disk bytes (length == sizeBytes).
func TestFileDownloadBearer(t *testing.T) {
	srv := testharness.New(t)
	token, fileID, size := duneFile(t, srv)

	status, body := srv.AuthGET("/api/v1/files/"+fileID+"/download", token, nil)
	if status != http.StatusOK {
		t.Fatalf("download status = %d, want 200; body: %s", status, body)
	}
	if int64(len(body)) != size {
		t.Fatalf("download body = %d bytes, want %d (full File)", len(body), size)
	}
}

// TestFileDownloadQueryToken: with NO Authorization header, a valid ?token=
// authenticates — this is the path an external player (VLC) actually takes.
func TestFileDownloadQueryToken(t *testing.T) {
	srv := testharness.New(t)
	token, fileID, size := duneFile(t, srv)

	status, body := srv.GET("/api/v1/files/"+fileID+"/download?token="+token, nil)
	if status != http.StatusOK {
		t.Fatalf("query-token download status = %d, want 200; body: %s", status, body)
	}
	if int64(len(body)) != size {
		t.Fatalf("download body = %d bytes, want %d", len(body), size)
	}
}

// TestFileDownloadRange: a Range request gets 206 Partial Content (seek support
// from http.ServeContent), so an external player can scrub.
func TestFileDownloadRange(t *testing.T) {
	srv := testharness.New(t)
	token, fileID, _ := duneFile(t, srv)

	req, err := http.NewRequest(http.MethodGet, srv.URL("/api/v1/files/"+fileID+"/download?token="+token), nil)
	if err != nil {
		t.Fatalf("building range request: %v", err)
	}
	req.Header.Set("Range", "bytes=0-99")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp.StatusCode)
	}
	if resp.ContentLength != 100 {
		t.Fatalf("range content-length = %d, want 100", resp.ContentLength)
	}
}

// TestFileDownloadNoAuth: neither a bearer header nor a token query param → 401.
func TestFileDownloadNoAuth(t *testing.T) {
	srv := testharness.New(t)
	_, fileID, _ := duneFile(t, srv)

	if status, body := srv.GET("/api/v1/files/"+fileID+"/download", nil); status != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401; body: %s", status, body)
	}
}

// TestFileDownloadBadToken: a garbage ?token= is rejected (validated against the
// DB like any credential), not silently served.
func TestFileDownloadBadToken(t *testing.T) {
	srv := testharness.New(t)
	_, fileID, _ := duneFile(t, srv)

	if status, body := srv.GET("/api/v1/files/"+fileID+"/download?token=not-a-real-token", nil); status != http.StatusUnauthorized {
		t.Fatalf("bad-token status = %d, want 401; body: %s", status, body)
	}
}

// TestFileDownloadUnknownFile: an authenticated request for a nonexistent File
// id is hidden as a 404.
func TestFileDownloadUnknownFile(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	status, body := srv.AuthGET("/api/v1/files/00000000-0000-0000-0000-000000000000/download", token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown-file status = %d, want 404; body: %s", status, body)
	}
}
