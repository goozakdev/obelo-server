package api_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box HTTP tests for collections-playlists issue 03: the User-owned, ordered,
// single-media-kind, PRIVATE Playlist end-to-end — create/list/get/rename/delete,
// append, single-kind enforcement, and strict ownership 404-hiding. Ordering ops
// (reorder, remove-by-item-id) and the explicit access/Missing-survival tests are
// issue 04 and are NOT exercised here. Every issue-03 acceptance-criterion checkbox
// is covered over real HTTP via the testharness seam.

// --- wire shapes ------------------------------------------------------------

type playlistResp struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type playlistCardResp struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	ItemCount int    `json:"itemCount"`
}

type playlistsListResp struct {
	Playlists []playlistCardResp `json:"playlists"`
}

// playlistMemberResp embeds the full browse Title-summary shape (memberSummaryResp,
// from collection_test.go) so the decoration-parity test can compare a Playlist
// member against a browse-list entry field-for-field, PLUS the itemId that makes
// duplicates distinguishable.
type playlistMemberResp struct {
	memberSummaryResp
	ItemID string `json:"itemId"`
}

type playlistDetailResp struct {
	ID          string               `json:"id"`
	Kind        string               `json:"kind"`
	Name        string               `json:"name"`
	MemberCount int                  `json:"memberCount"`
	Members     []playlistMemberResp `json:"members"`
}

// --- helpers ----------------------------------------------------------------

// createPlaylist POSTs a Playlist as the given caller and returns its id, asserting
// 201 and an empty (untyped) kind on a fresh Playlist.
func createPlaylist(t *testing.T, srv *testharness.Server, token, name string) string {
	t.Helper()
	var out playlistResp
	st, raw := srv.JSON(http.MethodPost, "/api/v1/playlists", token, map[string]any{"name": name}, &out)
	if st != http.StatusCreated {
		t.Fatalf("create playlist %q: status %d; body: %s", name, st, raw)
	}
	if out.ID == "" {
		t.Fatalf("create playlist %q: empty id; body: %s", name, raw)
	}
	if out.Kind != "" {
		t.Errorf("fresh playlist kind = %q, want empty/untyped", out.Kind)
	}
	return out.ID
}

// appendPlaylistItem POSTs a titleId to a Playlist and returns the HTTP status and
// the decoded error envelope (empty on success), so callers can assert both the
// happy path (204) and the rejection codes (422).
func appendPlaylistItem(t *testing.T, srv *testharness.Server, token, plID, titleID string) (int, errorEnvelope) {
	t.Helper()
	var env errorEnvelope
	st, _ := srv.JSON(http.MethodPost, "/api/v1/playlists/"+plID+"/items", token,
		map[string]any{"titleId": titleID}, &env)
	return st, env
}

func getPlaylistDetail(t *testing.T, srv *testharness.Server, token, plID string) playlistDetailResp {
	t.Helper()
	var out playlistDetailResp
	st, raw := srv.AuthGET("/api/v1/playlists/"+plID, token, &out)
	if st != http.StatusOK {
		t.Fatalf("get playlist %s: status %d; body: %s", plID, st, raw)
	}
	return out
}

func playlistMemberIDs(d playlistDetailResp) []string {
	ids := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		ids = append(ids, m.ID)
	}
	return ids
}

