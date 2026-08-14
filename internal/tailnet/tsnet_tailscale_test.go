//go:build tailscale

package tailnet

import (
	"bytes"
	"context"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/empty"
)

// The tagged half of the CI matrix (ADR-0043). Nothing here joins a Tailnet —
// there is no honest way to unit-test that, which is the whole reason the seam is
// where it is. What IS tested is the part of the adapter that is OURS rather than
// Tailscale's: the projection of their state machine onto this package's small
// enum, which is where a mistake is silent and lands six months later.
//
// It is an in-package test because that projection is deliberately unexported: no
// `tsnet` or `ipn` type may cross out of this file's package (ADR-0043), and a
// black-box test would have to be handed one to say anything.

func ptr[T any](v T) *T { return &v }

// TestToStatusMapsTailscalesStatesOntoTheEnum walks every state this server can
// observe. The enum is deliberately smaller than Tailscale's, so each line here is
// a decision about what an operator is shown.
func TestToStatusMapsTailscalesStatesOntoTheEnum(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	future := now.Add(180 * 24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	running := &ipnstate.PeerStatus{
		DNSName:      "obelo.tail1a2b.ts.net.",
		TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.101.102.103")},
		KeyExpiry:    &future,
	}

	cases := []struct {
		name      string
		view      nodeView
		wantState State
	}{
		{
			name:      "running",
			view:      nodeView{state: ipn.Running, self: running},
			wantState: StateRunning,
		},
		{
			// The interactive first join, which ADR-0043 makes the DEFAULT path rather
			// than the fallback: no netmap yet, so no self, and a URL for the human.
			name:      "first join waits on a human",
			view:      nodeView{state: ipn.NeedsLogin, loginURL: "https://login.tailscale.com/a/deadbeef"},
			wantState: StateNeedsLogin,
		},
		{
			// THE DISTINCTION THIS TICKET EXISTS FOR. A lapsed key surfaces on the IPN bus
			// as plain NeedsLogin with a fresh URL — ipnlocal maps keyExpired to NeedsLogin
			// outright — so without reading the node's own record first, this and the case
			// above are byte-identical on the wire, and the UI cannot honour ADR-0043's
			// "re-authorize, not an error".
			name: "a lapsed key is not a first join",
			view: nodeView{
				state:    ipn.NeedsLogin,
				loginURL: "https://login.tailscale.com/a/feedface",
				self:     &ipnstate.PeerStatus{DNSName: "obelo.tail1a2b.ts.net.", Expired: true, KeyExpiry: &past},
			},
			wantState: StateKeyExpired,
		},
		{
			// The same conclusion from the other signal: the control plane has not yet
			// recomputed the flag, but the date has passed. Two signals because they fail
			// in opposite directions.
			name: "an expiry in the past is expired even without the flag",
			view: nodeView{
				state: ipn.Running,
				self:  &ipnstate.PeerStatus{DNSName: "obelo.tail1a2b.ts.net.", KeyExpiry: &past},
			},
			wantState: StateKeyExpired,
		},
		{
			// Device approval is on for this Tailnet. NOT needsLogin, because there is no
			// link to click: needsLogin without a URL is a dead end for the operator.
			name:      "waiting for device approval",
			view:      nodeView{state: ipn.NeedsMachineAuth},
			wantState: StateError,
		},
		{
			name:      "backend has not decided yet",
			view:      nodeView{state: ipn.NoState},
			wantState: StateStarting,
		},
		{
			name:      "starting",
			view:      nodeView{state: ipn.Starting},
			wantState: StateStarting,
		},
		{
			name:      "stopped",
			view:      nodeView{state: ipn.Stopped},
			wantState: StateStopped,
		},
		{
			name:      "a backend error is an error",
			view:      nodeView{state: ipn.NoState, errMsg: "control server said no"},
			wantState: StateError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.view.toStatus(now)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (status: %+v)", got.State, tc.wantState, got)
			}
		})
	}
}

// TestToStatusCarriesTheAddressAndTheExpiry: the fields the settings screen and
// the boot log are built out of.
func TestToStatusCarriesTheAddressAndTheExpiry(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(90 * 24 * time.Hour)
	v := nodeView{
		state: ipn.Running,
		self: &ipnstate.PeerStatus{
			// Tailscale's DNSName carries a trailing dot; ours must not, because it is
			// pasted into a browser and joined to a scheme.
			DNSName:      "obelo.tail1a2b.ts.net.",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.101.102.103"), netip.MustParseAddr("fd7a:115c:a1e0::1")},
			KeyExpiry:    &expiry,
		},
	}

	got := v.toStatus(now)
	if got.FQDN != "obelo.tail1a2b.ts.net" {
		t.Errorf("FQDN = %q, want the name without the trailing dot", got.FQDN)
	}
	if len(got.Addresses) != 2 {
		t.Errorf("Addresses = %v, want both the v4 and the v6", got.Addresses)
	}
	if got.KeyExpiry == nil || !got.KeyExpiry.Equal(expiry) {
		t.Errorf("KeyExpiry = %v, want %v", got.KeyExpiry, expiry)
	}

	// NO EXPIRY MUST STAY NIL rather than collapsing to the zero time: a node joined
	// with a tagged auth key never expires, which is a different and better state
	// than "expired in year 1" — and the zero time would fire the ~14-day warning
	// forever on exactly the nodes that are safe.
	v.self.KeyExpiry = nil
	if got := v.toStatus(now); got.KeyExpiry != nil {
		t.Errorf("KeyExpiry = %v for a node with no expiry, want nil", got.KeyExpiry)
	}
	if got := v.toStatus(now).State; got != StateRunning {
		t.Errorf("state = %q for a node with no expiry, want %q — no expiry is the GOOD case", got, StateRunning)
	}
}

