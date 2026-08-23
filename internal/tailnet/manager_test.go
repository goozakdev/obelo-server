package tailnet_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// The state machine (ADR-0043). Boot, the settings PUT, and the three verbs all
// funnel through Apply, so these tests drive the Manager directly; the HTTP half
// is black-box in internal/api/tailnet_test.go.

// memStore is an in-memory ManagerStore + SeedStore.
type memStore struct {
	settings store.TailnetSettings
	readErr  error
}

func (m *memStore) TailnetSettings() (store.TailnetSettings, error) {
	if m.readErr != nil {
		return store.TailnetSettings{}, m.readErr
	}
	return m.settings, nil
}

func (m *memStore) SetTailnetEnabled(enabled bool) error {
	m.settings.Enabled = enabled
	return nil
}

// newManager wires a Manager over a store, a node, and a fresh state directory,
// counting the refetch nudges.
func newManager(t *testing.T, s *memStore, node tailnet.Node) (*tailnet.Manager, string, *atomic.Int64) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tailscale")
	var nudges atomic.Int64
	m := tailnet.NewManager(s, node, tailnet.ManagerOptions{
		StateDir: dir,
		AuthKey:  func() string { return "" },
		OnChange: func() { nudges.Add(1) },
	})
	t.Cleanup(func() { _ = m.Close() })
	return m, dir, &nudges
}

// TestDisabledStartsNothing is the shipped default: with the feature off, Apply
// starts no node and — the part that is easy to get wrong — creates no state
// directory. A directory that appears on every boot of every install is a trace
// of a feature nobody turned on.
func TestDisabledStartsNothing(t *testing.T) {
	node := &tailnet.Fake{}
	m, dir, nudges := newManager(t, &memStore{}, node)

	// Counted directly, because "no goroutine starts" is the criterion and this
	// package's test binary is quiet enough to count in. The watcher is the only
	// goroutine this feature ever starts, and it starts with the node.
	before := runtime.NumGoroutine()
	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines went %d → %d; a disabled feature must start none", before, after)
	}
	if node.Starts() != 0 {
		t.Errorf("node started %d time(s) with the feature off", node.Starts())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state directory %q exists with the feature off (stat err = %v)", dir, err)
	}
	if got := m.Status().State; got != tailnet.StateStopped {
		t.Errorf("state = %q, want %q", got, tailnet.StateStopped)
	}
	if n := nudges.Load(); n != 0 {
		t.Errorf("published %d refetch nudge(s) for a no-op apply", n)
	}
}

// TestConnectDisconnectReconnectKeepsState: the whole point of Disconnect being
// its own verb. The second connect must NOT re-authenticate, and the only honest
// way to show that is that the state directory survived and the node found it.
func TestConnectDisconnectReconnectKeepsState(t *testing.T) {
	node := &tailnet.Fake{
		Fresh:     tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/deadbeef"},
		Returning: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"},
	}
	s := &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}
	m, dir, _ := newManager(t, s, node)
	ctx := context.Background()

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !s.settings.Enabled {
		t.Error("connect did not persist the desired state; a restart would strand the operator")
	}
	if got := m.Status().State; got != tailnet.StateNeedsLogin {
		t.Fatalf("state after a fresh join = %q, want %q", got, tailnet.StateNeedsLogin)
	}
	// The human completes the login out of band, as they do.
	node.Transition(tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"})

	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if s.settings.Enabled {
		t.Error("disconnect did not persist the desired state; a restart would reconnect")
	}
	if got := m.Status().State; got != tailnet.StateStopped {
		t.Errorf("state after disconnect = %q, want %q", got, tailnet.StateStopped)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("disconnect destroyed the node state (%v); that is Forget's job", err)
	}

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if node.Joins() != 1 {
		t.Errorf("the node authenticated %d time(s); a reconnect must reuse the kept state", node.Joins())
	}
	if got := m.Status(); got.State != tailnet.StateRunning || got.FQDN != "obelo.tail1a2b.ts.net" {
		t.Errorf("status after reconnect = %+v, want running with the FQDN", got)
	}
}

// TestForgetWipesStateAndRejoinsFresh: Forget is the move-to-another-Tailnet
// button, and the next connect must be a genuinely fresh join.
func TestForgetWipesStateAndRejoinsFresh(t *testing.T) {
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/1"}}
	m, dir, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := m.Forget(ctx); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("forget left the state directory behind (stat err = %v)", err)
	}
	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect after forget: %v", err)
	}
	if node.Joins() != 2 {
		t.Errorf("joins = %d, want 2 — the connect after a forget must re-authenticate", node.Joins())
	}
}

