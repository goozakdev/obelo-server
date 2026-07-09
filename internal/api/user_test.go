package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box tests for the Admin-scope user-management surface (/users). They
// drive the wired server over HTTP and assert wire shapes + observable state
// (login succeeds/fails, tokens revoke) — never the store internals.

type usersListResp struct {
	Users []userResp `json:"users"`
}

// TestCreateUserMemberAndAdmin: POST /users defaults to a Member, and honors an
// explicit admin role; the response carries id/username/role.
func TestCreateUserMemberAndAdmin(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var member userResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/users", token, map[string]any{
		"username": "kid", "password": "memberpass123",
	}, &member)
	if status != http.StatusCreated {
		t.Fatalf("create member status = %d, want 201; body: %s", status, body)
	}
	if member.ID == "" || member.Username != "kid" || member.Role != "member" {
		t.Errorf("member = %+v, want id set, username kid, role member; body: %s", member, body)
	}

	var admin userResp
	status, body = srv.JSON(http.MethodPost, "/api/v1/users", token, map[string]any{
		"username": "spouse", "password": "adminpass123", "role": "admin",
	}, &admin)
	if status != http.StatusCreated {
		t.Fatalf("create admin status = %d, want 201; body: %s", status, body)
	}
	if admin.Role != "admin" {
		t.Errorf("role = %q, want admin; body: %s", admin.Role, body)
	}
}

// TestCreateUserInvalidRole: an unknown role is rejected as a 400, not persisted.
func TestCreateUserInvalidRole(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/users", token, map[string]any{
		"username": "x", "password": "memberpass123", "role": "superadmin",
	}, &env)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", status, body)
	}
	if env.Error.Code != "BAD_REQUEST" {
		t.Errorf("code = %q, want BAD_REQUEST", env.Error.Code)
	}
}

// TestCreatedMemberCanLogin: a Member made via the API can log in and gets a token.
func TestCreatedMemberCanLogin(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	srv.CreateUser(admin, "kid", "memberpass123", "member")
	tok := srv.LoginAs("kid", "memberpass123")
	if tok == "" {
		t.Fatal("member login returned empty token")
	}
}

// TestListAndGetUsers: list includes the Admin + created Member; get-one returns
// a User; an unknown id is 404.
func TestListAndGetUsers(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	memberID := srv.CreateUser(token, "kid", "memberpass123", "")

	var list usersListResp
	status, body := srv.AuthGET("/api/v1/users", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", status, body)
	}
	if len(list.Users) != 2 {
		t.Fatalf("got %d users, want 2 (admin + member); body: %s", len(list.Users), body)
	}

	var one userResp
	status, body = srv.AuthGET("/api/v1/users/"+memberID, token, &one)
	if status != http.StatusOK {
		t.Fatalf("get-one status = %d, want 200; body: %s", status, body)
	}
	if one.ID != memberID || one.Role != "member" {
		t.Errorf("got %+v, want id %s role member", one, memberID)
	}

	status, _ = srv.AuthGET("/api/v1/users/does-not-exist", token, nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown-id status = %d, want 404", status)
	}
}

// TestDeleteUserCascades: deleting a Member revokes their access — a token they
// held no longer authenticates, and the credentials no longer log in.
func TestDeleteUserCascades(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "")
	memberTok := srv.LoginAs("kid", "memberpass123")

	status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+memberID, admin, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body: %s", status, body)
	}

	// The member's prior token is now invalid (tokens cascaded with the User).
	status, _ = srv.AuthGET("/api/v1/devices", memberTok, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("deleted member's token status = %d, want 401", status)
	}

	// And the credentials no longer log in.
	status, _ = srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "kid", "password": "memberpass123",
		"device": map[string]any{"name": "d", "platform": "test", "clientId": "c"},
	}, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("deleted member login status = %d, want 401", status)
	}
}

