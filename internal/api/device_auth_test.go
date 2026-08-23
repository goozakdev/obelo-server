package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
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

// --- the two ways a start can be refused ------------------------------------
//
// Both caps are sized and reasoned about in auth/device_auth.go, and both are
// tested there where the clock can be moved. What is HERE is the wire: a client
// has to be able to tell "you are asking too often" from "the server has nothing
// left", because the correct behaviour differs — back off versus retry — and the
// only thing carrying that distinction across the wire is the status and code.

// startFrom runs POST /auth/device/code as if it arrived from remoteAddr,
// returning the status, the response headers, and the decoded error envelope.
// It goes through JSONFrom rather than the listener because every request over
// the listener arrives from 127.0.0.1, which would make "per source" untestable.
func startFrom(t *testing.T, srv *testharness.Server, remoteAddr string) (int, http.Header, errEnvelope) {
	t.Helper()
	var env errEnvelope
	status, header, _ := srv.JSONFrom(http.MethodPost, "/api/v1/auth/device/code", "",
		remoteAddr, nil, tvDeviceBody(), &env)
	return status, header, env
}

// startUntilRefusedFrom hammers the start endpoint from one address until it is
// refused, and returns that refusal. probe bounds the loop so a cap that stopped
// capping fails the test rather than running forever.
func startUntilRefusedFrom(t *testing.T, srv *testharness.Server, remoteAddr string, probe int) (int, http.Header, errEnvelope) {
	t.Helper()
	for i := 0; i < probe; i++ {
		status, header, env := startFrom(t, srv, remoteAddr)
		if status != http.StatusCreated {
			return status, header, env
		}
	}
	t.Fatalf("%d starts from %s and none was refused", probe, remoteAddr)
	return 0, nil, errEnvelope{}
}

// TestDeviceCodeQuotaAnswers429WithRetryAfter covers the refusal a client can do
// something about. It is a 429 and not the 503 below on purpose: 503 means the
// server is full and a retry may work at any moment, while 429 means this caller
// is over its own budget and retrying is the one thing that makes it worse. The
// Retry-After is what lets a TV wait exactly long enough instead of guessing.
func TestDeviceCodeQuotaAnswers429WithRetryAfter(t *testing.T) {
	srv := testharness.New(t)

	status, header, env := startUntilRefusedFrom(t, srv, "192.0.2.10:51000", deviceStartProbe)
	if status != http.StatusTooManyRequests || env.Error.Code != "TOO_MANY_ATTEMPTS" {
		t.Fatalf("over-quota start = %d/%s, want 429/TOO_MANY_ATTEMPTS", status, env.Error.Code)
	}
	retry, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", header.Get("Retry-After"), err)
	}
	if retry < 1 {
		t.Errorf("Retry-After = %d; zero or negative reads as 'go ahead now', which is "+
			"the one thing this response means to deny", retry)
	}

	// A different address is untouched. Behind a reverse proxy this is the case
	// that cannot happen — every request keys to the proxy — which is exactly why
	// the quota is sized above the old global cap rather than tuned for a household.
	if status, _, env := startFrom(t, srv, "198.51.100.7:51000"); status != http.StatusCreated {
		t.Errorf("start from a second address = %d/%s, want 201", status, env.Error.Code)
	}
}

// TestDeviceCodeQuotaIgnoresForwardedHeaders is the reason the quota is worth
// anything at all. clientIP reads RemoteAddr, and X-Forwarded-For only from a
// peer the operator listed in OBELO_TRUSTED_PROXIES — which the harness leaves
// empty here, as it ships. A caller who could mint a fresh "address" per request
// would face a quota that is decorative.
//
// The cost of that default is real and is accepted elsewhere: behind a reverse
// proxy nobody declared, every request shares one budget. See clientIP in
// middleware.go, and the declared-proxy half below.
func TestDeviceCodeQuotaIgnoresForwardedHeaders(t *testing.T) {
	srv := testharness.New(t)

	if status, _, env := startUntilRefusedFrom(t, srv, "192.0.2.10:51000", deviceStartProbe); status != http.StatusTooManyRequests {
		t.Fatalf("over-quota start = %d/%s, want 429", status, env.Error.Code)
	}

	var env errEnvelope
	spoofed := http.Header{"X-Forwarded-For": []string{"203.0.113.99"}, "X-Real-IP": []string{"203.0.113.99"}}
	status, _, _ := srv.JSONFrom(http.MethodPost, "/api/v1/auth/device/code", "",
		"192.0.2.10:51000", spoofed, tvDeviceBody(), &env)
	if status != http.StatusTooManyRequests {
		t.Errorf("start with a spoofed X-Forwarded-For = %d, want 429 — a client that can "+
			"claim its own source address has no quota at all", status)
	}
}

