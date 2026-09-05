package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the `remote` role over HTTP (.scratch/linked-servers issue
// 01, ADR-0054): a linked Server is a User, managed on the Admin Users surface
// like any other, and is the ONE User that has no password, cannot log in, and
// writes no watch state.

// createRemoteUser mints a `remote` User through the real Admin API with no
// password, asserting the 201. The username is the label the sharing Admin chose.
func createRemoteUser(t *testing.T, srv *testharness.Server, adminTok, label string) string {
	t.Helper()
	var out struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/users", adminTok,
		map[string]any{"username": label, "role": "remote"}, &out)
	if status != http.StatusCreated {
		t.Fatalf("create remote user: status %d, want 201; body: %s", status, body)
	}
	if out.Role != "remote" {
		t.Fatalf("created role = %q, want remote; body: %s", out.Role, body)
	}
	return out.ID
}

// TestRemoteUserLifecycleOverTheAdminAPI: create, list, grant, and delete — the
// four things the sharing Admin does to a linked Server, all through the same
// /users surface a Member is managed on.
func TestRemoteUserLifecycleOverTheAdminAPI(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))

	id := createRemoteUser(t, srv, admin, "Brandon's server")

	// Listed on the Admin Users page — that page is where the operator manages
	// what the other household may reach, so the role must be visible there.
	var list struct {
		Users []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"users"`
	}
	if status, body := srv.AuthGET("/api/v1/users", admin, &list); status != http.StatusOK {
		t.Fatalf("list users: status %d; body: %s", status, body)
	}
	found := false
	for _, u := range list.Users {
		if u.ID == id {
			found = true
			if u.Role != "remote" {
				t.Errorf("listed role = %q, want remote", u.Role)
			}
		}
	}
	if !found {
		t.Error("the remote User is absent from GET /users")
	}

	// Granting works exactly as it does for a Member: the replace-set, the same
	// 204, and the grant reads back on the detail.
	grantLibraries(t, srv, admin, id, lib)
	var detail struct {
		LibraryIDs []string `json:"libraryIds"`
	}
	if status, body := srv.AuthGET("/api/v1/users/"+id, admin, &detail); status != http.StatusOK {
		t.Fatalf("get remote user: status %d; body: %s", status, body)
	}
	if len(detail.LibraryIDs) != 1 || detail.LibraryIDs[0] != lib {
		t.Errorf("libraryIds = %v, want [%s]", detail.LibraryIDs, lib)
	}

	// Deleting is the sharer's kill switch.
	if status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+id, admin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete remote user: status %d, want 204; body: %s", status, body)
	}
	if status, _ := srv.AuthGET("/api/v1/users/"+id, admin, nil); status != http.StatusNotFound {
		t.Errorf("get deleted remote user: status %d, want 404", status)
	}
}

// TestCreateRemoteUserRefusesAPassword: the role takes none, and a supplied one
// is a 400 rather than a field quietly stored and never used.
func TestCreateRemoteUserRefusesAPassword(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	status, body := srv.JSON(http.MethodPost, "/api/v1/users", admin,
		map[string]any{"username": "peer", "password": "a-password", "role": "remote"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("create remote user with a password: status %d, want 400; body: %s", status, body)
	}
}

// TestRemoteUserCannotLogInOverHTTP: no password signs it in, and the refusal is
// the same generic 401 an unknown username gets — the role must not be readable
// off the answer.
func TestRemoteUserCannotLogInOverHTTP(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	createRemoteUser(t, srv, admin, "peer-server")

	login := func(username, password string) (int, []byte) {
		return srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
			"username": username,
			"password": password,
			"device": map[string]any{
				"name": "peer", "platform": "server", "clientId": "peer-client",
			},
		}, nil)
	}

	for _, password := range []string{"", "guess", "hunter2"} {
		if status, body := login("peer-server", password); status != http.StatusUnauthorized {
			t.Errorf("login as the remote User with %q: status %d, want 401; body: %s", password, status, body)
		}
	}
	if status, _ := login("nobody-at-all", "guess"); status != http.StatusUnauthorized {
		t.Errorf("login as an unknown username: status %d, want the same 401", status)
	}
}

// TestRemoteUserResolvesAScopeLikeAMember: the whole reason a linked Server is a
// User (ADR-0054). No new enforcement code runs for it — an ungranted Library is
// hidden as 404 exactly as it is for a person, and the granted one opens up with
// the same titles a Member with the same grant sees.
func TestRemoteUserResolvesAScopeLikeAMember(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	remoteID := createRemoteUser(t, srv, admin, "Brandon's server")
	peer := srv.IssueTokenForUser(remoteID, "peer-server-id")

	// Empty grant set = sees no catalog, hidden as 404 (never 403).
	if status, _ := srv.AuthGET("/api/v1/libraries/"+lib+"/titles", peer, nil); status != http.StatusNotFound {
		t.Fatalf("ungranted library for a remote User: status %d, want 404", status)
	}

	grantLibraries(t, srv, admin, remoteID, lib)
	var peerList titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles", peer, &peerList); status != http.StatusOK {
		t.Fatalf("granted library for a remote User: status %d; body: %s", status, body)
	}

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	member := srv.LoginAs("kid", "memberpass123")
	var memberList titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles", member, &memberList); status != http.StatusOK {
		t.Fatalf("granted library for a member: status %d; body: %s", status, body)
	}
	if len(peerList.Titles) == 0 || len(peerList.Titles) != len(memberList.Titles) {
		t.Errorf("remote User saw %d titles, member saw %d; want the same non-empty set",
			len(peerList.Titles), len(memberList.Titles))
	}
}

// TestARelayPlayLeavesWatchStateUntouched is the ADR-0054 promise, end to end: a
// linked Server plays a Title — negotiating a session, reporting progress past
// the watched ceiling, and even asking to mark it watched — and this Server
// stores NOTHING per-Title for it. The watch state belongs to the person
// watching, who is on the other Server.
//
// The session itself is deliberately real: it is created, it streams, and it is
// ended. That half is what the sharer's own transcode observability runs on, so
// the guard has to be narrower than "the remote User does not play".
func TestARelayPlayLeavesWatchStateUntouched(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")
	var list titlesListResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles", admin, &list); status != http.StatusOK {
		t.Fatalf("list titles: status %d; body: %s", status, body)
	}
	duneID := findTitle(t, list, "Dune")

	remoteID := createRemoteUser(t, srv, admin, "Brandon's server")
	grantLibraries(t, srv, admin, remoteID, lib)
	// Stands in for redeeming an Invite (ADR-0055, a later slice): the ordinary
	// Device-bound bearer that flow produces.
	peer := srv.IssueTokenForUser(remoteID, "peer-server-id")

	// It resolves a Scope identical to a Member's with the same grants: the Title
	// is visible and negotiable.
	dur := titleDuration(t, srv, peer, duneID)
	dec := negotiateDune(t, srv, peer, duneID)
	if dec.SessionID == "" {
		t.Fatal("the relay play produced no session; a remote User still plays")
	}

	// Past the 90% ceiling — the single most write-happy progress report there is.
	out := postProgress(t, srv, peer, dec.SessionID, dur-1, http.StatusOK)
	if out.Watched {
		t.Errorf("progress reported watched = true for a remote User, want false (nothing is stored)")
	}
	if out.ResumePositionMs != 0 {
		t.Errorf("progress reported resume = %d for a remote User, want 0", out.ResumePositionMs)
	}

	// And the manual toggle is accepted and dropped rather than failing on a
	// surface the role does not have.
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+duneID+"/watchState", peer,
		map[string]any{"watched": true}, nil); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("mark watched as a remote User: status %d; body: %s", status, body)
	}

	ws, audio, video := srv.CountWatchRowsForUser(remoteID)
	if ws != 0 || audio != 0 || video != 0 {
		t.Errorf("remote User wrote watch rows: watch_state=%d audioMemory=%d videoMemory=%d, want 0/0/0",
			ws, audio, video)
	}

	// The control: the same play under a person writes the row it always did, so
	// the guard is about the ROLE and not about something that stopped working.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	member := srv.LoginAs("kid", "memberpass123")
	memberDec := negotiateDune(t, srv, member, duneID)
	if got := postProgress(t, srv, member, memberDec.SessionID, dur-1, http.StatusOK); !got.Watched {
		t.Error("the same progress report under a member did not mark watched")
	}
	if ws, _, _ := srv.CountWatchRowsForUser(memberID); ws == 0 {
		t.Error("the member wrote no watch_state row; the guard is too wide")
	}
}
