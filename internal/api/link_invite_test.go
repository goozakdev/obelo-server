package api_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the sharing half of linking over HTTP
// (.scratch/linked-servers issue 03, ADR-0055 §1–§4): an Admin mints a one-time
// invite for a `remote` User and another household's Server redeems it once for
// an ordinary Device-bound bearer.
//
// The rules that turn on the CLOCK — the 24-hour TTL, the per-address rate-limit
// window — are pinned in internal/auth/link_invite_test.go, where the service's
// clock can be moved. What is asserted here is the wire: the string, the
// statuses, the codes, and the fact that two refusals answer identically.

const (
	// The redeeming Server's identity (ADR-0034), which becomes the Device.
	testHomeServerID   = "3f6a1c2e-0000-4000-8000-abcdefabcdef"
	testHomeServerName = "Brandon's server"
)

// inviteBody is the decoded `obelo-link:` payload (ADR-0055 §2).
type inviteBody struct {
	V       int      `json:"v"`
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Origins []string `json:"origins"`
	Code    string   `json:"code"`
	Exp     string   `json:"exp"`
}

type mintInviteResp struct {
	Invite    string `json:"invite"`
	ExpiresAt string `json:"expiresAt"`
}

// mintInvite asks for an invite over the real Admin API and asserts the 201.
func mintInvite(t *testing.T, srv *testharness.Server, adminTok, userID string, origins ...string) mintInviteResp {
	t.Helper()
	var out mintInviteResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/users/"+userID+"/invite", adminTok,
		map[string]any{"origins": origins}, &out)
	if status != http.StatusCreated {
		t.Fatalf("mint invite: status %d, want 201; body: %s", status, body)
	}
	return out
}

// decodeInvite unwraps the one string an Admin sends.
func decodeInvite(t *testing.T, invite string) inviteBody {
	t.Helper()
	rest, ok := strings.CutPrefix(invite, "obelo-link:")
	if !ok {
		t.Fatalf("invite %q does not carry the obelo-link: scheme", invite)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		t.Fatalf("invite payload is not base64url: %v", err)
	}
	var out inviteBody
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invite payload is not JSON: %v (%s)", err, raw)
	}
	return out
}

// redeem posts a redemption as another Server would. It returns the raw status
// and body so each test can assert its own outcome.
func redeem(t *testing.T, srv *testharness.Server, code string, version int, serverID, serverName string, out any) (int, []byte) {
	t.Helper()
	return srv.JSON(http.MethodPost, "/api/v1/auth/link/redeem", "", map[string]any{
		"code":                code,
		"linkProtocolVersion": version,
		"server":              map[string]any{"id": serverID, "name": serverName},
	}, out)
}

// TestMintedInviteCarriesTheServerIdentityAndOrigins is ADR-0055 §2: one
// self-contained string, not "a hostname and a code".
func TestMintedInviteCarriesTheServerIdentityAndOrigins(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	var info struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if status, body := srv.GET("/api/v1/server", &info); status != http.StatusOK {
		t.Fatalf("handshake: status %d; body: %s", status, body)
	}

	out := mintInvite(t, srv, admin, peerID,
		"http://obelo.tail1a2b.ts.net", "https://media.example.org/")
	payload := decodeInvite(t, out.Invite)

	if payload.V != server.LinkProtocolVersion {
		t.Errorf("invite v = %d, want %d", payload.V, server.LinkProtocolVersion)
	}
	if payload.ID != info.ID || payload.ID == "" {
		t.Errorf("invite id = %q, want the sharer's server id %q", payload.ID, info.ID)
	}
	if payload.Name != info.Name || payload.Name == "" {
		t.Errorf("invite name = %q, want the sharer's server name %q", payload.Name, info.Name)
	}
	// Order is meaningful: the home Server tries them in order (ADR-0055 §2). The
	// trailing slash on the second is folded away; nothing else is rewritten.
	want := []string{"http://obelo.tail1a2b.ts.net", "https://media.example.org"}
	if len(payload.Origins) != len(want) {
		t.Fatalf("invite origins = %v, want %v", payload.Origins, want)
	}
	for i := range want {
		if payload.Origins[i] != want[i] {
			t.Errorf("invite origins = %v, want %v", payload.Origins, want)
			break
		}
	}
	if payload.Code == "" {
		t.Error("invite carries no code")
	}
	if payload.Exp != out.ExpiresAt {
		t.Errorf("payload exp %q != response expiresAt %q", payload.Exp, out.ExpiresAt)
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q is not RFC 3339: %v", out.ExpiresAt, err)
	}
	// 24 hours (ADR-0055 §1), give or take the time the test took.
	if d := time.Until(exp); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("invite expires in %s, want ~24h", d)
	}
}