// scanMovieAndEpisode boots a server and scans a Movie + a TV Library, returning
// the server, admin token, a Movie Title id (Dune), and an Episode Title id —
// the cross-kind pair the single-kind-mismatch test needs (mirrors
// TestCollectionCrossKind's drill listShows→seasons→episodes).
func scanMovieAndEpisode(t *testing.T) (srv *testharness.Server, admin, movieID, episodeID string) {
	t.Helper()
	srv = testharness.New(t)
	admin = adminToken(t, srv)
	libMovie := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	libTV := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, libMovie, "")
	scanLib(t, srv, admin, libTV, "")

	var movies titlesListResp
	srv.AuthGET("/api/v1/libraries/"+libMovie+"/titles?limit=50", admin, &movies)
	movieID = findTitle(t, movies, "Dune")

	shows := listShows(t, srv, admin, libTV)
	if len(shows.Shows) == 0 {
		t.Fatal("tv fixture produced no shows")
	}
	seasons := showSeasons(t, srv, admin, shows.Shows[0].ID)
	if len(seasons.Seasons) == 0 {
		t.Fatal("tv show has no seasons")
	}
	eps := seasonEpisodes(t, srv, admin, seasons.Seasons[0].ID)
	if len(eps.Episodes) == 0 {
		t.Fatal("tv season has no episodes")
	}
	episodeID = eps.Episodes[0].ID
	return srv, admin, movieID, episodeID
}

// --- tests ------------------------------------------------------------------

// TestPlaylistCRUDOwnerScoped covers the create/list/rename/delete lifecycle and
// the owner-scoped list: POST creates an empty (untyped) Playlist; GET /playlists
// lists ONLY the caller's own (User A's is absent from User B's list); PUT renames;
// DELETE removes. Acceptance: POST creates empty; GET lists only the caller's; PUT
// renames; DELETE removes.
func TestPlaylistCRUDOwnerScoped(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "alice", "alicepass123", "member")
	srv.CreateUser(admin, "bob", "bobpass1234", "member")
	alice := srv.LoginAs("alice", "alicepass123")
	bob := srv.LoginAs("bob", "bobpass1234")

	// POST creates an empty, untyped Playlist (kind asserted empty in createPlaylist).
	plID := createPlaylist(t, srv, alice, "Watch Later")

	// GET /playlists returns only the caller's own: Alice sees hers; Bob sees none.
	var aliceList, bobList playlistsListResp
	if st, _ := srv.AuthGET("/api/v1/playlists", alice, &aliceList); st != http.StatusOK {
		t.Fatalf("alice list = %d, want 200", st)
	}
	if len(aliceList.Playlists) != 1 || aliceList.Playlists[0].ID != plID {
		t.Fatalf("alice list = %+v, want exactly her playlist %s", aliceList.Playlists, plID)
	}
	if aliceList.Playlists[0].ItemCount != 0 {
		t.Errorf("fresh playlist itemCount = %d, want 0", aliceList.Playlists[0].ItemCount)
	}
	if st, _ := srv.AuthGET("/api/v1/playlists", bob, &bobList); st != http.StatusOK {
		t.Fatalf("bob list = %d, want 200", st)
	}
	if len(bobList.Playlists) != 0 {
		t.Errorf("bob list = %+v, want empty (A's playlist is private to A)", bobList.Playlists)
	}

	// PUT renames (owner).
	var renamed playlistResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/playlists/"+plID, alice,
		map[string]any{"name": "Tonight"}, &renamed); st != http.StatusOK {
		t.Fatalf("rename: status %d; body: %s", st, body)
	}
	if renamed.Name != "Tonight" {
		t.Errorf("after rename = %q, want Tonight", renamed.Name)
	}

	// DELETE removes (owner) → gone.
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/playlists/"+plID, alice, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete: status %d; body: %s", st, body)
	}
	if st, _ := srv.AuthGET("/api/v1/playlists/"+plID, alice, nil); st != http.StatusNotFound {
		t.Errorf("get deleted playlist = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/playlists", alice, &aliceList); st == http.StatusOK && len(aliceList.Playlists) != 0 {
		t.Errorf("after delete, alice list = %d playlists, want 0", len(aliceList.Playlists))
	}

	// A blank name on create is a 400 BAD_REQUEST.
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/playlists", alice, map[string]any{"name": "  "}, &env); st != http.StatusBadRequest || env.Error.Code != "BAD_REQUEST" {
		t.Errorf("blank-name create = %d/%s, want 400 BAD_REQUEST", st, env.Error.Code)
	}
}

