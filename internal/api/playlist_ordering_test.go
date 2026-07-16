package api_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box HTTP tests for collections-playlists issue 04: Playlist ORDERING
// (reorder, remove-by-item-id) plus the access-filtered, Missing-aware resolved
// VIEW survival. The reorder/remove production code is new this slice; the survival
// behavior already works via issue 03's PlaylistMembers → ResolveVisibleTitles (it
// omits Missing/out-of-scope members while the item rows persist), so it is exercised
// here with EXPLICIT tests. Every issue-04 acceptance-criterion checkbox is covered
// over real HTTP via the testharness seam.

// --- helpers ----------------------------------------------------------------

// reorderPlaylist PUTs the full desired item-id order to a Playlist and returns the
// HTTP status and decoded error envelope (empty on success), so callers can assert
// both the happy path (204) and the rejection (422 ITEM_SET_MISMATCH).
func reorderPlaylist(t *testing.T, srv *testharness.Server, token, plID string, itemIDs []string) (int, errorEnvelope) {
	t.Helper()
	var env errorEnvelope
	st, _ := srv.JSON(http.MethodPut, "/api/v1/playlists/"+plID+"/items", token,
		map[string]any{"itemIds": itemIDs}, &env)
	return st, env
}

// removePlaylistItem DELETEs one entry by its item id and returns the HTTP status.
func removePlaylistItem(t *testing.T, srv *testharness.Server, token, plID, itemID string) int {
	t.Helper()
	st, _ := srv.JSON(http.MethodDelete, "/api/v1/playlists/"+plID+"/items/"+itemID, token, nil, nil)
	return st
}

// appendAndCaptureItemIDs appends each title in turn, then returns the Playlist's
// member item ids in position order — the item ids the reorder/remove ops target.
// (Append returns 204 with no body, so the item ids are read back from the detail.)
func appendAndCaptureItemIDs(t *testing.T, srv *testharness.Server, token, plID string, titleIDs ...string) []string {
	t.Helper()
	for _, tid := range titleIDs {
		if st, env := appendPlaylistItem(t, srv, token, plID, tid); st != http.StatusNoContent {
			t.Fatalf("append %s = %d/%s, want 204", tid, st, env.Error.Code)
		}
	}
	detail := getPlaylistDetail(t, srv, token, plID)
	ids := make([]string, 0, len(detail.Members))
	for _, m := range detail.Members {
		ids = append(ids, m.ItemID)
	}
	return ids
}

func playlistMemberItemIDs(d playlistDetailResp) []string {
	ids := make([]string, 0, len(d.Members))
	for _, m := range d.Members {
		ids = append(ids, m.ItemID)
	}
	return ids
}

// --- tests ------------------------------------------------------------------

// TestPlaylistReorder: PUT /playlists/{id}/items with the full desired item-id order
// rewrites positions transactionally; GET reflects the new order. Append A,B,C then
// reorder to C,A,B and assert GET returns C,A,B (by item id AND title id). A repeat
// of the same order is idempotent. Acceptance: reorder reflected in GET.
func TestPlaylistReorder(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	a := findTitle(t, list, "Dune")
	b := findTitle(t, list, "Blade Runner")
	c := findTitle(t, list, "Sample Movie")

	plID := createPlaylist(t, srv, admin, "Reorder Me")
	items := appendAndCaptureItemIDs(t, srv, admin, plID, a, b, c) // item ids for A,B,C
	itemA, itemB, itemC := items[0], items[1], items[2]

	// Sanity: initial order is the append order A,B,C.
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{a, b, c}) {
		t.Fatalf("initial order = %v, want [A,B,C]", got)
	}

	// Reorder to C,A,B.
	if st, env := reorderPlaylist(t, srv, admin, plID, []string{itemC, itemA, itemB}); st != http.StatusNoContent {
		t.Fatalf("reorder = %d/%s, want 204", st, env.Error.Code)
	}
	detail := getPlaylistDetail(t, srv, admin, plID)
	if got := playlistMemberItemIDs(detail); !reflect.DeepEqual(got, []string{itemC, itemA, itemB}) {
		t.Errorf("item-id order after reorder = %v, want [C,A,B]", got)
	}
	if got := playlistMemberIDs(detail); !reflect.DeepEqual(got, []string{c, a, b}) {
		t.Errorf("title order after reorder = %v, want [C,A,B]", got)
	}

	// Idempotent: the same desired order again → same result.
	if st, _ := reorderPlaylist(t, srv, admin, plID, []string{itemC, itemA, itemB}); st != http.StatusNoContent {
		t.Fatalf("idempotent reorder = %d, want 204", st)
	}
	if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{itemC, itemA, itemB}) {
		t.Errorf("order after idempotent reorder = %v, want [C,A,B]", got)
	}
}

