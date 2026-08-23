package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/config"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/tailnet"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The third listener (ADR-0043) — the first one in this server whose lifetime is
// SHORTER than the process's. These tests are about that difference and nothing
// else: binding when the node comes up, dropping it when the node goes away, and
// the two stops being different calls because one of them must not close the
// events broker.
//
// They run in BOTH builds. Nothing here needs `tsnet`: the supervisor talks to the
// state machine through a narrow interface, which is the property that keeps the
// build tag confined to one file.

// fakeNode drives the supervisor: a state that a test moves, and a Listen that
// hands out loopback listeners standing in for the node's userspace stack.
//
// ListenTLS is modelled on what `tsnet` actually returns rather than on what would
// be convenient: a listener that ALREADY terminates TLS (tsnet wraps its own in
// tls.NewListener with the MagicDNS certificate), so the tests below exercise the
// real shape — an http.Server with no TLSConfig serving a listener that is already
// encrypted. tlsErr stands in for the two console prerequisites being absent, which
// is where the real thing fails and is most of what this ticket is about.
type fakeNode struct {
	mu      sync.Mutex
	state   tailnet.State
	listens int
	err     error
	// addr is where the last handed-out listener actually landed. The supervisor
	// binds the Tailnet's :80, which is a port on the node's own userspace stack and
	// not something a test can dial — so the stand-in stack records the loopback
	// address it substituted.
	addr string

	// https is the operator's opt-in (the HTTPSEnabled setting).
	https bool
	// tlsCert is the stand-in for the MagicDNS certificate; nil means ListenTLS
	// hands back a plain loopback listener, which is enough for the tests that only
	// care about whether it bound.
	tlsCert *tls.Certificate
	// tlsErr fails every ListenTLS — the console prerequisite that is missing.
	tlsErr error
	// tlsBlock, when non-nil, parks ListenTLS until it is closed, standing in for
	// tsnet's own unbounded wait for the node to come up.
	tlsBlock   chan struct{}
	tlsListens int
	tlsAddr    string

	// reports is what the supervisor handed back through ReportHTTPS — the
	// observation the settings screen is built on. Recorded rather than ignored
	// because "the supervisor said :443 is up" and ":443 is up" have to be the same
	// fact, and a fake that swallowed the call would let them drift.
	reports       int
	reportedBound bool
	reportedErr   string
}

func (f *fakeNode) ReportHTTPS(bound bool, failure string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports++
	f.reportedBound, f.reportedErr = bound, failure
}

func (f *fakeNode) lastReport() (bound bool, failure string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reportedBound, f.reportedErr
}

func (f *fakeNode) Status() tailnet.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return tailnet.Status{State: tailnet.StateStopped}
	}
	return tailnet.Status{State: f.state, FQDN: "obelo.tail1a2b.ts.net"}
}

func (f *fakeNode) HTTPSEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.https && f.state == tailnet.StateRunning
}

func (f *fakeNode) ListenTLS(network, addr string) (net.Listener, error) {
	f.mu.Lock()
	f.tlsListens++
	err, block, cert := f.tlsErr, f.tlsBlock, f.tlsCert
	running := f.state == tailnet.StateRunning
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, tailnet.ErrNotRunning
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.tlsAddr = ln.Addr().String()
	f.mu.Unlock()
	if cert == nil {
		return ln, nil
	}
	return tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}), nil
}

func (f *fakeNode) setHTTPS(on bool) {
	f.mu.Lock()
	f.https = on
	f.mu.Unlock()
}

func (f *fakeNode) dialTLSAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tlsAddr
}

func (f *fakeNode) tlsListenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tlsListens
}

func (f *fakeNode) Listen(network, addr string) (net.Listener, error) {
	f.mu.Lock()
	f.listens++
	err := f.err
	running := f.state == tailnet.StateRunning
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, tailnet.ErrNotRunning
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.addr = ln.Addr().String()
	f.mu.Unlock()
	return ln, nil
}

func (f *fakeNode) dialAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeNode) set(s tailnet.State) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}

func (f *fakeNode) listenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listens
}

// boundAddr returns the address the supervisor is currently serving on, or "".
func boundAddr(t *tailnetListener) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		return ""
	}
	return t.srv.Addr
}

// serving reports whether the supervisor currently holds a listener.
func serving(t *tailnetListener) bool { return boundAddr(t) != "" }

// boundTLSAddr and servingTLS are the same two for tailnet :443.
func boundTLSAddr(t *tailnetListener) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tlsSrv == nil {
		return ""
	}
	return t.tlsSrv.Addr
}

func servingTLS(t *tailnetListener) bool { return boundTLSAddr(t) != "" }

// eventually polls a condition for up to a second. The supervisor reacts on its
// own goroutine — it is nudged, it does not block the transition that nudged it —
// so every assertion about it is necessarily an eventual one.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestListener(t *testing.T, node tailnetBinder, onShutdown func()) *tailnetListener {
	t.Helper()
	cfg := config.Defaults()
	cfg.ListenAddr = ":8080"
	tn := newTailnetListener(node, cfg, http.NotFoundHandler(), onShutdown)
	// Production waits half a minute between HTTPS bind attempts, which is right for
	// an operator walking to another browser tab and wrong for a test. Shortened
	// here, and only here; the constants themselves are asserted in
	// TestTailnetTLSIntervalsAreProductionShaped.
	tn.retryEvery = 5 * time.Millisecond
	tn.bindTimeout = 2 * time.Second
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tn.Shutdown(ctx)
	})
	return tn
}

// fakeMagicDNSCert stands in for the certificate the Tailnet issues for the
// MagicDNS name. Generated in-process for the reason writeKeyPairForTest gives: a
// committed fixture expires, and the test that depends on it then fails on a date
// nobody chose.
func fakeMagicDNSCert(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	certFile, keyFile, pool := writeKeyPairForTest(t, t.TempDir(), "obelo.tail1a2b.ts.net")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("loading the stand-in MagicDNS certificate: %v", err)
	}
	return &cert, pool
}

