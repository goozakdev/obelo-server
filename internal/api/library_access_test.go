package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for access-control issue 03: per-User library-access grants,
// enforced across the read/play surface. A Member sees only the Libraries
// granted to them; everything else is hidden as 404 (api-contract.md).

// userDetailResp is the GET /users/{id} shape (with granted libraryIds and the
// Rating ceiling).
type userDetailResp struct {
	ID            string   `json:"id"`
	Username      string   `json:"username"`
	Role          string   `json:"role"`
	LibraryIDs    []string `json:"libraryIds"`
	RatingCeiling string   `json:"ratingCeiling"`
}

// grantLibraries replaces a User's granted Library set via the admin API,
// asserting a clean 204. With no ids it clears the set.
func grantLibraries(t *testing.T, srv *testharness.Server, adminTok, userID string, libIDs ...string) {
	t.Helper()
	ids := libIDs
	if ids == nil {
		ids = []string{}
	}
	st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+userID+"/libraryAccess", adminTok,
		map[string]any{"libraryIds": ids}, nil)
	if st != http.StatusNoContent {
		t.Fatalf("grant libraries %v: status %d; body: %s", ids, st, body)
	}
}

func containsHomeTitle(rows []upNextHomeTitle, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestGrantedMemberSeesSameAsAdmin: a Member granted a Library observes exactly
// what an Admin does within it (browse / detail / home / search) — the threaded
// Scope opens up cleanly once a grant exists.
func TestGrantedMemberSeesSameAsAdmin(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	member := srv.LoginAs("kid", "memberpass123")

	var adminList, memberList titlesListResp
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &adminList); st != http.StatusOK {
		t.Fatalf("admin list status = %d; body: %s", st, body)
	}
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", member, &memberList); st != http.StatusOK {
		t.Fatalf("member list status = %d; body: %s", st, body)
	}
	if len(adminList.Titles) == 0 || len(memberList.Titles) != len(adminList.Titles) {
		t.Fatalf("member saw %d titles, admin saw %d; want equal and non-zero",
			len(memberList.Titles), len(adminList.Titles))
	}
	titleID := adminList.Titles[0].ID
	if st, _ := srv.AuthGET("/api/v1/titles/"+titleID, member, nil); st != http.StatusOK {
		t.Errorf("member get-title status = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/home", member, nil); st != http.StatusOK {
		t.Errorf("member home status = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/search?q=a", member, nil); st != http.StatusOK {
		t.Errorf("member search status = %d, want 200", st)
	}
}

// TestLibraryGrantsHideUngrantedLibrary: with two Libraries and one granted, the
// ungranted Library and its Titles are invisible to the Member (404), while the
// Admin sees both. Two Movie Libraries (a fixture copy) make the same Title
// appear in each, so search/detail can be compared by count.
func TestLibraryGrantsHideUngrantedLibrary(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	root2 := testharness.MutableLibraryDir(t, fixtureRoot(t))
	lib1 := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	lib2 := createMovieLibrary(t, srv, admin, root2)
	scanLib(t, srv, admin, lib1, "")
	scanLib(t, srv, admin, lib2, "")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib1) // lib2 ungranted
	member := srv.LoginAs("kid", "memberpass123")

	// /libraries: member sees only lib1; admin sees both.
	var ml, al librariesListResp
	srv.AuthGET("/api/v1/libraries", member, &ml)
	srv.AuthGET("/api/v1/libraries", admin, &al)
	if len(ml.Libraries) != 1 || ml.Libraries[0].ID != lib1 {
		t.Fatalf("member libraries = %+v, want only %s", ml.Libraries, lib1)
	}
	if len(al.Libraries) != 2 {
		t.Errorf("admin libraries = %d, want 2", len(al.Libraries))
	}

	// Titles list: granted lib1 200 (non-empty), ungranted lib2 404.
	var l1 titlesListResp
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib1+"/titles?limit=50", member, &l1); st != http.StatusOK {
		t.Fatalf("member lib1 titles = %d; body: %s", st, body)
	}
	if len(l1.Titles) == 0 {
		t.Fatal("member saw no titles in granted lib1")
	}
	if st, _ := srv.AuthGET("/api/v1/libraries/"+lib2+"/titles", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted lib2 titles = %d, want 404", st)
	}

	// Detail: a granted Title 200; the ungranted-Library copy 404 for the member,
	// 200 for the admin. Artwork + playback on the ungranted Title also 404.
	var l2admin titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib2+"/titles?limit=50", admin, &l2admin)
	dune1 := findTitle(t, l1, "Dune")
	dune2 := findTitle(t, l2admin, "Dune")
	if st, _ := srv.AuthGET("/api/v1/titles/"+dune1, member, nil); st != http.StatusOK {
		t.Errorf("member granted-title detail = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/titles/"+dune2, member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted-title detail = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/titles/"+dune2, admin, nil); st != http.StatusOK {
		t.Errorf("admin ungranted-title detail = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/titles/"+dune2+"/artwork/poster", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted-title artwork = %d, want 404", st)
	}
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+dune2+"/playback", member, mp4Profile(), nil); st != http.StatusNotFound {
		t.Errorf("member ungranted-title playback = %d, want 404", st)
	}

	// Home: member RecentlyAdded only from lib1; admin sees more (both libs).
	mh := getHome(t, srv, member)
	if len(mh.RecentlyAdded) != len(l1.Titles) {
		t.Errorf("member RecentlyAdded = %d, want %d (granted lib only)", len(mh.RecentlyAdded), len(l1.Titles))
	}
	ah := getHome(t, srv, admin)
	if len(ah.RecentlyAdded) <= len(l1.Titles) {
		t.Errorf("admin RecentlyAdded = %d, want > %d (both libs)", len(ah.RecentlyAdded), len(l1.Titles))
	}

	// Search: a Title present in both libs returns one match for the member
	// (granted only) and two for the admin.
	var ms, as searchResp
	srv.AuthGET("/api/v1/search?q=Dune", member, &ms)
	srv.AuthGET("/api/v1/search?q=Dune", admin, &as)
	if len(ms.Movies) != 1 {
		t.Errorf("member search Dune = %d movies, want 1 (granted only)", len(ms.Movies))
	}
	if len(as.Movies) != 2 {
		t.Errorf("admin search Dune = %d movies, want 2 (both libs)", len(as.Movies))
	}
}

