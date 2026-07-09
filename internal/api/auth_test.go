package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// These are black-box tests: they drive the fully wired server over HTTP and
// assert only on the wire shapes and observable state (PRD testing contract).

type userResp struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type deviceResp struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	ClientID string `json:"clientId"`
}

type loginResp struct {
	Token  string     `json:"token"`
	User   userResp   `json:"user"`
	Device deviceResp `json:"device"`
}

type setupResp struct {
	User userResp `json:"user"`
}

type devicesResp struct {
	Devices []deviceResp `json:"devices"`
}

// setupAdmin completes first-Admin bootstrap with the server's real claim token
// and returns the created admin. It asserts a clean 201.
func setupAdmin(t *testing.T, srv *testharness.Server, username, password string) userResp {
	t.Helper()
	var out setupResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/setup", "", map[string]any{
		"claimToken": srv.ClaimToken(),
		"username":   username,
		"password":   password,
	}, &out)
	if status != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201; body: %s", status, body)
	}
	return out.User
}

// login performs a login and returns the decoded response, asserting 200.
func login(t *testing.T, srv *testharness.Server, username, password, name, platform, clientID string) loginResp {
	t.Helper()
	var out loginResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": username,
		"password": password,
		"device": map[string]any{
			"name":     name,
			"platform": platform,
			"clientId": clientID,
		},
	}, &out)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want 200; body: %s", status, body)
	}
	return out
}

// TestSetupHappyPath: the correct claim token creates the first Admin, and
// afterwards setupRequired flips to false.
func TestSetupHappyPath(t *testing.T) {
	srv := testharness.New(t)

	admin := setupAdmin(t, srv, "brandon", "correct horse battery staple")
	if admin.Role != "admin" {
		t.Errorf("role = %q, want admin", admin.Role)
	}
	if admin.Username != "brandon" {
		t.Errorf("username = %q, want brandon", admin.Username)
	}
	if admin.ID == "" {
		t.Errorf("admin id is empty")
	}

	var info serverInfo
	status, body := srv.GET("/api/v1/server", &info)
	if status != http.StatusOK {
		t.Fatalf("server info status = %d; body: %s", status, body)
	}
	if info.SetupRequired {
		t.Errorf("setupRequired = true after admin created, want false")
	}
}

// TestSetupWrongToken: a wrong claim token is rejected and no Admin is created
// (setupRequired stays true, so a subsequent correct setup still works).
func TestSetupWrongToken(t *testing.T) {
	srv := testharness.New(t)

	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/setup", "", map[string]any{
		"claimToken": "definitely-not-the-token",
		"username":   "brandon",
		"password":   "hunter2hunter2",
	}, &env)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", status, body)
	}
	if env.Error.Code != "INVALID_CLAIM_TOKEN" {
		t.Errorf("code = %q, want INVALID_CLAIM_TOKEN; body: %s", env.Error.Code, body)
	}

	// Still required, and a correct setup now succeeds.
	var info serverInfo
	srv.GET("/api/v1/server", &info)
	if !info.SetupRequired {
		t.Errorf("setupRequired = false after a rejected setup, want true")
	}
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
}

// TestSetupClosedAfterAdmin: once an Admin exists, /setup is refused even with
// (what was) the right token, and setupRequired is false.
func TestSetupClosedAfterAdmin(t *testing.T) {
	srv := testharness.New(t)
	token := srv.ClaimToken()
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/setup", "", map[string]any{
		"claimToken": token,
		"username":   "intruder",
		"password":   "anotherpassword",
	}, &env)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body: %s", status, body)
	}
	if env.Error.Code != "SETUP_CLOSED" {
		t.Errorf("code = %q, want SETUP_CLOSED; body: %s", env.Error.Code, body)
	}
}

// TestLoginHappyPath: valid credentials return an opaque token bound to a
// Device, with matching user/device fields.
func TestLoginHappyPath(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	res := login(t, srv, "brandon", "hunter2hunter2", "Brandon's Laptop", "macos", "client-uuid-1")
	if res.Token == "" {
		t.Errorf("token is empty")
	}
	if res.User.Username != "brandon" {
		t.Errorf("user.username = %q, want brandon", res.User.Username)
	}
	if res.Device.ClientID != "client-uuid-1" {
		t.Errorf("device.clientId = %q, want client-uuid-1", res.Device.ClientID)
	}
	if res.Device.Name != "Brandon's Laptop" || res.Device.Platform != "macos" {
		t.Errorf("device fields not echoed: %+v", res.Device)
	}
}

// TestLoginBadPassword: wrong password is rejected with the generic invalid
// credentials envelope and no token.
func TestLoginBadPassword(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "brandon",
		"password": "wrong-password",
		"device":   map[string]any{"name": "x", "platform": "y", "clientId": "c"},
	}, &env)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body: %s", status, body)
	}
	if env.Error.Code != "INVALID_CREDENTIALS" {
		t.Errorf("code = %q, want INVALID_CREDENTIALS; body: %s", env.Error.Code, body)
	}
}

