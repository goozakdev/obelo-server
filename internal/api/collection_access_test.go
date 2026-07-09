package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box HTTP tests for collections-playlists issue 02: per-viewer Collection
// access-filtering. A shared, Admin-curated Collection must never leak a restricted
// Title — not as a member, not as a count, not as a representative poster, and not
// even by its existence (a Collection with zero visible members is hidden from a
// non-Admin: absent from GET /collections, 404 on GET /collections/{id}). An Admin
// (all Libraries, no ceiling) always sees full membership and every Collection.
//
// These reuse the access-control harness helpers (grantLibraries, setRatingCeiling,
// SetTitleContentRating) and the issue-01 collection helpers (createCollection,
// addCollectionItems, getCollectionDetail, memberIDs, containsID).

// listCollectionIDs returns the ids present in a viewer's GET /collections, and
// fails on a non-200.
func listCollectionIDs(t *testing.T, srv *testharness.Server, token string) ([]string, collectionsListResp) {
	t.Helper()
	var ls collectionsListResp
	if st, body := srv.AuthGET("/api/v1/collections", token, &ls); st != http.StatusOK {
		t.Fatalf("list collections: status %d; body: %s", st, body)
	}
	ids := make([]string, 0, len(ls.Collections))
	for _, c := range ls.Collections {
		ids = append(ids, c.ID)
	}
	return ids, ls
}

// cardFor returns the card for colID in a list, or a zero card + false.
func cardFor(ls collectionsListResp, colID string) (collectionCardResp, bool) {
	for _, c := range ls.Collections {
		if c.ID == colID {
			return c, true
		}
	}
	return collectionCardResp{}, false
}