// TestFailedStartIsAWarningNotAFailure: every failure here is a warning. Apply
// reports no error (its caller is the boot path), the node is not considered up,
// and the reason is preserved where the operator will see it.
func TestFailedStartIsAWarningNotAFailure(t *testing.T) {
	boom := errors.New("cannot reach the coordination server")
	node := &tailnet.Fake{StartErr: boom}
	m, dir, _ := newManager(t, &memStore{settings: store.TailnetSettings{Enabled: true, Hostname: "obelo"}}, node)

	if err := m.Apply(context.Background()); err != nil {
		t.Fatalf("apply returned %v; a node that will not start must never fail its caller", err)
	}
	got := m.Status()
	if got.State != tailnet.StateError || got.LastError != boom.Error() {
		t.Errorf("status = %+v, want the error state carrying %q", got, boom)
	}

	// A start that never wrote anything leaves nothing behind. The commonest way to
	// reach this is a build with no Tailnet support, where the directory could never
	// hold anything — and a server that cannot use a feature should not litter its
	// data directory with the feature's furniture.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a failed start left the state directory %q behind (stat err = %v)", dir, err)
	}

	// Turning the feature off answers the failure: the panel must go back to
	// "stopped" rather than keeping a red line about something nobody is running.
	if err := m.Disconnect(context.Background()); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if got := m.Status(); got.State != tailnet.StateStopped || got.LastError != "" {
		t.Errorf("status after disabling = %+v, want a clean stopped", got)
	}
}

// TestNoNodeAnswersWithTheBuild: a build with no Tailnet support linked in still
// answers — with an error naming the build, not silence and not a fabricated
// success.
func TestNoNodeAnswersWithTheBuild(t *testing.T) {
	m, dir, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, nil)

	if err := m.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	got := m.Status()
	if got.State != tailnet.StateError || got.LastError != tailnet.ErrNoNode.Error() {
		t.Errorf("status = %+v, want the error state carrying ErrNoNode", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a build with no node created a state directory anyway (stat err = %v)", err)
	}
}

// TestKeyExpiryDistinguishesAbsentFromSet is the nil-vs-zero pin. A tagged node
// never expires, which is a genuinely different — and better — state than
// "expired at the zero time", and collapsing them would fire the expiry warning
// forever on exactly the nodes that are safe.
func TestKeyExpiryDistinguishesAbsentFromSet(t *testing.T) {
	expiry := time.Date(2026, 12, 25, 10, 30, 0, 0, time.UTC)
	node := &tailnet.Fake{Returning: tailnet.Status{State: tailnet.StateRunning}}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)

	if err := m.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := m.Status().KeyExpiry; got != nil {
		t.Errorf("KeyExpiry = %v, want nil (no expiry — a tagged node)", got)
	}
	node.Transition(tailnet.Status{State: tailnet.StateRunning, KeyExpiry: &expiry})
	got := m.Status().KeyExpiry
	if got == nil || !got.Equal(expiry) {
		t.Errorf("KeyExpiry = %v, want %v", got, expiry)
	}
}

// TestNodeTransitionsNudge: a login URL arriving — or a key lapsing months later
// — reaches the admin UI without anything polling for it, and the watcher that
// carries it stops when the node does.
func TestNodeTransitionsNudge(t *testing.T) {
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateStarting}}
	m, _, nudges := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	before := nudges.Load()
	node.Transition(tailnet.Status{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/2"})

	deadline := time.Now().Add(2 * time.Second)
	for nudges.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("a node-reported transition published no refetch nudge; the login URL would never appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := m.Status().LoginURL; got == "" {
		t.Error("the node reported a login URL and the status does not carry it")
	}
}

// TestApplyReJoinsOnAHostnameChange: a setting that visibly saves and does
// nothing until the next restart is the worst of the available behaviours, and
// the operator is watching the panel at the time.
func TestApplyReJoinsOnAHostnameChange(t *testing.T) {
	node := &tailnet.Fake{}
	s := &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}
	m, _, _ := newManager(t, s, node)
	ctx := context.Background()

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	s.settings.Hostname = "attic"
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if node.Starts() != 2 {
		t.Fatalf("starts = %d, want 2 — a changed hostname must re-join", node.Starts())
	}
	if got := node.LastConfig().Hostname; got != "attic" {
		t.Errorf("re-joined as %q, want %q", got, "attic")
	}
	// An apply with nothing changed is a no-op, so a settings save that touched
	// something else does not bounce the operator's connection.
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if node.Starts() != 2 {
		t.Errorf("starts = %d after an unchanged apply, want 2", node.Starts())
	}
}

