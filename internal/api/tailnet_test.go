package api_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/tailnet"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Admin-scope Tailnet remote-access black-box tests (ADR-0043): the five
// endpoints, the connect/disconnect/forget state machine, the first-boot env
// seeding and the handoff to the DB, the admin-only event, and the two properties
// that fail SILENTLY if they regress — the join key never surfacing anywhere, and
// a node that cannot start never taking the server down with it.
//
// Everything runs against tailnet.Fake: no coordination server, no network, and
// nothing in go.mod.

const tailnetPath = "/api/v1/settings/tailscale"

// --- Wire shape -------------------------------------------------------------

type tailnetStatusView struct {
	State     string   `json:"state"`
	FQDN      string   `json:"fqdn"`
	Addresses []string `json:"addresses"`
	// KeyExpiry is a POINTER here on purpose: the test needs to tell "no expiry"
	// (null) from "expires at T", which is the distinction the feature exists to
	// keep and the one a plain string would quietly collapse.
	KeyExpiry *string `json:"keyExpiry"`
	LoginURL  string  `json:"loginURL"`
	LastError string  `json:"lastError"`
	// HTTPSBound / HTTPSError are the OUTCOME of the httpsEnabled setting above:
	// whether tailnet :443 is accepting connections right now, and why not.
	HTTPSBound bool   `json:"httpsBound"`
	HTTPSError string `json:"httpsError"`
}

type tailnetView struct {
	Enabled      bool              `json:"enabled"`
	Hostname     string            `json:"hostname"`
	ControlURL   string            `json:"controlURL"`
	HTTPSEnabled bool              `json:"httpsEnabled"`
	Status       tailnetStatusView `json:"status"`
}

func getTailnet(t *testing.T, srv *testharness.Server, token string) (tailnetView, []byte) {
	t.Helper()
	var v tailnetView
	status, body := srv.AuthGET(tailnetPath, token, &v)
	if status != http.StatusOK {
		t.Fatalf("GET tailnet = %d, want 200; body: %s", status, body)
	}
	return v, body
}

// postTailnet drives one of the three verbs and returns the resulting view.
func postTailnet(t *testing.T, srv *testharness.Server, token, verb string) tailnetView {
	t.Helper()
	var v tailnetView
	status, body := srv.JSON(http.MethodPost, tailnetPath+"/"+verb, token, nil, &v)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d, want 200; body: %s", verb, status, body)
	}
	return v
}

// --- Off by default ---------------------------------------------------------

// TestTailnetOffByDefaultIsInert: the shipped posture. With the feature untouched
// the node is never started, no state directory exists, and the settings still
// answer — a screen that can explain an off feature is the point of the GET.
func TestTailnetOffByDefaultIsInert(t *testing.T) {
	node := &tailnet.Fake{}
	srv := testharness.New(t, testharness.WithTailnet(node))
	token := adminToken(t, srv)

	v, _ := getTailnet(t, srv, token)
	if v.Enabled {
		t.Error("remote access is enabled by default; it must be opt-in (ADR-0043)")
	}
	if v.Status.State != "stopped" {
		t.Errorf("state = %q, want %q", v.Status.State, "stopped")
	}
	if node.Starts() != 0 {
		t.Errorf("the node was started %d time(s) on a server that never enabled it", node.Starts())
	}
	if _, err := os.Stat(filepath.Join(srv.DataDir, "tailscale")); !os.IsNotExist(err) {
		t.Errorf("a state directory exists with the feature off (stat err = %v)", err)
	}
	// The hostname is seeded from the Server's display name, sanitized to a DNS
	// label — so the operator has something sensible to accept when they do turn it
	// on, resolved ONCE rather than tracking a cosmetic rename forever.
	if v.Hostname == "" {
		t.Error("no MagicDNS hostname was seeded")
	}
}

// --- Admin-only -------------------------------------------------------------

