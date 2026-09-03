package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for linked-servers issue 11: a Library that arrived over a
// Link is never re-shared onward (ADR-0054 §4, ADR-0056 §7). Three surfaces have
// to agree — the grant write, the export, and the resolved Scope — and the
// fourth guard is that none of this touches a Member, for whom a mirror is an
// ordinary Library (ADR-0056 §2).
//
// The mirror here is made by hand rather than by linking two real Servers (that
// fixture lives in linked_library_test.go): these tests are about the ROLE and
// the LIBRARY SOURCE, and a Library whose `source` is `linked` is exactly what
// every one of the three surfaces reads.

// mirrorLibrary creates an ordinary Library and turns it into a mirror of
// somebody else's, the way a Link would have (`source = linked`). No `links` row
// is needed: nothing under test joins one, and a mirror whose Link is gone must
// behave the same way.
func mirrorLibrary(t *testing.T, srv *testharness.Server, adminTok, name string) string {
	t.Helper()
	id := createLibraryNamed(t, srv, adminTok, name, t.TempDir())
	srv.Exec(`UPDATE libraries SET source = 'linked', remote_library_id = ? WHERE id = ?`,
		"remote-"+id, id)
	return id
}

// grantedLibraryIDs reads a User's grant set back off the Admin detail view.
func grantedLibraryIDs(t *testing.T, srv *testharness.Server, adminTok, userID string) []string {
	t.Helper()
	var ud userDetailResp
	if st, body := srv.AuthGET("/api/v1/users/"+userID, adminTok, &ud); st != http.StatusOK {
		t.Fatalf("GET user %s = %d; body: %s", userID, st, body)
	}
	return ud.LibraryIDs
}

// TestGrantingALinkedLibraryToARemoteUserIsRefused: the replace-set refuses the
// WHOLE set with 422 LINKED_GRANT and leaves the prior set standing — the same
// shape the unknown-id rejection has. A Member is granted the same Library in
// the same breath, because it is the second hop that is refused, not the first.
func TestGrantingALinkedLibraryToARemoteUserIsRefused(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	local := createLibraryNamed(t, srv, admin, "Films", t.TempDir())
	mirror := mirrorLibrary(t, srv, admin, "Their films")

	peerID := createRemoteUser(t, srv, admin, "Brandon's server")
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")

	// The prior set: one local Library, granted the ordinary way.
	grantLibraries(t, srv, admin, peerID, local)

	// The mirror, alongside a perfectly good local Library → the whole set is
	// refused.
	var env errorEnvelope
	st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+peerID+"/libraryAccess", admin,
		map[string]any{"libraryIds": []string{local, mirror}}, &env)
	if st != http.StatusUnprocessableEntity || env.Error.Code != "LINKED_GRANT" {
		t.Fatalf("granting a mirror to a linked server = %d/%s, want 422 LINKED_GRANT; body: %s",
			st, env.Error.Code, body)
	}
	if got := grantedLibraryIDs(t, srv, admin, peerID); len(got) != 1 || got[0] != local {
		t.Errorf("after the refusal, grants = %v, want just the prior [%s]", got, local)
	}

	// The mirror alone is refused too — it is not the company it keeps.
	env = errorEnvelope{}
	if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+peerID+"/libraryAccess", admin,
		map[string]any{"libraryIds": []string{mirror}}, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "LINKED_GRANT" {
		t.Errorf("granting only a mirror = %d/%s, want 422 LINKED_GRANT; body: %s",
			st, env.Error.Code, body)
	}

	// A set of local Libraries still applies cleanly (the guard is not a blanket
	// refusal of the role).
	grantLibraries(t, srv, admin, peerID, local)
	if got := grantedLibraryIDs(t, srv, admin, peerID); len(got) != 1 || got[0] != local {
		t.Errorf("remote grants = %v, want [%s]", got, local)
	}

	// The regression: a Member IS granted the mirror, as any Library.
	grantLibraries(t, srv, admin, memberID, local, mirror)
	got := grantedLibraryIDs(t, srv, admin, memberID)
	if len(got) != 2 || !contains(got, mirror) {
		t.Errorf("member grants = %v, want both %s and the mirror %s", got, local, mirror)
	}
}

