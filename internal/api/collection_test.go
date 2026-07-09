package api_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box HTTP tests for collections-playlists issue 01: the Admin-curated,
// shared Collection end-to-end, returning FULL membership (the per-viewer access
// filter is issue 02). Every acceptance-criterion checkbox in the issue is
// exercised here over real HTTP via the testharness seam.

// --- wire shapes ------------------------------------------------------------

type collectionResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type collectionCardResp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MemberCount int    `json:"memberCount"`
	PosterURL   string `json:"posterUrl"`
}

type collectionsListResp struct {
	Collections []collectionCardResp `json:"collections"`
}

// memberSummaryResp captures the full decorated Title-summary shape so the parity
// test can compare a Collection member against a browse-list entry field-for-field.
type memberSummaryResp struct {
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Title            string   `json:"title"`
	Year             int      `json:"year"`
	ContentRating    string   `json:"contentRating"`
	Watched          bool     `json:"watched"`
	ResumePositionMs int64    `json:"resumePositionMs"`
	Genres           []string `json:"genres"`
	ArtworkVersion   string   `json:"artworkVersion"`
	AddedAt          string   `json:"addedAt"`
}

type memberListResp struct {
	Titles []memberSummaryResp `json:"titles"`
}

type collectionDetailResp struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	MemberCount int                 `json:"memberCount"`
	Members     []memberSummaryResp `json:"members"`
}

// --- helpers ----------------------------------------------------------------

// createCollection POSTs a Collection (Admin) and returns its id, asserting 201.
func createCollection(t *testing.T, srv *testharness.Server, token, name, description string) string {
	t.Helper()
	body := map[string]any{"name": name}
	if description != "" {
		body["description"] = description
	}
	var out collectionResp
	st, raw := srv.JSON(http.MethodPost, "/api/v1/collections", token, body, &out)
	if st != http.StatusCreated {
		t.Fatalf("create collection %q: status %d; body: %s", name, st, raw)
	}
	if out.ID == "" {
		t.Fatalf("create collection %q: empty id; body: %s", name, raw)
	}
	return out.ID
}

// addCollectionItems POSTs titleIDs to a Collection (Admin), asserting 204.
func addCollectionItems(t *testing.T, srv *testharness.Server, token, colID string, titleIDs ...string) {
	t.Helper()
	st, raw := srv.JSON(http.MethodPost, "/api/v1/collections/"+colID+"/items", token,
		map[string]any{"titleIds": titleIDs}, nil)
	if st != http.StatusNoContent {
		t.Fatalf("add items %v: status %d; body: %s", titleIDs, st, raw)
	}
}

func getCollectionDetail(t *testing.T, srv *testharness.Server, token, colID string) collectionDetailResp {
	t.Helper()
	var out collectionDetailResp
	st, raw := srv.AuthGET("/api/v1/collections/"+colID, token, &out)
	if st != http.StatusOK {
		t.Fatalf("get collection %s: status %d; body: %s", colID, st, raw)
	}
	return out
}

func memberIDs(d collectionDetailResp) []string {
	ids := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		ids = append(ids, m.ID)
	}
	return ids
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// scanMovies boots a server, creates+scans a Movie Library, and returns the
// server, admin token, library id, and the browse list of its Titles.
func scanMovies(t *testing.T) (*testharness.Server, string, string, titlesListResp) {
	t.Helper()
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")
	var list titlesListResp
	if st, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &list); st != http.StatusOK {
		t.Fatalf("browse list: status %d; body: %s", st, body)
	}
	return srv, admin, lib, list
}

// --- tests ------------------------------------------------------------------