// TestCollectionAccessFiltering is the headline correctness test. A Collection
// mixes a granted-Library in-ceiling Title, a granted-Library above-ceiling Title,
// and an ungranted-Library Title. The Member sees ONLY the first; the Admin sees
// all three. Covers acceptance criteria 1, 2, 3, and 6.
func TestCollectionAccessFiltering(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	// Two Movie Libraries from two distinct roots (so each yields its own Title ids):
	// libA is granted to the Member, libB is not.
	libA := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	libB := createMovieLibrary(t, srv, admin, testharness.MutableLibraryDir(t, fixtureRoot(t)))
	scanLib(t, srv, admin, libA, "")
	scanLib(t, srv, admin, libB, "")

	var aList, bList titlesListResp
	srv.AuthGET("/api/v1/libraries/"+libA+"/titles?limit=50", admin, &aList)
	srv.AuthGET("/api/v1/libraries/"+libB+"/titles?limit=50", admin, &bList)

	duneA := findTitle(t, aList, "Dune")          // granted lib, at-ceiling → visible to Member
	bladeA := findTitle(t, aList, "Blade Runner") // granted lib, above-ceiling → hidden from Member
	duneB := findTitle(t, bList, "Dune")          // ungranted lib → hidden from Member

	// duneA at the PG-13 ceiling stays visible; bladeA is R (above PG-13) → hidden.
	srv.SetTitleContentRating(duneA, "PG-13")
	srv.SetTitleContentRating(bladeA, "R")

	// Member: granted libA only, ceiling PG-13.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	grantLibraries(t, srv, admin, memberID, libA)
	setRatingCeiling(t, srv, admin, memberID, "PG-13")

	colID := createCollection(t, srv, admin, "Mixed", "")
	addCollectionItems(t, srv, admin, colID, duneA, bladeA, duneB)

	// Admin detail: full membership (all three).
	adminDetail := getCollectionDetail(t, srv, admin, colID)
	if adminDetail.MemberCount != 3 {
		t.Fatalf("admin memberCount = %d, want 3 (full membership)", adminDetail.MemberCount)
	}
	for _, id := range []string{duneA, bladeA, duneB} {
		if !containsID(memberIDs(adminDetail), id) {
			t.Errorf("admin detail missing member %s; got %v", id, memberIDs(adminDetail))
		}
	}

	// Member detail: ONLY duneA — bladeA (above ceiling) and duneB (ungranted lib)
	// are filtered in SQL and never transit the boundary.
	memberDetail := getCollectionDetail(t, srv, member, colID)
	if got := memberIDs(memberDetail); len(got) != 1 || got[0] != duneA {
		t.Fatalf("member detail members = %v, want only %s (duneA)", got, duneA)
	}
	if memberDetail.MemberCount != 1 {
		t.Errorf("member memberCount = %d, want 1", memberDetail.MemberCount)
	}

	// GET /collections cards are per-viewer: Admin count 3, Member count 1
	// (Member count < Admin count), and the representative poster points at the
	// Member's only visible member, not at a restricted one.
	_, adminList := listCollectionIDs(t, srv, admin)
	adminCard, ok := cardFor(adminList, colID)
	if !ok || adminCard.MemberCount != 3 {
		t.Fatalf("admin card = %+v (ok=%v), want memberCount 3", adminCard, ok)
	}

	memberIDsList, memberList := listCollectionIDs(t, srv, member)
	memberCard, ok := cardFor(memberList, colID)
	if !ok {
		t.Fatalf("Mixed absent from member list %v, want present (has 1 visible member)", memberIDsList)
	}
	if memberCard.MemberCount != 1 {
		t.Errorf("member card memberCount = %d, want 1", memberCard.MemberCount)
	}
	if memberCard.MemberCount >= adminCard.MemberCount {
		t.Errorf("member count %d not < admin count %d", memberCard.MemberCount, adminCard.MemberCount)
	}
	wantPoster := "/api/v1/titles/" + duneA + "/artwork/poster"
	if memberCard.PosterURL != wantPoster {
		t.Errorf("member card posterUrl = %q, want %q (duneA, the only visible member)", memberCard.PosterURL, wantPoster)
	}

	// No restricted member id (bladeA, duneB) appears in ANY field of the Member's
	// list OR detail responses — verified by absence over HTTP (criterion 6's
	// no-leak guarantee, including the poster URL which embeds a Title id).
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal for leak check: %v", err)
		}
		return string(b)
	}
	assertAbsent := func(where, blob string) {
		for _, restricted := range []string{bladeA, duneB} {
			if strings.Contains(blob, restricted) {
				t.Errorf("%s leaks restricted title id %s: %s", where, restricted, blob)
			}
		}
	}
	assertAbsent("member detail", marshal(memberDetail))
	assertAbsent("member list", marshal(memberList))

	// Live, not cached: lower the Member's ceiling to G → duneA (PG-13) now drops,
	// leaving the Member zero visible members → the Collection becomes hidden on the
	// NEXT request (absent from list, 404 on detail). Proves the filter is evaluated
	// per-request, not memoized.
	setRatingCeiling(t, srv, admin, memberID, "G")
	if st, _ := srv.AuthGET("/api/v1/collections/"+colID, member, nil); st != http.StatusNotFound {
		t.Errorf("after lowering ceiling, member detail = %d, want 404 (zero visible → hidden)", st)
	}
	afterIDs, _ := listCollectionIDs(t, srv, member)
	if containsID(afterIDs, colID) {
		t.Errorf("after lowering ceiling, Mixed still in member list %v, want absent", afterIDs)
	}
	// The Admin is unaffected — still sees full membership.
	if c := getCollectionDetail(t, srv, admin, colID).MemberCount; c != 3 {
		t.Errorf("admin memberCount after member ceiling change = %d, want 3 (unaffected)", c)
	}
}

