package tailnet

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ManagerStore is the persistence the Manager reads the desired state from and
// records connect/disconnect into. *store.DB satisfies it; the narrow interface
// keeps the seam explicit and lets a test drive the state machine without a live
// database (mirroring subfetch.ManagerStore).
type ManagerStore interface {
	TailnetSettings() (store.TailnetSettings, error)
	SetTailnetEnabled(enabled bool) error
}

// ManagerOptions are the wiring a Manager needs beyond its store and its Node.
type ManagerOptions struct {
	// StateDir is the durable node state directory (<DataDir>/tailscale). It is
	// created 0700 at JOIN TIME, never at construction: a server with this feature
	// off must leave no trace on disk (ADR-0043), and a directory that appears on
	// every boot of every install is a trace.
	StateDir string
	// AuthKey supplies the optional pre-authorized join key, called once per join
	// and never stored. Production passes EnvAuthKey (read OBELO_TAILSCALE_AUTHKEY
	// from the environment); nil means "no key", the interactive-login default.
	AuthKey func() string
	// OnChange is the "go refetch" nudge fired after every transition — a settings
	// save, a connect, a disconnect, and every state change the node reports on its
	// own (a login URL appearing, a login completing, a key lapsing). It carries no
	// state: the GET stays authoritative. Nil is fine (a narrow test with no Broker).
	OnChange func()
}

// EnvAuthKey reads the pre-authorized join key from the environment at the moment
// of the call. It is a function rather than a value so the key is read AT JOIN
// TIME and dropped: nothing holds it between joins, no config struct carries it,
// and there is no column for it (ADR-0043 — no long-lived Tailnet credential is
// ever persisted here).
func EnvAuthKey() string { return os.Getenv("OBELO_TAILSCALE_AUTHKEY") }

// Manager is the ONE place the Tailnet node is started and stopped. Enable/disable
// (the settings PUT), connect/disconnect/forget (the admin verbs), and boot all
// funnel through Apply, which reconciles the running node against the persisted
// desired state — so there is a single answer to "should the node be up?" and no
// pair of paths that can disagree.
//
// EVERY FAILURE HERE IS A WARNING. A coordination server that is down, a login
// nobody completed, an auth key that was rejected: none of them is the operator's
// mistake and none of them may stop the server booting or touch the LAN listener.
// This is `acme` mode's posture verbatim (cmd/obelo/tls.go) and for the same
// reason — the alternative is a media server that refuses to play films in the
// living room because a third party is having an outage.
type Manager struct {
	store    ManagerStore
	node     Node
	stateDir string
	authKey  func() string
	onChange func()

	// mu serializes the reconciliation so a settings PUT racing a connect cannot
	// interleave into two half-applied states.
	mu sync.Mutex
	// running is whether THIS Manager has the node started. Distinct from the
	// node's own reported state: a node that is running-but-needs-login is started.
	running bool
	// applied is the settings snapshot the running node was started with, so a
	// hostname or control-URL change is noticed and re-applied rather than silently
	// waiting for the next restart.
	applied store.TailnetSettings
	// lastErr is why the last start attempt failed, surfaced through Status while
	// the node is down. Cleared by a successful start.
	lastErr string
	// httpsBound / httpsError are the listener supervisor's REPORT about tailnet
	// :443 (ReportHTTPS), held here because Status is the one place the settings
	// screen reads and the supervisor is not something the API layer can see.
	//
	// They are deliberately NOT derived from applied.HTTPSEnabled. That field is
	// the request; these are the outcome, and the whole reason they exist is that
	// the two come apart whenever the Tailscale console prerequisites are missing.
	httpsBound bool
	httpsError string
	// closed marks the Manager shut down (server exiting); further Applies are
	// refused rather than resurrecting a node during teardown.
	closed bool
	// watchStop/watchDone bound the state-change watcher goroutine's life to the
	// node's: it starts when the node starts and is joined before the node closes,
	// so a stopped Manager leaves nothing running (and `-race` has nothing to find).
	watchStop chan struct{}
	watchDone chan struct{}

	// subMu guards subscribers, and it is a SECOND mutex on purpose. notify runs
	// both under mu (the reconciliation paths) and outside it (the watcher), so
	// keeping the subscriber list on mu would either deadlock the watcher against a
	// stopLocked that is waiting to join it or force notify to be called from
	// nowhere useful. Nothing ever takes mu while holding subMu.
	subMu       sync.Mutex
	subscribers []func()
}