// TestTailnetAdminOnly: remote access is the operator's configuration. A Member
// gets the same refusal on all five routes as on any other admin route.
func TestTailnetAdminOnly(t *testing.T) {
	srv := testharness.New(t, testharness.WithTailnet(&tailnet.Fake{}))
	adminTok := adminToken(t, srv)
	srv.CreateUser(adminTok, "bob", "correct horse battery staple", "member")
	member := srv.LoginAs("bob", "correct horse battery staple")

	if status, _ := srv.AuthGET(tailnetPath, member, nil); status != http.StatusForbidden {
		t.Errorf("member GET = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPut, tailnetPath, member, map[string]any{"enabled": true}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT = %d, want 403", status)
	}
	for _, verb := range []string{"connect", "disconnect", "forget"} {
		if status, _ := srv.JSON(http.MethodPost, tailnetPath+"/"+verb, member, nil, nil); status != http.StatusForbidden {
			t.Errorf("member POST %s = %d, want 403", verb, status)
		}
	}
	// And the Member's refusal is not a side effect of the node being absent: an
	// unauthenticated caller is refused too.
	if status, _ := srv.AuthGET(tailnetPath, "", nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous GET = %d, want 401", status)
	}
}

// --- First-boot seeding and the handoff -------------------------------------

// TestTailnetEnvSeedsFirstBootOnly covers BOTH directions of the source-of-truth
// handoff over two real boots of one install: the OBELO_TAILSCALE_* values seed a
// fresh database, and after the UI changes them a later boot with DIFFERENT env
// values leaves the operator's choice alone.
//
// The second direction is the one that fails silently: an env var that keeps
// winning looks like nothing at all until somebody notices their saved settings
// revert on every restart, months later, with no error anywhere.
func TestTailnetEnvSeedsFirstBootOnly(t *testing.T) {
	dir := t.TempDir()

	first := testharness.New(t,
		testharness.WithDataDir(dir),
		testharness.WithTailnet(&tailnet.Fake{}),
		testharness.WithTailnetHostname("from-env"),
		testharness.WithTailnetControlURL("https://headscale.example.com"),
		testharness.WithTailnetHTTPS(true),
	)
	token := adminToken(t, first)

	v, _ := getTailnet(t, first, token)
	if v.Hostname != "from-env" || v.ControlURL != "https://headscale.example.com" || !v.HTTPSEnabled {
		t.Fatalf("first boot did not seed from the environment: %+v", v)
	}

	// The Admin changes their mind in the UI.
	var saved tailnetView
	status, body := first.JSON(http.MethodPut, tailnetPath, token, map[string]any{
		"hostname":     "chosen-in-ui",
		"controlURL":   "",
		"httpsEnabled": false,
	}, &saved)
	if status != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body: %s", status, body)
	}
	if saved.Hostname != "chosen-in-ui" || saved.ControlURL != "" || saved.HTTPSEnabled {
		t.Fatalf("PUT did not apply: %+v", saved)
	}
	first.Close()

	// A restart of the SAME install, with the environment still set — and now
	// saying something else again.
	second := testharness.New(t,
		testharness.WithDataDir(dir),
		testharness.WithTailnet(&tailnet.Fake{}),
		testharness.WithTailnetHostname("second-boot-env"),
		testharness.WithTailnetControlURL("https://elsewhere.example.com"),
		testharness.WithTailnetHTTPS(true),
	)
	token2 := second.LoginAs("brandon", "hunter2hunter2")

	v2, _ := getTailnet(t, second, token2)
	if v2.Hostname != "chosen-in-ui" {
		t.Errorf("hostname after the second boot = %q, want %q — the DB is authoritative and the env is ignored at runtime",
			v2.Hostname, "chosen-in-ui")
	}
	if v2.ControlURL != "" || v2.HTTPSEnabled {
		t.Errorf("second boot = %+v, want the UI's cleared control URL and HTTPS off", v2)
	}
}

// --- The join key -----------------------------------------------------------