// TestSetPassword: an Admin password reset invalidates the old password and the
// new one works.
func TestSetPassword(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "oldpassword123", "")

	status, body := srv.JSON(http.MethodPut, "/api/v1/users/"+memberID+"/password", admin,
		map[string]any{"password": "newpassword123"}, nil)
	if status != http.StatusNoContent {
		t.Fatalf("set-password status = %d, want 204; body: %s", status, body)
	}

	// Old password fails, new password succeeds.
	status, _ = srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "kid", "password": "oldpassword123",
		"device": map[string]any{"name": "d", "platform": "test", "clientId": "c"},
	}, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("old-password login status = %d, want 401", status)
	}
	if tok := srv.LoginAs("kid", "newpassword123"); tok == "" {
		t.Error("new-password login returned empty token")
	}
}

// TestUsersRequireAdmin: every /users endpoint is 403 for a Member and 401 for
// an unauthenticated caller.
func TestUsersRequireAdmin(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "")
	memberTok := srv.LoginAs("kid", "memberpass123")

	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/users", nil},
		{http.MethodPost, "/api/v1/users", map[string]any{"username": "z", "password": "memberpass123"}},
		{http.MethodGet, "/api/v1/users/" + memberID, nil},
		{http.MethodDelete, "/api/v1/users/" + memberID, nil},
		{http.MethodPut, "/api/v1/users/" + memberID + "/password", map[string]any{"password": "memberpass123"}},
	}
	for _, c := range cases {
		// Member → 403.
		status, body := srv.JSON(c.method, c.path, memberTok, c.body, nil)
		if status != http.StatusForbidden {
			t.Errorf("%s %s as member: status = %d, want 403; body: %s", c.method, c.path, status, body)
		}
		// Unauthenticated → 401.
		status, body = srv.JSON(c.method, c.path, "", c.body, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: status = %d, want 401; body: %s", c.method, c.path, status, body)
		}
	}
}

// TestDuplicateUsername: a username collision is a clean 409, not a 500.
func TestDuplicateUsername(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	srv.CreateUser(token, "kid", "memberpass123", "")

	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/users", token, map[string]any{
		"username": "kid", "password": "anotherpass123",
	}, &env)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", status, body)
	}
	if env.Error.Code != "USERNAME_TAKEN" {
		t.Errorf("code = %q, want USERNAME_TAKEN", env.Error.Code)
	}
}

// TestLastAdminGuard: the final Admin cannot be deleted; once a second Admin
// exists the first can go, and Members are always deletable.
func TestLastAdminGuard(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	// The bootstrap Admin is the only Admin → delete is refused with 409 LAST_ADMIN.
	// Find the admin's own id via the list.
	var self userResp
	var list usersListResp
	srv.AuthGET("/api/v1/users", admin, &list)
	for _, u := range list.Users {
		if u.Role == "admin" {
			self = u
		}
	}
	if self.ID == "" {
		t.Fatal("could not find bootstrap admin in list")
	}

	var env errorEnvelope
	status, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+self.ID, admin, nil, &env)
	if status != http.StatusConflict || env.Error.Code != "LAST_ADMIN" {
		t.Fatalf("delete last admin: status = %d code = %q, want 409 LAST_ADMIN; body: %s",
			status, env.Error.Code, body)
	}

	// A Member is deletable regardless.
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "")
	if status, _ := srv.JSON(http.MethodDelete, "/api/v1/users/"+memberID, admin, nil, nil); status != http.StatusNoContent {
		t.Errorf("delete member status = %d, want 204", status)
	}

	// With a second Admin, the first Admin becomes deletable.
	secondID := srv.CreateUser(admin, "spouse", "adminpass123", "admin")
	if secondID == "" {
		t.Fatal("second admin not created")
	}
	if status, _ := srv.JSON(http.MethodDelete, "/api/v1/users/"+self.ID, admin, nil, nil); status != http.StatusNoContent {
		t.Errorf("delete first admin (with a second present) status = %d, want 204", status)
	}
}