// NewManager wires a Manager over the settings store and a Node. node may be nil
// — that is a build with no Tailnet support linked in, where every operation
// answers ErrNoNode and the settings still read and write normally, so the admin
// screen can explain the situation instead of 404ing at it.
func NewManager(s ManagerStore, node Node, opts ManagerOptions) *Manager {
	return &Manager{
		store:    s,
		node:     node,
		stateDir: opts.StateDir,
		authKey:  opts.AuthKey,
		onChange: opts.OnChange,
	}
}

// Status reports the node's current condition. While the node is down it is a
// synthesized snapshot rather than the node's own: stopped when nothing has gone
// wrong, error carrying the last failure when something has — which is what makes
// a failed connect visible on the settings screen instead of looking like an
// operator who never pressed the button.
//
// The two HTTPS fields are OVERLAID on whichever of those three snapshots is
// returned, because they are the listener supervisor's observation rather than
// anything a Node reports (see ReportHTTPS). They are the only part of a Status
// that does not come from the node.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s Status
	switch {
	case m.running && m.node != nil:
		s = m.node.Status()
	case m.lastErr != "":
		s = Status{State: StateError, LastError: m.lastErr}
	default:
		s = Status{State: StateStopped}
	}
	s.HTTPSBound, s.HTTPSError = m.httpsBound, m.httpsError
	return s
}

// ReportHTTPS records what the listener supervisor OBSERVED about tailnet :443:
// whether it is bound right now, and — when it is not — the failure in the same
// words the log used.
//
// It is a report, not a request. The supervisor is the only thing that binds that
// listener, so it is the only thing that can know; nothing here may compute this
// from HTTPSEnabled, which is the operator's intent and was being read as though
// it were the outcome (the bug this method exists to end). "bound" means a
// listener is accepting connections, not that a certificate has been proven good
// — with tsnet the certificate is fetched inside the node on the first handshake
// and we never see it.
//
// It fires the refetch nudge ONLY on a change. The supervisor reconciles on a
// timer while HTTPS is wanted-and-unbound, and a nudge per attempt would be one
// SSE event every retry interval, forever, on a misconfigured box — and each of
// those wakes the supervisor, which reports again. Deduplicating here is what
// keeps that loop from spinning.
func (m *Manager) ReportHTTPS(bound bool, failure string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.httpsBound == bound && m.httpsError == failure {
		return
	}
	m.httpsBound, m.httpsError = bound, failure
	m.notify()
}

// Listen binds a listener on the Tailnet interface — tailnet :80, bound by
// cmd/obelo whenever the node is up (ADR-0043).
//
// The Manager rather than the Node is the thing to ask, for the same reason it is
// the only thing that starts and stops the node: "is the node up?" has exactly one
// answer, and a caller holding a Node directly could bind one that this state
// machine is in the middle of taking down.
//
// Both failures are ordinary and neither is fatal: ErrNoNode on a build with no
// Tailnet support, ErrNotRunning while the node is down. A caller logs and carries
// on serving the LAN.
func (m *Manager) Listen(network, addr string) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.node == nil {
		return nil, ErrNoNode
	}
	if !m.running {
		return nil, ErrNotRunning
	}
	return m.node.Listen(network, addr)
}

