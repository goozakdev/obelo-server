package tailnet

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Fake is an in-memory Node for tests, and it lives in this package rather than
// in a _test file for the same reason transcode.StaticDetector does: the seam's
// consumers — the API handlers, the state machine, the app wiring — are in other
// packages, and every one of them needs to drive a Tailnet node that cannot exist
// on a CI box.
//
// It is honest about the one thing that actually matters to the state machine:
// whether a join has to authenticate is decided by WHAT IS ON DISK in the state
// directory, not by the Fake's memory. So Disconnect→Connect really does skip
// re-authorization and Forget→Connect really is a fresh join, because the Fake
// writes and reads the same directory the real node would — which is what makes
// the test of those two verbs worth anything.
//
// Set the scripted fields before handing it over; read the observed counters
// afterwards. Every method is safe to call from any goroutine.
type Fake struct {
	// StartErr, when set, fails every Start — the coordination server that cannot
	// be reached, the auth key that was rejected, the node that never starts.
	StartErr error
	// CloseErr, ListenErr, ListenTLSErr, DialErr fail the corresponding call, so
	// each step can be failed independently. DialErr is how a test models the
	// origin that LOOKS like a tailnet name but is not shared into this Tailnet —
	// the case ADR-0055 §5 leaves to the operator and the caller falls back from.
	CloseErr     error
	ListenErr    error
	ListenTLSErr error
	DialErr      error

	// DialFunc, when set, is what Dial actually opens. A linking test points it at
	// the other test Server's listener, which is the whole trick: the origin under
	// test is a MagicDNS name that resolves nowhere, and this is the only thing
	// standing between "the dialer chose the Tailnet" and a connection that works.
	// Unset, Dial opens an ordinary TCP connection to addr, so a Fake needs no
	// scripting to stand in for a node on a reachable network.
	DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

	// Fresh is the status a join settles into when the state directory holds no
	// prior state — the interactive first run, typically StateNeedsLogin carrying a
	// LoginURL. The zero value settles into a plain running node.
	Fresh Status
	// Returning is the status a start settles into when prior state IS on disk —
	// typically StateRunning with an FQDN, and the reason a reconnect is instant.
	// The zero value settles into a plain running node.
	Returning Status

	mu sync.Mutex
	// changes is the transition stream, created lazily and never closed: it lives
	// across Start/Close cycles because a Node is reusable (see Node.Changes).
	changes chan Status
	status  Status
	started bool

	// starts counts Start calls; joins counts only those that had to authenticate
	// (no state on disk). The pair is what a test asserts on to prove a reconnect
	// did NOT re-authenticate.
	starts int
	joins  int
	// lastConfig is the Config of the most recent Start, so a test can prove the
	// auth key reached the join — and then prove it reached nothing else.
	lastConfig Config

	// dials counts Dial calls and dialAddrs records what they asked for, so a test
	// can prove which of ADR-0055 §5's two dialers an origin chose. A count of zero
	// against a successful fetch is the assertion that says "that one went out over
	// the operating system", which no amount of looking at the response can show.
	dials     int
	dialAddrs []string
}

// stateFile is the marker the Fake writes into the state directory on a join. Its
// presence is what makes the next start a returning one, and Forget wiping the
// directory is what makes the start after that a fresh join.
const stateFile = "fake-node.state"

// Start joins (or re-joins) the fake Tailnet.
func (f *Fake) Start(ctx context.Context, cfg Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.starts++
	f.lastConfig = cfg
	if f.StartErr != nil {
		f.setLocked(Status{State: StateError, LastError: f.StartErr.Error()})
		return f.StartErr
	}

	marker := filepath.Join(cfg.StateDir, stateFile)
	settled := f.Returning
	if _, err := os.Stat(marker); err != nil {
		// No state on disk: this join has to authenticate. Record it the way a real
		// node does — by leaving something behind that survives a disconnect.
		f.joins++
		if err := os.WriteFile(marker, []byte(cfg.Hostname), 0o600); err != nil {
			f.setLocked(Status{State: StateError, LastError: err.Error()})
			return err
		}
		settled = f.Fresh
	}
	if settled.State == "" {
		settled.State = StateRunning
	}
	f.started = true
	f.setLocked(settled)
	return nil
}

