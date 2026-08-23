package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/config"
	"github.com/goozakdev/obelo-server/internal/tailnet"
)

// tailnetListenAddr is where the third listener binds on the Tailnet (ADR-0043).
//
// THIS IS NOT AN OS SOCKET. The port lives on the node's own userspace network
// stack inside this process — gVisor netstack over userspace WireGuard — so it
// needs no privilege, collides with nothing on the host, and is reached only by
// peers on the operator's Tailnet. That is why it is :80 and not :8080: there is
// no port 80 on the host being taken, and a client typing the MagicDNS name should
// not have to remember a port. Tailnet :443 lives in tailnet_tls.go and is opt-in.
//
// NOTHING REDIRECTS FROM HERE TO :443, even with HTTPS on. This server emits no
// absolute URLs and performs no redirects anywhere (ADR-0005's retired-claim note
// records why that property is worth keeping); both listeners serve the same
// handler, and the runbook tells people which address to use.
const tailnetListenAddr = ":80"

// tailnetBinder is the slice of the Tailnet state machine this file needs.
// Narrow, so the listener supervisor can be driven by a fake in tests without a
// database, and so no `tsnet` type can reach cmd/obelo — the moment one does, the
// build tag stops being one file (ADR-0043).
type tailnetBinder interface {
	Status() tailnet.Status
	Listen(network, addr string) (net.Listener, error)
	// ListenTLS binds tailnet :443 with the MagicDNS certificate. It may BLOCK (the
	// node waits for itself to be up first), which is why every call below is bounded
	// in wall-clock time rather than trusted to return.
	ListenTLS(network, addr string) (net.Listener, error)
	// HTTPSEnabled is the opt-in: tailnet :443 is bound only when the operator asked
	// for it, because it needs two Tailscale console settings this server can neither
	// set nor detect in advance (ADR-0043). It is read on every reconcile rather than
	// captured once, so ticking the box takes effect with no restart.
	HTTPSEnabled() bool
	// ReportHTTPS hands the OUTCOME of that opt-in back to the state machine, which
	// is the only place the settings screen can read it from.
	//
	// This is the seam's one upward call, and it exists because HTTPSEnabled and
	// "HTTPS is actually serving" are different facts that the UI was conflating:
	// the request is a saved setting, the outcome is a listener that either came up
	// or did not, and the gap between them is exactly the misconfiguration this
	// feature is most likely to be in. Only this file can observe the outcome, so
	// only this file may report it.
	ReportHTTPS(bound bool, failure string)
}