// ListenTLS binds the Tailnet's HTTPS listener — tailnet :443, bound by cmd/obelo
// only when the operator has opted in (issue 03). Same two ordinary failures as
// Listen, plus the ones that are the whole point of the opt-in: MagicDNS or HTTPS
// certificates not enabled in the Tailscale console, which the node reports from
// here with a message naming the setting.
//
// IT DOES NOT HOLD THE LOCK ACROSS THE CALL, and that is the one deliberate
// difference from Listen. `tsnet.Server.ListenTLS` waits for the node to be up
// before it does anything (its own `Up(context.Background())` — no deadline), so a
// node that goes down at the wrong moment can park this call indefinitely. Under
// mu that would freeze the whole state machine: a settings GET, a Disconnect, and
// the shutdown path all take the same mutex, so an operator's panel would hang on
// a certificate. The cost of releasing it is that the node may be stopped between
// the check and the call, in which case the node itself answers with an error and
// the caller logs it and keeps serving tailnet :80 — which is the posture anyway.
// The caller additionally bounds the call in wall-clock time; see cmd/obelo.
func (m *Manager) ListenTLS(network, addr string) (net.Listener, error) {
	m.mu.Lock()
	node, running := m.node, m.running
	m.mu.Unlock()
	if node == nil {
		return nil, ErrNoNode
	}
	if !running {
		return nil, ErrNotRunning
	}
	return node.ListenTLS(network, addr)
}

// HTTPSEnabled reports whether the RUNNING node's settings opt into tailnet :443.
//
// It answers from the settings the node was started with rather than re-reading
// the database, for two reasons: it is called from the listener supervisor's
// reconcile loop, which must not be able to fail or block on a database, and
// "what is the running node configured for" is exactly what `applied` means.
// Apply is what keeps it current — including for a change that does not restart
// the node, which is the only way this field ever moves.
//
// False while the node is down is not a policy decision, it is the truth: there is
// nothing to bind a TLS listener on.
func (m *Manager) HTTPSEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running && m.applied.HTTPSEnabled
}

// Subscribe registers f to run after every transition, alongside the Broker nudge.
// It is how the listener supervisor in cmd/obelo learns that the node came up or
// went down without polling for it.
//
// THE CALLBACK MUST NOT CALL BACK INTO THIS MANAGER, and it must not block. It
// runs on whichever goroutine caused the transition — often one holding the
// reconciliation lock — so a callback that reaches for Status or Listen deadlocks
// on a mutex that is not reentrant. The contract is the Broker's, deliberately:
// this is a "go and look" nudge carrying no state, and the looking happens on the
// subscriber's own goroutine.
func (m *Manager) Subscribe(f func()) {
	if f == nil {
		return
	}
	m.subMu.Lock()
	defer m.subMu.Unlock()
	m.subscribers = append(m.subscribers, f)
}

// Apply reconciles the running node with the persisted desired state. It is
// idempotent and is the single entry point every other path funnels through:
// boot calls it, the settings PUT calls it, and Connect/Disconnect/Forget call it
// after writing their one column.
//
// It returns an error ONLY for a settings read that failed — something is wrong
// with the database, and the caller cannot proceed on a guess. A node that fails
// to start is NOT an error here: it is logged as a warning and recorded in
// Status, because it must never fail a boot and must never fail an HTTP request
// the operator will read as "your button is broken" when the truth is "the
// coordination server is unreachable".
func (m *Manager) Apply(ctx context.Context) error {
	s, err := m.store.TailnetSettings()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	switch {
	case !s.Enabled:
		m.stopLocked()
		// A failure the operator has since answered by turning the feature off is no
		// longer describing anything: "stopped" is the truth, and leaving a stale red
		// line on a panel for something nobody is trying to run is how an operator
		// learns to ignore the panel.
		m.lastErr = ""
	case !m.running:
		m.startLocked(ctx, s)
	case m.applied.Hostname != s.Hostname || m.applied.ControlURL != s.ControlURL:
		// The node is up under a name (or a coordination server) the operator has
		// since changed. Re-join rather than waiting for the next restart: a setting
		// that visibly saves and does nothing is the worst of the three options, and
		// the operator is watching the panel right now.
		m.stopLocked()
		m.startLocked(ctx, s)
	default:
		// The node is up and nothing about the JOIN changed — but the row can still
		// have moved underneath it. HTTPSEnabled is the case: it is deliberately not a
		// join parameter (Config carries no such field, because whether to serve TLS on
		// the Tailnet is the CALLER's decision), so nothing above would notice it, and
		// the listener supervisor would go on serving tailnet :80 alone until the next
		// restart. A toggle that visibly saves and does nothing is the failure this
		// branch exists to prevent — the same reasoning as the hostname case above, one
		// field along.
		//
		// Re-joining is emphatically NOT the answer: a certificate is not a membership,
		// and tearing the node down would drop every in-flight stream to make a
		// listener appear beside the one already serving.
		if m.applied.HTTPSEnabled != s.HTTPSEnabled {
			m.applied = s
			m.notify()
		}
	}
	return nil
}