// TestPlaylistReorderRejection: a payload that omits an id, adds an unknown id, or
// duplicates an id is a 422 ITEM_SET_MISMATCH no-op — the existing order is left
// UNCHANGED on a subsequent GET. Acceptance: non-matching itemIds rejected, order
// intact.
func TestPlaylistReorderRejection(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	a := findTitle(t, list, "Dune")
	b := findTitle(t, list, "Blade Runner")
	c := findTitle(t, list, "Sample Movie")

	plID := createPlaylist(t, srv, admin, "No Partial")
	items := appendAndCaptureItemIDs(t, srv, admin, plID, a, b, c)
	itemA, itemB, itemC := items[0], items[1], items[2]
	const unknown = "00000000-0000-0000-0000-000000000000"

	bad := map[string][]string{
		"omits an id":      {itemC, itemA},          // missing itemB (count mismatch)
		"adds unknown id":  {itemC, itemA, unknown}, // foreign id, right count
		"duplicates an id": {itemA, itemA, itemB},   // itemA repeated, itemC absent
	}
	for name, payload := range bad {
		st, env := reorderPlaylist(t, srv, admin, plID, payload)
		if st != http.StatusUnprocessableEntity || env.Error.Code != "ITEM_SET_MISMATCH" {
			t.Errorf("reorder that %s = %d/%s, want 422 ITEM_SET_MISMATCH", name, st, env.Error.Code)
		}
		// Order is UNCHANGED (still the append order A,B,C by item id).
		if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{itemA, itemB, itemC}) {
			t.Errorf("order after rejected reorder (%s) = %v, want unchanged [A,B,C]", name, got)
		}
	}
}

// TestPlaylistRemoveByItemID: DELETE /playlists/{id}/items/{itemId} removes exactly
// that entry by item id. With a DUPLICATE Title (A,B,A) removing one A leaves the
// OTHER A and B, relative order preserved; the surviving A is the right one (asserted
// by item id). A second delete of the same (now-gone) item id → 404; an unknown item
// id → 404. Acceptance: remove-by-item-id, duplicate's other entry untouched, order
// preserved.
func TestPlaylistRemoveByItemID(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	a := findTitle(t, list, "Dune") // appears twice (duplicate)
	b := findTitle(t, list, "Blade Runner")

	plID := createPlaylist(t, srv, admin, "Has Duplicate")
	items := appendAndCaptureItemIDs(t, srv, admin, plID, a, b, a) // A1, B, A2
	a1, bItem, a2 := items[0], items[1], items[2]

	// Remove the FIRST A (a1). The other A (a2) and B survive, in order B, A2.
	if st := removePlaylistItem(t, srv, admin, plID, a1); st != http.StatusNoContent {
		t.Fatalf("remove a1 = %d, want 204", st)
	}
	detail := getPlaylistDetail(t, srv, admin, plID)
	if got := playlistMemberItemIDs(detail); !reflect.DeepEqual(got, []string{bItem, a2}) {
		t.Errorf("item ids after removing a1 = %v, want [B, A2]", got)
	}
	if got := playlistMemberIDs(detail); !reflect.DeepEqual(got, []string{b, a}) {
		t.Errorf("titles after removing a1 = %v, want [B, A]", got)
	}
	// The surviving duplicate is specifically a2 (not a1).
	if containsID(playlistMemberItemIDs(detail), a1) {
		t.Errorf("removed item %s still present, want gone", a1)
	}

	// Removing the now-gone item id again → 404 (hide-existence).
	if st := removePlaylistItem(t, srv, admin, plID, a1); st != http.StatusNotFound {
		t.Errorf("re-remove a1 = %d, want 404", st)
	}
	// An unknown item id → 404.
	if st := removePlaylistItem(t, srv, admin, plID, "00000000-0000-0000-0000-000000000000"); st != http.StatusNotFound {
		t.Errorf("remove unknown item = %d, want 404", st)
	}
}

