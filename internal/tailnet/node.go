// Package tailnet is the seam between this server and the operator's Tailnet
// (ADR-0043): the narrow interface the rest of the server talks to, the
// DB-backed settings behind it, and the state machine that drives
// connect/disconnect/forget and the connect-at-boot.
//
// # Why the seam exists at all
//
// Joining a Tailnet means linking `tailscale.com` — 500-odd modules, a userspace
// WireGuard stack, and a security-sensitive dependency that moves fast (ADR-0043
// measured it: 274 packages compiled become 610, and the binary 21 MiB becomes
// 40). That implementation is linked only under
// `-tags tailscale`, and it is the ONLY file behind the tag. Everything above it
// — the settings, the admin endpoints, the state machine, the events — compiles
// and tests in BOTH builds against the Fake in this package, because you cannot
// join a real Tailnet from a unit test and the only question worth arguing about
// is how much code is stranded on the far side of that fact.
//
// # The vocabulary
//
// A **Tailnet node** is this Server's own membership of the operator's Tailnet
// (CONTEXT.md). It is emphatically NOT a Device: a Device is a client
// installation that holds a credential against this Server, whereas a Tailnet
// node is this Server's presence on somebody else's network and grants nothing.
// Being on the Tailnet is not being signed in — a Tailnet peer still does an
// ordinary login, with the same authorization and the same rate limits.
package tailnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"
)

// State is the closed set of states a Tailnet node can be in. It is a defined
// type with a fixed set of values, not a free string, because the admin UI
// BRANCHES on it: a typo'd string is not a compile error, it is a dead branch
// that renders nothing and is discovered by an operator staring at a blank panel.
// The wire spellings are the JSON the settings endpoint emits.
type State string

const (
	// StateStopped is the resting state: the node is not running and nothing has
	// gone wrong. It is the zero value's meaning and the default posture — the
	// feature is off until an Admin turns it on (ADR-0043).
	StateStopped State = "stopped"
	// StateStarting is "asked to come up, has not settled yet". A node that is
	// starting has made contact with no one yet, so it carries no address.
	StateStarting State = "starting"
	// StateNeedsLogin is the interactive join waiting on a human (ADR-0043: joining
	// is interactive by DEFAULT, so this is the normal first-run state, not an
	// error). Status.LoginURL carries the URL the Admin must open; without it this
	// state is a dead end, which is why the status carries one.
	StateNeedsLogin State = "needsLogin"
	// StateRunning is a node that is up and reachable on the Tailnet: it has an
	// FQDN and addresses, and its listeners can be bound.
	StateRunning State = "running"
	// StateKeyExpired is a node whose key has LAPSED: it was joined, it worked, and
	// its membership has since run out (ADR-0043 — 180 days by default on an
	// interactively joined node). Mechanically it is StateNeedsLogin with a fresh
	// LoginURL, and for a long time that is all it was — which collapsed two
	// situations an operator experiences completely differently: "set this up" and
	// "the thing that was working has stopped". ADR-0043 is explicit that the second
	// is framed as RE-AUTHORIZATION rather than as an error, and a UI cannot honour
	// that framing if the two arrive byte-identical on the wire.
	//
	// It is a state rather than a boolean beside StateNeedsLogin because the UI
	// branches on State and nothing else; a parallel flag is a branch somebody
	// forgets. Status.KeyExpiry carries the date it lapsed and Status.LoginURL the
	// link that fixes it.
	StateKeyExpired State = "keyExpired"
	// StateError is a node that tried and failed — an unreachable coordination
	// server, a rejected auth key, an unusable state directory. Status.LastError
	// says what happened. It is never a boot failure and never touches the LAN
	// listener (ADR-0043); it is a thing the operator is TOLD about, not a thing
	// the server dies of.
	StateError State = "error"
)

// KeyExpiryWarningWindow is how close to lapsing a node key has to be before the
// boot log stops stating the date and starts WARNING about it.
//
// This constant and EXPIRY_WARNING_DAYS in web/src/admin/AdminRemoteAccessScreen.tsx
// are two expressions of ONE number — ADR-0043's "a warning fires under ~14 days"
// — and they must agree. They are two because the log reaches the operator who has
// not opened the web UI since setup (precisely the person this failure finds, six
// months after the last time anybody touched the box) and the panel reaches the one
// who has. Neither channel can be dropped in favour of the other, and a
// disagreement between them means the panel is calm while the log is shouting.
const KeyExpiryWarningWindow = 14 * 24 * time.Hour