// TestTailnetListenerFollowsTheNode: the listener appears when the node comes up
// and is gone when the node goes away, with no restart on either side — which is
// the whole reason ADR-0043 made connect/disconnect live rather than boot-time.
func TestTailnetListenerFollowsTheNode(t *testing.T) {
	node := &fakeNode{}
	tn := newTestListener(t, node, func() {})
	tn.start()

	// The default: the feature is off, the node is stopped, nothing binds. A server
	// that opened a Tailnet listener for an operator who never asked would be the
	// opposite of opt-in.
	time.Sleep(20 * time.Millisecond)
	if serving(tn) {
		t.Fatal("the Tailnet listener bound while the node was stopped")
	}
	if node.listenCount() != 0 {
		t.Errorf("Listen was called %d time(s) with the node stopped", node.listenCount())
	}

	node.set(tailnet.StateRunning)
	tn.Wake()
	eventually(t, "the listener to bind once the node is running", func() bool { return serving(tn) })
	if got := boundAddr(tn); got != tailnetListenAddr {
		t.Errorf("bound %q, want %q — the Tailnet port is on the node's own stack, so :80 is free", got, tailnetListenAddr)
	}

	node.set(tailnet.StateStopped)
	tn.Wake()
	eventually(t, "the listener to go when the node does", func() bool { return !serving(tn) })
}

// TestTailnetListenerIgnoresStatesThatAreNotRunning: a node that is starting,
// waiting on a human, or whose key has lapsed has no reachable address. Binding
// then would produce a listener nothing can route to and a log line claiming the
// server is reachable at a name that answers nothing.
func TestTailnetListenerIgnoresStatesThatAreNotRunning(t *testing.T) {
	for _, state := range []tailnet.State{
		tailnet.StateStarting, tailnet.StateNeedsLogin, tailnet.StateKeyExpired, tailnet.StateError,
	} {
		t.Run(string(state), func(t *testing.T) {
			node := &fakeNode{state: state}
			tn := newTestListener(t, node, func() {})
			tn.start()
			time.Sleep(20 * time.Millisecond)
			if serving(tn) {
				t.Errorf("bound a listener with the node in %q", state)
			}
		})
	}
}

// TestTailnetListenerMirrorsTheHTTPPosture: the Tailnet listener is built from
// newHTTPServer for the same reason the TLS one is — so the two cannot drift. In
// particular WriteTimeout must stay zero, or every HLS segment, direct download
// and SSE stream served over the Tailnet is cut off mid-response, which would look
// like a flaky VPN rather than a config mistake.
func TestTailnetListenerMirrorsTheHTTPPosture(t *testing.T) {
	cfg := config.Defaults()
	plain := newHTTPServer(cfg, http.NotFoundHandler())
	srv := newTailnetServer(cfg, http.NotFoundHandler(), func() {})

	if srv.Addr != tailnetListenAddr {
		t.Errorf("Addr = %q, want %q — the Tailnet port is on the node's own userspace stack, so :80 is free and needs no privilege", srv.Addr, tailnetListenAddr)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero value truncates media and the SSE stream", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != plain.ReadHeaderTimeout || srv.ReadTimeout != plain.ReadTimeout || srv.IdleTimeout != plain.IdleTimeout {
		t.Errorf("timeouts differ from the plain-HTTP listener (%v/%v/%v vs %v/%v/%v); a request's treatment must not depend on which listener accepted it",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout,
			plain.ReadHeaderTimeout, plain.ReadTimeout, plain.IdleTimeout)
	}
	// No certificate: tailnet :443 is issue 03 and is opt-in, so nothing here calls
	// ListenTLS. (TLSConfig itself is not asserted nil — net/http allocates an empty
	// one on any server it configures HTTP/2 for, including the plain listener.)
	if srv.TLSConfig != nil {
		t.Error("the Tailnet listener was built with a TLSConfig; tailnet :443 is issue 03")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil — all three listeners serve the same handler (ADR-0043)")
	}
}

// TestDisconnectMustNotCloseTheEventsBroker is THE trap this listener introduces,
// and the reason stopping it at runtime and stopping it at shutdown are different
// calls.
//
// Every listener registers application.Events.Close as an onShutdown hook, because
// GET /events only returns when the broker closes. That is correct exactly once —
// when the process is exiting. This listener also disappears when an admin clicks
// Disconnect, and http.Server.Shutdown would run the hook then too: remote access
// being switched off would silently kill the SSE stream for every LAN client and
// leave it closed for the life of the process. Close does not run hooks, which is
// why unbind uses it.
func TestDisconnectMustNotCloseTheEventsBroker(t *testing.T) {
	var brokerClosed atomic.Int64
	node := &fakeNode{state: tailnet.StateRunning}
	tn := newTestListener(t, node, func() { brokerClosed.Add(1) })
	tn.start()
	eventually(t, "the listener to bind", func() bool { return serving(tn) })

	// The node goes away at runtime — an admin clicked Disconnect, or the key lapsed.
	node.set(tailnet.StateStopped)
	tn.Wake()
	eventually(t, "the listener to go", func() bool { return !serving(tn) })

	if n := brokerClosed.Load(); n != 0 {
		t.Fatalf("stopping the Tailnet listener at runtime closed the events broker %d time(s). "+
			"That ends SSE for every LAN client for the life of the process — Disconnect must not "+
			"reach for Shutdown, whose hooks do exactly this.", n)
	}

	// It comes back, which it could not do if the broker had gone with it.
	node.set(tailnet.StateRunning)
	tn.Wake()
	eventually(t, "the listener to come back", func() bool { return serving(tn) })
}

// TestShutdownDrainsTheTailnetListenerWithLiveSSESubscriber is the three-listener
// version of the assertion native-tls/01 had to sharpen: the Tailnet listener is
// drained ON ITS OWN, so its own close hook is what releases a subscriber parked
// on it. Draining it as part of the whole set would pass even with no hook at all,
// because the other listeners' hooks close the same broker concurrently — the
// exact bug that test found, moved one listener sideways.
func TestShutdownDrainsTheTailnetListenerWithLiveSSESubscriber(t *testing.T) {
	release := make(chan struct{})
	closeBroker := sync.OnceFunc(func() { close(release) })

	subscribed := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(subscribed)
		<-release
	})

	node := &fakeNode{state: tailnet.StateRunning}
	cfg := config.Defaults()
	tn := newTailnetListener(node, cfg, handler, closeBroker)
	tn.start()
	eventually(t, "the listener to bind", func() bool { return serving(tn) })

	// Park an SSE subscriber on the Tailnet listener specifically.
	go func() {
		resp, err := http.Get("http://" + node.dialAddr() + "/api/v1/events")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSE subscriber never reached the handler")
	}

	// The real budget, so a regression fails on the clock rather than on a number
	// this test invented.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if err := tn.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("draining the Tailnet listener alone took %v with an SSE subscriber parked on it — "+
			"its own close hook is missing, so Shutdown waited out the whole timeout", elapsed)
	}
	// Idempotent: run() may reach it from the signal path and the deferred path both.
	if err := tn.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown = %v, want nil", err)
	}
}

