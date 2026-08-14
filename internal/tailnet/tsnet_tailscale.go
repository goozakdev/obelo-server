//go:build tailscale

// This is THE ONLY FILE in this repository that imports `tailscale.com`, and
// keeping it that way is the whole design (ADR-0043). Everything else in this
// package — the seam, the settings, the state machine, the expiry reporting —
// compiles and tests in BOTH builds against Fake, which is what makes "run the
// suite with and without the tag" a cheap CI matrix instead of a doubled test
// suite. If a `tsnet` type ever appears in a signature outside this file, that
// property is gone and so is the reason the build tag was affordable.
//
// Nothing here has a unit test that talks to a coordination server, because
// there is no honest way to write one: joining a Tailnet needs a Tailnet. What
// IS tested here is the part that is ours rather than theirs — the mapping from
// Tailscale's state machine onto this package's small enum (nodeView/toStatus),
// which is where the interesting mistakes live.

package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Supported reports that this build can actually join a Tailnet. It is what
// GET /server's features.tailscale advertises, and it is a build constant rather
// than a runtime probe because that is exactly what it describes: which of the two
// binaries the operator is running. See the !tailscale file for the other half.
const Supported = true

// NewNode builds the `tsnet`-backed Tailnet node.
//
// It takes no arguments on purpose: everything a join needs arrives in the Config
// handed to Start, including the auth key, which is read at join time and dropped
// (ADR-0043 — nothing between joins holds a Tailnet credential).
func NewNode() Node { return &tsnetNode{changes: make(chan Status, 8)} }

// tsnetNode adapts tailscale.com/tsnet to the Node seam.
//
// A tsnet.Server is single-use — its exported fields may only be set before the
// first method call, and Close is terminal — so a NEW one is constructed on every
// Start. That is what makes a Node reusable across the disconnect/reconnect cycle
// the seam promises: whether the second Start has to re-authenticate is decided by
// what survived in Config.StateDir, which is precisely the distinction between
// Disconnect and Forget.
type tsnetNode struct {
	// changes is the transition stream. Created once, at construction, and never
	// closed: it outlives any individual tsnet.Server, because consumers hold it
	// across Start/Close cycles (see Node.Changes).
	changes chan Status

	mu     sync.Mutex
	srv    *tsnet.Server
	status Status
	// cancelWatch stops the IPN-bus watcher; watchDone closes once it has fully
	// exited. Close joins the watcher BEFORE tearing the server down, so nothing is
	// reading a LocalClient whose backend is being shut down underneath it.
	cancelWatch context.CancelFunc
	watchDone   chan struct{}
}

// noLogUploads is the one process-global Tailscale knob this adapter sets, and it
// is set for an ADR-0001 reason rather than a tidiness one.
//
// tsnet uploads its client logs to log.tailscale.com by default — a THIRD data
// flow to the vendor that ADR-0043 never contemplated. The ADR argues carefully
// for exactly two: the coordination server (opt-in, and swappable for a Headscale
// via the Control URL setting) and DERP (a relay that may carry media). Shipping
// this server's Tailscale client logs to a company the operator may have
// deliberately routed around — a Headscale user gets the uploads too, since the
// log endpoint is not derived from the control URL — would quietly add a third,
// with no setting to turn it off.
//
// The cost is Tailscale's own support for this node, which is not a thing an
// embedded node in somebody else's media server was ever going to use.
//
// It is a Setenv (that is the only interface Tailscale exposes for it), done once,
// before any tsnet code runs.
var noLogUploads sync.Once