// Status is a snapshot of the node's condition — everything the settings screen
// and the boot log need, and nothing else. It is a value, not a handle: reading
// it never blocks on the network.
type Status struct {
	// State is the closed enum above; the UI branches on this field alone.
	State State
	// FQDN is the MagicDNS name the Tailnet gives this Server
	// ("obelo.tail1a2b.ts.net"), empty until the node is running. It reaches
	// AUTHENTICATED responses only (ADR-0043) — publishing it on the
	// unauthenticated handshake would tell a scanner on the port-forward that a
	// Tailnet exists and what it is called.
	FQDN string
	// Addresses are the node's own Tailnet IPs (the 100.64.0.0/10 v4 and the v6).
	// They are what the FQDN resolves to, and they are shown so an operator whose
	// MagicDNS is off still has something to type.
	Addresses []netip.Addr
	// KeyExpiry is when this node's membership lapses — the one scheduled failure
	// this path carries (ADR-0043: default 180 days, and the symptom is "remote
	// access stopped working" six months after anybody last touched the box).
	//
	// It is a POINTER because nil is a real, different, and GOOD state: a node
	// joined with a tagged auth key never expires. Collapsing that into the zero
	// time would render as "expired in year 1" and would make the ~14-day warning
	// fire forever on exactly the nodes that are safe. Under StateKeyExpired it is
	// the date the key LAPSED, and so is in the past.
	KeyExpiry *time.Time
	// LoginURL is the interactive-join URL to open in a browser, set while the node
	// is in StateNeedsLogin (and again once a key has lapsed). It is not a secret
	// in the credential sense — it is a one-time authorization link the Admin is
	// meant to click — but it is only ever shown to an authenticated Admin.
	LoginURL string
	// LastError is the human-readable reason the node is in StateError, empty
	// otherwise. A string rather than an error because this crosses to JSON and is
	// read by a person, not matched on by code.
	LastError string

	// HTTPSBound is whether tailnet :443 IS ACCEPTING CONNECTIONS RIGHT NOW.
	//
	// IT IS NOT THE SETTING. The settings row's HTTPSEnabled is what the operator
	// ASKED FOR; this is what the server ACHIEVED. The two come apart in exactly
	// the case this feature is most likely to be misconfigured in — the opt-in
	// needs MagicDNS and HTTPS certificates enabled in the Tailscale console, which
	// this server can neither set nor detect ahead of time — and a UI that reads
	// the first as though it were the second tells the operator to use an
	// https:// address that refuses connections. Deriving this from the setting
	// reintroduces that bug; it must only ever be set from an observed bind.
	//
	// It means BOUND, not "certificate proven good": with tsnet the certificate is
	// fetched inside the node on the first handshake and an issuance that fails
	// after a successful bind is not visible from above the seam at all.
	HTTPSBound bool
	// HTTPSError is why tailnet :443 is not bound, in the same words the log used,
	// or empty. It exists because "not yet" is unactionable: the message names both
	// console settings, which is the entire fix, and it is passed through rather
	// than recomposed for the UI so the two cannot drift.
	HTTPSError string
}

// NOTE ON THE TWO HTTPS FIELDS ABOVE: a Node never fills them in. The listener
// that binds tailnet :443 lives in cmd/obelo — above this seam, because whether
// to serve TLS on the Tailnet is the CALLER's decision (see Config) — so the
// supervisor there reports what it observed to Manager.ReportHTTPS, and the
// Manager is the only thing that puts them on a Status. A Node implementation
// leaving them zero is correct.

// Config is everything the node needs to join. It is assembled fresh at each
// join, and it is deliberately NOT the settings row: HTTPSEnabled lives in the
// settings but not here, because whether to serve TLS on the Tailnet is a
// decision of the CALLER (whether it calls ListenTLS at all, issue 03), not
// something the node needs to know to come up.
type Config struct {
	// Hostname is the label the node joins under, which becomes the first part of
	// the MagicDNS name. Already sanitized to a legal DNS label by the settings
	// layer — a bad one fails at join time inside somebody else's code, with a
	// message written for somebody else's user.
	Hostname string
	// ControlURL is the coordination server, empty for Tailscale's own. The escape
	// hatch that keeps ADR-0001 defensible: an operator who will not depend on a
	// vendor points this at their own Headscale and everything above still works.
	ControlURL string
	// StateDir is the durable node state (its keys), at <DataDir>/tailscale mode
	// 0700. Losing it costs a re-authorization, which is exactly what Forget is
	// for; keeping it is what makes Disconnect→Connect instant.
	StateDir string
	// AuthKey is a pre-authorized join key read from OBELO_TAILSCALE_AUTHKEY at
	// JOIN TIME and dropped, for headless installs and the test harness. It is
	// never persisted, never logged, and never leaves this struct — see String,
	// which exists to keep a careless log line from being the exception.
	AuthKey string
}

