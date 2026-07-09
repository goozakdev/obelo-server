package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for access-control issue 04: the per-User Rating ceiling. A
// capped Member never sees a Title above their ceiling; unrated content stays
// visible; clearing the ceiling restores everything; an Admin is never capped.

// setRatingCeiling sets (or, with rating "", clears) a User's ceiling via the
// admin API, asserting a clean 204.
func setRatingCeiling(t *testing.T, srv *testharness.Server, adminTok, userID, rating string) {
	t.Helper()
	st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+userID+"/ratingCeiling", adminTok,
		map[string]any{"rating": rating}, nil)
	if st != http.StatusNoContent {
		t.Fatalf("set ceiling %q: status %d; body: %s", rating, st, body)
	}
}

// TestRatingCeilingHidesAboveCeilingMovies: a PG-13-capped Member sees the
// PG-13 and unrated Titles but not the R one, across list / detail / search /
// home / playback; clearing the ceiling brings the R Title back; the Admin
// always sees it.
func TestRatingCeilingHidesAboveCeilingMovies(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	var all titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &all)
	blade := findTitle(t, all, "Blade Runner")
	dune := findTitle(t, all, "Dune")
	// Blade Runner = R (above PG-13), Dune = PG-13 (at ceiling), Sample Movie left
	// unrated ("") — should stay visible under the documented policy.
	srv.SetTitleContentRating(blade, "R")
	srv.SetTitleContentRating(dune, "PG-13")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	member := srv.LoginAs("kid", "memberpass123")

	// Titles list: the R Title is gone; PG-13 + unrated remain (3 → 2).
	var ml titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", member, &ml)
	if len(ml.Titles) != len(all.Titles)-1 {
		t.Fatalf("capped member saw %d titles, want %d (R hidden)", len(ml.Titles), len(all.Titles)-1)
	}
	for _, tt := range ml.Titles {
		if tt.ID == blade {
			t.Errorf("capped member's list still contains the R title")
		}
	}

	// Detail: R 404; PG-13 and unrated 200.
	if st, _ := srv.AuthGET("/api/v1/titles/"+blade, member, nil); st != http.StatusNotFound {
		t.Errorf("capped member R-title detail = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/titles/"+dune, member, nil); st != http.StatusOK {
		t.Errorf("capped member PG-13-title detail = %d, want 200", st)
	}

	// Search + home + playback all exclude the R Title for the member.
	var ms searchResp
	srv.AuthGET("/api/v1/search?q=Blade%20Runner", member, &ms)
	if len(ms.Movies) != 0 {
		t.Errorf("capped member search for R title = %d, want 0", len(ms.Movies))
	}
	if home := getHome(t, srv, member); containsHomeTitle(home.RecentlyAdded, blade) {
		t.Errorf("capped member home still surfaces the R title")
	}
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+blade+"/playback", member, mp4Profile(), nil); st != http.StatusNotFound {
		t.Errorf("capped member R-title playback = %d, want 404", st)
	}

	// The Admin always sees the R Title (never capped).
	if st, _ := srv.AuthGET("/api/v1/titles/"+blade, admin, nil); st != http.StatusOK {
		t.Errorf("admin R-title detail = %d, want 200", st)
	}
	var as searchResp
	srv.AuthGET("/api/v1/search?q=Blade%20Runner", admin, &as)
	if len(as.Movies) != 1 {
		t.Errorf("admin search for R title = %d, want 1", len(as.Movies))
	}

	// Clearing the ceiling brings the R Title back for the member.
	setRatingCeiling(t, srv, admin, memberID, "")
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", member, &ml)
	if len(ml.Titles) != len(all.Titles) {
		t.Errorf("after clearing ceiling, member saw %d titles, want %d", len(ml.Titles), len(all.Titles))
	}
	if st, _ := srv.AuthGET("/api/v1/titles/"+blade, member, nil); st != http.StatusOK {
		t.Errorf("after clearing ceiling, R-title detail = %d, want 200", st)
	}
}