// TestTailnetAuthKeyNeverSurfaces is the credential test, and every part of it
// guards a silent failure: a key that leaks into the database, into a log an
// operator will paste into an issue, or into an API response is a disclosure
// nobody would notice until somebody else has their Tailnet.
//
// It puts a known key through a real join — so the join genuinely received it —
// and then goes looking for it in every place it must not be.
func TestTailnetAuthKeyNeverSurfaces(t *testing.T) {
	const key = "tskey-auth-obelo-test-do-not-persist"

	// Capture everything the server logs across the join. Restored before the
	// assertions, so no goroutine can still be writing when the buffer is read.
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	restored := false
	restore := func() {
		if !restored {
			log.SetOutput(prev)
			restored = true
		}
	}
	defer restore()

	node := &tailnet.Fake{Returning: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}}
	srv := testharness.New(t,
		testharness.WithTailnet(node),
		testharness.WithTailnetAuthKey(key),
	)
	token := adminToken(t, srv)
	postTailnet(t, srv, token, "connect")
	_, body := getTailnet(t, srv, token)
	restore()

	// It really did reach the join — otherwise this test proves only that a value
	// nobody used cannot leak.
	if got := node.LastConfig().AuthKey; got != key {
		t.Fatalf("the join was given AuthKey %q, want %q", got, key)
	}
	if strings.Contains(string(body), key) {
		t.Errorf("the GET payload carries the join key: %s", body)
	}
	if strings.Contains(logs.String(), key) {
		t.Errorf("the join key was logged:\n%s", logs.String())
	}
	// No table, and no file under the data directory either — which covers the
	// SQLite database, its write-ahead log, and the node's own state directory.
	if hit := findInDataDir(t, srv.DataDir, key); hit != "" {
		t.Errorf("the join key was persisted in %s", hit)
	}
	// Belt and braces on the redaction that makes this structural rather than a
	// promise: a Config that is logged wholesale must not disclose the key.
	if s := node.LastConfig().String(); strings.Contains(s, key) {
		t.Errorf("tailnet.Config.String() disclosed the key: %s", s)
	}
}

// findInDataDir returns the first file under root whose bytes contain needle, or
// "" — the crude, honest form of "appears in no table".
func findInDataDir(t *testing.T, root, needle string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil //nolint:nilerr // an unreadable entry is not what this test is about
		}
		b, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(b, []byte(needle)) {
			found = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the data dir: %v", err)
	}
	return found
}

// --- The state machine over HTTP --------------------------------------------

// TestTailnetConnectDisconnectReconnect: all three with no restart, and the
// second connect does NOT re-authenticate — which is the whole reason Disconnect
// keeps the state directory.
func TestTailnetConnectDisconnectReconnect(t *testing.T) {
	node := &tailnet.Fake{
		Fresh:     tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/deadbeef"},
		Returning: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"},
	}
	srv := testharness.New(t, testharness.WithTailnet(node))
	token := adminToken(t, srv)

	v := postTailnet(t, srv, token, "connect")
	if !v.Enabled {
		t.Error("connect did not persist the desired state; a restart would strand the operator outside")
	}
	if v.Status.State != "needsLogin" || v.Status.LoginURL == "" {
		t.Fatalf("first connect = %+v, want needsLogin carrying a login URL", v.Status)
	}
	// The Admin completes the login in their own browser, as the interactive join
	// requires; the node reports the transition on its own.
	node.Transition(tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"})
	if v, _ = getTailnet(t, srv, token); v.Status.State != "running" || v.Status.FQDN != "obelo.tail1a2b.ts.net" {
		t.Fatalf("after login = %+v, want running with the FQDN", v.Status)
	}

	v = postTailnet(t, srv, token, "disconnect")
	if v.Enabled || v.Status.State != "stopped" {
		t.Fatalf("disconnect = %+v, want disabled and stopped", v)
	}
	if _, err := os.Stat(filepath.Join(srv.DataDir, "tailscale")); err != nil {
		t.Fatalf("disconnect wiped the node state (%v); that is Forget's job, and this reconnect is meant to be instant", err)
	}

	v = postTailnet(t, srv, token, "connect")
	if v.Status.State != "running" {
		t.Errorf("reconnect = %+v, want running straight away", v.Status)
	}
	if node.Joins() != 1 {
		t.Errorf("the node authenticated %d times; a disconnect→connect must not re-authenticate", node.Joins())
	}
}

// TestTailnetForgetWipesStateAndRejoins: Forget is the other off switch, and it
// means something different — the next connect is a fresh join.
func TestTailnetForgetWipesStateAndRejoins(t *testing.T) {
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/1"}}
	srv := testharness.New(t, testharness.WithTailnet(node))
	token := adminToken(t, srv)
	stateDir := filepath.Join(srv.DataDir, "tailscale")

	postTailnet(t, srv, token, "connect")
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("a join left no state directory: %v", err)
	}

	v := postTailnet(t, srv, token, "forget")
	if v.Enabled || v.Status.State != "stopped" {
		t.Errorf("forget = %+v, want disabled and stopped", v)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("forget left the state directory behind (stat err = %v)", err)
	}

	postTailnet(t, srv, token, "connect")
	if node.Joins() != 2 {
		t.Errorf("joins = %d, want 2 — the connect after a forget must be a fresh join", node.Joins())
	}
}