// Status returns the current snapshot.
func (f *Fake) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status.State == "" {
		return Status{State: StateStopped}
	}
	return f.status
}

// Changes returns the transition stream (see Node.Changes: owned by the node,
// never closed, non-blocking sends).
func (f *Fake) Changes() <-chan Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changesLocked()
}

// Transition pushes a later state change the way a real node reports one — a
// login completing, a key lapsing, a node dropping into error while up. It is how
// a test drives the half of the lifecycle that happens after Start returns.
func (f *Fake) Transition(s Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = s.State != StateStopped
	f.setLocked(s)
}

// Listen returns a loopback listener standing in for tailnet :80. It fails while
// the node is down, which is the real constraint: there is no Tailnet address to
// bind until the node has one.
func (f *Fake) Listen(network, addr string) (net.Listener, error) {
	return f.listen(f.ListenErr)
}

// ListenTLS is Listen for tailnet :443 (issue 03). The Fake serves no TLS — it
// exists so the seam's shape is exercised and so a caller can be made to fail
// exactly there.
func (f *Fake) ListenTLS(network, addr string) (net.Listener, error) {
	return f.listen(f.ListenTLSErr)
}

// Dial opens an outbound connection "over the Tailnet". Like Listen it fails
// while the node is down — that is the real constraint, not a convenience: the
// stack the connection originates from does not exist until the node is up.
func (f *Fake) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	running, scripted, dial := f.started, f.DialErr, f.DialFunc
	f.dials++
	f.dialAddrs = append(f.dialAddrs, addr)
	f.mu.Unlock()

	if scripted != nil {
		return nil, scripted
	}
	if !running {
		return nil, ErrNotRunning
	}
	if dial != nil {
		return dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Dials is how many times Dial was called and DialAddrs what each asked for —
// the observation that says which dialer an origin chose.
func (f *Fake) Dials() int { f.mu.Lock(); defer f.mu.Unlock(); return f.dials }

// DialAddrs returns a copy of the addresses Dial was asked for, in order.
func (f *Fake) DialAddrs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialAddrs...)
}

func (f *Fake) listen(scripted error) (net.Listener, error) {
	f.mu.Lock()
	running := f.started
	f.mu.Unlock()
	if scripted != nil {
		return nil, scripted
	}
	if !running {
		return nil, errors.New("tailnet: the node is not running")
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// Close stops the node, keeping the state directory — so the next Start is a
// returning one. Idempotent.
func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return f.CloseErr
	}
	f.started = false
	f.setLocked(Status{State: StateStopped})
	return f.CloseErr
}

// Starts is how many times Start was called; Joins is how many of those had to
// authenticate (found no state on disk). A reconnect that re-authenticates shows
// up here as a second join, which is the assertion the disconnect/forget tests
// are built on.
func (f *Fake) Starts() int { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }
func (f *Fake) Joins() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.joins }

// LastConfig is the Config the most recent Start was given, including the auth
// key — so a test can prove the key reached the join, and then prove it reached
// no table, no log line, and no API response.
func (f *Fake) LastConfig() Config { f.mu.Lock(); defer f.mu.Unlock(); return f.lastConfig }

// setLocked records a status and publishes it, dropping the notification if
// nobody is reading (the consumer re-reads Status; this is a nudge, not a queue).
func (f *Fake) setLocked(s Status) {
	f.status = s
	select {
	case f.changesLocked() <- s:
	default:
	}
}

func (f *Fake) changesLocked() chan Status {
	if f.changes == nil {
		f.changes = make(chan Status, 8)
	}
	return f.changes
}