// String redacts AuthKey so that logging a Config — the single most likely way a
// join key ends up in a log file an operator later pastes into an issue — cannot
// disclose it. This is structural rather than a promise: fmt's %v/%s/%+v all
// route through it.
func (c Config) String() string {
	key := "unset"
	if c.AuthKey != "" {
		key = "[redacted]"
	}
	return "tailnet.Config{Hostname: " + c.Hostname + ", ControlURL: " + c.ControlURL +
		", StateDir: " + c.StateDir + ", AuthKey: " + key + "}"
}

// Node is this Server's membership of a Tailnet. Every method here is a method
// the `tsnet` adapter must implement against an API we do not control, and a
// method the Fake has to be honest about — which is why the interface is this
// small and must stay this small. Anything that can be computed above the seam
// belongs above the seam.
//
// A Node is reusable: Start → Close → Start is the disconnect/reconnect cycle and
// must work without reconstructing it. Whether the second Start has to
// re-authenticate is decided by what survives in Config.StateDir, not by the
// Node's memory of the first one.
type Node interface {
	// Start brings the node up and returns once it has been ASKED to join —
	// promptly, not once it is running. An interactive join settles in
	// StateNeedsLogin and completes later (the human clicks the link), which is
	// reported through Changes. ctx bounds the attempt, not the membership.
	Start(ctx context.Context, cfg Config) error
	// Status is the current snapshot, safe to call from any goroutine at any time
	// including before Start and after Close.
	Status() Status
	// Changes streams state transitions so a login URL appearing does not depend on
	// anything polling for it. The channel is owned by the Node and lives across
	// Start/Close cycles; it is never closed, so a consumer stops by returning from
	// its own select rather than by watching for a close. Sends are non-blocking:
	// a consumer that stalls misses intermediate transitions and re-reads Status,
	// exactly like the events Broker's "go refetch" nudges.
	Changes() <-chan Status
	// Listen binds a listener on the Tailnet interface (tailnet :80). It fails while
	// the node is not STARTED: the port lives on the node's own userspace stack, so
	// until Start there is no stack to bind on.
	//
	// A started node that has not JOINED yet does bind successfully, and that is not
	// a bug in either implementation — the stack is local, so the thing a join adds
	// is peers who can route to it, not the ability to listen. Callers that only want
	// a reachable listener gate on Status().State instead (cmd/obelo does).
	Listen(network, addr string) (net.Listener, error)
	// ListenTLS binds a TLS listener using the MagicDNS certificate (tailnet :443,
	// issue 03). It is opt-in because it additionally requires MagicDNS and HTTPS
	// certificates to be enabled in the Tailscale console — a prerequisite outside
	// this web UI — so its failure costs HTTPS on the Tailnet and nothing else.
	ListenTLS(network, addr string) (net.Listener, error)
	// Close stops the node and releases its listeners, KEEPING the state directory
	// so the next Start needs no re-authorization. Idempotent: closing a node that
	// never started is not an error.
	Close() error
}

// ErrNoNode is what every operation answers on a build with no Tailnet node
// linked in. It names the build on purpose: a bare "not found" or a silent
// success reads identically to a typo, and the operator has no way to tell "this
// feature is not in your binary" from "this feature is broken" (ADR-0043).
var ErrNoNode = errors.New("tailnet: this build has no Tailnet support linked in — use the Docker image or a release binary (built with -tags tailscale)")

// ErrNotRunning is what Listen and ListenTLS answer while the node is down.
// Binding a Tailnet port is not like binding an OS socket: the port lives on the
// node's own userspace stack, so until the node is up there is no stack to bind
// ON, not merely no address to bind TO.
var ErrNotRunning = errors.New("tailnet: the node is not running, so there is nothing to listen on")