// Connect records that the node SHOULD be up and brings it up. The desire is
// persisted first and deliberately: the point of the setting is that a restart
// puts the node back, so an operator who connects from a hotel is not one power
// cut away from being locked out of the house they are trying to reach.
func (m *Manager) Connect(ctx context.Context) error {
	if err := m.store.SetTailnetEnabled(true); err != nil {
		return err
	}
	return m.Apply(ctx)
}

// Disconnect stops the node and its listeners but KEEPS the state directory, so
// reconnecting is instant and needs no re-authorization. It is a different verb
// from Forget on purpose (ADR-0043): "not right now" and "not this Tailnet ever
// again" are different intentions, and collapsing them into a flag on one route
// means an operator who wanted the first one discovers they got the second only
// when they try to come back.
func (m *Manager) Disconnect(ctx context.Context) error {
	if err := m.store.SetTailnetEnabled(false); err != nil {
		return err
	}
	return m.Apply(ctx)
}

// Forget stops the node AND wipes the state directory, so the next connect is a
// fresh join — the move-this-server-to-a-different-Tailnet button.
//
// It cannot finish the job, and the UI must say so: the now-dead node row remains
// in the operator's Tailscale console and only they can delete it. Nothing here
// can reach across and remove it.
//
// Unlike the other verbs this DOES return a filesystem failure, because a state
// directory that could not be wiped means the node was not actually forgotten —
// the next connect would rejoin as the same node, which is the opposite of what
// was asked for, and silence would be a lie.
func (m *Manager) Forget(ctx context.Context) error {
	if err := m.store.SetTailnetEnabled(false); err != nil {
		return err
	}
	if err := m.Apply(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.RemoveAll(m.stateDir); err != nil {
		return fmt.Errorf("tailnet: wiping node state %q: %w", m.stateDir, err)
	}
	m.notify()
	return nil
}

// Close stops the node and the watcher for good (server shutdown). It leaves the
// settings alone — an enabled node must come back up on the next boot — and is
// idempotent.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.stopLocked()
	return nil
}

// startLocked brings the node up under the given settings. Called with mu held.
func (m *Manager) startLocked(ctx context.Context, s store.TailnetSettings) {
	if m.node == nil {
		m.failLocked(ErrNoNode)
		return
	}
	// The state directory is created here — at join time, 0700 — and nowhere else.
	// It holds the node's keys, alongside the ACME cache and the server identity and
	// for the same reason (ADR-0007/0043): durable state whose loss costs a re-auth.
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		m.failLocked(fmt.Errorf("tailnet: creating node state directory %q: %w", m.stateDir, err))
		return
	}
	warnIfStateDirIsExposed(m.stateDir)
	cfg := Config{
		Hostname:   s.Hostname,
		ControlURL: s.ControlURL,
		StateDir:   m.stateDir,
	}
	if m.authKey != nil {
		cfg.AuthKey = m.authKey()
	}
	if err := m.node.Start(ctx, cfg); err != nil {
		m.failLocked(err)
		// A start that failed before writing anything leaves an EMPTY directory behind,
		// and the most common way to get here is a build with no Tailnet support linked
		// in, where it can never contain anything. os.Remove refuses a non-empty
		// directory, so a node that did get as far as writing its keys keeps them —
		// which is the whole difference between this and Forget.
		_ = os.Remove(m.stateDir)
		return
	}
	m.running = true
	m.applied = s
	m.lastErr = ""

	// Watch the node's own transitions so a login URL appearing — or a key lapsing
	// months later — reaches the admin UI without anything polling for it. The
	// goroutine exists only while the node does, which is what keeps a server with
	// the feature off from starting one.
	m.watchStop = make(chan struct{})
	m.watchDone = make(chan struct{})
	go m.watch(m.node.Changes(), m.node.Status(), m.watchStop, m.watchDone)

	log.Printf("obelo: tailnet: node %q started (%s)", s.Hostname, m.node.Status().State)
	m.notify()
}