// TestRatingCeilingCrossSystemTV: a PG-13 ceiling hides a TV-MA Show and shows a
// TV-14 Show — proving the single maturity ladder spans the movie and TV systems.
func TestRatingCeilingCrossSystemTV(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, lib, "")

	shows := listShows(t, srv, admin, lib)
	if len(shows.Shows) < 2 {
		t.Fatalf("tv fixture produced %d shows, need >= 2", len(shows.Shows))
	}
	tv14 := shows.Shows[0].ID
	tvMA := shows.Shows[1].ID
	srv.SetShowContentRating(tv14, "TV-14") // at PG-13's rung → visible
	srv.SetShowContentRating(tvMA, "TV-MA") // above → hidden

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	member := srv.LoginAs("kid", "memberpass123")

	// The Member's TV grid (GET /libraries/{id}/titles → shows) hides the TV-MA
	// Show and keeps the TV-14 Show.
	mShows := listShows(t, srv, member, lib)
	var sawTV14, sawTVMA bool
	for _, s := range mShows.Shows {
		if s.ID == tv14 {
			sawTV14 = true
		}
		if s.ID == tvMA {
			sawTVMA = true
		}
	}
	if !sawTV14 {
		t.Errorf("capped member should see the TV-14 show in the grid")
	}
	if sawTVMA {
		t.Errorf("capped member should NOT see the TV-MA show in the grid")
	}

	// Drill-down hide-existence: the TV-MA show's seasons 404; the TV-14 show's
	// seasons resolve.
	if st, _ := srv.AuthGET("/api/v1/shows/"+tvMA+"/seasons", member, nil); st != http.StatusNotFound {
		t.Errorf("capped member TV-MA seasons = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/shows/"+tv14+"/seasons", member, nil); st != http.StatusOK {
		t.Errorf("capped member TV-14 seasons = %d, want 200", st)
	}

	// The Admin sees both shows (never capped).
	aShows := listShows(t, srv, admin, lib)
	if len(aShows.Shows) != len(shows.Shows) {
		t.Errorf("admin saw %d shows, want %d (uncapped)", len(aShows.Shows), len(shows.Shows))
	}
}

// TestRatingCeilingManagement: the ceiling endpoint validates its input and GET
// reflects the stored value.
func TestRatingCeilingManagement(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")

	// Set + GET reflects.
	setRatingCeiling(t, srv, admin, memberID, "PG-13")
	var ud userDetailResp
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if ud.RatingCeiling != "PG-13" {
		t.Errorf("ratingCeiling = %q, want PG-13", ud.RatingCeiling)
	}

	// Unknown label → 422 UNKNOWN_RATING.
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+memberID+"/ratingCeiling", admin,
		map[string]any{"rating": "BOGUS"}, &env); st != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_RATING" {
		t.Errorf("unknown label = %d/%s, want 422 UNKNOWN_RATING", st, env.Error.Code)
	}

	// Setting a ceiling on an Admin → 422 ADMIN_CEILING.
	var users usersListResp
	srv.AuthGET("/api/v1/users", admin, &users)
	var adminID string
	for _, u := range users.Users {
		if u.Role == "admin" {
			adminID = u.ID
		}
	}
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+adminID+"/ratingCeiling", admin,
		map[string]any{"rating": "G"}, &env); st != http.StatusUnprocessableEntity || env.Error.Code != "ADMIN_CEILING" {
		t.Errorf("ceiling on admin = %d/%s, want 422 ADMIN_CEILING", st, env.Error.Code)
	}

	// Clearing → GET reflects empty.
	setRatingCeiling(t, srv, admin, memberID, "")
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if ud.RatingCeiling != "" {
		t.Errorf("after clear, ratingCeiling = %q, want empty", ud.RatingCeiling)
	}
}