// TestPlaylistAppendSingleKind: append fixes the Playlist kind on the FIRST item; a
// subsequent append of a mismatched kind is a clean 422 KIND_MISMATCH that leaves
// the kind unchanged; a matching-kind append succeeds. Acceptance: append fixes
// kind on first item; mismatched kind → 422 KIND_MISMATCH (kind unchanged); matching
// kind succeeds.
func TestPlaylistAppendSingleKind(t *testing.T) {
	requireFixtures(t)
	srv, admin, movieID, episodeID := scanMovieAndEpisode(t)

	plID := createPlaylist(t, srv, admin, "Movies Queue")

	// First append fixes kind → movie (a Movie Title maps to "movie").
	if st, env := appendPlaylistItem(t, srv, admin, plID, movieID); st != http.StatusNoContent {
		t.Fatalf("append movie = %d/%s, want 204", st, env.Error.Code)
	}
	if k := getPlaylistDetail(t, srv, admin, plID).Kind; k != "movie" {
		t.Fatalf("kind after first movie = %q, want movie", k)
	}

	// Mismatched kind (an Episode maps to "tv") → 422 KIND_MISMATCH, kind unchanged.
	st, env := appendPlaylistItem(t, srv, admin, plID, episodeID)
	if st != http.StatusUnprocessableEntity || env.Error.Code != "KIND_MISMATCH" {
		t.Fatalf("append episode into movie playlist = %d/%s, want 422 KIND_MISMATCH", st, env.Error.Code)
	}
	detail := getPlaylistDetail(t, srv, admin, plID)
	if detail.Kind != "movie" {
		t.Errorf("kind after rejected mismatch = %q, want movie (unchanged)", detail.Kind)
	}
	if detail.MemberCount != 1 || !containsID(playlistMemberIDs(detail), movieID) {
		t.Errorf("members after rejected mismatch = %v, want unchanged [movie]", playlistMemberIDs(detail))
	}

	// A matching-kind append (a second, different Movie) succeeds.
	bladeID := findSecondMovie(t, srv, admin, movieID)
	if st, env := appendPlaylistItem(t, srv, admin, plID, bladeID); st != http.StatusNoContent {
		t.Fatalf("append second movie = %d/%s, want 204 (matching kind)", st, env.Error.Code)
	}
	if c := getPlaylistDetail(t, srv, admin, plID).MemberCount; c != 2 {
		t.Errorf("memberCount after matching append = %d, want 2", c)
	}
}