// TestNeedsMachineAuthSaysWhereToGo: the one state whose only cure is somewhere
// this server cannot reach. A bare "error" would send an operator to the wrong
// place.
func TestNeedsMachineAuthSaysWhereToGo(t *testing.T) {
	got := nodeView{state: ipn.NeedsMachineAuth}.toStatus(time.Now())
	if !strings.Contains(got.LastError, "Tailscale console") {
		t.Errorf("lastError = %q, want it to name the Tailscale console", got.LastError)
	}
}

// TestAbsorbAccumulatesTheBusRatherThanReplacingIt. A Notify carries only what
// CHANGED, so a state message that arrives with no URL attached must not erase the
// URL that arrived a moment earlier — that would blank the login link out of the
// admin panel while the operator is looking at it.
func TestAbsorbAccumulatesTheBusRatherThanReplacingIt(t *testing.T) {
	var v nodeView

	if !v.absorb(ipn.Notify{State: ptr(ipn.NeedsLogin)}) {
		t.Fatal("a state change reported nothing changed")
	}
	if !v.absorb(ipn.Notify{BrowseToURL: ptr("https://login.tailscale.com/a/deadbeef")}) {
		t.Fatal("a login URL reported nothing changed")
	}
	// A repeat of what we already know is not a change; publishing it would nudge
	// every connected admin to refetch for nothing.
	if v.absorb(ipn.Notify{State: ptr(ipn.NeedsLogin)}) {
		t.Error("re-reporting the current state counted as a change")
	}
	if v.loginURL == "" {
		t.Error("the login URL was lost to a state message that did not carry one")
	}

	// Login completed: the URL is spent, and a spent link that still renders is
	// worse than none — it looks live and authorizes nothing.
	if !v.absorb(ipn.Notify{LoginFinished: &empty.Message{}}) {
		t.Error("a completed login reported nothing changed")
	}
	if v.loginURL != "" {
		t.Errorf("loginURL = %q after the login finished, want it cleared", v.loginURL)
	}

	// Leaving the needs-login state also drops the URL, for the same reason.
	v.loginURL = "https://login.tailscale.com/a/deadbeef"
	v.absorb(ipn.Notify{State: ptr(ipn.Running)})
	if v.loginURL != "" {
		t.Errorf("loginURL = %q on a running node, want it cleared", v.loginURL)
	}
}

// TestSupportedIsTrueWithTheTag pins the build constant behind
// GET /server's features.tailscale — the only way a client can tell the two
// binaries apart, since the routes exist in both.
func TestSupportedIsTrueWithTheTag(t *testing.T) {
	if !Supported {
		t.Error("tailnet.Supported is false in a build with -tags tailscale")
	}
}

// TestUnreachableCoordinationServerIsNotAFailure drives the REAL adapter — a real
// tsnet.Server, a real userspace stack — against a coordination server that is not
// there, which is the failure ADR-0043 says must never stop this server booting.
//
// It needs no network: 127.0.0.1:1 refuses instantly, and the node simply never
// gets past "needs login". What the test pins is the timing contract, because that
// is what a boot depends on — Start returns as soon as the node has been ASKED to
// join, and Close does not wait for a control server that is never going to
// answer. A Start that blocked here would put a third party's outage on the boot
// path of a media server in somebody's living room.
func TestUnreachableCoordinationServerIsNotAFailure(t *testing.T) {
	node := NewNode()
	dir := t.TempDir()

	start := time.Now()
	if err := node.Start(context.Background(), Config{
		Hostname:   "obelo",
		ControlURL: "http://127.0.0.1:1", // Nothing listens here, and nothing will.
		StateDir:   dir,
	}); err != nil {
		t.Fatalf("Start against a dead coordination server = %v; it must not fail the boot", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Start took %v against a dead coordination server; it must return once the node has been ASKED to join", elapsed)
	}

	// It certainly must not claim to be running. Anything else — starting, needs
	// login, error — is an honest answer to "we cannot reach control".
	if got := node.Status().State; got == StateRunning {
		t.Errorf("state = %q with no reachable coordination server", got)
	}

	// Worth stating because it surprises: Listen SUCCEEDS here. The port lives on
	// this process's own userspace stack, which exists as soon as the node starts, so
	// binding it never needed a coordination server — what is missing is anybody who
	// can route to it. That is why cmd/obelo's supervisor gates on StateRunning
	// rather than on whether Listen returned an error: a listener nothing can reach
	// is not an error condition, it is just pointless, and the state machine is the
	// thing that knows the difference.
	ln, err := node.Listen("tcp", ":80")
	if err != nil {
		t.Errorf("Listen on a started node = %v", err)
	} else {
		_ = ln.Close()
	}

	stop := time.Now()
	if err := node.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if elapsed := time.Since(stop); elapsed > 10*time.Second {
		t.Errorf("Close took %v; a node that never joined must not have to be waited out", elapsed)
	}
	// Idempotent, because shutdown paths call it without knowing what happened.
	if err := node.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	// And once it is down there is no stack left to bind on.
	if _, err := node.Listen("tcp", ":80"); err == nil {
		t.Error("Listen succeeded on a closed node")
	}
}

// TestStateDirectoryIsCreated0700: the node key lives here, alongside the ACME
// cache and the server identity and for the same reason (ADR-0007/0043). The
// adapter creates it itself rather than trusting its caller, because Node.Start
// cannot assume who called it.
func TestStateDirectoryIsCreated0700(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tailscale")
	node := NewNode()
	if err := node.Start(context.Background(), Config{
		Hostname:   "obelo",
		ControlURL: "http://127.0.0.1:1",
		StateDir:   dir,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = node.Close() })

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the node state directory was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state directory is mode %04o, want 0700 — it holds the node key, and anyone who can read it can be this node on the operator's Tailnet", perm)
	}
}