// TestPlaylistAccessSurvival: the resolved view applies the OWNER's current access
// Scope. A Member owner granted a Library appends two Titles (PG-13 + R); with no
// ceiling GET shows both; a PG-13 ceiling omits the R Title (others keep order);
// clearing the ceiling makes the R Title REAPPEAR in its original position — the
// item rows persist throughout. Acceptance: lost-access member omitted then reappears.
func TestPlaylistAccessSurvival(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, list := scanMovies(t)
	pg := findTitle(t, list, "Dune")        // → PG-13
	r := findTitle(t, list, "Blade Runner") // → R (above a PG-13 ceiling)
	srv.SetTitleContentRating(pg, "PG-13")
	srv.SetTitleContentRating(r, "R")

	ownerID := srv.CreateUser(admin, "viewer", "viewerpass12", "member")
	owner := srv.LoginAs("viewer", "viewerpass12")
	grantLibraries(t, srv, admin, ownerID, lib)

	plID := createPlaylist(t, srv, owner, "Survives Access")
	// Append PG-13 then R; both visible with no ceiling.
	if st, _ := appendPlaylistItem(t, srv, owner, plID, pg); st != http.StatusNoContent {
		t.Fatal("append pg failed")
	}
	if st, _ := appendPlaylistItem(t, srv, owner, plID, r); st != http.StatusNoContent {
		t.Fatal("append r failed")
	}
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{pg, r}) {
		t.Fatalf("no-ceiling order = %v, want [PG, R]", got)
	}

	// PG-13 ceiling: the R Title drops out of the VIEW; the PG-13 Title remains.
	setRatingCeiling(t, srv, admin, ownerID, "PG-13")
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{pg}) {
		t.Errorf("capped order = %v, want [PG] only (R omitted)", got)
	}

	// Clearing the ceiling: the R Title REAPPEARS in its original position (after PG).
	setRatingCeiling(t, srv, admin, ownerID, "")
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{pg, r}) {
		t.Errorf("uncapped order = %v, want [PG, R] (R reappears in place)", got)
	}
}

// TestPlaylistMissingSurvival: a member whose Files go Missing (hidden=1) is omitted
// from the resolved view; restoring the Files (hidden=0) makes it reappear in its
// original position — no re-add. The playlist_items row persists throughout (ADR-0008).
// Acceptance: Missing member omitted then reappears.
func TestPlaylistMissingSurvival(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	a := findTitle(t, list, "Dune")
	b := findTitle(t, list, "Blade Runner")
	c := findTitle(t, list, "Sample Movie")

	plID := createPlaylist(t, srv, admin, "Survives Missing")
	appendAndCaptureItemIDs(t, srv, admin, plID, a, b, c)
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{a, b, c}) {
		t.Fatalf("initial order = %v, want [A,B,C]", got)
	}

	// B goes Missing → omitted from the view; A and C keep their order.
	srv.SetTitleHidden(b, true)
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{a, c}) {
		t.Errorf("with B Missing = %v, want [A,C] (B omitted)", got)
	}

	// B's Files return → it reappears in its ORIGINAL position (between A and C), no re-add.
	srv.SetTitleHidden(b, false)
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{a, b, c}) {
		t.Errorf("after B restored = %v, want [A,B,C] (B reappears in place)", got)
	}
}

// TestPlaylistReorderWithMissingMember: a Playlist holding a Missing member is still
// reorderable. The payload is the order of the VISIBLE items — the only ids a client
// can name, since the Missing member is omitted from the view — and the Missing item
// keeps its INDEX in the sequence rather than counting against the payload.
//
// This is a regression test. Reorder used to validate the payload against every item
// ROW, so a client that could only ever see (and therefore only ever send) the visible
// ids could never match the count: one Missing file froze a Playlist's order forever,
// with no client-side recourse. Acceptance: reorder reachable with a Missing member;
// the Missing member neither moves nor leaks.
func TestPlaylistReorderWithMissingMember(t *testing.T) {
	requireFixtures(t)
	srv, admin, _, list := scanMovies(t)
	a := findTitle(t, list, "Dune")
	b := findTitle(t, list, "Blade Runner")
	c := findTitle(t, list, "Sample Movie")

	plID := createPlaylist(t, srv, admin, "Missing But Sortable")
	items := appendAndCaptureItemIDs(t, srv, admin, plID, a, b, c) // A@1, B@2, C@3
	itemA, itemB, itemC := items[0], items[1], items[2]

	// B goes Missing: the client now sees only [A, C].
	srv.SetTitleHidden(b, true)
	if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{itemA, itemC}) {
		t.Fatalf("visible items with B Missing = %v, want [A,C]", got)
	}

	// Reordering the two visible items to C,A succeeds — this is the whole bug.
	if st, env := reorderPlaylist(t, srv, admin, plID, []string{itemC, itemA}); st != http.StatusNoContent {
		t.Fatalf("reorder of visible items = %d/%s, want 204 (a Missing member must not freeze the order)", st, env.Error.Code)
	}
	if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{itemC, itemA}) {
		t.Errorf("visible order after reorder = %v, want [C,A]", got)
	}

	// Naming the Missing item id is still a mismatch: you cannot reorder by an id you
	// were never shown, so the hidden row stays unnameable rather than half-addressable.
	if st, env := reorderPlaylist(t, srv, admin, plID, []string{itemC, itemB, itemA}); st != http.StatusUnprocessableEntity || env.Error.Code != "ITEM_SET_MISMATCH" {
		t.Errorf("reorder naming the Missing item = %d/%s, want 422 ITEM_SET_MISMATCH", st, env.Error.Code)
	}

	// B's Files return: it reappears at the INDEX it held (second), between the
	// reordered C and A. It was never moved by a reorder it took no part in.
	srv.SetTitleHidden(b, false)
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID)); !reflect.DeepEqual(got, []string{c, b, a}) {
		t.Errorf("after B restored = %v, want [C,B,A] (B pinned at its index through the reorder)", got)
	}
}