// TestListenNeedsARunningNode: binding a Tailnet port is not like binding an OS
// socket. The port lives on the node's own userspace stack, so while the node is
// down there is no stack to bind ON — and the caller (cmd/obelo's listener
// supervisor) has to be able to tell that from "this build cannot join at all",
// because one of them resolves when the operator clicks Connect and the other
// never does.
func TestListenNeedsARunningNode(t *testing.T) {
	node := &tailnet.Fake{}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	if _, err := m.Listen("tcp", ":80"); !errors.Is(err, tailnet.ErrNotRunning) {
		t.Errorf("Listen with the node down = %v, want ErrNotRunning", err)
	}

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ln, err := m.Listen("tcp", ":80")
	if err != nil {
		t.Fatalf("Listen with the node up: %v", err)
	}
	_ = ln.Close()

	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if _, err := m.Listen("tcp", ":80"); !errors.Is(err, tailnet.ErrNotRunning) {
		t.Errorf("Listen after a disconnect = %v, want ErrNotRunning", err)
	}

	// A build with no Tailnet support says so, rather than reporting the node as
	// merely not running — which would send an operator looking for a button to
	// press that cannot exist in their binary.
	noNode, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, nil)
	if _, err := noNode.Listen("tcp", ":80"); !errors.Is(err, tailnet.ErrNoNode) {
		t.Errorf("Listen on a build with no node = %v, want ErrNoNode", err)
	}
}

// TestSubscribeSeesEveryTransition: the listener supervisor learns that the node
// came up or went away the same way the admin UI does — by being nudged — rather
// than by polling. A missed nudge is a Tailnet listener that never binds, and the
// symptom is a MagicDNS name that resolves and then refuses the connection.
func TestSubscribeSeesEveryTransition(t *testing.T) {
	node := &tailnet.Fake{}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	var nudges atomic.Int64
	m.Subscribe(func() { nudges.Add(1) })

	if err := m.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if nudges.Load() == 0 {
		t.Fatal("a connect nudged no subscriber; the Tailnet listener would never bind")
	}

	before := nudges.Load()
	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if nudges.Load() <= before {
		t.Error("a disconnect nudged no subscriber; the Tailnet listener would keep serving a node that is gone")
	}
}

// TestConnectDisconnectCyclesLeakNoGoroutines: an operator can toggle remote
// access all afternoon, and this server is one nobody restarts. Each connect
// starts the state-change watcher and each disconnect must join it — a cycle that
// leaked one would accumulate invisibly for months.
//
// Counted directly, like TestDisabledStartsNothing: this package's test binary is
// quiet enough to count in, and the watcher is the only goroutine the state
// machine ever starts.
func TestConnectDisconnectCyclesLeakNoGoroutines(t *testing.T) {
	node := &tailnet.Fake{}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)
	ctx := context.Background()

	cycle := func() {
		if err := m.Connect(ctx); err != nil {
			t.Fatalf("connect: %v", err)
		}
		if err := m.Disconnect(ctx); err != nil {
			t.Fatalf("disconnect: %v", err)
		}
	}

	// One cycle before the baseline, so anything allocated once is not counted as a
	// leak by the ten that follow.
	cycle()
	before := runtime.NumGoroutine()
	for range 10 {
		cycle()
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines went %d → %d across ten connect/disconnect cycles; each cycle leaks one",
			before, after)
	}
	if node.Starts() != 11 {
		t.Errorf("the node started %d time(s), want 11 — the cycles must actually be happening", node.Starts())
	}
}

// --- tailnet :443, the opt-in (tailscale/03) ---------------------------------