// --- Key expiry -------------------------------------------------------------

// TestTailnetKeyExpiryNullVsSet pins the nil case on the wire. "No expiry" is a
// tagged node — the good state — and it must not be renderable as a date, or the
// ~14-day warning fires forever on exactly the nodes that never lapse.
func TestTailnetKeyExpiryNullVsSet(t *testing.T) {
	node := &tailnet.Fake{Returning: tailnet.Status{State: tailnet.StateRunning}}
	srv := testharness.New(t, testharness.WithTailnet(node))
	token := adminToken(t, srv)

	postTailnet(t, srv, token, "connect")
	v, body := getTailnet(t, srv, token)
	if v.Status.KeyExpiry != nil {
		t.Errorf("keyExpiry = %v, want null (no expiry)", *v.Status.KeyExpiry)
	}
	// The field must be PRESENT and null, not absent: an omitted field is
	// indistinguishable from a server that forgot to send one.
	if !strings.Contains(string(body), `"keyExpiry":null`) {
		t.Errorf("no explicit null keyExpiry in the payload: %s", body)
	}

	expiry := time.Date(2026, 12, 25, 10, 30, 0, 0, time.UTC)
	node.Transition(tailnet.Status{State: tailnet.StateRunning, KeyExpiry: &expiry})
	v, _ = getTailnet(t, srv, token)
	if v.Status.KeyExpiry == nil || *v.Status.KeyExpiry != "2026-12-25T10:30:00Z" {
		t.Errorf("keyExpiry = %v, want 2026-12-25T10:30:00Z", v.Status.KeyExpiry)
	}
}

// --- What was asked for vs what was achieved --------------------------------