// TestShutdownAllDrainsAllThreeListeners: the entry point run() actually calls,
// with all three at once and one of them a supervisor rather than a server.
func TestShutdownAllDrainsAllThreeListeners(t *testing.T) {
	release := make(chan struct{})
	closeBroker := sync.OnceFunc(func() { close(release) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-release
	})

	cfg := config.Defaults()
	cfg.ListenAddr = "127.0.0.1:0"
	servers, err := newServers(cfg, handler, closeBroker)
	if err != nil {
		t.Fatalf("newServers: %v", err)
	}
	node := &fakeNode{state: tailnet.StateRunning}
	tn := newTailnetListener(node, cfg, handler, closeBroker)
	tn.start()
	eventually(t, "the Tailnet listener to bind", func() bool { return serving(tn) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := shutdownAll(ctx, servers, tn); err != nil {
		t.Errorf("shutdownAll = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdownAll took %v, want a prompt drain", elapsed)
	}
}

// TestTailnetListenerCyclesLeakNoGoroutines: an operator can toggle remote access
// all afternoon. Each cycle binds a listener, starts a serve goroutine and drops
// both; a cycle that leaked either would accumulate silently on a server that is
// never restarted, which is the deployment this product is for.
func TestTailnetListenerCyclesLeakNoGoroutines(t *testing.T) {
	node := &fakeNode{}
	tn := newTestListener(t, node, func() {})
	tn.start()

	cycle := func() {
		node.set(tailnet.StateRunning)
		tn.Wake()
		eventually(t, "the listener to bind", func() bool { return serving(tn) })
		node.set(tailnet.StateStopped)
		tn.Wake()
		eventually(t, "the listener to go", func() bool { return !serving(tn) })
	}

	// One cycle first, so the baseline includes whatever the first bind allocates
	// once (the http.Server's own bookkeeping) rather than counting it as a leak.
	cycle()
	settle()
	before := runtime.NumGoroutine()

	for range 10 {
		cycle()
	}
	settle()

	// A small allowance: this binary's test goroutines are not perfectly quiet, and
	// the assertion that matters is "does not grow with the number of cycles" — ten
	// cycles leaking one goroutine each would be twenty over the slack.
	if after := runtime.NumGoroutine(); after > before+3 {
		t.Errorf("goroutines went %d → %d across ten up/down cycles; each cycle leaks one",
			before, after)
	}
}

// TestBindFailureIsAWarningNotAFailure: the node can go away between the nudge and
// the Listen. That costs the Tailnet listener and nothing else — ADR-0043's
// posture, and the same one `acme` mode takes about a certificate.
func TestBindFailureIsAWarningNotAFailure(t *testing.T) {
	node := &fakeNode{state: tailnet.StateRunning, err: errors.New("the node went away")}
	tn := newTestListener(t, node, func() {})
	tn.start()

	eventually(t, "the bind to be attempted", func() bool { return node.listenCount() > 0 })
	if serving(tn) {
		t.Error("the supervisor kept a listener it never managed to bind")
	}

	// And it recovers: the next transition tries again rather than giving up for the
	// life of the process.
	node.mu.Lock()
	node.err = nil
	node.mu.Unlock()
	tn.Wake()
	eventually(t, "the listener to bind on a later nudge", func() bool { return serving(tn) })
}

// settle gives goroutines a moment to finish exiting before they are counted.
func settle() {
	for range 3 {
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
}

// --- tailnet :443, the MagicDNS certificate (tailscale/03) -------------------
//
// The failure paths are the substance here, and they are the ones easiest to skip:
// a certificate cannot be verified from a unit test, but "the prerequisites are
// missing and the household's media server carries on regardless" can, and that is
// the criterion this feature lives or dies by. Every test below runs in BOTH
// builds — the supervisor talks to the state machine through a narrow interface,
// which is what keeps `tsnet` confined to one file.

// TestTailnetHTTPSIsOffUntilAskedFor is the shipped default. HTTPS on the Tailnet
// needs MagicDNS and HTTPS certificates enabled in the Tailscale console —
// prerequisites this server can neither set nor detect — so a fresh install that
// bound :443 anyway would fail at something the operator has no way to discover
// from the failure. Off until asked for, and NOTHING is requested: no listener, and
// no call that would make the node ask the Tailnet for a certificate.
func TestTailnetHTTPSIsOffUntilAskedFor(t *testing.T) {
	node := &fakeNode{state: tailnet.StateRunning} // running, but the box is unticked
	tn := newTestListener(t, node, func() {})
	tn.start()

	eventually(t, "the plain Tailnet listener to bind", func() bool { return serving(tn) })
	time.Sleep(30 * time.Millisecond)

	if servingTLS(tn) {
		t.Error("tailnet :443 bound with HTTPSEnabled off — it is opt-in (ADR-0043)")
	}
	if n := node.tlsListenCount(); n != 0 {
		t.Errorf("ListenTLS was called %d time(s) with the opt-in off; no certificate may ever be requested", n)
	}
}

// TestTailnetHTTPSBindsAndAnnouncesOnce: with the opt-in on and the prerequisites
// met, :443 joins :80 on the same node and the fact is announced ONCE. Once per
// name rather than once per bind or per handshake, because this is the line an
// operator greps for to confirm the thing worked and it is worthless if it repeats
// — `acmeCertificates.announce`'s rule, restated for a listener.
func TestTailnetHTTPSBindsAndAnnouncesOnce(t *testing.T) {
	logs := captureLog(t)

	cert, _ := fakeMagicDNSCert(t)
	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsCert: cert}
	tn := newTestListener(t, node, func() {})
	tn.start()

	eventually(t, "tailnet :443 to bind", func() bool { return servingTLS(tn) })
	if got := boundTLSAddr(tn); got != tailnetTLSListenAddr {
		t.Errorf("bound %q, want %q", got, tailnetTLSListenAddr)
	}
	if !serving(tn) {
		t.Error("tailnet :80 is not bound; HTTPS is an addition, never a replacement")
	}

	// Nudge it repeatedly. A supervisor that re-announced on every reconcile would
	// fill the log of a healthy server, which is how the one useful line gets lost.
	for range 5 {
		tn.Wake()
	}
	time.Sleep(30 * time.Millisecond)

	if n := strings.Count(logs.String(), "HTTPS is serving on the Tailnet"); n != 1 {
		t.Errorf("announced HTTPS %d times, want exactly 1; log was:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "obelo.tail1a2b.ts.net") {
		t.Errorf("the announcement does not name the MagicDNS name, which is the address it is about; log was:\n%s", logs.String())
	}
}

// TestTailnetHTTPSFailureKeepsPlainHTTPServing IS THE CRITERION THAT MATTERS MOST
// and the one easiest to skip: the operator ticked the box without enabling the two
// console settings first, which is the overwhelmingly likely first run.
//
// Everything must survive it. The server stays up, tailnet :80 stays bound AND
// keeps answering requests, and the log names both console settings so the operator
// knows where to go — the node's own error names only whichever one it noticed
// first, which is why the message is written here rather than passed through.
func TestTailnetHTTPSFailureKeepsPlainHTTPServing(t *testing.T) {
	logs := captureLog(t)

	// tsnet's own words for the first prerequisite, verbatim.
	prereq := errors.New("tsnet: you must enable MagicDNS in the DNS page of the admin panel to proceed. See https://tailscale.com/s/https")
	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsErr: prereq}

	cfg := config.Defaults()
	cfg.ListenAddr = ":8080"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	tn := newTailnetListener(node, cfg, handler, func() {})
	tn.retryEvery = 5 * time.Millisecond
	tn.bindTimeout = 2 * time.Second
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tn.Shutdown(ctx)
	})
	tn.start()

	eventually(t, "the HTTPS bind to be attempted", func() bool { return node.tlsListenCount() > 0 })
	eventually(t, "tailnet :80 to bind regardless", func() bool { return serving(tn) })
	if servingTLS(tn) {
		t.Error("the supervisor kept an HTTPS listener it never managed to bind")
	}

	// The whole point: the Tailnet still serves, over plain HTTP, on the address the
	// operator already has.
	resp, err := http.Get("http://" + node.dialAddr() + "/")
	if err != nil {
		t.Fatalf("plain HTTP on the Tailnet after the certificate failure: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("plain HTTP on the Tailnet = %d %q, want 200 \"ok\" — a certificate problem costs HTTPS, not the Tailnet", resp.StatusCode, body)
	}

	// The message has to answer "what do I go and look at" with no second round trip.
	//
	// These assert the FACTS the message must carry, not its phrasing. The message
	// is read in two places (this log and the settings panel), so its wording gets
	// tuned for both — see failureMessage. A test that pins capitalisation fails on
	// a reword that improved it, which teaches the next person to delete the test.
	got := logs.String()
	for _, want := range []string{
		"serving normally",             // what is unaffected, which is the reassurance
		"MagicDNS",                     // console setting one
		"HTTPS Certificates",           // console setting two, which the node's error never mentions
		"Tailscale admin console",      // and where both of them live
		"http://obelo.tail1a2b.ts.net", // the address that still works
		":8080",                        // and the LAN listener, which was never in question
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the certificate failure was not logged with %q; log was:\n%s", want, got)
		}
	}

	// And it recovers with no restart, which is the other half of the failure being
	// survivable: the fix happens in a browser tab somewhere else, and nothing in
	// this process observes it, so the supervisor has to come back on its own.
	node.mu.Lock()
	node.tlsErr = nil
	node.mu.Unlock()
	eventually(t, "tailnet :443 to bind once the console settings are fixed", func() bool { return servingTLS(tn) })
}