// TestDeviceCodeQuotaPerClientBehindDeclaredProxy is the other half: the
// degradation maxDeviceAuthStartsPerSource documents — 128 per five minutes for
// the whole household, because every request keys to the proxy — is a
// consequence of not declaring the proxy, not a fact about proxies. Declared, the
// household's TVs each hold their own budget again, and one TV burning its own
// does not stop the next one signing in.
//
// The forged half matters as much as the honest one: the exhausted client pads
// its chain with entries an attacker would actually send, including one
// impersonating the proxy itself, and stays refused.
func TestDeviceCodeQuotaPerClientBehindDeclaredProxy(t *testing.T) {
	srv := testharness.New(t, testharness.WithTrustedProxies("10.0.0.1/32"))

	const proxy = "10.0.0.1:41000"
	startVia := func(chain string) int {
		t.Helper()
		var env errEnvelope
		status, _, _ := srv.JSONFrom(http.MethodPost, "/api/v1/auth/device/code", "",
			proxy, http.Header{"X-Forwarded-For": []string{chain}}, tvDeviceBody(), &env)
		return status
	}

	var status int
	for i := 0; i < deviceStartProbe && status != http.StatusTooManyRequests; i++ {
		status = startVia("198.51.100.7")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("%d starts from one client behind a declared proxy and none was refused", deviceStartProbe)
	}

	// A different TV behind the same proxy, over the same connection: unaffected.
	if got := startVia("198.51.100.8"); got != http.StatusCreated {
		t.Errorf("second client behind the declared proxy = %d, want 201 — the quota must "+
			"discriminate once the operator has said who the proxy is", got)
	}

	// The exhausted one cannot buy itself a new budget by padding the chain.
	for _, chain := range []string{
		"9.9.9.9, 198.51.100.7",
		"10.0.0.9, 198.51.100.7",
		"198.51.100.8, 198.51.100.7",
	} {
		if got := startVia(chain); got != http.StatusTooManyRequests {
			t.Errorf("start with chain %q = %d, want 429 — forged entries sit to the LEFT "+
				"of what the proxy appended and must never be read", chain, got)
		}
	}
}

// TestDeviceCodeBusyAnswers503 keeps the pre-existing refusal reachable and
// tested. Filling the code space now takes a spread of source addresses, which is
// the per-source quota doing its job; what must come back at the end is still a
// 503 DEVICE_AUTH_BUSY, addressed to a caller that has done nothing wrong.
func TestDeviceCodeBusyAnswers503(t *testing.T) {
	srv := testharness.New(t)

	var filled int
	var status int
	var env errEnvelope
	for i := 0; i < deviceSpaceProbe; i++ {
		addr := fmt.Sprintf("192.0.2.%d:51000", i/deviceStartsPerAddr)
		status, _, env = startFrom(t, srv, addr)
		if status != http.StatusCreated {
			break
		}
		filled++
	}
	if status != http.StatusServiceUnavailable || env.Error.Code != "DEVICE_AUTH_BUSY" {
		t.Fatalf("start after %d live codes = %d/%s, want 503/DEVICE_AUTH_BUSY",
			filled, status, env.Error.Code)
	}
	// And no Retry-After: nothing here knows when a slot frees up, and inventing a
	// number would be a promise the server cannot keep.
	if _, _, env := startFrom(t, srv, "198.51.100.7:51000"); env.Error.Code != "DEVICE_AUTH_BUSY" {
		t.Errorf("an unseen address with the space full got %s, want DEVICE_AUTH_BUSY", env.Error.Code)
	}
}

// Loop bounds for the two cap tests. Named rather than inlined for the same
// reason the service-level tests name theirs: they must stay comfortably past the
// server's own constants without restating them.
const (
	// deviceStartProbe bounds a single address's run at the per-source quota.
	deviceStartProbe = 512
	// deviceSpaceProbe bounds the fill-the-code-space loop.
	deviceSpaceProbe = 4096
	// deviceStartsPerAddr is how many starts that loop takes from one address
	// before rotating, well under the per-source quota so the global cap is what
	// it ends up measuring.
	deviceStartsPerAddr = 32
)

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
//
// The proxy is declared (WithTrustedProxies), which is what the scheme half now
// requires: X-Forwarded-Proto is read only from a listed peer (ADR-0041). The
// HOST half is not gated — see externalBaseURL for why forging it only changes a
// string the forger already controlled — so an undeclared proxy still gets the
// right host here, with an http:// scheme it fixes by declaring itself.
func TestVerificationURIHonoursForwardedHeaders(t *testing.T) {
	srv := testharness.New(t, testharness.WithTrustedProxies("127.0.0.1/32", "::1/128"))

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
	req.Header.Set("X-Forwarded-Host", "obelo.example.com")

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

	const want = "https://obelo.example.com/link"
	if out.VerificationURI != want {
		t.Errorf("verificationUri behind a proxy = %q, want %q", out.VerificationURI, want)
	}
}