// TestTailnetHTTPSBoundIsSeparateFromHTTPSEnabled is the wire half of
// tailscale/06. `httpsEnabled` is a SETTING — the operator's request, saved and
// answered with success. `status.httpsBound` is an OBSERVATION — whether tailnet
// :443 came up, which needs MagicDNS and HTTPS certificates enabled in the
// Tailscale console, neither of which this server can set or detect ahead of time.
//
// They come apart in exactly the case this feature is most likely to be
// misconfigured in, and a client that reads the first as though it were the second
// tells the operator to use an `https://` address that refuses connections. So the
// payload has to carry both, and the failure has to reach the client BYTE FOR
// BYTE: the server's message names both console settings, which is the entire fix,
// and a paraphrase loses it.
//
// The bind itself happens in cmd/obelo — the listener supervisor is the only thing
// that can observe it — so this test plays that part through the same method the
// supervisor calls in production and then asks the endpoint what an operator would
// be shown.
func TestTailnetHTTPSBoundIsSeparateFromHTTPSEnabled(t *testing.T) {
	// The message the server actually composes, in its own words, with the
	// punctuation and the arrows that make it a paragraph rather than a token.
	const failure = `There is no HTTPS on the Tailnet for "obelo.tail1a2b.ts.net": ` +
		`tsnet: you must enable MagicDNS in the DNS page of the admin panel to proceed — ` +
		`Tailnet HTTPS needs TWO settings in the TAILSCALE ADMIN CONSOLE: ` +
		`DNS -> MagicDNS enabled, and DNS -> HTTPS Certificates enabled.`

	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}}
	srv := testharness.New(t,
		testharness.WithTailnet(node),
		testharness.WithTailnetHTTPS(true),
	)
	token := adminToken(t, srv)
	postTailnet(t, srv, token, "connect")

	// The request is on and the outcome has not happened. This is the combination
	// the ticket exists for.
	v, body := getTailnet(t, srv, token)
	if !v.HTTPSEnabled {
		t.Fatalf("httpsEnabled = false; this test proves nothing unless the request is on: %s", body)
	}
	if v.Status.HTTPSBound {
		t.Errorf("httpsBound = true with nothing bound — the payload is echoing the setting, and a client "+
			"that believes it will display an https:// address that refuses connections: %s", body)
	}
	// PRESENT and false, not absent: a field that vanishes when it is false cannot
	// be told from a server that never sends it, and false is the whole point.
	if !strings.Contains(string(body), `"httpsBound":false`) {
		t.Errorf("no explicit httpsBound in the payload: %s", body)
	}

	// The supervisor reports what it observed. Both directions cross the wire.
	srv.Tailnet().ReportHTTPS(false, failure)
	v, body = getTailnet(t, srv, token)
	if v.Status.HTTPSBound {
		t.Errorf("httpsBound = true after a reported failure: %s", body)
	}
	if v.Status.HTTPSError != failure {
		t.Errorf("httpsError crossed the wire changed.\n got: %q\nwant: %q", v.Status.HTTPSError, failure)
	}

	srv.Tailnet().ReportHTTPS(true, "")
	v, body = getTailnet(t, srv, token)
	if !v.Status.HTTPSBound {
		t.Errorf("httpsBound = false after a successful bind was reported: %s", body)
	}
	if v.Status.HTTPSError != "" {
		t.Errorf("httpsError = %q once HTTPS is serving, want empty", v.Status.HTTPSError)
	}

	// A node that is down cannot be serving HTTPS, whatever the setting says.
	postTailnet(t, srv, token, "disconnect")
	v, body = getTailnet(t, srv, token)
	if v.Status.HTTPSBound {
		t.Errorf("httpsBound = true on a stopped node: %s", body)
	}
	if !v.HTTPSEnabled {
		t.Error("disconnecting cleared the httpsEnabled SETTING; it is the operator's choice and survives the node going down")
	}
}

// --- Validation -------------------------------------------------------------

// TestTailnetHostnameValidation: the hostname becomes a DNS label at join time,
// inside somebody else's code, and a bad one fails there with a message written
// for somebody else's user. It is rejected here instead — and a rejected save
// leaves the settings exactly as they were.
func TestTailnetHostnameValidation(t *testing.T) {
	srv := testharness.New(t, testharness.WithTailnet(&tailnet.Fake{}))
	token := adminToken(t, srv)

	putOK := func(body map[string]any) tailnetView {
		t.Helper()
		var v tailnetView
		status, raw := srv.JSON(http.MethodPut, tailnetPath, token, body, &v)
		if status != http.StatusOK {
			t.Fatalf("PUT %v = %d, want 200; body: %s", body, status, raw)
		}
		return v
	}
	before := putOK(map[string]any{"hostname": "attic"})

	for _, bad := range []string{"", "  ", "not a label", "obelo.example.com", "-leading", "trailing-", strings.Repeat("a", 64)} {
		status, raw := srv.JSON(http.MethodPut, tailnetPath, token, map[string]any{"hostname": bad}, nil)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("PUT hostname %q = %d, want 422; body: %s", bad, status, raw)
		}
	}
	if status, raw := srv.JSON(http.MethodPut, tailnetPath, token,
		map[string]any{"controlURL": "headscale.example.com"}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("PUT relative controlURL = %d, want 422; body: %s", status, raw)
	}

	after, _ := getTailnet(t, srv, token)
	if after.Hostname != before.Hostname {
		t.Errorf("a rejected update changed the settings: %q → %q", before.Hostname, after.Hostname)
	}
	// A legal label with a trailing space is a save, not a refusal — the operator
	// meant the name, not the whitespace.
	if v := putOK(map[string]any{"hostname": " movie-box "}); v.Hostname != "movie-box" {
		t.Errorf("hostname = %q, want %q", v.Hostname, "movie-box")
	}
}