// TestTailnetHTTPSComplaintsAreRateLimited: an unattended box retries forever, and
// one paragraph per attempt buries the single line that matters under a day of
// identical ones. The first failure is never suppressed — that is the one an
// operator who has just ticked the box is waiting for — and the swallowed ones are
// COUNTED, so a reader can tell "this happened once" from "this is happening
// constantly". `acmeCertificates.complain`'s contract, and the same test shape.
func TestTailnetHTTPSComplaintsAreRateLimited(t *testing.T) {
	logs := captureLog(t)

	certs := &tailnetCertificates{
		plainAddr:     ":8080",
		complainEvery: time.Hour,
		retryEvery:    30 * time.Second,
		announced:     map[string]bool{},
	}
	for range 25 {
		certs.complain("obelo.tail1a2b.ts.net", errors.New("you must enable HTTPS in the admin panel"))
	}
	if n := strings.Count(logs.String(), "no HTTPS on the Tailnet"); n != 1 {
		t.Errorf("logged %d failures for 25 attempts, want 1 within the interval", n)
	}

	certs.complainEvery = 0
	certs.complain("obelo.tail1a2b.ts.net", errors.New("still no"))
	if !strings.Contains(logs.String(), "24 further attempts suppressed") {
		t.Errorf("the suppressed attempts were not counted; log was:\n%s", logs.String())
	}

	// A node with no MagicDNS name at all is the state one of the two missing
	// settings actually produces, and the message must still read as a sentence.
	certs.complain("", errors.New("no name"))
	if !strings.Contains(logs.String(), "no MagicDNS name") {
		t.Errorf("a nameless node produced no usable message; log was:\n%s", logs.String())
	}
}