// Start brings the node up and returns as soon as it has been ASKED to join.
//
// It deliberately does NOT wait for the node to be running: an interactive join
// (the default, ADR-0043) settles in StateNeedsLogin and completes minutes later
// when a human clicks the link, and a coordination server that is unreachable
// never settles at all. Blocking here would put both of those on the boot path,
// which is the one thing this feature must never do.
func (n *tsnetNode) Start(ctx context.Context, cfg Config) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.srv != nil {
		return errors.New("tailnet: the node is already running")
	}
	// ctx bounds the ATTEMPT, and this is the only place it can honestly be
	// consulted: tsnet.Server.Start takes no context, and everything after it is
	// asynchronous by design. What it buys is that a caller who has already given up
	// — a boot that timed out, a request the operator cancelled — does not get a node
	// started behind its back with nothing left holding the handle.
	if err := ctx.Err(); err != nil {
		return err
	}

	// The state directory holds the node key. Created 0700 here as well as in the
	// Manager because Node.Start cannot assume its caller did it — and warned about
	// rather than refused when the mode is wrong, mirroring newACMECertificates for
	// the directory that holds the ACME account key: the operator's filesystem is
	// theirs, and a server that will not start because a directory is group-readable
	// is a worse outcome than one that says so.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		err = fmt.Errorf("tailnet: creating node state directory %q: %w", cfg.StateDir, err)
		n.setLocked(Status{State: StateError, LastError: err.Error()})
		return err
	}
	warnIfStateDirIsExposed(cfg.StateDir)

	noLogUploads.Do(envknob.SetNoLogsNoSupport)

	srv := &tsnet.Server{
		Dir:      cfg.StateDir,
		Hostname: cfg.Hostname,
		// Empty means Tailscale's own coordination server, which is exactly what the
		// setting means, so no translation is needed in either direction.
		ControlURL: cfg.ControlURL,
		// The join key, used only if there is no node key on disk already. It is not
		// copied onto this struct, not logged, and not echoed anywhere — Config.String
		// redacts it so that even a careless %v of the config cannot disclose it.
		//
		// It is NOT zeroed out of srv after Start returns, and that is deliberate:
		// tsnet owns that field, a future version may legitimately re-read it (a
		// headless node re-authenticating after an expiry, say), and clearing it would
		// buy no real secrecy — the key was in this process's memory the moment it was
		// read from the environment — in exchange for a silent breakage.
		AuthKey: cfg.AuthKey,
		// UserLogf is Tailscale's channel for things meant for a human: chiefly the
		// "to authenticate, visit ..." line, which is the single most useful thing in
		// the log of a headless install whose operator has not opened the web UI.
		UserLogf: func(format string, args ...any) {
			log.Printf("obelo: tailnet: "+strings.TrimRight(format, "\n"), args...)
		},
		// Logf is the backend's verbose debug stream (magicsock, netcheck, the control
		// client's every poll). Discarded: it is thousands of lines an hour on a
		// healthy node, and this server's log is read by an operator looking for their
		// own problem, not by somebody debugging WireGuard.
		Logf: func(string, ...any) {},
	}

	if err := srv.Start(); err != nil {
		// A start that fails here is a local failure — an unusable state directory, a
		// hostname the backend rejects — not a coordination server being down, which
		// fails later and asynchronously. Either way the Manager logs it as a warning
		// and the LAN keeps serving (ADR-0043).
		_ = srv.Close()
		err = fmt.Errorf("tailnet: starting the node: %w", err)
		n.setLocked(Status{State: StateError, LastError: err.Error()})
		return err
	}

	// The watcher's context is derived from Background, NOT from ctx, and this is the
	// subtle one. ctx bounds the join ATTEMPT — app.New gives it 30 seconds so a
	// wedged start cannot hang a boot — while the membership that attempt begins
	// lasts until Close, which may be months. Deriving the watcher from ctx would
	// tear the node's own event stream down half a minute after a perfectly good
	// boot, and the symptom would be a login URL that never appears in the panel.
	watchCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	n.srv, n.cancelWatch, n.watchDone = srv, cancel, done
	n.setLocked(Status{State: StateStarting})
	go n.watch(watchCtx, srv, done)
	return nil
}

// Status is the latest snapshot the watcher published. It never touches the
// network — a settings GET must not be able to block on a coordination server.
func (n *tsnetNode) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.status.State == "" {
		return Status{State: StateStopped}
	}
	return n.status
}

// Changes is the transition stream (owned here, never closed, non-blocking sends).
func (n *tsnetNode) Changes() <-chan Status { return n.changes }