// TestLibraryGrantsHideTVChildDrilldown: every level of an ungranted TV Library
// — the Show's seasons, a Season's episodes, an Episode's detail, and a Show's
// artwork — is 404 for the Member, while the Admin drills it normally.
func TestLibraryGrantsHideTVChildDrilldown(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	libMovie := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	libTV := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, libMovie, "")
	scanLib(t, srv, admin, libTV, "")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libMovie) // TV library ungranted
	member := srv.LoginAs("kid", "memberpass123")

	// Drill the ungranted TV library as the Admin to get ids to probe.
	shows := listShows(t, srv, admin, libTV)
	if len(shows.Shows) == 0 {
		t.Fatal("tv fixture produced no shows")
	}
	showID := shows.Shows[0].ID
	seasons := showSeasons(t, srv, admin, showID)
	if len(seasons.Seasons) == 0 {
		t.Fatal("tv show has no seasons")
	}
	seasonID := seasons.Seasons[0].ID
	eps := seasonEpisodes(t, srv, admin, seasonID)

	if st, _ := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted show seasons = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/seasons/"+seasonID+"/episodes", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted season episodes = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/shows/"+showID+"/artwork/poster", member, nil); st != http.StatusNotFound {
		t.Errorf("member ungranted show artwork = %d, want 404", st)
	}
	if len(eps.Episodes) > 0 {
		if st, _ := srv.AuthGET("/api/v1/titles/"+eps.Episodes[0].ID, member, nil); st != http.StatusNotFound {
			t.Errorf("member ungranted episode detail = %d, want 404", st)
		}
	}
	// Admin sanity: the same drill-down works.
	if st, _ := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", admin, nil); st != http.StatusOK {
		t.Errorf("admin show seasons = %d, want 200", st)
	}
}

// TestLibraryGrantManagement covers the replace-set write rules: unknown user,
// admin target, and unknown library are rejected (the prior set kept); a valid
// set (with a duplicate) is deduped; an empty set clears; GET reflects each.
func TestLibraryGrantManagement(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")

	// Unknown user → 404.
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/ghost/libraryAccess", admin,
		map[string]any{"libraryIds": []string{lib}}, nil); st != http.StatusNotFound {
		t.Errorf("grant unknown user = %d, want 404", st)
	}

	// Granting to an Admin → 422 ADMIN_GRANT.
	var users usersListResp
	srv.AuthGET("/api/v1/users", admin, &users)
	var adminID string
	for _, u := range users.Users {
		if u.Role == "admin" {
			adminID = u.ID
		}
	}
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+adminID+"/libraryAccess", admin,
		map[string]any{"libraryIds": []string{lib}}, &env); st != http.StatusUnprocessableEntity || env.Error.Code != "ADMIN_GRANT" {
		t.Errorf("grant admin = %d/%s, want 422 ADMIN_GRANT", st, env.Error.Code)
	}

	// Unknown library → 422 UNKNOWN_LIBRARY, prior (empty) set unchanged.
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+memberID+"/libraryAccess", admin,
		map[string]any{"libraryIds": []string{"no-such-lib"}}, &env); st != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_LIBRARY" {
		t.Errorf("grant unknown lib = %d/%s, want 422 UNKNOWN_LIBRARY", st, env.Error.Code)
	}
	var ud userDetailResp
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if len(ud.LibraryIDs) != 0 {
		t.Errorf("after rejected grant, libraryIds = %v, want empty (unchanged)", ud.LibraryIDs)
	}

	// Valid replace-set with a duplicate → 204; deduped to one.
	grantLibraries(t, srv, admin, memberID, lib, lib)
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if len(ud.LibraryIDs) != 1 || ud.LibraryIDs[0] != lib {
		t.Errorf("after grant, libraryIds = %v, want [%s]", ud.LibraryIDs, lib)
	}

	// Replace with empty → grants cleared.
	grantLibraries(t, srv, admin, memberID)
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if len(ud.LibraryIDs) != 0 {
		t.Errorf("after clear, libraryIds = %v, want empty", ud.LibraryIDs)
	}
}