// tailnetListener binds and unbinds the Tailnet listener as the node comes and
// goes, serving the same handler as the other two listeners.
//
// THIS IS THE FIRST LISTENER IN THIS SERVER WHOSE LIFETIME IS SHORTER THAN THE
// PROCESS'S, and everything odd about this type follows from that one fact.
// ADR-0043 makes connect and disconnect take effect with no restart, so this
// listener appears and disappears while the server is running, driven by an admin
// clicking a button — a thing neither the plain-HTTP listener nor the ADR-0041
// HTTPS listener has ever done.
//
// # The shutdown trap, and why the two stops are different calls
//
// Both other listeners carry RegisterOnShutdown(application.Events.Close), because
// GET /events only returns when the broker's channel closes and Shutdown otherwise
// waits out its whole timeout on a parked SSE subscriber (see newServers). This
// listener carries the same hook — a subscriber can be parked on THIS listener,
// and Shutdown only runs the hooks of the server it was called on, so without it
// process shutdown would hang exactly as it did before that hook existed.
//
// But that hook closes the process-wide events broker, which is correct precisely
// once: when the process is exiting. Firing it because an admin clicked Disconnect
// would take the SSE stream away from every LAN client and leave it closed for the
// life of the process — remote access being switched off would silently break live
// updates for everyone who is not remote. That is the trap, and it is why the two
// stops are DIFFERENT CALLS rather than one method with a flag:
//
//   - stop (the node went away): srv.Close(). net/http does not run onShutdown
//     hooks on Close, so the broker survives. It is also the honest call: the
//     node's userspace stack is being torn down in the same breath, so a graceful
//     drain would be draining into a network that no longer exists, and by the time
//     we are woken the listener has usually already been closed underneath us by
//     tsnet itself.
//   - shutdown (the process is exiting): srv.Shutdown(ctx), which DOES run the
//     hook, releases any SSE subscriber parked here, and drains in-flight requests
//     inside the caller's budget alongside the other two listeners.
//
// events.Broker.Close is idempotent, so the hook firing here as well as on the
// other two servers plus run()'s deferred application.Close() amounts to one real
// close and N no-ops.
//
// # The second listener, and why it is a different shape
//
// Tailnet :443 (tailscale/03) hangs off the same supervisor rather than getting
// one of its own, because it is bound by the same node and must disappear with it.
// It differs from :80 in three ways, each of which earns a piece of the machinery
// below: it is OPT-IN (HTTPSEnabled, because it needs two Tailscale console
// settings this server cannot reach), it can fail for a reason the operator fixes
// SOMEWHERE ELSE (hence the retry timer — nothing in this process observes a
// console setting changing), and its failure is REPETITIVE (hence the rate-limited
// complaint). tailnet :80 keeps serving through every one of those failures. See
// tailnet_tls.go.
type tailnetListener struct {
	node       tailnetBinder
	cfg        config.Config
	handler    http.Handler
	onShutdown func()
	// certs owns the log lines about tailnet HTTPS: announced once per name,
	// complained about at most once per interval. See tailnet_tls.go.
	certs *tailnetCertificates
	// retryEvery is how long to wait before trying a wanted-but-unbound HTTPS
	// listener again; bindTimeout bounds one attempt. Fields rather than constants
	// so a test can drive both without waiting out production intervals.
	retryEvery  time.Duration
	bindTimeout time.Duration

	// wake is the nudge from the state machine (Manager.Subscribe), buffered 1 and
	// sent to non-blockingly: transitions coalesce, and the reconcile that follows
	// re-reads the node's CURRENT state rather than trusting the nudge to describe
	// it. The events Broker's "go and look" contract, applied to a listener.
	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	mu     sync.Mutex
	srv    *http.Server
	tlsSrv *http.Server
	shut   bool
}