// stopLocked takes the node down, keeping its state directory. Called with mu held.
func (m *Manager) stopLocked() {
	if !m.running {
		return
	}
	// Join the watcher BEFORE closing the node: the watcher reads a channel the node
	// owns, and a node being torn down underneath a live reader is how a "harmless"
	// shutdown path becomes an intermittent race nobody can reproduce.
	close(m.watchStop)
	<-m.watchDone
	m.watchStop, m.watchDone = nil, nil

	m.running = false
	m.applied = store.TailnetSettings{}
	// The HTTPS listener lives on the node's own userspace stack, so a node that is
	// going down takes tailnet :443 with it — "not bound" is an observation here,
	// not an assumption. The supervisor reports the same thing a moment later when
	// it reconciles; this is what keeps the panel from claiming an https:// address
	// during the gap. The failure goes with it: a message about a listener nobody
	// is trying to bind any more is stale (Apply's lastErr reasoning, one field on).
	m.httpsBound, m.httpsError = false, ""
	if err := m.node.Close(); err != nil {
		// Nothing to do about it and nothing to fail: the node is down either way,
		// and the operator asked for it to be down.
		log.Printf("obelo: tailnet: WARNING — stopping the node: %v", err)
	}
	m.notify()
}

// failLocked records a failed start as a warning: logged once, kept in Status so
// the settings screen can explain it, and never returned upward as a fatal.
func (m *Manager) failLocked(err error) {
	m.lastErr = err.Error()
	log.Printf("obelo: tailnet: WARNING — %v; the server is serving the LAN normally and remote access over the Tailnet is off", err)
	m.notify()
}

// watch fans the node's state transitions out as refetch nudges, and reports the
// node key's expiry to the log, until the node is stopped. It never touches the
// Manager's mutex — it only pokes the Broker and its own local reporter — so it
// cannot deadlock against the reconciliation that is waiting to join it.
//
// initial is the status the node was in when it started, so a node that comes up
// already running (a reconnect, which needs no login) logs its expiry too rather
// than waiting for a transition that may not come for six months.
func (m *Manager) watch(changes <-chan Status, initial Status, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	// The reporter's state is a LOCAL, which is what keeps it correct with no lock:
	// its life is exactly the node's, so "have we already logged this node's expiry"
	// cannot outlive the node it was about.
	var expiry expiryReporter
	expiry.observe(initial, time.Now())
	for {
		select {
		case <-stop:
			return
		case s := <-changes:
			expiry.observe(s, time.Now())
			m.notify()
		}
	}
}

// notify fires the "go refetch" nudge at the Broker and every Subscribe caller. It
// is called both from the reconciliation paths (which hold mu) and from the
// watcher (which does not): neither the Broker's Publish nor a well-behaved
// subscriber re-enters this package, so firing it under the lock is safe and keeps
// the nudge on the same side of the mutex as the transition that caused it. See
// Subscribe for the obligation that puts on a subscriber.
func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
	m.subMu.Lock()
	subs := make([]func(), len(m.subscribers))
	copy(subs, m.subscribers)
	m.subMu.Unlock()
	for _, f := range subs {
		f()
	}
}