// TestHTTPSOptInIsOffByDefaultAndFollowsTheSettings: HTTPSEnabled is the whole
// opt-in for tailnet :443, and the listener supervisor reads it here rather than
// from the database — on a reconcile that must never be able to fail or block.
//
// It is false while the node is down not as a policy but as a fact: there is no
// userspace stack to bind a TLS listener on.
func TestHTTPSOptInIsOffByDefaultAndFollowsTheSettings(t *testing.T) {
	node := &tailnet.Fake{}
	s := &memStore{settings: store.TailnetSettings{Enabled: true, Hostname: "obelo"}}
	m, _, _ := newManager(t, s, node)
	ctx := context.Background()

	if m.HTTPSEnabled() {
		t.Error("HTTPS is on before anything started; the default posture is off (ADR-0043)")
	}
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if m.HTTPSEnabled() {
		t.Error("a running node with the box unticked reports HTTPS on; tailnet :443 would bind for an operator who never asked")
	}

	s.settings.HTTPSEnabled = true
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !m.HTTPSEnabled() {
		t.Error("the HTTPS opt-in did not reach the listener supervisor")
	}

	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if m.HTTPSEnabled() {
		t.Error("HTTPS is reported on with the node down; there is no stack to bind :443 on")
	}
}

// TestHTTPSToggleDoesNotRestartTheNode is the reason Apply has a default branch at
// all. Ticking the HTTPS box saves a setting that is NOT a join parameter — the
// node does not need to know — so nothing above would notice it moved, and the
// supervisor would go on serving tailnet :80 alone until the next restart. A
// setting that visibly saves and does nothing is the failure this guards.
//
// The other half is what it must NOT do: re-join. A certificate is not a
// membership, and tearing the node down would drop every in-flight stream to make
// a listener appear beside the one already serving.
func TestHTTPSToggleDoesNotRestartTheNode(t *testing.T) {
	node := &tailnet.Fake{}
	s := &memStore{settings: store.TailnetSettings{Enabled: true, Hostname: "obelo"}}
	m, _, _ := newManager(t, s, node)
	ctx := context.Background()

	var nudges atomic.Int64
	m.Subscribe(func() { nudges.Add(1) })

	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	starts, before := node.Starts(), nudges.Load()

	s.settings.HTTPSEnabled = true
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if nudges.Load() <= before {
		t.Error("ticking the HTTPS box nudged no subscriber, so the listener supervisor never learned; tailnet :443 would appear only after a restart")
	}
	if got := node.Starts(); got != starts {
		t.Errorf("the node restarted (%d starts, was %d) because of a certificate setting; that drops every in-flight stream for a listener that is merely being added", got, starts)
	}

	// And an Apply that changes nothing stays a no-op, or the nudge stops meaning
	// anything and the supervisor rebinds on a timer it was not given.
	quiet := nudges.Load()
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if nudges.Load() != quiet {
		t.Errorf("a no-op apply published %d nudge(s)", nudges.Load()-quiet)
	}
}

// TestListenTLSFailsOrdinarilyWhileTheNodeIsDown: both refusals are ordinary and
// neither is fatal. The caller logs and carries on serving tailnet :80 and the LAN
// — which is the entire posture of this feature.
func TestListenTLSFailsOrdinarilyWhileTheNodeIsDown(t *testing.T) {
	node := &tailnet.Fake{}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, node)

	if _, err := m.ListenTLS("tcp", ":443"); !errors.Is(err, tailnet.ErrNotRunning) {
		t.Errorf("ListenTLS with the node down = %v, want ErrNotRunning", err)
	}

	noNode, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Hostname: "obelo"}}, nil)
	if _, err := noNode.ListenTLS("tcp", ":443"); !errors.Is(err, tailnet.ErrNoNode) {
		t.Errorf("ListenTLS on a build with no node = %v, want ErrNoNode", err)
	}
}

// TestListenTLSDoesNotFreezeTheStateMachine. `tsnet.Server.ListenTLS` waits for the
// node to be up before it looks at anything, on a context with no deadline of its
// own — so it can park indefinitely. Holding the reconciliation lock across that
// call would freeze EVERYTHING: a settings GET, a Disconnect, and the shutdown path
// all take the same mutex, so an operator's panel would hang on a certificate and
// the server would not exit.
//
// The trade is stated where the lock is released: the node may be stopped between
// the check and the call, in which case the node itself answers with an error and
// the caller logs it. That is the posture anyway.
func TestListenTLSDoesNotFreezeTheStateMachine(t *testing.T) {
	node := &blockingTLSNode{
		Fake:    &tailnet.Fake{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	m, _, _ := newManager(t, &memStore{settings: store.TailnetSettings{Enabled: true, Hostname: "obelo"}}, node)
	ctx := context.Background()
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.ListenTLS("tcp", ":443")
	}()
	<-node.entered

	// The panel, and then the button that takes the machine down. Both would block
	// forever if the bind held the lock.
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_ = m.Status()
		_ = m.Disconnect(ctx)
	}()
	select {
	case <-answered:
	case <-time.After(5 * time.Second):
		close(node.release)
		t.Fatal("the state machine was frozen while a TLS bind was in flight — Status and Disconnect could not be served, so the panel hangs and the server cannot be stopped")
	}

	close(node.release)
	<-done
}