// newTailnetListener wires the supervisor. It binds nothing until start.
func newTailnetListener(node tailnetBinder, cfg config.Config, h http.Handler, onShutdown func()) *tailnetListener {
	return &tailnetListener{
		node:        node,
		cfg:         cfg,
		handler:     h,
		onShutdown:  onShutdown,
		certs:       newTailnetCertificates(cfg),
		retryEvery:  tailnetTLSRetryEvery,
		bindTimeout: tailnetTLSBindTimeout,
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// start runs the supervisor loop.
//
// It reconciles ONCE before waiting for the first nudge, and that is load-bearing
// rather than tidy: an enabled node connects during app.New (ADR-0043's
// connect-at-boot), which is before this exists to be subscribed, so a supervisor
// that only ever reacted to nudges would sit idle next to a running node until the
// operator happened to toggle something.
//
// The retry arm is the HTTPS half: the prerequisites for tailnet :443 are two
// settings in the Tailscale console, and an operator turning one on produces NO
// event here — no transition, no nudge, nothing to react to. Without a timer the
// listener would stay down until the next restart and the toggle would look broken.
// The timer is armed only while an HTTPS listener is wanted and not bound, so a
// server with the feature off (the default) still parks on a channel forever.
func (t *tailnetListener) start() {
	go func() {
		defer close(t.done)
		for {
			var retry <-chan time.Time
			if t.reconcile() {
				retry = time.After(t.retryEvery)
			}
			select {
			case <-t.stop:
				return
			case <-t.wake:
			case <-retry:
			}
		}
	}()
}

// Wake is the Manager.Subscribe callback: a non-blocking nudge, never a message.
// It must not block and must not call back into the state machine — it is invoked
// on the goroutine that made the transition, often holding that machine's lock.
func (t *tailnetListener) Wake() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// reconcile makes the listeners match the node: tailnet :80 bound while it is
// running, tailnet :443 additionally bound when the operator opted in. Idempotent,
// and the only place any of it happens.
//
// It reports whether an HTTPS bind is still outstanding, which is the caller's cue
// to come back on a timer rather than waiting for a nudge that is not coming.
//
// THE TWO LISTENERS ARE INDEPENDENT, and that ordering is the ticket: a
// certificate problem must cost HTTPS and nothing else, so the plain listener is
// reconciled first, on its own terms, and no HTTPS failure can reach it.
//
// It also tells the state machine what it observed about tailnet :443, which is
// what puts an honest address on the settings screen (tailscale/06). The report is
// taken from the LISTENER FIELD, re-read after the bind or unbind above, and never
// from wantTLS: wantTLS is what the operator asked for, and the screen promising
// an https:// address on the strength of the request rather than the outcome is
// the entire bug that field exists to end.
func (t *tailnetListener) reconcile() (retryTLS bool) {
	// Only StateRunning. A node that is starting, waiting on a login, or whose key
	// has lapsed has no reachable Tailnet address, so binding would either fail or
	// produce a listener nothing can route to.
	status := t.node.Status()
	want := status.State == tailnet.StateRunning
	// Read even when the node is down, because it must be read on every reconcile:
	// this is the field an operator changes in the UI while the server runs.
	wantTLS := want && t.node.HTTPSEnabled()

	t.mu.Lock()
	bound, tlsBound, shut := t.srv != nil, t.tlsSrv != nil, t.shut
	t.mu.Unlock()
	if shut {
		return false
	}

	switch {
	case want && !bound:
		t.bind()
	case !want && bound:
		t.unbind()
	}

	var failure error
	switch {
	case wantTLS && !tlsBound:
		failure = t.bindTLS(status)
	case !wantTLS && tlsBound:
		t.unbindTLS()
	}

	t.mu.Lock()
	nowBound := t.tlsSrv != nil
	t.mu.Unlock()
	t.report(nowBound, failure)
	return wantTLS && !nowBound
}

// report tells the state machine whether tailnet :443 is serving, and why not.
//
// A bound listener carries no failure: the message would be about an attempt that
// has since succeeded, and a panel showing a green address next to a red paragraph
// is worse than either alone. An unbound one with nothing to say is the moment
// between the opt-in being saved and the first attempt returning — rare, brief,
// and the UI has a floor for it.
func (t *tailnetListener) report(bound bool, failure error) {
	msg := ""
	if !bound && failure != nil {
		msg = failure.Error()
	}
	t.node.ReportHTTPS(bound, msg)
}

// newTailnetServer builds the server the Tailnet listener runs. It is a function
// rather than a literal inside bind for the reason newHTTPServer is: the timeouts
// can be asserted without binding anything, on a server nothing is serving on —
// which matters more here than elsewhere, because reading a field of a LIVE
// http.Server races with net/http's own HTTP/2 setup.
//
// It starts from newHTTPServer and changes only the address, so the two listeners
// CANNOT drift: in particular the zero WriteTimeout that keeps HLS, the
// direct-file download and the never-ending SSE stream from being cut off
// mid-response is not a decision that changes because a request arrived over
// WireGuard. The same instruction the TLS listener got, for the same reason.
//
// onShutdown is registered here and fires only on Shutdown, never on Close — see
// the type comment above for why that distinction is the whole design.
func newTailnetServer(cfg config.Config, h http.Handler, onShutdown func()) *http.Server {
	srv := newHTTPServer(cfg, h)
	srv.Addr = tailnetListenAddr
	srv.RegisterOnShutdown(onShutdown)
	return srv
}

// bind opens the Tailnet listener and starts serving on it.
func (t *tailnetListener) bind() {
	ln, err := t.node.Listen("tcp", tailnetListenAddr)
	if err != nil {
		// A warning, never a fatal, and never anything to do with the LAN listener.
		// The node may have gone down between the nudge and this call, or the state
		// directory may be unusable; either way ADR-0043's posture holds — remote
		// access is missing, the household's media server is not.
		log.Printf("obelo: tailnet: WARNING — cannot serve on the Tailnet (%v); the LAN listener is unaffected", err)
		return
	}

	srv := newTailnetServer(t.cfg, t.handler, t.onShutdown)

	t.mu.Lock()
	if t.shut {
		// A shutdown landed between the Listen and here. Do not leave a serving
		// listener behind a supervisor that will never stop it.
		t.mu.Unlock()
		_ = ln.Close()
		return
	}
	t.srv = srv
	t.mu.Unlock()

	log.Printf("obelo: tailnet: serving on the Tailnet at %s (%s)", tailnetListenAddr, describeNode(t.node.Status()))
	go func() {
		// Serve returns when the listener closes — which happens on our own Close or
		// Shutdown, and also when tsnet tears the node down underneath us. Neither is
		// news; anything else is worth a line.
		if err := srv.Serve(ln); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("obelo: tailnet: WARNING — the Tailnet listener stopped: %v", err)
		}
	}()
}

// unbind drops the listener because the node went away. See the type comment for
// why this is Close and not Shutdown: Shutdown would run the close hook, and the
// close hook ends the events broker for the whole process.
func (t *tailnetListener) unbind() {
	t.mu.Lock()
	srv := t.srv
	t.srv = nil
	t.mu.Unlock()
	if srv == nil {
		return
	}
	_ = srv.Close()
	log.Printf("obelo: tailnet: the Tailnet listener is down (the node is no longer running); the LAN listener is unaffected")
}

// bindTLS opens the Tailnet HTTPS listener and starts serving on it, returning
// nil once it is serving and the operator-facing failure otherwise.
//
// THE RETURNED ERROR IS THE ONE THE LOG PRINTED, not the node's raw one. The
// node's error names only whichever console prerequisite it noticed first, which
// is why the complaint is composed here in the first place — and the settings
// screen needs the composed one for the same reason the log does: to answer "what
// do I go and look at" without a second round trip. Composing a separate message
// for the UI would be two paragraphs about one failure, drifting apart.
//
// EVERY FAILURE HERE IS A COMPLAINT AND NOTHING ELSE. Tailnet :80 was reconciled
// before this ran and is serving; the LAN listener has never been in question; the
// server does not exit, does not restart the node, and does not stop trying. That
// is ADR-0043's posture, which is ADR-0041's posture, which is `acme` mode's
// posture — a certificate problem costs HTTPS and nothing else — and it is the
// whole substance of this half of the feature, because the prerequisites live in a
// console this server cannot read.
func (t *tailnetListener) bindTLS(status tailnet.Status) error {
	ln, err := t.listenTLS()
	if err != nil {
		return errors.New(t.certs.complain(status.FQDN, err))
	}

	srv := newTailnetTLSServer(t.cfg, t.handler, t.onShutdown)

	t.mu.Lock()
	if t.shut {
		// The process is exiting; nobody is reading a settings panel. Reported as
		// unbound with nothing to say, which is true and is not a complaint.
		t.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	t.tlsSrv = srv
	t.mu.Unlock()

	t.certs.announce(status.FQDN)
	go func() {
		// Serve, NOT ServeTLS: the node handed back a listener that already terminates
		// TLS with the MagicDNS certificate. See newTailnetTLSServer.
		if err := srv.Serve(ln); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("obelo: tailnet: WARNING — the Tailnet HTTPS listener stopped: %v; plain HTTP on the Tailnet is unaffected", err)
		}
	}()
	return nil
}

// listenTLS binds tailnet :443, bounded in wall-clock time.
//
// The bound is not defensive tidiness. `tsnet.Server.ListenTLS` waits for the node
// to be up before it looks at anything, using a context with no deadline of its
// own, so a node that stops between the status read above and this call parks the
// caller indefinitely — and the caller is the supervisor's only goroutine, which
// Shutdown joins BEFORE it drains anything. An unbounded bind is therefore not a
// slow certificate; it is a server that cannot be stopped, and the operator's
// symptom is a container that has to be killed.
//
// A late arrival is closed rather than kept: by then the supervisor has already
// concluded the bind failed and will try again, and two listeners on one port on
// the node's stack is a state nothing else in this file could unwind. The goroutine
// is bounded by the node's own life — closing the node makes the parked call
// return — so it cannot accumulate.
func (t *tailnetListener) listenTLS() (net.Listener, error) {
	type bound struct {
		ln  net.Listener
		err error
	}
	done := make(chan bound, 1)
	go func() {
		ln, err := t.node.ListenTLS("tcp", tailnetTLSListenAddr)
		done <- bound{ln, err}
	}()

	timer := time.NewTimer(t.bindTimeout)
	defer timer.Stop()
	select {
	case b := <-done:
		return b.ln, b.err
	case <-timer.C:
		go func() {
			if b := <-done; b.ln != nil {
				_ = b.ln.Close()
			}
		}()
		return nil, fmt.Errorf("binding %s on the Tailnet did not return within %s (the node is most likely no longer up)", tailnetTLSListenAddr, t.bindTimeout)
	}
}

// unbindTLS drops the HTTPS listener because the node went away or the operator
// turned the opt-in off. Close and not Shutdown, for the reason given on the type:
// Shutdown runs the close hook, and the close hook ends the events broker for the
// whole process — so unticking a checkbox would take SSE away from every LAN
// client until the next restart.
func (t *tailnetListener) unbindTLS() {
	t.mu.Lock()
	srv := t.tlsSrv
	t.tlsSrv = nil
	t.mu.Unlock()
	if srv == nil {
		return
	}
	_ = srv.Close()
	log.Printf("obelo: tailnet: HTTPS on the Tailnet is down; plain HTTP on the Tailnet and the LAN listener are unaffected")
}

// Shutdown stops the supervisor and drains BOTH Tailnet listeners within ctx, as
// part of the set run() shuts down together. Idempotent.
//
// The supervisor loop is stopped and JOINED first, so nothing can rebind behind
// the drain — a node that reports itself running while the process is exiting must
// not be able to open a listener nobody will ever close. (That join is also why
// every bind in this file is bounded in wall-clock time: a supervisor parked inside
// a third party's call would make this wait forever, ignoring ctx entirely.)
//
// The two listeners are drained CONCURRENTLY and both errors are kept, for the
// reason shutdownAll gives about the three: they share one budget, and a caller
// that spent it on the first would leave a client mid-download on the second with
// nothing left.
func (t *tailnetListener) Shutdown(ctx context.Context) error {
	t.mu.Lock()
	if t.shut {
		t.mu.Unlock()
		return nil
	}
	t.shut = true
	t.mu.Unlock()

	close(t.stop)
	<-t.done

	t.mu.Lock()
	srv, tlsSrv := t.srv, t.tlsSrv
	t.srv, t.tlsSrv = nil, nil
	t.mu.Unlock()

	if srv == nil && tlsSrv == nil {
		// Nothing was bound, so nothing is parked here — but the hook still has to run,
		// because the other listeners' hooks are the only thing releasing SSE
		// subscribers and this method is part of the same shutdown. Broker.Close is
		// idempotent; calling it here costs nothing and makes this path uniform.
		t.onShutdown()
		return nil
	}

	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	for i, s := range [2]*http.Server{srv, tlsSrv} {
		if s == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Shutdown(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs[0], errs[1])
}

// describeNode names the node in the log line that announces the listener. The
// FQDN is the whole point of the feature — it is the address a client away from
// home actually types — so the line is worth little without it.
func describeNode(s tailnet.Status) string {
	if s.FQDN == "" {
		return "no MagicDNS name yet"
	}
	return "http://" + s.FQDN
}
