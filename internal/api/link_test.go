package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/server"
	"github.com/goozakdev/obelo-server/internal/tailnet"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the RECEIVING half of linking (.scratch/linked-servers
// issue 06, ADR-0055, ADR-0056 §6), and they are deliberately end-to-end: two
// whole Servers, one minting an invite and one pasting it, talking over real
// HTTP.
//
// That shape is the point. Every previous slice of this feature could be tested
// against one Server, but a Link is by definition the thing two of them have,
// and the failures worth catching — a redemption the sharer refuses, an identity
// that does not match, a version the two disagree about — are all failures of the
// space BETWEEN them. A mock of the sharing side here would be a mock of code
// that lives twenty files away in the same repository, and would agree with
// whatever this side happened to do.
//
// The unit-level cases (parse failures, the dialer choice, the origin walk) are
// in internal/link, where the peer can be misconfigured at will.

type linkResp struct {
	ID           string   `json:"id"`
	ServerID     string   `json:"serverId"`
	ServerName   string   `json:"serverName"`
	State        string   `json:"state"`
	ActiveOrigin string   `json:"activeOrigin"`
	Origins      []string `json:"origins"`
	LastSyncedAt *string  `json:"lastSyncedAt"`
	LastError    string   `json:"lastError"`
	Libraries    []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"libraries"`
}

// sharing stands up a Server that has a `remote` User ready to be linked to, and
// hands back the Server, its Admin token and that User's id.
func sharing(t *testing.T, opts ...testharness.Option) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t, opts...)
	admin := adminToken(t, srv)
	return srv, admin, createRemoteUser(t, srv, admin, "Brandon's household")
}

// postLink pastes an invite into a home Server.
func postLink(t *testing.T, home *testharness.Server, adminTok, invite string, out any) (int, []byte) {
	t.Helper()
	return home.JSON(http.MethodPost, "/api/v1/links", adminTok,
		map[string]any{"invite": invite}, out)
}

// reencodeInvite rewrites one field of an invite and re-renders the string,
// keeping the CODE untouched. It is how a test puts the two Servers on different
// protocol versions, or backdates an expiry, without a second build.
func reencodeInvite(t *testing.T, invite string, mutate func(map[string]any)) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(invite[len("obelo-link:"):])
	if err != nil {
		t.Fatalf("decoding invite: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoding invite payload: %v", err)
	}
	mutate(payload)
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("re-encoding invite: %v", err)
	}
	return "obelo-link:" + base64.RawURLEncoding.EncodeToString(out)
}

// remoteUserLastSeen reports the sharer's view of whether a linked Server has
// ever redeemed: GET /users omits lastSeenAt entirely for a User with no Device
// (user_handlers.go), which makes it the one observable answer to "is there a
// Device for that household over there".
func remoteUserLastSeen(t *testing.T, sharer *testharness.Server, adminTok, userID string) (string, bool) {
	t.Helper()
	var out struct {
		Users []struct {
			ID         string `json:"id"`
			LastSeenAt string `json:"lastSeenAt"`
		} `json:"users"`
	}
	if status, body := sharer.AuthGET("/api/v1/users", adminTok, &out); status != http.StatusOK {
		t.Fatalf("listing users on the sharer: status %d; body: %s", status, body)
	}
	for _, u := range out.Users {
		if u.ID == userID {
			return u.LastSeenAt, u.LastSeenAt != ""
		}
	}
	t.Fatalf("the remote User %q is gone from the sharer's roster", userID)
	return "", false
}

// TestTwoServersLink is the whole issue in one test: a sharing Admin mints one
// string, a home Admin pastes it, and from then on this Server holds a
// credential for that one.
func TestTwoServersLink(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); linked {
		t.Fatal("the remote User has a Device before anybody redeemed its invite")
	}

	var sharerInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if status, body := sharer.GET("/api/v1/server", &sharerInfo); status != http.StatusOK {
		t.Fatalf("sharer handshake: status %d; body: %s", status, body)
	}
	var homeInfo struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if status, body := home.GET("/api/v1/server", &homeInfo); status != http.StatusOK {
		t.Fatalf("home handshake: status %d; body: %s", status, body)
	}

	invite := mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite

	var got linkResp
	status, body := postLink(t, home, homeAdmin, invite, &got)
	if status != http.StatusCreated {
		t.Fatalf("POST /links: status %d, want 201; body: %s", status, body)
	}
	if got.ServerID != sharerInfo.ID {
		t.Errorf("link serverId = %q, want the sharer's identity %q", got.ServerID, sharerInfo.ID)
	}
	if got.ServerName != sharerInfo.Name {
		t.Errorf("link serverName = %q, want %q", got.ServerName, sharerInfo.Name)
	}
	if got.State != "connected" {
		t.Errorf("link state = %q, want connected", got.State)
	}
	if got.ActiveOrigin != sharer.URL("") {
		t.Errorf("activeOrigin = %q, want %q", got.ActiveOrigin, sharer.URL(""))
	}
	if got.LastSyncedAt != nil {
		t.Errorf("lastSyncedAt = %v on a Link nothing has synced yet, want null", *got.LastSyncedAt)
	}
	// Issue 07's seam: the mirror does not exist, so the list is EMPTY rather than
	// absent. The field's shape does not change when issue 07 fills it in.
	if got.Libraries == nil || len(got.Libraries) != 0 {
		t.Errorf("libraries = %v, want an empty list until issue 07 lands", got.Libraries)
	}
	// THE TOKEN MUST NOT BE ON THE WIRE. It is an outbound credential against
	// somebody else's Server, held here the way a provider key is.
	if bodyHasToken(body) {
		t.Errorf("the link response carries a token-shaped field: %s", body)
	}

	// The sharer now has a Device for this household (ADR-0055 §4).
	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); !linked {
		t.Error("the sharer records no Device for the linked Server after a successful redemption")
	}

	// And GET /links reads back what POST returned.
	var list []linkResp
	if status, body := home.AuthGET("/api/v1/links", homeAdmin, &list); status != http.StatusOK {
		t.Fatalf("GET /links: status %d; body: %s", status, body)
	}
	if len(list) != 1 || list[0].ID != got.ID || list[0].ActiveOrigin != got.ActiveOrigin {
		t.Fatalf("GET /links = %+v, want the one Link POST created", list)
	}
}

// bodyHasToken looks for the credential anywhere in a response. A field test
// would only catch the field somebody thought of; this catches a nested one too.
func bodyHasToken(body []byte) bool {
	var any map[string]json.RawMessage
	if err := json.Unmarshal(body, &any); err != nil {
		return false
	}
	for k := range any {
		if k == "token" || k == "bearer" || k == "credential" {
			return true
		}
	}
	return false
}

// TestPastingSomethingThatIsNotAnInvite: one answer for every shape failure,
// because the operator's move is the same in all of them.
func TestPastingSomethingThatIsNotAnInvite(t *testing.T) {
	home := testharness.New(t)
	admin := adminToken(t, home)

	for _, tc := range []struct{ name, invite string }{
		{"empty", ""},
		{"a bare URL", "https://media.example.org"},
		{"the scheme with nothing after it", "obelo-link:"},
		{"a truncated paste", "obelo-link:eyJ2IjoxLCJpZCI6"},
		{"a sentence", "hey here's my server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errBody errorEnvelope
			status, body := postLink(t, home, admin, tc.invite, &errBody)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body: %s", status, body)
			}
			if errBody.Error.Code != "BAD_INVITE" {
				t.Errorf("code = %q, want BAD_INVITE; body: %s", errBody.Error.Code, body)
			}
		})
	}
}

// TestAnExpiredInviteIsGoneNotBad: a well-formed string whose 24 hours ran out
// (ADR-0055 §1) answers 410, because nothing about the request was wrong and the
// operator's move is "ask for a fresh one" rather than "re-copy this one".
func TestAnExpiredInviteIsGoneNotBad(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	invite := mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite
	stale := reencodeInvite(t, invite, func(p map[string]any) {
		p["exp"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	})

	var errBody errorEnvelope
	status, body := postLink(t, home, homeAdmin, stale, &errBody)
	if status != http.StatusGone {
		t.Fatalf("status %d, want 410; body: %s", status, body)
	}
	if errBody.Error.Code != "INVITE_EXPIRED" {
		t.Errorf("code = %q, want INVITE_EXPIRED", errBody.Error.Code)
	}
	// The invite is refused HERE, without dialing, so nothing over there moved.
	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); linked {
		t.Error("an expired invite reached the sharer and created a Device")
	}
}

// TestAVersionMismatchNeverSpendsTheCode is ADR-0055 §3's promise, end to end:
// the refusal names which side needs an upgrade, and the SAME invite still
// redeems once the disagreement is gone. That last step is the whole assertion —
// a check that burned the code would leave the Admin re-minting for no reason.
func TestAVersionMismatchNeverSpendsTheCode(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	invite := mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite

	for _, tc := range []struct {
		name    string
		version int
		upgrade string
	}{
		{"the other server is ahead", server.LinkProtocolVersion + 1, "ours"},
		{"the other server is behind", server.LinkProtocolVersion + 1000, "ours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mismatched := reencodeInvite(t, invite, func(p map[string]any) { p["v"] = tc.version })
			var errBody errorEnvelope
			status, body := postLink(t, home, homeAdmin, mismatched, &errBody)
			if status != http.StatusConflict {
				t.Fatalf("status %d, want 409; body: %s", status, body)
			}
			if errBody.Error.Code != "LINK_PROTOCOL" {
				t.Fatalf("code = %q, want LINK_PROTOCOL", errBody.Error.Code)
			}
			// Named from the ASKING side, and `upgrade` says outright which of
			// ADR-0055 §3's two sentences to show, so no client compares the numbers.
			if theirs, _ := errBody.Error.Details["theirs"].(float64); int(theirs) != tc.version {
				t.Errorf("details.theirs = %v, want %d", errBody.Error.Details["theirs"], tc.version)
			}
			if ours, _ := errBody.Error.Details["ours"].(float64); int(ours) != server.LinkProtocolVersion {
				t.Errorf("details.ours = %v, want %d", errBody.Error.Details["ours"], server.LinkProtocolVersion)
			}
			if errBody.Error.Details["upgrade"] != tc.upgrade {
				t.Errorf("details.upgrade = %v, want %q", errBody.Error.Details["upgrade"], tc.upgrade)
			}
		})
	}

	// THE POINT: the untouched invite still works.
	var got linkResp
	if status, body := postLink(t, home, homeAdmin, invite, &got); status != http.StatusCreated {
		t.Fatalf("after the refusals, the original invite gave status %d, want 201; body: %s", status, body)
	}
	if got.State != "connected" {
		t.Errorf("state = %q after a redemption that should have succeeded", got.State)
	}
}

// TestOriginsAreTriedInOrderOverHTTP: the dead address is FIRST, so a walk that
// reordered would pass by accident (ADR-0055 §2).
func TestOriginsAreTriedInOrderOverHTTP(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	// A listener that is closed: connecting is refused at once, which is what a
	// friend's port-forward looks like with their router off.
	dead := testharness.New(t)
	deadOrigin := dead.URL("")
	dead.Close()

	invite := mintInvite(t, sharer, sharerAdmin, remoteID, deadOrigin, sharer.URL("")).Invite

	var got linkResp
	status, body := postLink(t, home, homeAdmin, invite, &got)
	if status != http.StatusCreated {
		t.Fatalf("POST /links: status %d, want 201; body: %s", status, body)
	}
	if got.ActiveOrigin != sharer.URL("") {
		t.Errorf("activeOrigin = %q, want the address that answered", got.ActiveOrigin)
	}
	// The dead one is KEPT: it is the friend's router coming back tomorrow.
	if len(got.Origins) != 2 || got.Origins[0] != deadOrigin || got.Origins[1] != sharer.URL("") {
		t.Errorf("origins = %v, want both, in the invite's order", got.Origins)
	}
}

// TestNoOriginAnswers is ADR-0056 §6's `unreachable` arriving at link time: a
// 503 rather than a 4xx, because nothing about the request was wrong.
func TestNoOriginAnswers(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	invite := mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite
	sharer.Close()

	var errBody errorEnvelope
	status, body := postLink(t, home, homeAdmin, invite, &errBody)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body: %s", status, body)
	}
	if errBody.Error.Code != "LINK_UNREACHABLE" {
		t.Errorf("code = %q, want LINK_UNREACHABLE", errBody.Error.Code)
	}
	_ = remoteID
}

// TestLinkingOverATailnetOrigin is ADR-0055 §5's second dialer, end to end. The
// origin in the invite is a MagicDNS name that resolves NOWHERE on this machine
// — which is exactly the production situation for a machine a friend shared into
// the operator's Tailnet from the Tailscale console — and the only thing that
// makes it reachable is this Server's own Tailnet node.
func TestLinkingOverATailnetOrigin(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)

	// The home Server's node: up, with a MagicDNS name of its own, dialing into
	// the sharer's listener the way tsnet would dial a shared machine.
	st := tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}
	node := &tailnet.Fake{Fresh: st, Returning: st}
	sharerAddr := sharerListenerAddr(t, sharer)
	node.DialFunc = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, sharerAddr)
	}

	home := testharness.New(t,
		testharness.WithTailnet(node),
		testharness.WithTailnetEnabled(true))
	homeAdmin := adminToken(t, home)

	// The sharer's dialog pre-fills the MagicDNS origin when its own node is
	// connected (ADR-0055 §2); here the Admin simply typed it.
	const tailnetOrigin = "http://obelo.tailf00d.ts.net"
	invite := mintInvite(t, sharer, sharerAdmin, remoteID, tailnetOrigin).Invite

	var got linkResp
	status, body := postLink(t, home, homeAdmin, invite, &got)
	if status != http.StatusCreated {
		t.Fatalf("POST /links over a tailnet origin: status %d, want 201; body: %s", status, body)
	}
	if got.ActiveOrigin != tailnetOrigin {
		t.Errorf("activeOrigin = %q, want %q", got.ActiveOrigin, tailnetOrigin)
	}
	// The assertion that says it went over the Tailnet and not the operating
	// system: nothing else could have resolved that name.
	if node.Dials() == 0 {
		t.Fatal("the Tailnet node was never dialed; that origin cannot have been reached any other way")
	}
	for _, addr := range node.DialAddrs() {
		if addr != "obelo.tailf00d.ts.net:80" {
			t.Errorf("the node was asked for %q, want the origin's host and port", addr)
		}
	}
	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); !linked {
		t.Error("the sharer records no Device after a link made over the Tailnet")
	}
}

// sharerListenerAddr is the host:port the test Server is actually listening on,
// which is what a dialer has to be pointed at once the NAME is a fiction.
func sharerListenerAddr(t *testing.T, srv *testharness.Server) string {
	t.Helper()
	u := srv.URL("")
	addr := u[len("http://"):]
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("test server URL %q is not host:port after the scheme: %v", u, err)
	}
	return addr
}

// TestASecondInviteFromTheSameServerRekeysInPlace is why the server id is in the
// string (ADR-0055 §2): the friend at a new address with a fresh code is the
// SAME Link, so whatever the mirror has built behind it survives.
func TestASecondInviteFromTheSameServerRekeysInPlace(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	var first linkResp
	invite := mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite
	if status, body := postLink(t, home, homeAdmin, invite, &first); status != http.StatusCreated {
		t.Fatalf("first link: status %d; body: %s", status, body)
	}

	// A fresh invite for the same `remote` User — which is exactly what an Admin
	// does when a Link has gone revoked, or when the address changed.
	second := mintInvite(t, sharer, sharerAdmin, remoteID,
		"https://media.example.org", sharer.URL("")).Invite

	var again linkResp
	// 200, not 201: the relationship already existed and was updated.
	if status, body := postLink(t, home, homeAdmin, second, &again); status != http.StatusOK {
		t.Fatalf("re-key through POST /links: status %d, want 200; body: %s", status, body)
	}
	if again.ID != first.ID {
		t.Errorf("link id changed on re-key: %q → %q; the mirror behind it would be orphaned",
			first.ID, again.ID)
	}
	if len(again.Origins) != 2 {
		t.Errorf("origins = %v, want the new invite's two", again.Origins)
	}

	var list []linkResp
	if status, body := home.AuthGET("/api/v1/links", homeAdmin, &list); status != http.StatusOK {
		t.Fatalf("GET /links: status %d; body: %s", status, body)
	}
	if len(list) != 1 {
		t.Errorf("%d links on file, want 1 — a re-key must not fork the relationship", len(list))
	}
}

// TestRekeyEndpointRequiresTheSameServer: /links/{id}/rekey is addressed by a
// Link and refuses an invite that names a different Server, so an operator with
// two friends' invites in a clipboard cannot repoint one household at the other.
func TestRekeyEndpointRequiresTheSameServer(t *testing.T) {
	amy, amyAdmin, amyRemote := sharing(t)
	bob, bobAdmin, bobRemote := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	var link linkResp
	if status, body := postLink(t, home, homeAdmin,
		mintInvite(t, amy, amyAdmin, amyRemote, amy.URL("")).Invite, &link); status != http.StatusCreated {
		t.Fatalf("linking to amy: status %d; body: %s", status, body)
	}

	var errBody errorEnvelope
	status, body := home.JSON(http.MethodPost, "/api/v1/links/"+link.ID+"/rekey", homeAdmin,
		map[string]any{"invite": mintInvite(t, bob, bobAdmin, bobRemote, bob.URL("")).Invite}, &errBody)
	if status != http.StatusConflict {
		t.Fatalf("re-keying with another server's invite: status %d, want 409; body: %s", status, body)
	}
	if errBody.Error.Code != "LINK_SERVER_MISMATCH" {
		t.Errorf("code = %q, want LINK_SERVER_MISMATCH", errBody.Error.Code)
	}

	// And the right one still works, on the same route.
	var rekeyed linkResp
	if status, body := home.JSON(http.MethodPost, "/api/v1/links/"+link.ID+"/rekey", homeAdmin,
		map[string]any{"invite": mintInvite(t, amy, amyAdmin, amyRemote, amy.URL("")).Invite},
		&rekeyed); status != http.StatusOK {
		t.Fatalf("re-keying with the right invite: status %d, want 200; body: %s", status, body)
	}
	if rekeyed.ID != link.ID || rekeyed.State != "connected" {
		t.Errorf("re-key produced %+v, want the same Link, connected", rekeyed)
	}
}

// TestUnlinkLeavesNoDeviceOnTheSharer is ADR-0055's Consequences: the sharer's
// Device row disappears instead of lingering as a ghost with a last-seen.
func TestUnlinkLeavesNoDeviceOnTheSharer(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	var link linkResp
	if status, body := postLink(t, home, homeAdmin,
		mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite, &link); status != http.StatusCreated {
		t.Fatalf("linking: status %d; body: %s", status, body)
	}
	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); !linked {
		t.Fatal("no Device on the sharer after linking")
	}

	if status, body := home.JSON(http.MethodDelete, "/api/v1/links/"+link.ID, homeAdmin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE /links/{id}: status %d, want 204; body: %s", status, body)
	}

	if _, linked := remoteUserLastSeen(t, sharer, sharerAdmin, remoteID); linked {
		t.Error("the sharer still lists a Device for this household after the unlink")
	}
	var list []linkResp
	if status, _ := home.AuthGET("/api/v1/links", homeAdmin, &list); status != http.StatusOK || len(list) != 0 {
		t.Errorf("GET /links after unlinking = %d / %+v, want 200 and nothing", status, list)
	}
}

// TestUnlinkSucceedsWithAnUnreachableSharer is the half that would be got wrong.
// A friend whose server is off — often the very reason to unlink — must not be
// able to keep this household linked to them.
func TestUnlinkSucceedsWithAnUnreachableSharer(t *testing.T) {
	sharer, sharerAdmin, remoteID := sharing(t)
	home := testharness.New(t)
	homeAdmin := adminToken(t, home)

	var link linkResp
	if status, body := postLink(t, home, homeAdmin,
		mintInvite(t, sharer, sharerAdmin, remoteID, sharer.URL("")).Invite, &link); status != http.StatusCreated {
		t.Fatalf("linking: status %d; body: %s", status, body)
	}
	sharer.Close()

	if status, body := home.JSON(http.MethodDelete, "/api/v1/links/"+link.ID, homeAdmin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE /links/{id} against an unreachable sharer: status %d, want 204; body: %s", status, body)
	}
	var list []linkResp
	if status, _ := home.AuthGET("/api/v1/links", homeAdmin, &list); status != http.StatusOK || len(list) != 0 {
		t.Errorf("GET /links after unlinking = %d / %+v, want 200 and nothing", status, list)
	}
	// An unknown Link is a plain 404, and unlinking twice is not special.
	if status, _ := home.JSON(http.MethodDelete, "/api/v1/links/"+link.ID, homeAdmin, nil, nil); status != http.StatusNotFound {
		t.Errorf("unlinking twice: status %d, want 404", status)
	}
}

// TestLinkRoutesAreAdminOnly. A Link is the household's relationship with
// another household; a Member neither makes one nor sees the addresses in it.
func TestLinkRoutesAreAdminOnly(t *testing.T) {
	home := testharness.New(t)
	adminToken(t, home)
	home.CreateMember("kid", "correct horse battery staple")
	member := home.LoginAs("kid", "correct horse battery staple")

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"list", http.MethodGet, "/api/v1/links", nil},
		{"create", http.MethodPost, "/api/v1/links", map[string]any{"invite": "obelo-link:x"}},
		{"rekey", http.MethodPost, "/api/v1/links/whatever/rekey", map[string]any{"invite": "obelo-link:x"}},
		{"unlink", http.MethodDelete, "/api/v1/links/whatever", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, body := home.JSON(tc.method, tc.path, member, tc.body, nil); status != http.StatusForbidden {
				t.Errorf("as a Member: status %d, want 403; body: %s", status, body)
			}
			if status, body := home.JSON(tc.method, tc.path, "", tc.body, nil); status != http.StatusUnauthorized {
				t.Errorf("anonymous: status %d, want 401; body: %s", status, body)
			}
		})
	}
}