// TestCollectionCRUDAndItems covers the create/rename/delete lifecycle, the
// idempotent set-add, single-item remove, the list card metadata (member count +
// representative poster, ordered by sort_title), and the delete cascade.
// Acceptance: POST/PUT/DELETE; idempotent add + DELETE item; GET list with count
// + representative poster; delete-Collection cascade.
func TestCollectionCRUDAndItems(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)

	blade := findTitle(t, list, "Blade Runner")
	dune := findTitle(t, list, "Dune")
	sample := findTitle(t, list, "Sample Movie")

	// POST creates.
	colID := createCollection(t, srv, admin, "A24 Films", "hand-picked")

	// POST items, with a DUPLICATE id in one call and a re-add in a second call —
	// both must collapse (a Collection is a set, idempotent add).
	addCollectionItems(t, srv, admin, colID, dune, blade, dune)
	addCollectionItems(t, srv, admin, colID, blade) // re-add existing → no-op
	addCollectionItems(t, srv, admin, colID, sample)

	detail := getCollectionDetail(t, srv, admin, colID)
	if detail.Name != "A24 Films" || detail.Description != "hand-picked" {
		t.Errorf("detail name/desc = %q/%q, want A24 Films/hand-picked", detail.Name, detail.Description)
	}
	// Three distinct members despite the duplicate/re-add, in sort_title order:
	// Blade Runner, Dune, Sample Movie.
	wantOrder := []string{blade, dune, sample}
	if got := memberIDs(detail); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("members = %v, want %v (deduped, sort_title order)", got, wantOrder)
	}
	if detail.MemberCount != 3 {
		t.Errorf("memberCount = %d, want 3", detail.MemberCount)
	}

	// GET list: card carries member count + representative poster (first member by
	// sort_title = Blade Runner).
	var ls collectionsListResp
	if st, body := srv.AuthGET("/api/v1/collections", admin, &ls); st != http.StatusOK {
		t.Fatalf("list collections: status %d; body: %s", st, body)
	}
	if len(ls.Collections) != 1 {
		t.Fatalf("list = %d collections, want 1", len(ls.Collections))
	}
	card := ls.Collections[0]
	if card.MemberCount != 3 {
		t.Errorf("card memberCount = %d, want 3", card.MemberCount)
	}
	wantPoster := "/api/v1/titles/" + blade + "/artwork/poster"
	if card.PosterURL != wantPoster {
		t.Errorf("card posterUrl = %q, want %q (first member by sort_title)", card.PosterURL, wantPoster)
	}

	// DELETE one item → it drops out.
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/collections/"+colID+"/items/"+dune, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete item: status %d; body: %s", st, body)
	}
	detail = getCollectionDetail(t, srv, admin, colID)
	if containsID(memberIDs(detail), dune) || detail.MemberCount != 2 {
		t.Errorf("after remove: members = %v, count %d, want Dune gone / count 2", memberIDs(detail), detail.MemberCount)
	}
	// Removing a non-member is a harmless no-op (idempotent) → 204.
	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/collections/"+colID+"/items/"+dune, admin, nil, nil); st != http.StatusNoContent {
		t.Errorf("remove non-member = %d, want 204 (idempotent)", st)
	}

	// PUT renames + edits description.
	var renamed collectionResp
	if st, body := srv.JSON(http.MethodPut, "/api/v1/collections/"+colID, admin,
		map[string]any{"name": "Neo-Noir", "description": ""}, &renamed); st != http.StatusOK {
		t.Fatalf("rename: status %d; body: %s", st, body)
	}
	if renamed.Name != "Neo-Noir" || renamed.Description != "" {
		t.Errorf("after rename = %q/%q, want Neo-Noir/empty", renamed.Name, renamed.Description)
	}

	// DELETE the Collection → it's gone (its membership cascades away).
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/collections/"+colID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete collection: status %d; body: %s", st, body)
	}
	if st, _ := srv.AuthGET("/api/v1/collections/"+colID, admin, nil); st != http.StatusNotFound {
		t.Errorf("get deleted collection = %d, want 404", st)
	}
	if st, _ := srv.AuthGET("/api/v1/collections", admin, &ls); st == http.StatusOK && len(ls.Collections) != 0 {
		t.Errorf("after delete, list = %d collections, want 0", len(ls.Collections))
	}
}

// TestCollectionMemberDecorationParity asserts a Collection member is decorated
// EXACTLY like the same Title on a browse list (toTitleSummary parity, via the
// reused catalog bulk readers). Acceptance: GET detail members decorated
// identically to a browse list.
func TestCollectionMemberDecorationParity(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, _ := scanMovies(t)

	// Re-read browse list with the FULL summary shape, after setting a content
	// rating so the parity comparison covers an enrichment-sourced field too.
	var browse memberListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse)
	var duneID string
	for _, m := range browse.Titles {
		if m.Title == "Dune" {
			duneID = m.ID
		}
	}
	if duneID == "" {
		t.Fatal("Dune not in browse list")
	}
	srv.SetTitleContentRating(duneID, "PG-13")

	// Capture the browse summary AFTER the rating is set.
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &browse)
	var browseDune memberSummaryResp
	for _, m := range browse.Titles {
		if m.ID == duneID {
			browseDune = m
		}
	}

	colID := createCollection(t, srv, admin, "Parity", "")
	addCollectionItems(t, srv, admin, colID, duneID)
	detail := getCollectionDetail(t, srv, admin, colID)
	if len(detail.Members) != 1 {
		t.Fatalf("collection members = %d, want 1", len(detail.Members))
	}
	colDune := detail.Members[0]
	if !reflect.DeepEqual(colDune, browseDune) {
		t.Errorf("member summary parity mismatch:\n collection: %+v\n browse:     %+v", colDune, browseDune)
	}
	if colDune.ContentRating != "PG-13" {
		t.Errorf("member contentRating = %q, want PG-13 (decoration carries enrichment fields)", colDune.ContentRating)
	}
}