// TestGrantCascadesOnLibraryDelete: deleting a Library drops the grant rows that
// pointed at it (the ON DELETE CASCADE), observable as the id disappearing from
// the Member's libraryIds — no stale grant survives.
func TestGrantCascadesOnLibraryDelete(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	root2 := testharness.MutableLibraryDir(t, fixtureRoot(t))
	lib1 := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	lib2 := createMovieLibrary(t, srv, admin, root2)

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib1, lib2)

	// Delete lib2 → its grant row cascades away.
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/libraries/"+lib2, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete library status = %d, want 204; body: %s", st, body)
	}
	var ud userDetailResp
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if len(ud.LibraryIDs) != 1 || ud.LibraryIDs[0] != lib1 {
		t.Errorf("after deleting lib2, libraryIds = %v, want [%s] (lib2 grant cascaded)", ud.LibraryIDs, lib1)
	}
}

// TestContinueWatchingDropsOnGrantRevoke: a Member's resume on a Title drops out
// of Continue Watching the moment the grant covering it is revoked (Home
// recomputes against live Scope), and playback on the now-out-of-scope Title 404s.
func TestContinueWatchingDropsOnGrantRevoke(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")
	var titles titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &titles)
	duneID := findTitle(t, titles, "Dune")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	member := srv.LoginAs("kid", "memberpass123")

	// Play Dune and report a mid-band resume → it lands in Continue Watching.
	dur := titleDuration(t, srv, member, duneID)
	dec := negotiateDune(t, srv, member, duneID)
	postProgress(t, srv, member, dec.SessionID, dur/2, http.StatusOK)
	if home := getHome(t, srv, member); !containsHomeTitle(home.ContinueWatching, duneID) {
		t.Fatalf("granted member Continue Watching missing Dune: %+v", home.ContinueWatching)
	}

	// Revoke the grant → Dune drops out of Continue Watching, and playback 404s.
	grantLibraries(t, srv, admin, memberID) // empty set
	if home := getHome(t, srv, member); containsHomeTitle(home.ContinueWatching, duneID) {
		t.Errorf("revoked member still has Dune in Continue Watching: %+v", home.ContinueWatching)
	}
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", member, mp4Profile(), nil); st != http.StatusNotFound {
		t.Errorf("revoked member playback = %d, want 404", st)
	}
}

// TestNoGrantsEmptyCatalog: a Member with no grants sees an empty catalog
// (libraries, home, search all empty) — not an error.
func TestNoGrantsEmptyCatalog(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")
	srv.CreateUser(admin, "kid", "memberpass123", "member") // no grant
	member := srv.LoginAs("kid", "memberpass123")

	var libs librariesListResp
	srv.AuthGET("/api/v1/libraries", member, &libs)
	if len(libs.Libraries) != 0 {
		t.Errorf("no-grant member libraries = %d, want 0", len(libs.Libraries))
	}
	home := getHome(t, srv, member)
	if len(home.RecentlyAdded)+len(home.ContinueWatching)+len(home.UpNext) != 0 {
		t.Errorf("no-grant member home not empty: %+v", home)
	}
	var sr searchResp
	srv.AuthGET("/api/v1/search?q=Dune", member, &sr)
	if len(sr.Movies) != 0 {
		t.Errorf("no-grant member search = %d movies, want 0", len(sr.Movies))
	}
	if st, _ := srv.AuthGET("/api/v1/libraries/"+lib+"/titles", member, nil); st != http.StatusNotFound {
		t.Errorf("no-grant member titles = %d, want 404", st)
	}
}