// TestInviteRedeemsOnceOverHTTP: the happy path, and then the two refusals that
// must be indistinguishable — a second attempt and an expired invite.
func TestInviteRedeemsOnceOverHTTP(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)
	code := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code

	var res struct {
		Token string `json:"token"`
		User  struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"user"`
		Device struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Platform string `json:"platform"`
			ClientID string `json:"clientId"`
		} `json:"device"`
		LinkProtocolVersion int `json:"linkProtocolVersion"`
	}
	status, body := redeem(t, srv, code, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, &res)
	if status != http.StatusOK {
		t.Fatalf("redeem: status %d, want 200; body: %s", status, body)
	}
	if res.Token == "" {
		t.Error("redeem returned no token")
	}
	if res.User.ID != peerID || res.User.Role != "remote" {
		t.Errorf("redeemed as user %+v, want the remote User %q", res.User, peerID)
	}
	// ADR-0055 §4: the Device is the redeeming Server, keyed on its server id.
	if res.Device.ClientID != testHomeServerID {
		t.Errorf("device clientId = %q, want %q", res.Device.ClientID, testHomeServerID)
	}
	if res.Device.Name != testHomeServerName {
		t.Errorf("device name = %q, want %q", res.Device.Name, testHomeServerName)
	}
	if res.Device.Platform != "server" {
		t.Errorf("device platform = %q, want \"server\"", res.Device.Platform)
	}
	if res.LinkProtocolVersion != server.LinkProtocolVersion {
		t.Errorf("response linkProtocolVersion = %d, want %d",
			res.LinkProtocolVersion, server.LinkProtocolVersion)
	}

	// The token is an ordinary bearer: it authenticates, as the remote User.
	var devices struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	if status, body := srv.AuthGET("/api/v1/devices", res.Token, &devices); status != http.StatusOK {
		t.Fatalf("the redeemed token does not authenticate: status %d; body: %s", status, body)
	}
	if len(devices.Devices) != 1 || devices.Devices[0].ID != res.Device.ID {
		t.Errorf("the redemption left %d Devices, want exactly the one it returned", len(devices.Devices))
	}

	// Second attempt with the same code.
	var second errorEnvelope
	secondStatus, body := redeem(t, srv, code, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, &second)
	if secondStatus != http.StatusBadRequest || second.Error.Code != "INVALID_INVITE" {
		t.Fatalf("second redeem: status %d code %q, want 400 INVALID_INVITE; body: %s",
			secondStatus, second.Error.Code, body)
	}

	// An EXPIRED invite answers identically — same status, same code, same
	// message. A caller that could tell them apart would learn which codes exist.
	fresh := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code
	srv.ExpireLinkInvites(peerID)
	var expired errorEnvelope
	expiredStatus, body := redeem(t, srv, fresh, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, &expired)
	if expiredStatus != secondStatus || expired.Error.Code != second.Error.Code ||
		expired.Error.Message != second.Error.Message {
		t.Errorf("an expired invite answers %d/%q/%q but a spent one answers %d/%q/%q; "+
			"the two must be indistinguishable. body: %s",
			expiredStatus, expired.Error.Code, expired.Error.Message,
			secondStatus, second.Error.Code, second.Error.Message, body)
	}
}

// TestRedeemRefusesAVersionMismatch is ADR-0055 §3: the check runs BEFORE
// anything is redeemed, so a mismatch costs the Admin nothing, and the answer
// carries both numbers so the redeeming side can say which server to upgrade.
func TestRedeemRefusesAVersionMismatch(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)
	code := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code

	for _, theirs := range []int{server.LinkProtocolVersion + 1, server.LinkProtocolVersion - 1, 0} {
		var env errorEnvelope
		status, body := redeem(t, srv, code, theirs, testHomeServerID, testHomeServerName, &env)
		if status != http.StatusConflict || env.Error.Code != "LINK_PROTOCOL" {
			t.Fatalf("redeem at version %d: status %d code %q, want 409 LINK_PROTOCOL; body: %s",
				theirs, status, env.Error.Code, body)
		}
		if got, want := env.Error.Details["supported"], float64(server.LinkProtocolVersion); got != want {
			t.Errorf("details.supported = %v, want %v", got, want)
		}
		if got, want := env.Error.Details["requested"], float64(theirs); got != want {
			t.Errorf("details.requested = %v, want %v", got, want)
		}
	}

	// The invite survived every one of those: nothing was redeemed.
	if status, body := redeem(t, srv, code, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, nil); status != http.StatusOK {
		t.Fatalf("redeem after a version mismatch: status %d, want 200 (the invite must not "+
			"have been spent); body: %s", status, body)
	}
}

// TestInviteIsRemoteOnly is the acceptance criterion: `remote` is the only role
// an invite can be minted for.
func TestInviteIsRemoteOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	adminID := srv.CreateUser(admin, "second-admin", "adminpass1234", "admin")
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")

	for _, tc := range []struct{ name, id string }{
		{name: "admin", id: adminID},
		{name: "member", id: memberID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env errorEnvelope
			status, body := srv.JSON(http.MethodPost, "/api/v1/users/"+tc.id+"/invite", admin,
				map[string]any{"origins": []string{"https://media.example.org"}}, &env)
			if status != http.StatusUnprocessableEntity || env.Error.Code != "NOT_REMOTE_USER" {
				t.Errorf("mint for a %s: status %d code %q, want 422 NOT_REMOTE_USER; body: %s",
					tc.name, status, env.Error.Code, body)
			}
		})
	}

	if status, _ := srv.JSON(http.MethodPost, "/api/v1/users/no-such-user/invite", admin,
		map[string]any{"origins": []string{"https://media.example.org"}}, nil); status != http.StatusNotFound {
		t.Errorf("mint for an unknown User: status %d, want 404", status)
	}
}

// TestMintingIsAdminOnly: the invite is the sharer's whole disclosure decision,
// so a Member must not be able to mint one for anybody — including for a linked
// Server that already exists.
func TestMintingIsAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)
	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")

	status, body := srv.JSON(http.MethodPost, "/api/v1/users/"+peerID+"/invite", member,
		map[string]any{"origins": []string{"https://media.example.org"}}, nil)
	if status != http.StatusForbidden {
		t.Errorf("a Member minting an invite: status %d, want 403; body: %s", status, body)
	}
	if status, body := srv.JSON(http.MethodPost, "/api/v1/users/"+peerID+"/invite", "",
		map[string]any{"origins": []string{"https://media.example.org"}}, nil); status != http.StatusUnauthorized {
		t.Errorf("an anonymous mint: status %d, want 401; body: %s", status, body)
	}
}

// TestMintRefusesBadOrigins: an origin is a scheme and an authority and nothing
// else. Every one of these becomes an invite that can only fail on somebody
// else's machine, so it is refused here, where the Admin is looking at it.
func TestMintRefusesBadOrigins(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	for _, tc := range []struct {
		name    string
		origins []string
	}{
		{name: "no origins at all", origins: []string{}},
		{name: "a bare hostname", origins: []string{"media.example.org"}},
		{name: "a path prefix", origins: []string{"https://media.example.org/obelo"}},
		{name: "a query", origins: []string{"https://media.example.org?x=1"}},
		{name: "credentials", origins: []string{"https://user:pw@media.example.org"}},
		{name: "the wrong scheme", origins: []string{"ftp://media.example.org"}},
		{name: "empty string", origins: []string{"  "}},
		{name: "one bad among good", origins: []string{"https://ok.example.org", "nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env errorEnvelope
			status, body := srv.JSON(http.MethodPost, "/api/v1/users/"+peerID+"/invite", admin,
				map[string]any{"origins": tc.origins}, &env)
			if status != http.StatusUnprocessableEntity || env.Error.Code != "INVALID_ORIGIN" {
				t.Errorf("status %d code %q, want 422 INVALID_ORIGIN; body: %s",
					status, env.Error.Code, body)
			}
		})
	}
}

// TestMintingReplacesTheOutstandingInvite: over HTTP, the string the Admin sent
// first stops working the moment they mint again (ADR-0055 §1).
func TestMintingReplacesTheOutstandingInvite(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	first := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code
	second := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code
	if first == second {
		t.Fatal("two mints produced the same code")
	}

	var env errorEnvelope
	if status, body := redeem(t, srv, first, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, &env); status != http.StatusBadRequest || env.Error.Code != "INVALID_INVITE" {
		t.Errorf("the superseded invite: status %d code %q, want 400 INVALID_INVITE; body: %s",
			status, env.Error.Code, body)
	}
	if status, body := redeem(t, srv, second, server.LinkProtocolVersion, testHomeServerID, testHomeServerName, nil); status != http.StatusOK {
		t.Errorf("the current invite: status %d, want 200; body: %s", status, body)
	}
}

// TestRedeemingTwiceFromOneServerKeepsOneDevice is ADR-0055 §4 on the wire: a
// re-key from the same home updates the Device row rather than leaving a trail
// of dead ones on the sharer's Users page.
func TestRedeemingTwiceFromOneServerKeepsOneDevice(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	peerID := createRemoteUser(t, srv, admin, testHomeServerName)

	redeemOnce := func(name string) (deviceID, token string) {
		t.Helper()
		code := decodeInvite(t, mintInvite(t, srv, admin, peerID, "https://media.example.org").Invite).Code
		var res struct {
			Token  string `json:"token"`
			Device struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"device"`
		}
		if status, body := redeem(t, srv, code, server.LinkProtocolVersion, testHomeServerID, name, &res); status != http.StatusOK {
			t.Fatalf("redeem: status %d; body: %s", status, body)
		}
		return res.Device.ID, res.Token
	}

	firstDevice, _ := redeemOnce(testHomeServerName)
	secondDevice, token := redeemOnce("Brandon's new server")
	if firstDevice != secondDevice {
		t.Errorf("a re-link created Device %q, want the existing %q", secondDevice, firstDevice)
	}

	var devices struct {
		Devices []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"devices"`
	}
	if status, body := srv.AuthGET("/api/v1/devices", token, &devices); status != http.StatusOK {
		t.Fatalf("list devices: status %d; body: %s", status, body)
	}
	if len(devices.Devices) != 1 {
		t.Fatalf("the linked Server holds %d Devices, want 1", len(devices.Devices))
	}
	if devices.Devices[0].Name != "Brandon's new server" {
		t.Errorf("device name = %q, want the renamed server", devices.Devices[0].Name)
	}
}

// TestHandshakeAdvertisesLinking: a redeeming Server checks the FLAG and the
// VERSION before it posts a code (ADR-0055 §3), so the handshake has to carry
// both.
func TestHandshakeAdvertisesLinking(t *testing.T) {
	srv := testharness.New(t)

	var info struct {
		Features            map[string]bool `json:"features"`
		LinkProtocolVersion int             `json:"linkProtocolVersion"`
	}
	if status, body := srv.GET("/api/v1/server", &info); status != http.StatusOK {
		t.Fatalf("handshake: status %d; body: %s", status, body)
	}
	if !info.Features["serverLinking"] {
		t.Error("features.serverLinking is not advertised, but the linking routes are served")
	}
	if info.LinkProtocolVersion != server.LinkProtocolVersion {
		t.Errorf("linkProtocolVersion = %d, want %d",
			info.LinkProtocolVersion, server.LinkProtocolVersion)
	}
}