// TestAuthKeyReachesTheJoinAndNothingElse is the headless-install half of
// ADR-0043's "no long-lived Tailnet credential is ever stored here", exercised
// against the REAL adapter rather than the Fake.
//
// The key arrives from OBELO_TAILSCALE_AUTHKEY at join time and must reach the
// join and nothing else: not this package's own struct, not the log — where an
// operator would later paste it into an issue — and not the state directory,
// which is the one place here that outlives the process.
//
// The coordination server is dead on purpose. The join therefore never completes,
// which is the WORST case for this property: failures are where credentials
// classically leak, because that is where somebody adds the value to a message to
// make debugging easier.
func TestAuthKeyReachesTheJoinAndNothingElse(t *testing.T) {
	const key = "tskey-auth-kY9ZZZZCNTRL-neverAppearAnywhere"

	logs := &strings.Builder{}
	flags := log.Flags()
	log.SetOutput(logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(flags) })

	dir := t.TempDir()
	node := NewNode()
	if err := node.Start(context.Background(), Config{
		Hostname:   "obelo",
		ControlURL: "http://127.0.0.1:1",
		StateDir:   dir,
		AuthKey:    key,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the backend get as far as it is going to get, so anything it was going to
	// log or write has happened.
	time.Sleep(500 * time.Millisecond)
	if err := node.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if strings.Contains(logs.String(), key) {
		t.Errorf("the join key appears in the log:\n%s", logs.String())
	}
	// Nothing this adapter holds may carry it either — Config.String is what stands
	// between a careless %v and a disclosed credential.
	if got := (Config{AuthKey: key}).String(); strings.Contains(got, key) {
		t.Errorf("Config.String discloses the join key: %s", got)
	}
	// And nothing durable: the state directory survives restarts, so a key written
	// here would be a long-lived credential on disk, which is exactly what ADR-0043
	// says this database and this server never store.
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if bytes.Contains(b, []byte(key)) {
			t.Errorf("the join key was written to %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking the state directory: %v", err)
	}
}

// TestStateSurvivesARestartAndIsReused is the mechanism behind "Disconnect then
// Connect needs no re-authorization" (ADR-0043), exercised on the REAL node
// rather than on the Fake's stand-in marker.
//
// What it can prove without a Tailnet is the part that is ours: the durable state
// the node writes survives a Close, a second Start finds it and does not mint a
// new identity, and Forget's os.RemoveAll is what makes the join after that a
// fresh one. What it cannot prove is the handshake at the other end — that a
// coordination server accepts the returning node without a login — and that is
// on the list of things only a real Tailnet can answer.
func TestStateSurvivesARestartAndIsReused(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Hostname: "obelo", ControlURL: "http://127.0.0.1:1", StateDir: dir}
	stateFile := filepath.Join(dir, "tailscaled.state")

	node := NewNode()
	if err := node.Start(context.Background(), cfg); err != nil {
		t.Fatalf("first start: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let the backend write its state
	if err := node.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	first, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("no durable node state after a start: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("the node state file is empty; a reconnect would have nothing to return with")
	}

	// A second start — the reconnect — must ADOPT that state rather than replace it.
	// A node that rewrote its identity here would need re-authorizing every time the
	// operator toggled the switch, which is precisely the difference between
	// Disconnect and Forget.
	again := NewNode()
	if err := again.Start(context.Background(), cfg); err != nil {
		t.Fatalf("second start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := again.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	second, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("the node state did not survive the restart: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the node state changed across a restart; the returning node is not the node that left, " +
			"so a reconnect would have to re-authenticate")
	}
}
