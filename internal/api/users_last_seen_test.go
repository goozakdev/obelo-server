package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// GET /users carries each User's Device last-seen (.scratch/linked-servers issue
// 04). It is the Admin Users list's only way to say whether a `remote` User — a
// linked Server — has ever redeemed its Invite: before the redemption there is
// no Device at all, and after it there is exactly one, whose last-seen moves
// every time the other household's Server speaks (ADR-0055 §4).

type adminUserEntry struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	LastSeenAt string `json:"lastSeenAt"`
}

func listUsersAdmin(t *testing.T, srv *testharness.Server, adminTok string) []adminUserEntry {
	t.Helper()
	var out struct {
		Users []adminUserEntry `json:"users"`
	}
	if status, body := srv.AuthGET("/api/v1/users", adminTok, &out); status != http.StatusOK {
		t.Fatalf("list users: status %d, want 200; body: %s", status, body)
	}
	return out.Users
}

func entryFor(t *testing.T, users []adminUserEntry, id string) adminUserEntry {
	t.Helper()
	for _, u := range users {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("user %q is missing from the Admin list", id)
	return adminUserEntry{}
}

// A `remote` User that has never been redeemed carries NO last-seen — the field
// is absent, not "", so "never linked" is the absence of a Device rather than a
// sentinel the client has to know about.
func TestUsersListOmitsLastSeenForAnUnredeemedRemoteUser(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	got := entryFor(t, listUsersAdmin(t, srv, admin), peerID)
	if got.LastSeenAt != "" {
		t.Errorf("an unredeemed remote User reports lastSeenAt %q, want none", got.LastSeenAt)
	}
	if got.Role != "remote" {
		t.Errorf("role = %q, want remote", got.Role)
	}
}

// Once the other household's Server has redeemed, the Device it left behind
// gives the row a real last-seen.
func TestUsersListCarriesLastSeenOnceTheLinkIsRedeemed(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	code := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code
	var res struct {
		Token string `json:"token"`
	}
	if status, body := redeem(t, srv, code, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, &res); status != http.StatusOK {
		t.Fatalf("redeem: status %d; body: %s", status, body)
	}

	got := entryFor(t, listUsersAdmin(t, srv, admin), peerID)
	if got.LastSeenAt == "" {
		t.Fatalf("a redeemed remote User has no lastSeenAt")
	}
	seen, err := time.Parse(time.RFC3339, got.LastSeenAt)
	if err != nil {
		t.Fatalf("lastSeenAt %q is not RFC3339: %v", got.LastSeenAt, err)
	}
	if time.Since(seen) > time.Hour || time.Until(seen) > time.Minute {
		t.Errorf("lastSeenAt = %v, want roughly now", seen)
	}

	// The Admin's own row is unaffected in kind: they have a Device (they logged
	// in), so they too carry a stamp — the field is about Devices, not the role.
	for _, u := range listUsersAdmin(t, srv, admin) {
		if u.Role == "admin" && u.LastSeenAt == "" {
			t.Errorf("the signed-in Admin %q has no lastSeenAt", u.Username)
		}
	}
}