// Listen binds a listener on the node's own userspace stack — tailnet :80.
//
// The port is not an OS socket: it lives on the gVisor netstack inside this
// process, needs no privilege, and cannot collide with anything on the host. That
// is why the caller asks for :80 rather than working around a port already in use.
func (n *tsnetNode) Listen(network, addr string) (net.Listener, error) {
	srv := n.server()
	if srv == nil {
		return nil, ErrNotRunning
	}
	return srv.Listen(network, addr)
}

// ListenTLS is Listen with the MagicDNS certificate (tailnet :443, issue 03). It
// additionally requires MagicDNS and HTTPS certificates to be enabled in the
// Tailscale console — a prerequisite outside this web UI — so its failure costs
// HTTPS on the Tailnet and nothing else.
//
// IT DELIBERATELY DOES NOT CALL tsnet's OWN ListenTLS, AND THE REASON IS HTTP/2.
//
// `tsnet.Server.ListenTLS` is a convenience wrapper that returns
// `tls.NewListener(ln, &tls.Config{GetCertificate: s.getCert})` — a config whose
// NextProtos is EMPTY. ALPN therefore agrees on nothing, and every connection to
// tailnet :443 is HTTP/1.1. Because that listener arrives already TLS-wrapped, the
// caller must Serve() rather than ServeTLS() it, so net/http never gets the chance
// to negotiate h2 for us either. The result was that the REMOTE path — the one
// with the highest latency, where multiplexing many small artwork and HLS segment
// requests matters most — was the one path that could not have HTTP/2, while the
// port-forwarded deployment of ADR-0041 got it for free.
//
// ADR-0043 originally recorded that as unfixable "without reimplementing ListenTLS
// against unexported internals". That was wrong. tsnet's own getCert is
//
//	lc, err := s.LocalClient(); return lc.GetCertificate(hi)
//
// and BOTH of those are exported — `local.Client.GetCertificate` documents itself
// as "the right signature to use as the value of tls.Config.GetCertificate" and
// carries "API maturity: this is considered a stable API". So we build the same
// config tsnet would, add NextProtos, and get HTTP/2 with no private API at all.
//
// What we give up is tsnet's two prerequisite checks, which produce the errors an
// operator needs when the console half is not done. We re-do them here rather than
// lose them: MagicDNS off and no cert domains are the two failures this path
// actually has, and cmd/obelo wraps whatever comes back in a message naming BOTH
// console toggles (tsnet's own error names only the one it noticed first).
func (n *tsnetNode) ListenTLS(network, addr string) (net.Listener, error) {
	srv := n.server()
	if srv == nil {
		return nil, ErrNotRunning
	}
	if network != "tcp" {
		return nil, fmt.Errorf("tailnet: ListenTLS(%q): only tcp is supported", network)
	}

	// Up() rather than Start(): the prerequisites below are read off the node's
	// status, which is not populated until it is up. tsnet's ListenTLS does the
	// same thing first, for the same reason.
	st, err := srv.Up(context.Background())
	if err != nil {
		return nil, err
	}
	// The two console prerequisites, checked in the same order and reported in
	// tsnet's own words so an operator searching for the message still finds
	// Tailscale's documentation for it.
	if st.CurrentTailnet == nil || !st.CurrentTailnet.MagicDNSEnabled {
		return nil, errors.New("tsnet: you must enable MagicDNS in the DNS page of the admin panel to proceed. See https://tailscale.com/s/https")
	}
	if len(st.CertDomains) == 0 {
		return nil, errors.New("tsnet: you must enable HTTPS in the admin panel to proceed. See https://tailscale.com/s/https")
	}

	lc, err := srv.LocalClient()
	if err != nil {
		return nil, err
	}
	ln, err := srv.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: lc.GetCertificate,
		// The whole point of not using tsnet's wrapper. "h2" first so a client
		// offering both gets HTTP/2; "http/1.1" stays for anything that does not.
		NextProtos: []string{"h2", "http/1.1"},
	}), nil
}

