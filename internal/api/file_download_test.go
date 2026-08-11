package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/obelo-server/internal/testharness"
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

	status, body := srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown-file status = %d, want 404; body: %s", status, body)
	}
}

// The access-Scope tests for this route. The direct-file download addresses a
// File by id and never passes through GetTitle, so the Scope has to be applied
// to the Title that OWNS the File — in both dimensions — or a Member who learns
// an id downloads the original bytes of anything in the catalog, using the one
// credential that travels in a URL.

// unknownFileID is a well-formed id no File ever has — the baseline "no such
// file" response every out-of-scope refusal is compared against.
const unknownFileID = "00000000-0000-0000-0000-000000000000"

// fileIDOfTitle returns the id of a Title's first File, read through the given
// token's Title detail. The scope tests pass the ADMIN token deliberately: an
// Admin is all-access, so this resolves ids for Titles the Member under test
// cannot see — which is precisely what probing an out-of-scope File requires.
func fileIDOfTitle(t *testing.T, srv *testharness.Server, token, titleID string) string {
	t.Helper()
	var detail titleDetailResp
	if status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &detail); status != http.StatusOK {
		t.Fatalf("title detail %s status = %d, want 200; body: %s", titleID, status, body)
	}
	if len(detail.Editions) == 0 || len(detail.Editions[0].Files) == 0 {
		t.Fatalf("title %s has no editions/files", titleID)
	}
	return detail.Editions[0].Files[0].ID
}

// TestFileDownloadRespectsLibraryGrants: a Member downloads a File in a granted
// Library and gets the same 404 as a nonexistent id for one in an ungranted
// Library — over the bearer header AND over ?token= (the external-player path,
// which must not be the loose one). The Admin downloads both.
func TestFileDownloadRespectsLibraryGrants(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	root2 := testharness.MutableLibraryDir(t, fixtureRoot(t))
	lib1 := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	lib2 := createMovieLibrary(t, srv, admin, root2)
	scanLib(t, srv, admin, lib1, "")
	scanLib(t, srv, admin, lib2, "")

	var l1, l2 titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib1+"/titles?limit=50", admin, &l1)
	srv.AuthGET("/api/v1/libraries/"+lib2+"/titles?limit=50", admin, &l2)
	grantedFile := fileIDOfTitle(t, srv, admin, findTitle(t, l1, "Dune"))
	ungrantedFile := fileIDOfTitle(t, srv, admin, findTitle(t, l2, "Dune"))

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib1) // lib2 ungranted
	member := srv.LoginAs("kid", "memberpass123")

	// Granted Library: the Member gets the whole File, exactly like the Admin.
	status, body := srv.AuthGET("/api/v1/files/"+grantedFile+"/download", member, nil)
	if status != http.StatusOK {
		t.Fatalf("member granted-library download = %d, want 200; body: %s", status, body)
	}
	if len(body) == 0 {
		t.Error("member granted-library download returned an empty body")
	}

	// Ungranted Library: 404, over both credentials.
	if st, _ := srv.AuthGET("/api/v1/files/"+ungrantedFile+"/download", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted-library download (bearer) = %d, want 404", st)
	}
	if st, _ := srv.GET("/api/v1/files/"+ungrantedFile+"/download?token="+member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted-library download (?token=) = %d, want 404", st)
	}

	// The Admin resolves to an all-access Scope, so both are unaffected — asserted
	// rather than assumed, since the guard is new on this route.
	if st, _ := srv.AuthGET("/api/v1/files/"+grantedFile+"/download", admin, nil); st != http.StatusOK {
		t.Errorf("admin lib1 download = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/files/"+ungrantedFile+"/download", admin, nil); st != http.StatusOK {
		t.Errorf("admin lib2 download = %d, want 200", st)
	}

	// Restoring the grant restores the download — the refusal tracks live Scope,
	// it is not a one-way state on the File.
	grantLibraries(t, srv, admin, memberID, lib1, lib2)
	member = srv.LoginAs("kid", "memberpass123")
	if st, _ := srv.AuthGET("/api/v1/files/"+ungrantedFile+"/download", member, nil); st != http.StatusOK {
		t.Errorf("member download after grant = %d, want 200", st)
	}
}