// TestCollectionCrossKind asserts a Collection may span media kinds — a Movie and
// a TV Episode in one Collection. Acceptance: cross-kind membership allowed.
func TestCollectionCrossKind(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	libMovie := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	libTV := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, libMovie, "")
	scanLib(t, srv, admin, libTV, "")

	var movies titlesListResp
	srv.AuthGET("/api/v1/libraries/"+libMovie+"/titles?limit=50", admin, &movies)
	movieID := findTitle(t, movies, "Dune")

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
	episodeID := eps.Episodes[0].ID

	colID := createCollection(t, srv, admin, "Mixed", "")
	addCollectionItems(t, srv, admin, colID, movieID, episodeID)

	detail := getCollectionDetail(t, srv, admin, colID)
	if detail.MemberCount != 2 {
		t.Fatalf("cross-kind memberCount = %d, want 2", detail.MemberCount)
	}
	kinds := map[string]string{}
	for _, m := range detail.Members {
		kinds[m.ID] = m.Kind
	}
	if kinds[movieID] != "movie" {
		t.Errorf("movie member kind = %q, want movie", kinds[movieID])
	}
	if kinds[episodeID] != "episode" {
		t.Errorf("episode member kind = %q, want episode", kinds[episodeID])
	}
}

// TestCollectionMissingMemberOmittedButPersists: a member whose Files all go
// Missing (hidden=1) is omitted from the resolved view, but its membership row
// persists — restoring the Files makes it reappear with no re-add. Acceptance:
// Missing-omitted-but-persists.
func TestCollectionMissingMemberOmittedButPersists(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")
	blade := findTitle(t, list, "Blade Runner")

	colID := createCollection(t, srv, admin, "Soft-state", "")
	addCollectionItems(t, srv, admin, colID, dune, blade)
	if c := getCollectionDetail(t, srv, admin, colID).MemberCount; c != 2 {
		t.Fatalf("initial memberCount = %d, want 2", c)
	}

	// Dune goes Missing (all Files off disk → hidden): omitted from the view.
	srv.SetTitleHidden(dune, true)
	detail := getCollectionDetail(t, srv, admin, colID)
	if containsID(memberIDs(detail), dune) || detail.MemberCount != 1 {
		t.Errorf("Missing member: members = %v, count %d, want Dune omitted / count 1", memberIDs(detail), detail.MemberCount)
	}
	// The list card count also reflects only visible members.
	var ls collectionsListResp
	srv.AuthGET("/api/v1/collections", admin, &ls)
	if len(ls.Collections) != 1 || ls.Collections[0].MemberCount != 1 {
		t.Errorf("card memberCount with Missing = %+v, want 1", ls.Collections)
	}

	// Files return: Dune reappears — no re-add needed (membership row persisted).
	srv.SetTitleHidden(dune, false)
	detail = getCollectionDetail(t, srv, admin, colID)
	if !containsID(memberIDs(detail), dune) || detail.MemberCount != 2 {
		t.Errorf("after restore: members = %v, count %d, want Dune back / count 2", memberIDs(detail), detail.MemberCount)
	}
}

// TestCollectionLibraryDeleteCascade: deleting a Library a member belongs to drops
// it from every Collection automatically (the title_id FK cascade, via the Library
// → titles cascade). Acceptance: cascade on title/library delete.
func TestCollectionLibraryDeleteCascade(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	libA := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	libB := createMovieLibrary(t, srv, admin, testharness.MutableLibraryDir(t, fixtureRoot(t)))
	scanLib(t, srv, admin, libA, "")
	scanLib(t, srv, admin, libB, "")

	var aList, bList titlesListResp
	srv.AuthGET("/api/v1/libraries/"+libA+"/titles?limit=50", admin, &aList)
	srv.AuthGET("/api/v1/libraries/"+libB+"/titles?limit=50", admin, &bList)
	duneA := findTitle(t, aList, "Dune")
	duneB := findTitle(t, bList, "Dune")

	colID := createCollection(t, srv, admin, "Both Libs", "")
	addCollectionItems(t, srv, admin, colID, duneA, duneB)
	if c := getCollectionDetail(t, srv, admin, colID).MemberCount; c != 2 {
		t.Fatalf("initial memberCount = %d, want 2", c)
	}

	// Delete libB → its Titles cascade away, and so does duneB's membership row.
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/libraries/"+libB, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete library: status %d; body: %s", st, body)
	}
	detail := getCollectionDetail(t, srv, admin, colID)
	if detail.MemberCount != 1 || !containsID(memberIDs(detail), duneA) || containsID(memberIDs(detail), duneB) {
		t.Errorf("after library delete: members = %v, count %d, want only duneA", memberIDs(detail), detail.MemberCount)
	}
}