// Close stops the node and releases its listeners, KEEPING the state directory so
// the next Start needs no re-authorization. Idempotent.
func (n *tsnetNode) Close() error {
	n.mu.Lock()
	srv, cancel, done := n.srv, n.cancelWatch, n.watchDone
	n.srv, n.cancelWatch, n.watchDone = nil, nil, nil
	n.mu.Unlock()
	if srv == nil {
		return nil
	}

	// Join the watcher BEFORE closing the server. The watcher holds a LocalClient
	// talking to an in-process API server that Close tears down; letting the two
	// overlap turns a routine disconnect into an intermittent error in somebody
	// else's goroutine, which is the class of bug that only ever reproduces in
	// production.
	cancel()
	<-done

	err := srv.Close()
	n.mu.Lock()
	n.setLocked(Status{State: StateStopped})
	n.mu.Unlock()
	return err
}

func (n *tsnetNode) server() *tsnet.Server {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.srv
}

// watch follows the IPN bus and republishes every transition through the seam.
//
// The bus is the reason this is a watcher and not a poll: the login URL appears
// when the backend decides it does, and an operator staring at the panel waiting
// for a link is the first thing anybody does with this feature.
func (n *tsnetNode) watch(ctx context.Context, srv *tsnet.Server, done chan<- struct{}) {
	defer close(done)

	lc, err := srv.LocalClient()
	if err != nil {
		n.publish(Status{State: StateError, LastError: fmt.Sprintf("tailnet: reading node state: %v", err)})
		return
	}

	// view accumulates what the bus reports piecemeal — a Notify carries only what
	// CHANGED — so a state message that arrives without a URL does not erase the URL
	// that arrived a moment earlier.
	var view nodeView
	for ctx.Err() == nil {
		w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// The bus is in-process, so this is not a network failure and retrying
			// tightly would spin. One second is invisible next to a join and short enough
			// that a transient failure does not strand the panel.
			n.publish(Status{State: StateError, LastError: fmt.Sprintf("tailnet: watching node state: %v", err)})
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		for {
			notify, err := w.Next()
			if err != nil {
				w.Close()
				break
			}
			if view.absorb(notify) {
				// Only the fields the bus does not carry need a fetch, and only when
				// something moved: the FQDN, the Tailnet addresses, and the key expiry all
				// live on the status snapshot rather than on the notification.
				view.self = selfStatus(ctx, lc)
				n.publish(view.toStatus(time.Now()))
			}
		}
	}
}

// publish records a status and pushes it onto the change stream.
func (n *tsnetNode) publish(s Status) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.setLocked(s)
}

// setLocked records a status and nudges the stream, dropping the nudge if nobody
// is reading. The consumer re-reads Status — this is a "go and look", not a queue,
// and a stalled consumer must never be able to block the node's own event loop.
func (n *tsnetNode) setLocked(s Status) {
	n.status = s
	select {
	case n.changes <- s:
	default:
	}
}

// selfStatus fetches this node's own entry, or nil when it cannot be had (the
// backend is mid-teardown, or there is no netmap yet). A nil self is a normal,
// early state, not an error: it simply means nothing is known about the node's
// address or expiry yet.
func selfStatus(ctx context.Context, lc *local.Client) *ipnstate.PeerStatus {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil || st == nil {
		return nil
	}
	return st.Self
}

// nodeView is the accumulated picture of the node built from the IPN bus. It is a
// plain struct with a pure projection onto Status (toStatus) so the mapping — the
// only part of this adapter that is ours rather than Tailscale's, and the only
// part where a mistake is silent — can be tested without a Tailnet.
type nodeView struct {
	state    ipn.State
	loginURL string
	errMsg   string
	self     *ipnstate.PeerStatus
}