// TestFileDownloadRespectsRatingCeiling: a Library grant is not a bypass of the
// Rating ceiling. A PG-13-capped Member downloads the PG-13 File and is refused
// the R one, inside the SAME granted Library; the Admin is never capped.
func TestFileDownloadRespectsRatingCeiling(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	var all titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &all)
	blade := findTitle(t, all, "Blade Runner")
	dune := findTitle(t, all, "Dune")
	srv.SetTitleContentRating(blade, "R")    // above a PG-13 ceiling
	srv.SetTitleContentRating(dune, "PG-13") // at the ceiling → visible
	aboveFile := fileIDOfTitle(t, srv, admin, blade)
	belowFile := fileIDOfTitle(t, srv, admin, dune)

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	member := srv.LoginAs("kid", "memberpass123")

	if st, body := srv.AuthGET("/api/v1/files/"+belowFile+"/download", member, nil); st != http.StatusOK {
		t.Errorf("capped member at-ceiling download = %d, want 200; body: %s", st, body)
	}
	if st, _ := srv.AuthGET("/api/v1/files/"+aboveFile+"/download", member, nil); st != http.StatusNotFound {
		t.Errorf("capped member above-ceiling download = %d, want 404", st)
	}
	if st, _ := srv.GET("/api/v1/files/"+aboveFile+"/download?token="+member, nil); st != http.StatusNotFound {
		t.Errorf("capped member above-ceiling download (?token=) = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/files/"+aboveFile+"/download", admin, nil); st != http.StatusOK {
		t.Errorf("admin above-ceiling download = %d, want 200", st)
	}

	// Clearing the ceiling restores the R File for the Member (the grant already
	// covered the Library — only the rating dimension was refusing it).
	setRatingCeiling(t, srv, admin, memberID, "")
	if st, _ := srv.AuthGET("/api/v1/files/"+aboveFile+"/download", member, nil); st != http.StatusOK {
		t.Errorf("after clearing ceiling, member above-ceiling download = %d, want 200", st)
	}
}

// TestFileDownloadOutOfScope404MatchesUnknownID asserts the security property
// itself: an out-of-scope File and a nonexistent File id produce the SAME status
// and the SAME response body, byte for byte, for BOTH Scope dimensions. If they
// ever diverge, this route becomes an oracle — a Member could confirm which File
// ids exist in Libraries they were never granted, which is exactly the fact the
// 404-not-403 posture (api-contract.md) exists to hide. Compare the bodies, not
// just the codes: a helpful "you don't have access to this library" message is a
// 404 too, and would leak everything.
func TestFileDownloadOutOfScope404MatchesUnknownID(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	root2 := testharness.MutableLibraryDir(t, fixtureRoot(t))
	lib1 := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	lib2 := createMovieLibrary(t, srv, admin, root2)
	scanLib(t, srv, admin, lib1, "")
	scanLib(t, srv, admin, lib2, "")

	var l1, l2 titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib1+"/titles?limit=50", admin, &l1)
	srv.AuthGET("/api/v1/libraries/"+lib2+"/titles?limit=50", admin, &l2)
	blade := findTitle(t, l1, "Blade Runner")
	srv.SetTitleContentRating(blade, "R")
	aboveCeilingFile := fileIDOfTitle(t, srv, admin, blade)
	ungrantedFile := fileIDOfTitle(t, srv, admin, findTitle(t, l2, "Dune"))

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib1) // lib2 ungranted
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	member := srv.LoginAs("kid", "memberpass123")

	baseStatus, baseBody := srv.AuthGET("/api/v1/files/"+unknownFileID+"/download", member, nil)
	if baseStatus != http.StatusNotFound {
		t.Fatalf("unknown-id status = %d, want 404; body: %s", baseStatus, baseBody)
	}

	for _, tc := range []struct {
		name   string
		fileID string
	}{
		{"ungranted library", ungrantedFile},
		{"above rating ceiling", aboveCeilingFile},
	} {
		status, body := srv.AuthGET("/api/v1/files/"+tc.fileID+"/download", member, nil)
		if status != baseStatus {
			t.Errorf("%s status = %d, want %d (identical to unknown id)", tc.name, status, baseStatus)
		}
		if string(body) != string(baseBody) {
			t.Errorf("%s body = %s, want %s (identical to unknown id)", tc.name, body, baseBody)
		}
	}
}
