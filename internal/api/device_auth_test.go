package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// The Admin these tests sign in as. Each test boots its own zero-user server, so
// the credentials are per-test furniture rather than shared state.
const (
	deviceAuthAdminUser = "brandon"
	deviceAuthAdminPass = "correct horse battery staple"
)

// newAdminServer boots a harness with its first Admin bootstrapped, returning
// the server and an Admin bearer token.
func newAdminServer(t *testing.T) (*testharness.Server, string) {
	t.Helper()
	srv := testharness.New(t)
	setupAdmin(t, srv, deviceAuthAdminUser, deviceAuthAdminPass)
	return srv, srv.LoginAs(deviceAuthAdminUser, deviceAuthAdminPass)
}

// HTTP-level tests for the Device authorization grant (ADR-0036). The rules
// about time (expiry, poll pacing, the rate-limit window) are tested at the
// service level in internal/auth/device_auth_test.go, where the clock can be
// moved; these cover the wire — status codes, envelope codes, the verification
// URL, and who is allowed to call what.

type deviceCodeBody struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURI         string `json:"verificationUri"`
	VerificationURIComplete string `json:"verificationUriComplete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func tvDeviceBody() map[string]any {
	return map[string]any{"device": map[string]any{
		"name": "Living Room TV", "platform": "tvos", "clientId": "tv-client-1",
	}}
}

// startFlow runs POST /auth/device/code and asserts a clean 201.
func startFlow(t *testing.T, srv *testharness.Server) deviceCodeBody {
	t.Helper()
	var out deviceCodeBody
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/code", "", tvDeviceBody(), &out)
	if status != http.StatusCreated {
		t.Fatalf("POST /auth/device/code = %d, want 201; body: %s", status, raw)
	}
	return out
}

// TestDeviceAuthFlowOverHTTP walks the grant end to end on the wire.
func TestDeviceAuthFlowOverHTTP(t *testing.T) {
	srv, admin := newAdminServer(t)

	start := startFlow(t, srv)
	if len(start.UserCode) != 4 {
		t.Errorf("userCode %q, want 4 characters", start.UserCode)
	}
	if start.ExpiresIn <= 0 || start.Interval <= 0 {
		t.Errorf("expiresIn=%d interval=%d, both must be positive", start.ExpiresIn, start.Interval)
	}

	// This test polls exactly once, and the pending case gets its own test below.
	// Two polls back to back would trip the poll-pacing rule against a clock this
	// harness cannot move — and would be measuring the pacing rule rather than the
	// happy path. A real client sleeps `interval` between polls and never sees it.

	// The phone approves, and is told what it just signed in.
	var approved struct {
		Device struct {
			Name     string `json:"name"`
			Platform string `json:"platform"`
		} `json:"device"`
	}
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/approve", admin,
		map[string]any{"userCode": start.UserCode}, &approved)
	if status != http.StatusOK {
		t.Fatalf("approve = %d, want 200; body: %s", status, raw)
	}
	if approved.Device.Name != "Living Room TV" || approved.Device.Platform != "tvos" {
		t.Errorf("approve echoed %q/%q, want Living Room TV/tvos",
			approved.Device.Name, approved.Device.Platform)
	}

	// The TV collects a session. This body must be shaped exactly like a login's.
	var session struct {
		Token string `json:"token"`
		User  struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
		Device struct {
			ClientID string `json:"clientId"`
			Name     string `json:"name"`
		} `json:"device"`
	}
	status, raw = srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": start.DeviceCode}, &session)
	if status != http.StatusOK {
		t.Fatalf("redeem = %d, want 200; body: %s", status, raw)
	}
	if session.Token == "" {
		t.Fatal("redeem returned no token")
	}
	if session.User.Username != deviceAuthAdminUser {
		t.Errorf("session user = %q, want the approving user %q",
			session.User.Username, deviceAuthAdminUser)
	}
	if session.Device.ClientID != "tv-client-1" {
		t.Errorf("session device clientId = %q, want tv-client-1", session.Device.ClientID)
	}

	// The granted token is a real session on the Public scope.
	var devices struct {
		Devices []struct {
			ClientID string `json:"clientId"`
		} `json:"devices"`
	}
	if status, raw := srv.AuthGET("/api/v1/devices", session.Token, &devices); status != http.StatusOK {
		t.Fatalf("GET /devices with a device-granted token = %d, want 200; body: %s", status, raw)
	}
	var found bool
	for _, d := range devices.Devices {
		if d.ClientID == "tv-client-1" {
			found = true
		}
	}
	if !found {
		t.Error("the TV does not appear in the approving user's Devices")
	}
}

// TestDeviceTokenPendingBeforeApproval covers the state a client spends almost
// all of its time in: a 400 it is expected to keep hitting until a human acts.
// It must be an error rather than a 2xx, or a client treating success as
// terminal would stop polling and never collect the session.
func TestDeviceTokenPendingBeforeApproval(t *testing.T) {
	srv := testharness.New(t)
	start := startFlow(t, srv)

	var env errEnvelope
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": start.DeviceCode}, &env)
	if status != http.StatusBadRequest || env.Error.Code != "AUTHORIZATION_PENDING" {
		t.Fatalf("poll before approval = %d/%s, want 400/AUTHORIZATION_PENDING; body: %s",
			status, env.Error.Code, raw)
	}
}

// TestDeviceAuthLoginResponsesAreIdentical is the contract that lets a client
// keep ONE way to establish a session. If these two shapes ever diverge, a
// client that switched on the payload would break on whichever it saw second.
func TestDeviceAuthLoginResponsesAreIdentical(t *testing.T) {
	srv, admin := newAdminServer(t)

	start := startFlow(t, srv)
	if status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/approve", admin,
		map[string]any{"userCode": start.UserCode}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d; body: %s", status, raw)
	}

	var viaDevice, viaPassword map[string]any
	if status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": start.DeviceCode}, &viaDevice); status != http.StatusOK {
		t.Fatalf("redeem = %d; body: %s", status, raw)
	}
	if status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"username": deviceAuthAdminUser, "password": deviceAuthAdminPass,
		"device": map[string]any{"name": "X", "platform": "tvos", "clientId": "x"},
	}, &viaPassword); status != http.StatusOK {
		t.Fatalf("login = %d; body: %s", status, raw)
	}

	if len(viaDevice) != len(viaPassword) {
		t.Fatalf("device grant returned keys %v, password login returned %v",
			keysOf(viaDevice), keysOf(viaPassword))
	}
	for k := range viaPassword {
		if _, ok := viaDevice[k]; !ok {
			t.Errorf("device-grant response is missing %q, which a login response has", k)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDeviceApproveRequiresAuth: approval is the one authenticated step, and the
// whole grant rests on it. An anonymous caller who could approve could sign a TV
// into an account without ever holding a credential.
func TestDeviceApproveRequiresAuth(t *testing.T) {
	srv := testharness.New(t)
	start := startFlow(t, srv)

	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/approve", "",
		map[string]any{"userCode": start.UserCode}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated approve = %d, want 401; body: %s", status, raw)
	}

	// And the code is untouched — a rejected approve must not consume it.
	var env errEnvelope
	status, _ = srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": start.DeviceCode}, &env)
	if status != http.StatusBadRequest || env.Error.Code != "AUTHORIZATION_PENDING" {
		t.Errorf("after a rejected approve, poll = %d/%s, want 400/AUTHORIZATION_PENDING",
			status, env.Error.Code)
	}
}

// TestDeviceApproveWrongCode: unknown, expired, and already-used all answer the
// same 404. Telling them apart would let a caller map the live code space, which
// is the one thing a 4-character code cannot afford.
func TestDeviceApproveWrongCode(t *testing.T) {
	srv, admin := newAdminServer(t)

	var env errEnvelope
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/approve", admin,
		map[string]any{"userCode": "ZZZZ"}, &env)
	if status != http.StatusNotFound || env.Error.Code != "INVALID_USER_CODE" {
		t.Fatalf("approve of an unknown code = %d/%s, want 404/INVALID_USER_CODE; body: %s",
			status, env.Error.Code, raw)
	}
}

// TestDeviceTokenUnknownCode: a poll with a garbage device code is refused, and
// refused as a device-code problem rather than as a pending one — a client told
// "pending" would poll a nonexistent flow until its deadline.
func TestDeviceTokenUnknownCode(t *testing.T) {
	srv := testharness.New(t)

	var env errEnvelope
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/token", "",
		map[string]any{"deviceCode": "not-a-real-device-code"}, &env)
	if status != http.StatusBadRequest || env.Error.Code != "INVALID_DEVICE_CODE" {
		t.Fatalf("poll with a bogus code = %d/%s, want 400/INVALID_DEVICE_CODE; body: %s",
			status, env.Error.Code, raw)
	}
}

// TestDeviceCodeRequiresClientID mirrors POST /auth/login's rule: without a
// stable clientId the redeem would mint a duplicate Device every sign-in.
func TestDeviceCodeRequiresClientID(t *testing.T) {
	srv := testharness.New(t)

	var env errEnvelope
	status, raw := srv.JSON(http.MethodPost, "/api/v1/auth/device/code", "", map[string]any{
		"device": map[string]any{"name": "TV", "platform": "tvos"},
	}, &env)
	if status != http.StatusBadRequest || env.Error.Code != "BAD_REQUEST" {
		t.Fatalf("start without clientId = %d/%s, want 400/BAD_REQUEST; body: %s",
			status, env.Error.Code, raw)
	}
}

// TestVerificationURIMatchesTheRequestHost pins what the QR encodes. The phone
// must reach the SAME server the TV reached, and the server cannot know its own
// address — only the one it was called on. A verificationUri built from config
// or from the listen address would be right in development and wrong behind a
// reverse proxy, or vice versa.
func TestVerificationURIMatchesTheRequestHost(t *testing.T) {
	srv := testharness.New(t)
	start := startFlow(t, srv)

	// The harness serves on 127.0.0.1:<port>; that is the Host the request
	// carried, so that is what the QR must point at.
	wantHost := strings.TrimPrefix(srv.URL(""), "http://")
	wantHost = strings.TrimSuffix(wantHost, "/")
	if !strings.HasPrefix(start.VerificationURI, "http://"+wantHost) {
		t.Errorf("verificationUri = %q, want it rooted at the request host %q",
			start.VerificationURI, wantHost)
	}
	if !strings.HasSuffix(start.VerificationURI, "/link") {
		t.Errorf("verificationUri = %q, want it to end at the SPA's /link route",
			start.VerificationURI)
	}
	// The complete form carries the code so a scan needs no typing at all.
	if start.VerificationURIComplete != start.VerificationURI+"/"+start.UserCode {
		t.Errorf("verificationUriComplete = %q, want %q",
			start.VerificationURIComplete, start.VerificationURI+"/"+start.UserCode)
	}
}

// TestVerificationURIHonoursForwardedHeaders covers the reverse-proxy
// deployment (ADR-0005). The server binds plain HTTP and never sees the TLS
// origin the TV actually used, so a QR built from r.Host alone would send the
// phone to an internal address it cannot reach.
func TestVerificationURIHonoursForwardedHeaders(t *testing.T) {
	srv := testharness.New(t)

	// Hand-built rather than via the harness helpers: this is the only test that
	// needs to set request headers, and one local request is cheaper than a
	// harness method nothing else would call.
	body, err := json.Marshal(tvDeviceBody())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		srv.URL("/api/v1/auth/device/code"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "juicebox.example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied start: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("proxied start = %d, want 201; body: %s", resp.StatusCode, raw)
	}
	var out deviceCodeBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v; body: %s", err, raw)
	}

	const want = "https://juicebox.example.com/link"
	if out.VerificationURI != want {
		t.Errorf("verificationUri behind a proxy = %q, want %q", out.VerificationURI, want)
	}
}