// TestARemoteScopeNeverResolvesALinkedLibrary is the Scope-level invariant: the
// filter lives where the Scope is COMPUTED, so a grant row inserted BEHIND the
// API — a restored database, a hand-edit, a Library that became a mirror after
// it was granted — still resolves to nothing for a `remote` User. The same row
// for a Member resolves normally.
func TestARemoteScopeNeverResolvesALinkedLibrary(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	local := createLibraryNamed(t, srv, admin, "Films", t.TempDir())
	mirror := mirrorLibrary(t, srv, admin, "Their films")

	peerID := createRemoteUser(t, srv, admin, "Brandon's server")
	grantLibraries(t, srv, admin, peerID, local)
	peer := srv.IssueTokenForUser(peerID, "peer-server-scope")

	// Straight into the grant table, past every check the API makes.
	srv.Exec(`INSERT OR IGNORE INTO user_library_access (user_id, library_id) VALUES (?, ?)`,
		peerID, mirror)
	if got := grantedLibraryIDs(t, srv, admin, peerID); !contains(got, mirror) {
		t.Fatalf("the smuggled grant row is not there (%v); the test would prove nothing", got)
	}

	var libs librariesListResp
	if st, body := srv.AuthGET("/api/v1/libraries", peer, &libs); st != http.StatusOK {
		t.Fatalf("linked server listing libraries = %d; body: %s", st, body)
	}
	for _, l := range libs.Libraries {
		if l.ID == mirror {
			t.Error("a linked server sees a mirrored Library in its Scope; sharing does not travel")
		}
	}
	if len(libs.Libraries) != 1 || libs.Libraries[0].ID != local {
		t.Errorf("linked server's libraries = %+v, want just the local one", libs.Libraries)
	}
	if st, _ := srv.AuthGET("/api/v1/libraries/"+mirror, peer, nil); st != http.StatusNotFound {
		t.Errorf("linked server reading the mirror directly = %d, want 404", st)
	}
	// The control: the same row, the same reads, for a Member.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, mirror)
	member := srv.LoginAs("kid", "memberpass123")
	var mine librariesListResp
	if st, body := srv.AuthGET("/api/v1/libraries", member, &mine); st != http.StatusOK {
		t.Fatalf("member listing libraries = %d; body: %s", st, body)
	}
	if len(mine.Libraries) != 1 || mine.Libraries[0].ID != mirror {
		t.Errorf("member's libraries = %+v, want the mirror %s", mine.Libraries, mirror)
	}
	if st, _ := srv.AuthGET("/api/v1/libraries/"+mirror, member, nil); st != http.StatusOK {
		t.Errorf("member reading the mirror = %d, want 200", st)
	}
}

// TestTheExportRefusesAMirrorForEveryCaller: `GET /libraries/{id}/export` on a
// mirrored Library is 404 for an ADMIN — the caller that has every Library in
// scope, so the refusal can only be about the Library's source — and equally for
// a linked Server. A local Library exports for both, which is what makes the
// 404 a statement about the mirror rather than about the route.
//
// (linked_library_test.go's TestTheMirrorIsNeverReShared asserts the same thing
// over a real Link; this one adds the local control and needs no second Server.)
func TestTheExportRefusesAMirrorForEveryCaller(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	local := createLibraryNamed(t, srv, admin, "Films", t.TempDir())
	mirror := mirrorLibrary(t, srv, admin, "Their films")
	peer := linkedServerToken(t, srv, admin, local)

	if st, body := srv.AuthGET("/api/v1/libraries/"+mirror+"/export", admin, nil); st != http.StatusNotFound {
		t.Errorf("an Admin exported a mirror = %d, want 404; body: %s", st, body)
	}
	if st, body := srv.AuthGET("/api/v1/libraries/"+mirror+"/export", peer, nil); st != http.StatusNotFound {
		t.Errorf("a linked server exported a mirror = %d, want 404; body: %s", st, body)
	}
	// The controls: the route works for both callers on a Library of this
	// Server's own.
	if st, body := srv.AuthGET("/api/v1/libraries/"+local+"/export", admin, nil); st != http.StatusOK {
		t.Errorf("an Admin exported a local Library = %d, want 200; body: %s", st, body)
	}
	if st, body := srv.AuthGET("/api/v1/libraries/"+local+"/export", peer, nil); st != http.StatusOK {
		t.Errorf("a linked server exported its granted local Library = %d, want 200; body: %s", st, body)
	}
}