// TestTailnetHTTPSToggleTakesEffectWithoutRestart: unticking the box drops :443 and
// touches nothing else. It is the live half of ADR-0043's departure from the TLS
// model — a setting that visibly saves and does nothing until a restart is the
// worst of the available options, and the operator is watching the panel right now.
//
// It also pins the trap the whole supervisor is built around: dropping this
// listener must not run the close hook, because that hook ends the events broker
// for the entire process. Unticking a checkbox would otherwise take SSE away from
// every LAN client until the next restart.
func TestTailnetHTTPSToggleTakesEffectWithoutRestart(t *testing.T) {
	var brokerClosed atomic.Int64

	cert, _ := fakeMagicDNSCert(t)
	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsCert: cert}
	tn := newTestListener(t, node, func() { brokerClosed.Add(1) })
	tn.start()
	eventually(t, "tailnet :443 to bind", func() bool { return servingTLS(tn) })

	node.setHTTPS(false)
	tn.Wake()
	eventually(t, "tailnet :443 to go when the opt-in is withdrawn", func() bool { return !servingTLS(tn) })

	if !serving(tn) {
		t.Error("turning HTTPS off took tailnet :80 with it")
	}
	if n := brokerClosed.Load(); n != 0 {
		t.Fatalf("dropping the Tailnet HTTPS listener closed the events broker %d time(s); that ends SSE for every LAN client for the life of the process", n)
	}

	// And back on, which it could not do if either half had been torn down for good.
	node.setHTTPS(true)
	tn.Wake()
	eventually(t, "tailnet :443 to come back", func() bool { return servingTLS(tn) })
}

// TestTailnetHTTPSFollowsTheNodeDown: :443 is bound on the node's own userspace
// stack, so it must vanish with the node exactly as :80 does. A listener left
// behind on a stack that no longer exists is a supervisor that has lost track of
// its own state, and the next connect would try to bind a port it still holds.
func TestTailnetHTTPSFollowsTheNodeDown(t *testing.T) {
	cert, _ := fakeMagicDNSCert(t)
	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsCert: cert}
	tn := newTestListener(t, node, func() {})
	tn.start()
	eventually(t, "both Tailnet listeners to bind", func() bool { return serving(tn) && servingTLS(tn) })

	node.set(tailnet.StateStopped)
	tn.Wake()
	eventually(t, "both to go when the node does", func() bool { return !serving(tn) && !servingTLS(tn) })

	node.set(tailnet.StateRunning)
	tn.Wake()
	eventually(t, "both to come back", func() bool { return serving(tn) && servingTLS(tn) })
}

// --- httpsBound: what the server ACHIEVED, not what was ASKED FOR (tailscale/06)
//
// The settings screen used to pick the scheme of the address it displays from the
// HTTPSEnabled setting. That is the operator's REQUEST, and it comes apart from
// reality in exactly the case this feature is most likely to be misconfigured in:
// tailnet HTTPS needs MagicDNS and HTTPS certificates enabled in the Tailscale
// console, which this server can neither set nor detect ahead of time, so an
// operator who ticks the box without doing the console half was shown an
// `https://` address that refuses connections while `:80` served happily.
//
// The fix is one fact travelling from the only place that can observe it — this
// supervisor, which is the only thing that binds tailnet :443 — to the only place
// the screen can read it, the state machine. These tests drive that whole chain
// with a REAL tailnet.Manager rather than a stub, because a stub would prove the
// call is made and this needs to prove the answer changes.

// tailnetSettingsStore is the settings row the state machine reads, in memory:
// enough to run a real Manager from this package with no database.
type tailnetSettingsStore struct {
	mu sync.Mutex
	s  store.TailnetSettings
}

func (m *tailnetSettingsStore) TailnetSettings() (store.TailnetSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s, nil
}

func (m *tailnetSettingsStore) SetTailnetEnabled(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Enabled = enabled
	return nil
}

func (m *tailnetSettingsStore) setHTTPS(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.HTTPSEnabled = on
}

// scriptedNode is tailnet.Fake with a ListenTLS failure the test can turn OFF
// while the supervisor is running — which is the actual sequence this feature has
// to survive, because the fix happens in a browser tab somewhere else and nothing
// in this process observes it. The Fake's own ListenTLSErr field cannot be flipped
// mid-run: it is read from the supervisor's goroutine, and writing it from the
// test's would be a race rather than a scenario.
type scriptedNode struct {
	*tailnet.Fake
	mu  sync.Mutex
	err error
}

func (n *scriptedNode) ListenTLS(network, addr string) (net.Listener, error) {
	n.mu.Lock()
	err := n.err
	n.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return n.Fake.ListenTLS(network, addr)
}

// consoleSettingsFixed is the operator enabling MagicDNS in the Tailscale console.
func (n *scriptedNode) consoleSettingsFixed() {
	n.mu.Lock()
	n.err = nil
	n.mu.Unlock()
}