// absorb folds one Notify into the view and reports whether anything moved.
func (v *nodeView) absorb(n ipn.Notify) bool {
	changed := false
	if n.State != nil && *n.State != v.state {
		v.state, changed = *n.State, true
		// A state that is no longer waiting on a human has no login URL to offer, and a
		// stale one is worse than none: it is a link that looks live and authorizes
		// nothing.
		if *n.State != ipn.NeedsLogin {
			v.loginURL = ""
		}
	}
	if n.BrowseToURL != nil && *n.BrowseToURL != v.loginURL {
		v.loginURL, changed = *n.BrowseToURL, true
	}
	if n.ErrMessage != nil && *n.ErrMessage != v.errMsg {
		v.errMsg, changed = *n.ErrMessage, true
	}
	if n.LoginFinished != nil {
		v.loginURL, v.errMsg, changed = "", "", true
	}
	// SelfChange means this node's own record moved — addresses, name, or KEY
	// EXPIRY, which is the one this feature cares about six months from now.
	if n.SelfChange != nil {
		changed = true
	}
	return changed
}

// toStatus projects the view onto the seam's Status at time now.
//
// THE ORDER OF THE CHECKS IS THE DESIGN. A lapsed key surfaces on the bus as
// plain NeedsLogin with a fresh URL — Tailscale's own state machine has no
// separate value for it (ipnlocal: "the node key expired, need to relogin") — so
// the expiry has to be read off the node's own record BEFORE the backend state is
// consulted, or the two collapse into one indistinguishable state and ADR-0043's
// "re-authorize, not an error" has nothing to stand on.
func (v nodeView) toStatus(now time.Time) Status {
	s := Status{LoginURL: v.loginURL, LastError: v.errMsg}
	if v.self != nil {
		s.FQDN = strings.TrimSuffix(v.self.DNSName, ".")
		s.Addresses = append([]netip.Addr(nil), v.self.TailscaleIPs...)
		// KeyExpiry is a pointer in BOTH types and passes straight through, nil and
		// all: a tagged node has no expiry, and the zero time would render as "expired
		// in year 1" and warn forever on exactly the nodes that are safe.
		s.KeyExpiry = v.self.KeyExpiry
	}

	if expired(v.self, now) {
		s.State = StateKeyExpired
		return s
	}

	switch v.state {
	case ipn.Running:
		s.State = StateRunning
	case ipn.Starting:
		s.State = StateStarting
	case ipn.NeedsLogin:
		s.State = StateNeedsLogin
	case ipn.NeedsMachineAuth:
		// Not needsLogin, because there is no URL to click: the node has authenticated
		// and is waiting for an admin to APPROVE the device in the Tailscale console.
		// Reporting needsLogin without a link would be a dead end (see StateNeedsLogin);
		// reporting the error state with these words names the one action that helps and
		// the only place it can be taken.
		s.State = StateError
		if s.LastError == "" {
			s.LastError = "this node is waiting to be approved in the Tailscale console " +
				"(Machines → approve this device); device approval is on for this Tailnet"
		}
	case ipn.Stopped:
		s.State = StateStopped
	case ipn.NoState:
		// The backend has not decided yet — "Loading…" in Tailscale's own UIs. Starting
		// is the honest thing to show an operator who has just pressed Connect.
		s.State = StateStarting
	default:
		s.State = StateStarting
	}
	if s.LastError != "" && s.State != StateRunning && s.State != StateNeedsLogin && s.State != StateKeyExpired {
		s.State = StateError
	}
	return s
}

// expired reports whether this node's key has lapsed.
//
// Two signals rather than one because they fail in opposite directions: Expired is
// what the coordination server (or the client's own clock) has concluded and is
// the authoritative answer when there is a netmap, while a KeyExpiry already in
// the past catches the window where the flag has not been recomputed yet. Neither
// fires for a node with no expiry at all, which is the common case after the
// runbook's step 3 and the one a naive zero-value read gets wrong.
//
// NOTE the case this cannot see: a key that lapses while the server is DOWN. The
// netmap is not persisted, so after a restart the coordination server simply
// refuses the node key and the backend reports NeedsLogin with nothing attached —
// indistinguishable, from here, from a first join. The distinction holds for the
// far more common case of a running server whose key runs out under it.
func expired(self *ipnstate.PeerStatus, now time.Time) bool {
	if self == nil {
		return false
	}
	if self.Expired {
		return true
	}
	return self.KeyExpiry != nil && !self.KeyExpiry.IsZero() && !self.KeyExpiry.After(now)
}