// TestCollectionZeroVisibleHiddenFromMember: a Collection whose every member is
// restricted for a Member is ABSENT from that Member's GET /collections and 404s on
// detail (hide-existence), while the Admin sees it in full. Covers acceptance
// criterion 4. Also exercises revoking a grant as the live-filter trigger.
func TestCollectionZeroVisibleHiddenFromMember(t *testing.T) {
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
	bladeB := findTitle(t, bList, "Blade Runner")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	grantLibraries(t, srv, admin, memberID, libA) // libB ungranted

	// "Restricted": only ungranted-Library members → zero visible for the Member.
	restricted := createCollection(t, srv, admin, "Restricted", "")
	addCollectionItems(t, srv, admin, restricted, duneB, bladeB)

	// "Visible": a granted-Library member → the Member can see this one.
	visible := createCollection(t, srv, admin, "Visible", "")
	addCollectionItems(t, srv, admin, visible, duneA)

	// Member list: only "Visible" appears; "Restricted" is hidden (no count leaks).
	memberIDsList, _ := listCollectionIDs(t, srv, member)
	if containsID(memberIDsList, restricted) {
		t.Errorf("member list = %v, want Restricted absent (zero visible members)", memberIDsList)
	}
	if !containsID(memberIDsList, visible) {
		t.Errorf("member list = %v, want Visible present", memberIDsList)
	}
	// Member detail on the fully-restricted Collection → 404 (hide-existence).
	if st, _ := srv.AuthGET("/api/v1/collections/"+restricted, member, nil); st != http.StatusNotFound {
		t.Errorf("member detail on fully-restricted collection = %d, want 404", st)
	}

	// The Admin sees the Restricted Collection in full (list + detail).
	adminIDs, _ := listCollectionIDs(t, srv, admin)
	if !containsID(adminIDs, restricted) {
		t.Errorf("admin list = %v, want Restricted present", adminIDs)
	}
	if c := getCollectionDetail(t, srv, admin, restricted).MemberCount; c != 2 {
		t.Errorf("admin Restricted memberCount = %d, want 2", c)
	}

	// Live filter via grant revocation: revoke libA → the Member now sees zero
	// members of "Visible" too, so it disappears on the next request.
	grantLibraries(t, srv, admin, memberID) // empty grant set = revoke all
	afterIDs, _ := listCollectionIDs(t, srv, member)
	if containsID(afterIDs, visible) {
		t.Errorf("after revoke, member list = %v, want Visible absent", afterIDs)
	}
	if st, _ := srv.AuthGET("/api/v1/collections/"+visible, member, nil); st != http.StatusNotFound {
		t.Errorf("after revoke, member detail on Visible = %d, want 404", st)
	}
}

// TestCollectionAdminSeesEmptyCollection: an Admin sees a genuinely empty
// Collection (200, zero members) — the zero-visible hiding rule must NOT break
// Admin management of a fresh/empty Collection. Covers acceptance: Admin exemption.
func TestCollectionAdminSeesEmptyCollection(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	empty := createCollection(t, srv, admin, "Empty", "no members yet")

	// Detail: 200 with zero members (NOT a 404).
	detail := getCollectionDetail(t, srv, admin, empty)
	if detail.MemberCount != 0 || len(detail.Members) != 0 {
		t.Errorf("admin empty detail = count %d / %d members, want 0/0", detail.MemberCount, len(detail.Members))
	}
	// List: the empty Collection appears for the Admin (Admin sees every Collection).
	adminIDs, adminList := listCollectionIDs(t, srv, admin)
	if !containsID(adminIDs, empty) {
		t.Fatalf("admin list = %v, want Empty present", adminIDs)
	}
	card, _ := cardFor(adminList, empty)
	if card.MemberCount != 0 || card.PosterURL != "" {
		t.Errorf("admin empty card = %+v, want memberCount 0 and no poster", card)
	}

	// A Member, by contrast, does NOT see the empty Collection (zero visible → hidden).
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	_ = memberID
	memberIDsList, _ := listCollectionIDs(t, srv, member)
	if containsID(memberIDsList, empty) {
		t.Errorf("member list = %v, want Empty absent (zero visible → hidden)", memberIDsList)
	}
	if st, _ := srv.AuthGET("/api/v1/collections/"+empty, member, nil); st != http.StatusNotFound {
		t.Errorf("member detail on empty collection = %d, want 404", st)
	}
}