// blockingTLSNode is a Fake whose ListenTLS parks until released, standing in for
// tsnet's unbounded wait for the node to come up.
type blockingTLSNode struct {
	*tailnet.Fake
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (n *blockingTLSNode) ListenTLS(network, addr string) (net.Listener, error) {
	n.once.Do(func() { close(n.entered) })
	<-n.release
	return nil, errors.New("tailnet: the node went away while binding")
}

// TestStatusCarriesTheOBSERVEDHTTPSBind pins the distinction the whole of
// tailscale/06 is about, at the seam where it is stored: HTTPSEnabled is what the
// operator ASKED FOR and lives in the settings row; HTTPSBound is what the server
// ACHIEVED and can only arrive from the listener supervisor, which is the only
// thing that binds tailnet :443.
//
// The Manager must never bridge the two. Every assertion below is made with the
// setting ON, because that is the only configuration in which a field derived from
// the setting would look right — and it is the configuration a misconfigured
// console leaves an operator in.
func TestStatusCarriesTheOBSERVEDHTTPSBind(t *testing.T) {
	ctx := context.Background()
	node := &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}}
	s := &memStore{settings: store.TailnetSettings{Enabled: true, Hostname: "obelo", HTTPSEnabled: true}}
	m, _, nudges := newManager(t, s, node)
	if err := m.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// A running node with the opt-in ON and nothing reported yet. Not bound: no
	// listener has been observed, and the honest answer to "is HTTPS serving" before
	// anything has bound is no.
	if st := m.Status(); st.HTTPSBound {
		t.Fatal("httpsBound is true before any bind was reported — it is derived from the setting, " +
			"which is the bug: the panel then promises an https:// address nothing is listening on")
	}

	// The supervisor reports a failure. The setting has not moved.
	const failure = `There is no HTTPS on the Tailnet for "obelo.tail1a2b.ts.net": you must enable MagicDNS`
	m.ReportHTTPS(false, failure)
	if st := m.Status(); st.HTTPSBound || st.HTTPSError != failure {
		t.Errorf("status = {bound:%v error:%q}, want unbound carrying the reason verbatim", st.HTTPSBound, st.HTTPSError)
	}
	if !s.settings.HTTPSEnabled {
		t.Fatal("the opt-in went off by itself; this test only means something while the request and the outcome disagree")
	}

	// Repeating an unchanged report is silent. The supervisor retries forever while
	// HTTPS is wanted and unbound, and a nudge per attempt is an SSE event twice a
	// minute for the life of a misconfigured server.
	before := nudges.Load()
	m.ReportHTTPS(false, failure)
	m.ReportHTTPS(false, failure)
	if got := nudges.Load() - before; got != 0 {
		t.Errorf("an unchanged report published %d nudge(s), want 0", got)
	}

	// It came up. That IS news — the panel has an address to change.
	m.ReportHTTPS(true, "")
	if st := m.Status(); !st.HTTPSBound || st.HTTPSError != "" {
		t.Errorf("status = {bound:%v error:%q}, want bound and quiet", st.HTTPSBound, st.HTTPSError)
	}
	if nudges.Load() == before {
		t.Error("a change of state published no nudge; the screen would keep showing the old address until something else happened")
	}

	// And the node goes down with the opt-in still on. The listener lives on the
	// node's own userspace stack, so it went too — claiming otherwise would leave an
	// https:// address on the panel of a stopped node.
	if err := m.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if st := m.Status(); st.HTTPSBound || st.HTTPSError != "" {
		t.Errorf("after the node stopped: status = {bound:%v error:%q}, want both cleared", st.HTTPSBound, st.HTTPSError)
	}
}