// warnIfStateDirIsExposed logs when the node's state directory is readable beyond
// its owner. A warning, never a refusal — warnIfACMECacheIsExposed's posture
// (cmd/obelo/tls.go), for the directory that holds the node key rather than the
// ACME account key: the operator may have arranged group access deliberately, and
// this server does not get to overrule their filesystem. But anyone who can read
// this directory can BE this node on the operator's Tailnet, and that is not a
// thing anybody discovers on their own, because everything works.
//
// It lives here rather than in the adapter so it compiles and is tested in both
// builds; the adapter calls it too, because Node.Start cannot assume its caller
// created the directory.
func warnIfStateDirIsExposed(dir string) {
	info, err := os.Stat(dir)
	if err != nil {
		return // The caller is already reporting whatever went wrong here.
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Printf("obelo: tailnet: WARNING: the node state directory %s is mode %04o — readable beyond its owner, and it holds this node's key; anyone who can read it can join the operator's Tailnet as this server. Consider chmod 700.", dir, perm)
	}
}

// expiryReporter puts the node key's expiry in the BOOT LOG, which ADR-0043
// promises and nothing built until now.
//
// It is the only channel that reaches an operator who has not opened the web UI
// since setup — precisely the person this failure finds, six months after the last
// time anybody touched the box, with the symptom "remote access stopped working"
// and nothing local broken. The settings panel (issue 04) is the other half; both
// are named in the ADR and neither substitutes for the other.
//
// It reports once per node that comes up, and again if the expiry itself changes
// (the operator disabling key expiry in the console is exactly such a change, and
// is the fix, so it deserves to be visible in the log that complained).
type expiryReporter struct {
	reported bool
	at       *time.Time
}

// observe reports on a status if it is one that carries a live node's expiry.
//
// A reporter's life is exactly one node up-cycle (it is a local in watch), so
// "once per node that comes up" needs no reset on the way down — and MUST NOT have
// one. A stopped status can still arrive on the stream after the node is back up:
// the change channel outlives any single Start/Close cycle by contract, so a
// disconnect's final transition can be read by the NEXT cycle's watcher, and a
// reporter that reset on it would log the same expiry twice on every reconnect.
func (r *expiryReporter) observe(s Status, now time.Time) {
	switch s.State {
	case StateRunning, StateKeyExpired:
	default:
		return
	}
	if r.reported && sameTime(r.at, s.KeyExpiry) {
		return
	}
	r.reported, r.at = true, s.KeyExpiry
	logKeyExpiry(s.KeyExpiry, now)
}

// logKeyExpiry writes the one line about the node key's expiry.
//
// now is a parameter rather than a call to time.Now so the threshold can be tested
// from both sides without a test that waits six months or one that fakes a clock
// package-wide.
func logKeyExpiry(expiry *time.Time, now time.Time) {
	// NO EXPIRY IS THE GOOD CASE and it must not print a zero time. A node joined
	// with a tagged auth key — or one whose expiry the operator disabled in the
	// console, which is what the runbook tells them to do — never lapses, and
	// rendering that as "expires 0001-01-01" would be a warning about the one
	// configuration that cannot fail.
	if expiry == nil || expiry.IsZero() {
		log.Printf("obelo: tailnet: the node key does not expire — remote access over the Tailnet will not lapse on its own")
		return
	}
	at := expiry.UTC().Format(time.RFC3339)
	switch remaining := expiry.Sub(now); {
	case remaining <= 0:
		log.Printf("obelo: tailnet: WARNING — the node key EXPIRED on %s, so remote access over the Tailnet is down until this node is re-authorized (the settings screen has a fresh login link). The LAN is unaffected. To stop this recurring, disable key expiry for this node in the Tailscale console (Machines → this node → Disable key expiry) — nothing in Obelo can extend it.", at)
	case remaining <= KeyExpiryWarningWindow:
		log.Printf("obelo: tailnet: WARNING — the node key expires in %d day(s), on %s, and remote access over the Tailnet stops when it does. Disable key expiry for this node in the Tailscale console (Machines → this node → Disable key expiry); nothing in Obelo can extend it.", days(remaining), at)
	default:
		log.Printf("obelo: tailnet: the node key expires on %s (in %d day(s))", at, days(remaining))
	}
}

// days rounds a remaining duration UP to whole days, so "23 hours left" reads as 1
// rather than 0 — a warning that says a key expires in zero days on a day it has
// not yet expired is a warning nobody trusts twice.
func days(d time.Duration) int {
	return int((d + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
}

// sameTime compares two optional instants, nil included.
func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}