// --- Boot -------------------------------------------------------------------

// TestTailnetEnabledConnectsAtBoot: an enabled node comes up on its own. Without
// this, a restart leaves the operator locked out of a house they cannot reach —
// the failure the feature exists to prevent.
func TestTailnetEnabledConnectsAtBoot(t *testing.T) {
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}}
	srv := testharness.New(t,
		testharness.WithTailnet(node),
		testharness.WithTailnetEnabled(true),
		testharness.WithTailnetHostname("attic"),
	)
	if node.Starts() != 1 {
		t.Fatalf("the node was started %d time(s) at boot, want 1", node.Starts())
	}
	if got := node.LastConfig().Hostname; got != "attic" {
		t.Errorf("joined as %q, want %q", got, "attic")
	}
	token := adminToken(t, srv)
	v, _ := getTailnet(t, srv, token)
	if !v.Enabled || v.Status.State != "running" || v.Status.FQDN != "obelo.tail1a2b.ts.net" {
		t.Errorf("after boot = %+v, want an enabled, running node", v)
	}
}

// TestTailnetBootSurvivesANodeThatFailsToStart: a coordination server that is
// down is the world being unavailable, not the operator's mistake. The server
// must boot and SERVE — so this asserts the LAN is being served, not merely that
// nothing panicked — while the failure is reported where an operator will see it.
func TestTailnetBootSurvivesANodeThatFailsToStart(t *testing.T) {
	node := &tailnet.Fake{StartErr: errors.New("cannot reach the coordination server")}
	srv := testharness.New(t,
		testharness.WithTailnet(node),
		testharness.WithTailnetEnabled(true),
	)

	// The LAN is serving: the unauthenticated handshake, and a real authenticated
	// admin flow behind it.
	if status, body := srv.GET("/api/v1/server", nil); status != http.StatusOK {
		t.Fatalf("GET /server = %d, want 200; body: %s", status, body)
	}
	token := adminToken(t, srv)
	libDir := t.TempDir()
	if id := createMovieLibrary(t, srv, token, libDir); id == "" {
		t.Fatal("the server is not serving the admin API after a failed Tailnet join")
	}

	// And the failure is not silent: it is on the settings screen, in words.
	v, _ := getTailnet(t, srv, token)
	if v.Status.State != "error" || !strings.Contains(v.Status.LastError, "coordination server") {
		t.Errorf("status = %+v, want the error state carrying the reason", v.Status)
	}
}

// --- The event --------------------------------------------------------------

// TestTailnetStateEventAdminOnly: the state nudge reaches an Admin's stream — so
// the login URL appears without a page reload — and reaches a Member's stream
// NEVER, while that Member still receives the events it is entitled to.
func TestTailnetStateEventAdminOnly(t *testing.T) {
	const tailnetEventName = "tailscaleState"
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/1"}}
	srv := testharness.New(t, testharness.WithTailnet(node))
	adminTok := adminToken(t, srv)

	// A Member granted a Library, so it is entitled to that Library's scanProgress
	// — the sentinel that proves the gate filters by TYPE rather than muting the
	// Member wholesale.
	memberID := srv.CreateUser(adminTok, "member", "correct horse battery staple", "member")
	libID := createMovieLibrary(t, srv, adminTok, t.TempDir())
	grantLibraries(t, srv, adminTok, memberID, libID)
	memberTok := srv.LoginAs("member", "correct horse battery staple")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adminLines := openEventStream(t, ctx, srv, adminTok)
	memberLines := openEventStream(t, ctx, srv, memberTok)

	postTailnet(t, srv, adminTok, "connect")

	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+tailnetEventName) })

	// The tailnet event was published BEFORE this scan, so a leak would sit in the
	// Member's buffer ahead of the sentinel.
	scanLib(t, srv, adminTok, libID, "")
	waitForLineForbidding(t, memberLines,
		func(s string) bool { return strings.Contains(s, "event: "+scanProgressEventName) },
		"event: "+tailnetEventName,
	)
}