// TestCollectionAdminOnlyWritesAndUnknownID: writes are Admin scope (a Member gets
// 403), reads are open to any authenticated User, and an unknown Collection id is
// 404 across the surface. Acceptance: Admin-only writes (403 for a Member);
// unknown id → 404.
func TestCollectionAdminOnlyWritesAndUnknownID(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")

	colID := createCollection(t, srv, admin, "Curated", "")
	addCollectionItems(t, srv, admin, colID, dune)

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	// Grant the Member access to the library so the access-filtered read (issue 02)
	// resolves a visible member — keeping this test's focus on the write-scope /
	// 404-hide-existence behavior, not on access-filtering (covered separately).
	grantLibraries(t, srv, admin, memberID, lib)

	// A Member can READ (list + detail; the granted member sees the collection).
	var ls collectionsListResp
	if st, _ := srv.AuthGET("/api/v1/collections", member, &ls); st != http.StatusOK {
		t.Errorf("member list collections = %d, want 200", st)
	}
	if st, _ := srv.AuthGET("/api/v1/collections/"+colID, member, nil); st != http.StatusOK {
		t.Errorf("member get collection = %d, want 200", st)
	}

	// Every WRITE is Admin-only → 403 for the Member.
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/collections", map[string]any{"name": "x"}},
		{http.MethodPut, "/api/v1/collections/" + colID, map[string]any{"name": "y"}},
		{http.MethodDelete, "/api/v1/collections/" + colID, nil},
		{http.MethodPost, "/api/v1/collections/" + colID + "/items", map[string]any{"titleIds": []string{dune}}},
		{http.MethodDelete, "/api/v1/collections/" + colID + "/items/" + dune, nil},
	}
	for _, wr := range writes {
		if st, body := srv.JSON(wr.method, wr.path, member, wr.body, nil); st != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403; body: %s", wr.method, wr.path, st, body)
		}
	}

	// Unknown Collection id → 404 across reads + writes (hide-existence).
	const ghost = "no-such-collection"
	probes := []struct {
		method, path string
		body         any
		token        string
	}{
		{http.MethodGet, "/api/v1/collections/" + ghost, nil, admin},
		{http.MethodPut, "/api/v1/collections/" + ghost, map[string]any{"name": "z"}, admin},
		{http.MethodDelete, "/api/v1/collections/" + ghost, nil, admin},
		{http.MethodPost, "/api/v1/collections/" + ghost + "/items", map[string]any{"titleIds": []string{dune}}, admin},
		{http.MethodDelete, "/api/v1/collections/" + ghost + "/items/" + dune, nil, admin},
	}
	for _, p := range probes {
		if st, body := srv.JSON(p.method, p.path, p.token, p.body, nil); st != http.StatusNotFound {
			t.Errorf("%s %s unknown id = %d, want 404; body: %s", p.method, p.path, st, body)
		}
	}
}

// TestCollectionUnknownTitleRejected: adding an unknown Title id is a clean 422
// (UNKNOWN_TITLE) that leaves the membership set unchanged (validate-all-then-
// insert). Acceptance: unknown title in POST items handled (documented as 422).
func TestCollectionUnknownTitleRejected(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	dune := findTitle(t, list, "Dune")

	colID := createCollection(t, srv, admin, "Validated", "")
	addCollectionItems(t, srv, admin, colID, dune)

	// One good id + one bad id in the same call → whole add rejected, set unchanged.
	var env errorEnvelope
	st, body := srv.JSON(http.MethodPost, "/api/v1/collections/"+colID+"/items", admin,
		map[string]any{"titleIds": []string{findTitle(t, list, "Blade Runner"), "no-such-title"}}, &env)
	if st != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_TITLE" {
		t.Fatalf("add unknown title = %d/%s, want 422 UNKNOWN_TITLE; body: %s", st, env.Error.Code, body)
	}
	detail := getCollectionDetail(t, srv, admin, colID)
	if detail.MemberCount != 1 || !containsID(memberIDs(detail), dune) {
		t.Errorf("after rejected add: members = %v, want unchanged [Dune]", memberIDs(detail))
	}

	// A blank name on create is a 400 BAD_REQUEST.
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/collections", admin, map[string]any{"name": "   "}, &env); st != http.StatusBadRequest || env.Error.Code != "BAD_REQUEST" {
		t.Errorf("blank-name create = %d/%s, want 400 BAD_REQUEST", st, env.Error.Code)
	}
}