// TestLoginUnknownUser: an unknown username also yields INVALID_CREDENTIALS
// (indistinguishable from a wrong password).
func TestLoginUnknownUser(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	var env errorEnvelope
	status, _ := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": "nobody",
		"password": "whatever",
		"device":   map[string]any{"name": "x", "platform": "y", "clientId": "c"},
	}, &env)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if env.Error.Code != "INVALID_CREDENTIALS" {
		t.Errorf("code = %q, want INVALID_CREDENTIALS", env.Error.Code)
	}
}

// TestClientIDDedup: two logins with the same clientId reuse one Device; a
// third with a different clientId adds a second.
func TestClientIDDedup(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	first := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "stable-client")
	second := login(t, srv, "brandon", "hunter2hunter2", "Laptop Renamed", "macos", "stable-client")
	if first.Device.ID != second.Device.ID {
		t.Errorf("re-login with same clientId made a new Device: %s != %s", first.Device.ID, second.Device.ID)
	}
	if second.Device.Name != "Laptop Renamed" {
		t.Errorf("device name not refreshed on re-login: %q", second.Device.Name)
	}

	// A different clientId is a distinct Device.
	login(t, srv, "brandon", "hunter2hunter2", "Phone", "ios", "other-client")

	var list devicesResp
	status, body := srv.AuthGET("/api/v1/devices", second.Token, &list)
	if status != http.StatusOK {
		t.Fatalf("list devices status = %d; body: %s", status, body)
	}
	if len(list.Devices) != 2 {
		t.Fatalf("device count = %d, want 2 (one per clientId); body: %s", len(list.Devices), body)
	}
}

// TestAuthenticatedRequestAcceptedAndRejected: a valid token is accepted on a
// protected route; a missing or garbage token is rejected with 401.
func TestAuthenticatedRequestAcceptedAndRejected(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	res := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "c1")

	// Accepted.
	var list devicesResp
	status, body := srv.AuthGET("/api/v1/devices", res.Token, &list)
	if status != http.StatusOK {
		t.Fatalf("authenticated GET /devices status = %d; body: %s", status, body)
	}

	// Missing token.
	var env errorEnvelope
	status, _ = srv.AuthGET("/api/v1/devices", "", &env)
	if status != http.StatusUnauthorized {
		t.Errorf("missing token status = %d, want 401", status)
	}
	if env.Error.Code != "UNAUTHORIZED" {
		t.Errorf("missing token code = %q, want UNAUTHORIZED", env.Error.Code)
	}

	// Garbage token.
	status, _ = srv.AuthGET("/api/v1/devices", "not-a-real-token", &env)
	if status != http.StatusUnauthorized {
		t.Errorf("garbage token status = %d, want 401", status)
	}
}

// TestLogoutRevokesToken: after logout the same token is rejected on a
// protected route — immediate revocation (ADR-0015).
func TestLogoutRevokesToken(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	res := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "c1")

	// Token works first.
	if status, _ := srv.AuthGET("/api/v1/devices", res.Token, nil); status != http.StatusOK {
		t.Fatalf("pre-logout GET /devices status = %d, want 200", status)
	}

	// Logout.
	status, body := srv.JSON(http.MethodPost, "/api/v1/auth/logout", res.Token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204; body: %s", status, body)
	}

	// Same token now rejected.
	status, _ = srv.AuthGET("/api/v1/devices", res.Token, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("post-logout GET /devices status = %d, want 401", status)
	}
}

// TestDeleteDeviceRevokesToken: deleting a Device immediately invalidates the
// token issued to it (ADR-0015).
func TestDeleteDeviceRevokesToken(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")

	// Two devices so the caller still has a live token to issue the DELETE with.
	keep := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "keep-client")
	victim := login(t, srv, "brandon", "hunter2hunter2", "Phone", "ios", "victim-client")

	// The victim token works before deletion.
	if status, _ := srv.AuthGET("/api/v1/devices", victim.Token, nil); status != http.StatusOK {
		t.Fatalf("victim token pre-delete status = %d, want 200", status)
	}

	// Delete the victim device using the keep token (same user).
	status, body := srv.JSON(http.MethodDelete, "/api/v1/devices/"+victim.Device.ID, keep.Token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("delete device status = %d, want 204; body: %s", status, body)
	}

	// The victim's token is now revoked.
	status, _ = srv.AuthGET("/api/v1/devices", victim.Token, nil)
	if status != http.StatusUnauthorized {
		t.Errorf("victim token post-delete status = %d, want 401", status)
	}

	// The keep token still works and now sees only one device.
	var list devicesResp
	status, body = srv.AuthGET("/api/v1/devices", keep.Token, &list)
	if status != http.StatusOK {
		t.Fatalf("keep token post-delete status = %d; body: %s", status, body)
	}
	if len(list.Devices) != 1 {
		t.Errorf("device count after delete = %d, want 1", len(list.Devices))
	}
}

// TestDeleteUnknownDevice returns 404 with the standard envelope.
func TestDeleteUnknownDevice(t *testing.T) {
	srv := testharness.New(t)
	setupAdmin(t, srv, "brandon", "hunter2hunter2")
	res := login(t, srv, "brandon", "hunter2hunter2", "Laptop", "macos", "c1")

	var env errorEnvelope
	status, _ := srv.JSON(http.MethodDelete, "/api/v1/devices/no-such-id", res.Token, nil, &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", env.Error.Code)
	}
}