// --- A lapsed key is not a first join ---------------------------------------

// TestTailnetLapsedKeyIsDistinguishableFromAFirstJoin is the criterion that
// exists because the two were, until now, byte-identical on the wire.
//
// Mechanically a lapsed key IS "needs login with a fresh URL" — that is what
// Tailscale's own state machine reports, and it is why the collapse happened. But
// the two are completely different events for the operator: "finish setting this
// up" and "the thing that has worked for six months has stopped". ADR-0043 is
// explicit that the second is framed as RE-AUTHORIZATION rather than as an error,
// and a UI cannot honour that framing if the payloads are the same.
//
// The adapter is where the distinction is observed (it reads the node's own
// record); this test proves it SURVIVES to the endpoint the panel reads, which is
// the half that would otherwise rot silently.
func TestTailnetLapsedKeyIsDistinguishableFromAFirstJoin(t *testing.T) {
	lapsed := time.Now().Add(-72 * time.Hour)

	// A first join: waiting on a human, with no expiry to speak of yet.
	firstJoin := &tailnet.Fake{Fresh: tailnet.Status{
		State:    tailnet.StateNeedsLogin,
		LoginURL: "https://login.tailscale.com/a/firstjoin",
	}}
	srv := testharness.New(t, testharness.WithTailnet(firstJoin))
	token := adminToken(t, srv)
	postTailnet(t, srv, token, "connect")
	first, firstBody := getTailnet(t, srv, token)

	// A node whose key has lapsed: the same login URL shape, and a date in the past.
	expired := &tailnet.Fake{Fresh: tailnet.Status{
		State:     tailnet.StateKeyExpired,
		LoginURL:  "https://login.tailscale.com/a/reauth",
		KeyExpiry: &lapsed,
		FQDN:      "obelo.tail1a2b.ts.net",
	}}
	srv2 := testharness.New(t, testharness.WithTailnet(expired))
	token2 := adminToken(t, srv2)
	postTailnet(t, srv2, token2, "connect")
	second, secondBody := getTailnet(t, srv2, token2)

	if first.Status.State == second.Status.State {
		t.Fatalf("a first join and a lapsed key both report state %q. They are the same screen "+
			"with completely different words, and a client cannot tell them apart.\n"+
			"first join: %s\nlapsed:     %s", first.Status.State, firstBody, secondBody)
	}
	if first.Status.State != string(tailnet.StateNeedsLogin) {
		t.Errorf("first join state = %q, want %q", first.Status.State, tailnet.StateNeedsLogin)
	}
	if second.Status.State != string(tailnet.StateKeyExpired) {
		t.Errorf("lapsed state = %q, want %q", second.Status.State, tailnet.StateKeyExpired)
	}

	// Both must still carry the link, because both are fixed by clicking it. The
	// distinction is in the framing, not in what the operator does next.
	if first.Status.LoginURL == "" || second.Status.LoginURL == "" {
		t.Error("a login URL is missing; both states are a dead end without one")
	}
	// And the lapsed one carries the date it lapsed, which is the whole difference
	// between "re-authorize" and an unexplained error.
	if second.Status.KeyExpiry == nil {
		t.Error("the lapsed node sent no keyExpiry; the panel has no date to name")
	}
	// A first join has nothing to say about expiry — and must say nothing, rather
	// than sending a zero time that renders as an expiry in year 1.
	if first.Status.KeyExpiry != nil {
		t.Errorf("a first join carries keyExpiry = %v, want null", *first.Status.KeyExpiry)
	}
}