// TestTailnetHTTPSBoundTracksTheListenerNotTheSetting IS THE CRITERION THIS TICKET
// EXISTS FOR: with the box ticked and the console prerequisites missing, the state
// machine must say HTTPS is NOT bound, and it must say so while the setting that
// asked for it is still true.
//
// It then proves the field MOVES with the listener in both directions — true after
// a bind that succeeded, false again when the listener goes away — because a field
// that merely mirrored the setting would pass a one-shot assertion and reproduce
// the original bug in every other state.
func TestTailnetHTTPSBoundTracksTheListenerNotTheSetting(t *testing.T) {
	logs := captureLog(t)
	ctx := context.Background()

	// The overwhelmingly likely first run: the operator ticked the box and has not
	// been to the Tailscale console. tsnet's own words for the first prerequisite,
	// verbatim — and note it names only ONE of the two, which is why the message the
	// operator ends up reading is composed rather than passed through.
	prereq := errors.New("tsnet: you must enable MagicDNS in the DNS page of the admin panel to proceed. See https://tailscale.com/s/https")
	node := &scriptedNode{
		Fake: &tailnet.Fake{
			Fresh:     tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"},
			Returning: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"},
		},
		err: prereq,
	}
	settings := &tailnetSettingsStore{s: store.TailnetSettings{
		Enabled: true, Hostname: "obelo", HTTPSEnabled: true,
	}}
	mgr := tailnet.NewManager(settings, node, tailnet.ManagerOptions{
		StateDir: filepath.Join(t.TempDir(), "tailscale"),
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if err := mgr.Apply(ctx); err != nil {
		t.Fatalf("apply: %v", err)
	}

	tn := newTestListener(t, mgr, func() {})
	mgr.Subscribe(tn.Wake)
	tn.start()

	// 1. Wanted, and NOT bound. The setting says yes; the listener is not there.
	eventually(t, "the state machine to report HTTPS unbound", func() bool {
		s := mgr.Status()
		return s.State == tailnet.StateRunning && !s.HTTPSBound && s.HTTPSError != ""
	})
	status := mgr.Status()
	if !settings.s.HTTPSEnabled {
		t.Fatal("the opt-in is off; this test proves nothing unless the request and the outcome disagree")
	}
	if status.HTTPSBound {
		t.Fatal("httpsBound is true with nothing bound — it is tracking the SETTING, which is the bug: " +
			"the screen then shows an https:// address that refuses connections")
	}

	// The reason travels with it, in the words that name BOTH console settings —
	// the node's own error names only the one it noticed first, and "not yet" with
	// no reason is unactionable.
	for _, want := range []string{"MagicDNS", "HTTPS Certificates", "Tailscale admin console"} {
		if !strings.Contains(status.HTTPSError, want) {
			t.Errorf("httpsError does not mention %q, so the panel cannot name the fix; it was:\n%s", want, status.HTTPSError)
		}
	}
	// And it is the SAME paragraph the log carries, byte for byte — not a second
	// message written for the UI, which would answer the same question in words
	// that drift apart the first time either is edited.
	if want := tn.certs.failureMessage("obelo.tail1a2b.ts.net", prereq); status.HTTPSError != want {
		t.Errorf("httpsError is not the composed failure message.\n got: %s\nwant: %s", status.HTTPSError, want)
	}
	if !strings.Contains(logs.String(), status.HTTPSError) {
		t.Errorf("httpsError is not the message that was logged.\nreported: %s\nlogged:   %s", status.HTTPSError, logs.String())
	}

	// 2. The operator fixes the console. Nothing in this process observes that, so
	//    the retry timer is what brings :443 up — and httpsBound follows the bind.
	node.consoleSettingsFixed()
	eventually(t, "httpsBound to go true once the listener is actually up", func() bool {
		return mgr.Status().HTTPSBound
	})
	if !servingTLS(tn) {
		t.Error("httpsBound is true with no HTTPS listener held by the supervisor")
	}
	if got := mgr.Status().HTTPSError; got != "" {
		t.Errorf("httpsError = %q after a successful bind, want empty — a green address beside a red paragraph is worse than either", got)
	}

	// 3. The listener goes away. The setting is what changed here, but the field
	//    must follow the LISTENER: it is reported after the unbind, from the
	//    supervisor's own state, not from the setting that caused it.
	settings.setHTTPS(false)
	if err := mgr.Apply(ctx); err != nil {
		t.Fatalf("apply after unticking: %v", err)
	}
	eventually(t, "httpsBound to go false when the listener goes", func() bool {
		return !mgr.Status().HTTPSBound && !servingTLS(tn)
	})
	if got := mgr.Status().HTTPSError; got != "" {
		t.Errorf("httpsError = %q with HTTPS switched off; a failure nobody is trying to fix any more is stale", got)
	}

	// 4. And when the node itself goes down with HTTPS on, the listener goes with
	//    it — it lives on the node's userspace stack — so the claim must too.
	settings.setHTTPS(true)
	if err := mgr.Apply(ctx); err != nil {
		t.Fatalf("apply after re-ticking: %v", err)
	}
	eventually(t, "HTTPS to come back", func() bool { return mgr.Status().HTTPSBound })
	if err := mgr.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if s := mgr.Status(); s.HTTPSBound {
		t.Error("httpsBound survived the node going down; the listener did not, and the address would promise https:// to a stopped node")
	}
}

// TestTailnetHTTPSReportsDoNotSpinTheEventStream: the supervisor retries every
// 30s forever while HTTPS is wanted and unbound, and each retry reports the same
// unchanged fact. A report that nudged unconditionally would put an SSE event on
// every admin's stream twice a minute for the life of a misconfigured server —
// and each of those nudges wakes the supervisor, which reports again.
func TestTailnetHTTPSReportsDoNotSpinTheEventStream(t *testing.T) {
	var nudges atomic.Int64
	node := &scriptedNode{
		Fake: &tailnet.Fake{Fresh: tailnet.Status{State: tailnet.StateRunning, FQDN: "obelo.tail1a2b.ts.net"}},
		err:  errors.New("tsnet: you must enable MagicDNS"),
	}
	settings := &tailnetSettingsStore{s: store.TailnetSettings{
		Enabled: true, Hostname: "obelo", HTTPSEnabled: true,
	}}
	mgr := tailnet.NewManager(settings, node, tailnet.ManagerOptions{
		StateDir: filepath.Join(t.TempDir(), "tailscale"),
		OnChange: func() { nudges.Add(1) },
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if err := mgr.Apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	tn := newTestListener(t, mgr, func() {})
	mgr.Subscribe(tn.Wake)
	tn.start()
	eventually(t, "the first failed bind to be reported", func() bool { return mgr.Status().HTTPSError != "" })

	// Long enough for ~20 retries at the shortened interval.
	settled := nudges.Load()
	time.Sleep(100 * time.Millisecond)
	if extra := nudges.Load() - settled; extra > 2 {
		t.Errorf("%d refetch nudges fired while nothing changed; the retry loop is publishing an event per attempt", extra)
	}
}

// TestTailnetTLSBindCannotWedgeTheSupervisor: `tsnet.Server.ListenTLS` waits for
// the node to be up before it does anything, on a context with no deadline of its
// own — so a node that stops at the wrong moment parks the call forever. That call
// is made from the supervisor's only goroutine, which Shutdown JOINS before it
// drains anything, so an unbounded bind is not a slow certificate: it is a process
// that cannot be stopped, and the operator's symptom is a container they have to
// kill.
func TestTailnetTLSBindCannotWedgeTheSupervisor(t *testing.T) {
	block := make(chan struct{})
	defer close(block) // release the parked call however this test ends

	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsBlock: block}
	tn := newTestListener(t, node, func() {})
	tn.bindTimeout = 50 * time.Millisecond
	tn.start()

	eventually(t, "the blocked bind to be attempted", func() bool { return node.tlsListenCount() > 0 })
	// The plain listener is bound and, crucially, the supervisor is still answering
	// nudges rather than sitting inside somebody else's call.
	eventually(t, "tailnet :80 to bind anyway", func() bool { return serving(tn) })
	node.set(tailnet.StateStopped)
	tn.Wake()
	eventually(t, "the supervisor to still be reacting to transitions", func() bool { return !serving(tn) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := tn.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v with a bind parked inside the node — the supervisor is wedged and no context can rescue it", elapsed)
	}
}

// TestNewTailnetTLSServerMirrorsTheHTTPPosture: the HTTPS listener is built from
// newHTTPServer for the reason the other two are — so no request's treatment
// depends on which listener accepted it — plus the two decisions that are specific
// to this one and silent when wrong.
func TestNewTailnetTLSServerMirrorsTheHTTPPosture(t *testing.T) {
	cfg := config.Defaults()
	plain := newHTTPServer(cfg, http.NotFoundHandler())
	srv := newTailnetTLSServer(cfg, http.NotFoundHandler(), func() {})

	if srv.Addr != tailnetTLSListenAddr {
		t.Errorf("Addr = %q, want %q — the Tailnet's :443 is on the node's own stack, so it needs no privilege and collides with nothing", srv.Addr, tailnetTLSListenAddr)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero value truncates media and the SSE stream, and arriving over TLS does not change that", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != plain.ReadHeaderTimeout || srv.ReadTimeout != plain.ReadTimeout || srv.IdleTimeout != plain.IdleTimeout {
		t.Error("the Tailnet HTTPS listener's timeouts differ from the plain-HTTP listener's; it must be built from newHTTPServer")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil — all three listeners serve the same handler (ADR-0043)")
	}

	// TLSConfig MUST stay nil. The node hands back a listener that already
	// terminates TLS; a TLSConfig here would send serve() to ListenAndServeTLS and
	// invite a double-wrap, which fails as a handshake that never completes.
	if srv.TLSConfig != nil {
		t.Error("the Tailnet HTTPS server carries a TLSConfig; the node's listener already terminates TLS")
	}
	// The one thing that makes a handshake failure attributable. Certificate
	// provisioning happens inside the node on the first handshake for a name, and
	// when it fails the error surfaces here and nowhere else.
	if srv.ErrorLog == nil {
		t.Fatal("ErrorLog is nil — handshake failures would be indistinguishable from the ADR-0041 listener's")
	}
	if got := srv.ErrorLog.Prefix(); !strings.Contains(got, "tailnet") {
		t.Errorf("ErrorLog prefix = %q, want it to name the Tailnet listener", got)
	}
}

// TestTailnetTLSIntervalsAreProductionShaped guards the constants the tests
// deliberately shorten. A retry measured in milliseconds would hammer the node's
// coordination client; one measured in hours would leave an operator who has just
// fixed the console staring at a panel that never changes.
func TestTailnetTLSIntervalsAreProductionShaped(t *testing.T) {
	if tailnetTLSRetryEvery < 5*time.Second || tailnetTLSRetryEvery > 2*time.Minute {
		t.Errorf("tailnetTLSRetryEvery = %v; want seconds-to-a-minute — long enough not to hammer the node, short enough that fixing the console does not need a restart", tailnetTLSRetryEvery)
	}
	if tailnetComplaintInterval < tailnetTLSRetryEvery {
		t.Errorf("tailnetComplaintInterval (%v) is shorter than the retry interval (%v), so every attempt logs a paragraph", tailnetComplaintInterval, tailnetTLSRetryEvery)
	}
	if tailnetTLSBindTimeout == 0 {
		t.Error("tailnetTLSBindTimeout is zero — an unbounded bind wedges the supervisor and with it the whole shutdown")
	}
}

// TestShutdownDrainsTheTailnetTLSListenerWithLiveSSESubscriber: the third
// listener's own third listener. GET /events only returns when the broker closes,
// and Shutdown runs only the hooks of the server it was called on — so a subscriber
// parked on tailnet :443 needs THAT server to carry the hook.
//
// THE PLAIN LISTENER IS DELIBERATELY PREVENTED FROM BINDING, and that is the whole
// design of this test rather than a convenience. Shutdown drains the two Tailnet
// listeners concurrently, so the plain one's hook closes the same broker and would
// release this subscriber even if the HTTPS server had no hook at all — the bug
// sails through, which is exactly what native-tls/01 discovered one listener
// earlier and had to sharpen its own test to catch. With :443 the only thing bound,
// the drain can only complete if its own hook exists.
func TestShutdownDrainsTheTailnetTLSListenerWithLiveSSESubscriber(t *testing.T) {
	release := make(chan struct{})
	closeBroker := sync.OnceFunc(func() { close(release) })

	subscribed := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(subscribed)
		<-release
	})

	cert, pool := fakeMagicDNSCert(t)
	node := &fakeNode{
		state:   tailnet.StateRunning,
		https:   true,
		tlsCert: cert,
		err:     errors.New("tailnet :80 is unavailable in this test on purpose"),
	}
	cfg := config.Defaults()
	tn := newTailnetListener(node, cfg, handler, closeBroker)
	tn.retryEvery = 5 * time.Millisecond
	tn.start()
	eventually(t, "tailnet :443 to bind", func() bool { return servingTLS(tn) })
	if serving(tn) {
		t.Fatal("tailnet :80 bound after all; this test only proves anything while :443 is the only listener")
	}

	go func() {
		resp, err := httpsClient(pool).Get("https://" + node.dialTLSAddr() + "/api/v1/events")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("the SSE subscriber never reached the handler over tailnet :443")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := tn.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("draining the Tailnet listeners took %v with an SSE subscriber parked on :443 — that listener's own close hook is missing, so Shutdown waited out the whole timeout", elapsed)
	}
	if err := tn.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown = %v, want nil", err)
	}
}

// TestTailnetPairSharesSessionsAndSplitsCookies is the pair ADR-0041's hardest-won
// paragraph is about, arriving for the third and fourth time: tailnet :80 and
// tailnet :443 are PLAIN HTTP AND HTTPS ON ONE HOSTNAME, which is precisely the
// collision that cost browser media in production.
//
// IT ALREADY WORKS, AND THE CORRECT DIFF TO internal/api/cookie.go IS ZERO LINES.
// The name is keyed on r.TLS, not on which listener accepted the connection, so
// __Secure-ms_media / ms_media_plain generalizes to a listener nobody had thought
// of when it was written. This test exists so that the next person to touch cookie
// naming finds out here rather than in production — and so that "do not add a third
// name" is a failing test rather than a paragraph in an ADR.
//
// It also pins the other thing that must not be "completed" now that a third
// listener has TLS: HSTS is absent on both halves of the pair. It is sticky per
// hostname, and the plain path still exists on that very hostname.
func TestTailnetPairSharesSessionsAndSplitsCookies(t *testing.T) {
	const (
		secureName = "__Secure-ms_media"
		plainName  = "ms_media_plain"
		legacyName = "ms_media" // the shared name that caused the collision
	)

	h := testharness.New(t)
	status, body := h.JSON(http.MethodPost, "/api/v1/setup", "", map[string]any{
		"claimToken": h.ClaimToken(),
		"username":   "brandon",
		"password":   "hunter2hunter2",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201; body: %s", status, body)
	}

	cert, pool := fakeMagicDNSCert(t)
	node := &fakeNode{state: tailnet.StateRunning, https: true, tlsCert: cert}
	cfg := config.Defaults()
	tn := newTailnetListener(node, cfg, h.Handler(), func() {})
	tn.retryEvery = 5 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tn.Shutdown(ctx)
	})
	tn.start()
	eventually(t, "both Tailnet listeners to bind", func() bool { return serving(tn) && servingTLS(tn) })

	plainBase := "http://" + node.dialAddr()
	tlsBase := "https://" + node.dialTLSAddr()
	tlsClient := httpsClient(pool)

	// One browser, both halves of one hostname: log in over each.
	tlsCookies := loginForCookies(t, tlsClient, tlsBase, "tailnet-https-browser")
	plainCookies := loginForCookies(t, http.DefaultClient, plainBase, "tailnet-plain-browser")

	secure := pickCookie(tlsCookies, secureName)
	if secure == nil {
		t.Fatalf("the tailnet :443 login set no %q cookie; it set %v", secureName, cookieNames(tlsCookies))
	}
	if !secure.Secure {
		t.Errorf("%q is not Secure — requestIsHTTPS is not seeing r.TLS on the Tailnet's TLS listener", secureName)
	}
	plain := pickCookie(plainCookies, plainName)
	if plain == nil {
		t.Fatalf("the tailnet :80 login set no %q cookie; it set %v", plainName, cookieNames(plainCookies))
	}
	if plain.Secure {
		t.Errorf("%q is Secure on tailnet :80; the browser would never send it back over http", plainName)
	}

	// The collision itself, on the new pair. Neither listener may write the other's
	// name, and neither may write the shared name they used before the split.
	for _, c := range tlsCookies {
		if c.Name == plainName || c.Name == legacyName {
			t.Errorf("the tailnet :443 login wrote %q — a name tailnet :80 also writes, on the same hostname, which is the shadowing bug", c.Name)
		}
	}
	for _, c := range plainCookies {
		if c.Name == secureName || c.Name == legacyName {
			t.Errorf("the tailnet :80 login wrote %q — a name tailnet :443 also writes, on the same hostname, which is the shadowing bug", c.Name)
		}
	}

	// Each half authenticates byte-serving from its own cookie with no Authorization
	// header — what a <video>/<img> actually does. 404 means authenticated (the
	// session id is unknown); 401 means it was not.
	if got := cookieOnlyStatus(t, tlsClient, tlsBase, secure); got != http.StatusNotFound {
		t.Errorf("media GET over tailnet :443 with only %q = %d, want 404 (authenticated)", secureName, got)
	}
	if got := cookieOnlyStatus(t, http.DefaultClient, plainBase, plain); got != http.StatusNotFound {
		t.Errorf("media GET over tailnet :80 with only %q = %d, want 404 (authenticated)", plainName, got)
	}
	if got := cookieOnlyStatus(t, http.DefaultClient, plainBase, nil); got != http.StatusUnauthorized {
		t.Errorf("media GET with no cookie = %d, want 401", got)
	}

	// A session created on one half works on the other: same handler, same accounts,
	// same authorization. Nothing in the request path learns which listener it
	// arrived on except through r.TLS.
	token := loginToken(t, tlsClient, tlsBase, "tailnet-roaming-client")
	assertBearerWorks(t, http.DefaultClient, plainBase, token, "a tailnet :443 session on tailnet :80")
	plainToken := loginToken(t, http.DefaultClient, plainBase, "tailnet-roaming-client-2")
	assertBearerWorks(t, tlsClient, tlsBase, plainToken, "a tailnet :80 session on tailnet :443")

	// HSTS stays absent on both, which is the third listener's version of the rule
	// ADR-0041 states: it is sticky per hostname, and the plain path on THIS hostname
	// is the one that always works and has no prerequisites. Do not complete the set.
	for _, tc := range []struct {
		what   string
		client *http.Client
		base   string
	}{
		{"tailnet :443", tlsClient, tlsBase},
		{"tailnet :80", http.DefaultClient, plainBase},
	} {
		req, _ := http.NewRequest(http.MethodGet, tc.base+"/api/v1/server", nil)
		resp, err := tc.client.Do(req)
		if err != nil {
			t.Fatalf("GET /server over %s: %v", tc.what, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s carries Strict-Transport-Security = %q, want it ABSENT — it is sticky per hostname and the plain path on that hostname still exists", tc.what, got)
		}
	}
}

// loginToken logs in over base and returns the bearer token, which is how a native
// client — and this test — carries a session between listeners.
func loginToken(t *testing.T, client *http.Client, base, clientID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"username": "brandon",
		"password": "hunter2hunter2",
		"device":   map[string]any{"name": "Roamer", "platform": "web", "clientId": clientID},
	})
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login at %s: %v", base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login at %s = %d, want 200; body: %s", base, resp.StatusCode, raw)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &login); err != nil || login.Token == "" {
		t.Fatalf("login response at %s has no token (%v); body: %s", base, err, raw)
	}
	return login.Token
}

// assertBearerWorks checks a token issued on one listener is honoured on another.
func assertBearerWorks(t *testing.T, client *http.Client, base, token, what string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/libraries", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("%s = %d, want 200 — a session must work on every listener; body: %s", what, resp.StatusCode, raw)
	}
}