// findSecondMovie returns a Movie Title id distinct from exclude by browsing every
// Movie Library (the harness scanned exactly one), so the single-kind test has a
// second matching-kind Title without threading library ids around.
func findSecondMovie(t *testing.T, srv *testharness.Server, admin, exclude string) string {
	t.Helper()
	var libs struct {
		Libraries []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"libraries"`
	}
	srv.AuthGET("/api/v1/libraries", admin, &libs)
	for _, l := range libs.Libraries {
		if l.Kind != "movie" {
			continue
		}
		var list titlesListResp
		srv.AuthGET("/api/v1/libraries/"+l.ID+"/titles?limit=50", admin, &list)
		for _, ts := range list.Titles {
			if ts.ID != exclude {
				return ts.ID
			}
		}
	}
	t.Fatal("no second movie title found")
	return ""
}

// TestPlaylistDuplicatesAllowed: appending the SAME Title twice produces two
// distinct entries — duplicates are allowed (a Playlist is a sequence, not a set) —
// each with its own item id. Acceptance: same Title twice → two member entries with
// distinct itemIds.
func TestPlaylistDuplicatesAllowed(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")

	plID := createPlaylist(t, srv, admin, "Repeat")
	if st, env := appendPlaylistItem(t, srv, admin, plID, dune); st != http.StatusNoContent {
		t.Fatalf("first append = %d/%s, want 204", st, env.Error.Code)
	}
	if st, env := appendPlaylistItem(t, srv, admin, plID, dune); st != http.StatusNoContent {
		t.Fatalf("second (duplicate) append = %d/%s, want 204", st, env.Error.Code)
	}

	detail := getPlaylistDetail(t, srv, admin, plID)
	if detail.MemberCount != 2 {
		t.Fatalf("duplicate memberCount = %d, want 2", detail.MemberCount)
	}
	if detail.Members[0].ID != dune || detail.Members[1].ID != dune {
		t.Errorf("members = %v, want both = Dune (%s)", playlistMemberIDs(detail), dune)
	}
	if detail.Members[0].ItemID == "" || detail.Members[1].ItemID == "" {
		t.Errorf("member itemIds = %q/%q, want both populated", detail.Members[0].ItemID, detail.Members[1].ItemID)
	}
	if detail.Members[0].ItemID == detail.Members[1].ItemID {
		t.Errorf("duplicate members share itemId %q, want DISTINCT ids", detail.Members[0].ItemID)
	}
}

// TestPlaylistDetailOrderAndDecorationParity: GET detail returns members in
// position (append) order, each decorated EXACTLY like the same Title on a browse
// list (toTitleSummary parity via the reused catalog bulk readers). Acceptance: GET
// detail members in position order, decorated identically to a browse list.
func TestPlaylistDetailOrderAndDecorationParity(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, _ := scanMovies(t)

	// Browse with the full summary shape after setting a content rating, so parity
	// covers an enrichment-sourced field too (mirrors the Collection parity test).
	var browse memberListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse)
	var duneID, bladeID string
	for _, m := range browse.Titles {
		switch m.Title {
		case "Dune":
			duneID = m.ID
		case "Blade Runner":
			bladeID = m.ID
		}
	}
	if duneID == "" || bladeID == "" {
		t.Fatal("expected Dune and Blade Runner in browse list")
	}
	srv.SetTitleContentRating(duneID, "PG-13")

	var browseDune memberSummaryResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse)
	for _, m := range browse.Titles {
		if m.ID == duneID {
			browseDune = m
		}
	}

	// Append Blade Runner THEN Dune; detail must echo that exact position order.
	plID := createPlaylist(t, srv, admin, "Ordered")
	if st, _ := appendPlaylistItem(t, srv, admin, plID, bladeID); st != http.StatusNoContent {
		t.Fatal("append blade failed")
	}
	if st, _ := appendPlaylistItem(t, srv, admin, plID, duneID); st != http.StatusNoContent {
		t.Fatal("append dune failed")
	}

	detail := getPlaylistDetail(t, srv, admin, plID)
	if got := playlistMemberIDs(detail); !reflect.DeepEqual(got, []string{bladeID, duneID}) {
		t.Fatalf("member order = %v, want [blade, dune] (append/position order)", got)
	}
	// Decoration parity: the Dune member's summary equals its browse-list summary.
	var playlistDune memberSummaryResp
	for _, m := range detail.Members {
		if m.ID == duneID {
			playlistDune = m.memberSummaryResp
		}
	}
	if !reflect.DeepEqual(playlistDune, browseDune) {
		t.Errorf("member summary parity mismatch:\n playlist: %+v\n browse:   %+v", playlistDune, browseDune)
	}
	if playlistDune.ContentRating != "PG-13" {
		t.Errorf("member contentRating = %q, want PG-13 (decoration carries enrichment fields)", playlistDune.ContentRating)
	}
}

// TestPlaylistOwnership404: a Playlist is private to its owner. A non-owner — a
// second Member AND an Admin (NO override) — gets 404 on GET/PUT/DELETE/append of
// another User's Playlist, and the Playlist is absent from their GET /playlists.
// Acceptance: non-owner (Member or Admin) gets 404 on detail + every write, and the
// Playlist is absent from their list.
func TestPlaylistOwnership404(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")

	ownerID := srv.CreateUser(admin, "owner", "ownerpass123", "member")
	srv.CreateUser(admin, "intruder", "intruderpass1", "member")
	owner := srv.LoginAs("owner", "ownerpass123")
	intruder := srv.LoginAs("intruder", "intruderpass1")
	// Grant the owner access to the library so their OWN resolved view (which applies
	// the owner's access Scope) surfaces the member they added — keeping this test's
	// focus on ownership-404, not access-filtering (issue 04).
	grantLibraries(t, srv, admin, ownerID, lib)

	plID := createPlaylist(t, srv, owner, "Private")
	if st, _ := appendPlaylistItem(t, srv, owner, plID, dune); st != http.StatusNoContent {
		t.Fatalf("owner append failed: %d", st)
	}

	// Every non-owner caller (the intruding Member AND the Admin) is denied with a
	// 404 (hide-existence) across detail + every write — no Admin override.
	for _, caller := range []struct{ name, token string }{
		{"member", intruder},
		{"admin", admin},
	} {
		probes := []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/v1/playlists/" + plID, nil},
			{http.MethodPut, "/api/v1/playlists/" + plID, map[string]any{"name": "hijack"}},
			{http.MethodDelete, "/api/v1/playlists/" + plID, nil},
			{http.MethodPost, "/api/v1/playlists/" + plID + "/items", map[string]any{"titleId": dune}},
		}
		for _, p := range probes {
			if st, body := srv.JSON(p.method, p.path, caller.token, p.body, nil); st != http.StatusNotFound {
				t.Errorf("%s %s %s = %d, want 404 (private)", caller.name, p.method, p.path, st)
				_ = body
			}
		}
		// The Playlist is absent from a non-owner's list.
		var ls playlistsListResp
		srv.AuthGET("/api/v1/playlists", caller.token, &ls)
		if containsID(playlistCardIDs(ls), plID) {
			t.Errorf("%s list contains private playlist %s, want absent", caller.name, plID)
		}
	}

	// The owner is unaffected: detail still resolves and the write survived.
	if c := getPlaylistDetail(t, srv, owner, plID).MemberCount; c != 1 {
		t.Errorf("owner detail memberCount = %d, want 1 (intruder writes were all rejected)", c)
	}
}

func playlistCardIDs(ls playlistsListResp) []string {
	ids := make([]string, 0, len(ls.Playlists))
	for _, c := range ls.Playlists {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestPlaylistUserDeleteCascade: deleting a User cascades their Playlists AND their
// items away (the owner_user_id ON DELETE CASCADE, and the playlist_id cascade from
// it). Acceptance: deleting a User cascades their Playlists + items.
func TestPlaylistUserDeleteCascade(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")
	blade := findTitle(t, list, "Blade Runner")

	memberID := srv.CreateUser(admin, "doomed", "doomedpass12", "member")
	member := srv.LoginAs("doomed", "doomedpass12")
	plID := createPlaylist(t, srv, member, "Queue")
	if st, _ := appendPlaylistItem(t, srv, member, plID, dune); st != http.StatusNoContent {
		t.Fatal("append dune failed")
	}
	if st, _ := appendPlaylistItem(t, srv, member, plID, blade); st != http.StatusNoContent {
		t.Fatal("append blade failed")
	}
	if pl, it := srv.CountPlaylistRowsForOwner(memberID); pl != 1 || it != 2 {
		t.Fatalf("before delete: playlists=%d items=%d, want 1/2", pl, it)
	}

	// Delete the User via the admin API → their Playlists and items cascade away.
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+memberID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete user: status %d; body: %s", st, body)
	}
	if pl, it := srv.CountPlaylistRowsForOwner(memberID); pl != 0 || it != 0 {
		t.Errorf("after delete: playlists=%d items=%d, want 0/0 (cascade)", pl, it)
	}
	// And the owner can no longer log in (the User is gone).
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "doomed", "password": "doomedpass12",
		"device": map[string]any{"name": "d", "platform": "test", "clientId": "c"},
	}, nil); st != http.StatusUnauthorized {
		t.Errorf("deleted owner login = %d, want 401", st)
	}
}