// TestPlaylistReorderWithOutOfScopeMember: the same reachability, for the other way a
// member leaves the view. An owner whose rating ceiling hides a member reorders the
// rest by the ids they can see; the out-of-scope member holds its index and reappears
// there when the ceiling lifts. Acceptance: reorder reachable under a narrowed Scope.
func TestPlaylistReorderWithOutOfScopeMember(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, list := scanMovies(t)
	pg := findTitle(t, list, "Dune")        // → PG-13
	r := findTitle(t, list, "Blade Runner") // → R (above a PG-13 ceiling)
	other := findTitle(t, list, "Sample Movie")
	srv.SetTitleContentRating(pg, "PG-13")
	srv.SetTitleContentRating(r, "R")

	ownerID := srv.CreateUser(admin, "sorter", "sorterpass12", "member")
	owner := srv.LoginAs("sorter", "sorterpass12")
	grantLibraries(t, srv, admin, ownerID, lib)

	plID := createPlaylist(t, srv, owner, "Capped But Sortable")
	items := appendAndCaptureItemIDs(t, srv, owner, plID, pg, r, other) // PG@1, R@2, other@3
	itemPG, itemOther := items[0], items[2]

	// A PG-13 ceiling hides R; the owner sees [PG, other].
	setRatingCeiling(t, srv, admin, ownerID, "PG-13")
	if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{itemPG, itemOther}) {
		t.Fatalf("visible items under ceiling = %v, want [PG, other]", got)
	}

	// The owner reorders what they can see.
	if st, env := reorderPlaylist(t, srv, owner, plID, []string{itemOther, itemPG}); st != http.StatusNoContent {
		t.Fatalf("reorder under ceiling = %d/%s, want 204", st, env.Error.Code)
	}

	// Lifting the ceiling: R is back at its index (second), untouched by the reorder.
	setRatingCeiling(t, srv, admin, ownerID, "")
	if got := playlistMemberIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{other, r, pg}) {
		t.Errorf("after ceiling lifted = %v, want [other, R, PG] (R pinned at its index)", got)
	}
}

// TestPlaylistOrderingOwnership404: the ordering ops are owner-only. A non-owner — a
// second Member AND an Admin (no override) — gets 404 on PUT /playlists/{id}/items
// (reorder) and DELETE /playlists/{id}/items/{itemId} (remove) of another User's
// Playlist, and the owner's order is untouched. Acceptance: non-owner 404 on reorder
// and remove.
func TestPlaylistOrderingOwnership404(t *testing.T) {
	requireFixtures(t)
	srv, admin, lib, list := scanMovies(t)
	a := findTitle(t, list, "Dune")
	b := findTitle(t, list, "Blade Runner")

	ownerID := srv.CreateUser(admin, "plowner", "plownerpass1", "member")
	srv.CreateUser(admin, "plintruder", "plintruderp1", "member")
	owner := srv.LoginAs("plowner", "plownerpass1")
	intruder := srv.LoginAs("plintruder", "plintruderp1")
	grantLibraries(t, srv, admin, ownerID, lib)

	plID := createPlaylist(t, srv, owner, "Private Order")
	items := appendAndCaptureItemIDs(t, srv, owner, plID, a, b)
	itemA, itemB := items[0], items[1]

	for _, caller := range []struct{ name, token string }{
		{"member", intruder},
		{"admin", admin},
	} {
		// Reorder of another User's Playlist → 404 (hide-existence).
		if st, _ := reorderPlaylist(t, srv, caller.token, plID, []string{itemB, itemA}); st != http.StatusNotFound {
			t.Errorf("%s reorder = %d, want 404", caller.name, st)
		}
		// Remove of another User's item → 404.
		if st := removePlaylistItem(t, srv, caller.token, plID, itemA); st != http.StatusNotFound {
			t.Errorf("%s remove = %d, want 404", caller.name, st)
		}
	}

	// The owner's order is untouched by the rejected intruder ops.
	if got := playlistMemberItemIDs(getPlaylistDetail(t, srv, owner, plID)); !reflect.DeepEqual(got, []string{itemA, itemB}) {
		t.Errorf("owner order after intruder attempts = %v, want unchanged [A,B]", got)
	}
}
