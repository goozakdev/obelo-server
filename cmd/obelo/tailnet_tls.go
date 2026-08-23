package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/config"
)

// Tailnet :443 — the MagicDNS certificate (ADR-0043, tailscale/03).
//
// This is the payoff ADR-0041 pointed at: one name, one publicly-trusted
// certificate, no warning, working identically on the LAN and from a hotel, with
// no port-forward and no DNS to own. It is also the one part of this feature that
// depends on TWO settings in the Tailscale admin console — MagicDNS, and HTTPS
// certificates — which this server can neither set, nor detect ahead of time, nor
// explain from inside its own UI. Hence: opt-in (the HTTPSEnabled setting), and a
// failure that costs HTTPS on the Tailnet and NOTHING else.
//
// Everything below is `acmeCertificates` in cmd/obelo/tls.go restated for a
// different certificate authority, including the parts of that type that are not
// obvious: the complaint is rate-limited because an unattended box otherwise buries
// the one line that matters, and success is announced once per name because it is
// the line an operator greps for and it is worthless if it repeats.

// tailnetTLSListenAddr is where the HTTPS listener binds on the Tailnet.
//
// Like tailnetListenAddr, this is a port on the node's own userspace stack rather
// than an OS socket: it needs no privilege, collides with nothing on the host, and
// is reached only by peers on the operator's Tailnet. :443 so that a client typing
// the MagicDNS name does not have to remember a port.
const tailnetTLSListenAddr = ":443"

// tailnetTLSRetryEvery is how often a wanted-but-unbound HTTPS listener tries
// again.
//
// A retry loop exists because the fix for the common failure happens in a browser
// tab somewhere else: the operator ticks the box here, reads the log, goes to the
// Tailscale console, and enables MagicDNS. NOTHING in this process observes that —
// there is no transition to nudge the supervisor with — so without a timer the
// listener would stay down until the next restart, and the operator would conclude
// the toggle is broken. Thirty seconds is short enough that they are still looking
// at the panel and long enough that a permanently misconfigured box is doing
// nothing measurable.
const tailnetTLSRetryEvery = 30 * time.Second

// tailnetTLSBindTimeout bounds ONE attempt to bind the HTTPS listener.
//
// It exists because `tsnet.Server.ListenTLS` waits for the node to come up first,
// with a context of its own that has no deadline, so a node that goes down at the
// wrong moment parks the call forever. That call is made from the supervisor's
// single goroutine, and Shutdown JOINS that goroutine before it drains — so an
// unbounded bind is not a slow certificate, it is a process that will not exit.
const tailnetTLSBindTimeout = 30 * time.Second

// tailnetComplaintInterval is the floor between two logged certificate failures.
// Longer than ACME's minute because these attempts are OURS — one per
// tailnetTLSRetryEvery, not one per handshake from a stranger's port scan — so the
// job here is only to keep a permanently misconfigured box from writing the same
// paragraph to disk twice a minute forever. The first failure is never suppressed,
// which is the one an operator who has just ticked the box is waiting for.
const tailnetComplaintInterval = 5 * time.Minute

// newTailnetTLSServer builds the server the Tailnet HTTPS listener runs.
//
// TLSConfig IS DELIBERATELY LEFT NIL, and this is the surprising part. The node
// hands back a listener that ALREADY terminates TLS — `tsnet.Server.ListenTLS`
// wraps its own listener in `tls.NewListener` with a GetCertificate that fetches
// the MagicDNS certificate from the Tailnet — so the TLS lives one layer below
// this http.Server. Setting TLSConfig here would not add a certificate; it would
// make `serve()` reach for ListenAndServeTLS and it would invite somebody to
// double-wrap the listener, which fails as a handshake that never completes.
//
// The consequence to know, because nothing will ever report it: THERE IS NO HTTP/2
// ON THIS LISTENER. ALPN is negotiated by the tls.Config the node owns, which lists
// no protocols, so every connection lands on HTTP/1.1 — unlike the ADR-0041 HTTPS
// listener, where net/http negotiates h2 for free. Nothing here can change that
// without reimplementing ListenTLS against unexported `tsnet` internals. The cost
// is the one ADR-0041 named: HLS segment fetches queue against the browser's
// per-origin connection limit instead of multiplexing. Do not "fix" it by setting
// TLSConfig.
//
// ErrorLog is redirected so that the failures we CANNOT see any other way are at
// least attributable. Certificate provisioning happens inside the node on the first
// handshake for a name; when it fails, the error surfaces as net/http's "TLS
// handshake error from …" on the shared standard logger, indistinguishable from the
// same line about the ADR-0041 HTTPS listener. The prefix names the listener.
func newTailnetTLSServer(cfg config.Config, h http.Handler, onShutdown func()) *http.Server {
	srv := newHTTPServer(cfg, h)
	srv.Addr = tailnetTLSListenAddr
	srv.ErrorLog = log.New(log.Writer(), "obelo: tailnet: HTTPS: ", log.Flags())
	srv.RegisterOnShutdown(onShutdown)
	return srv
}

// tailnetCertificates is the logging half of tailnet HTTPS: one announcement per
// name when it works, and a rate-limited complaint when it does not.
//
// It is a type rather than two log lines inline for the same reason
// acmeCertificates is: the two things an operator has to know — "HTTPS is working
// now" and "HTTPS is not working, and here is the short list of reasons" — are
// otherwise either invisible or repeated until they are noise.
type tailnetCertificates struct {
	// plainAddr is the LAN listener, quoted back in the failure message. An
	// operator reading "no certificate" needs to be told in the same breath that
	// the rest of their server is fine.
	plainAddr string

	// complainEvery bounds how often a failure is logged; zero means every time,
	// which is what the tests want and what nothing in production should ask for.
	complainEvery time.Duration
	// retryEvery is quoted in the message, because the single most useful thing the
	// message can say to somebody about to go and change a console setting is that
	// they will not have to restart anything afterwards.
	retryEvery time.Duration

	mu         sync.Mutex
	lastGripe  time.Time
	suppressed int
	announced  map[string]bool
}

func newTailnetCertificates(cfg config.Config) *tailnetCertificates {
	return &tailnetCertificates{
		plainAddr:     cfg.ListenAddr,
		complainEvery: tailnetComplaintInterval,
		retryEvery:    tailnetTLSRetryEvery,
		announced:     map[string]bool{},
	}
}

// announce logs the first successful HTTPS bind for each MagicDNS name. Once per
// name, not once per bind and not once per handshake: this is the line an operator
// greps for to confirm the thing worked, and it is worthless if it repeats. A node
// that goes down and comes back under the same name is the same fact, already
// stated; a hostname change is a new one and says so.
//
// It reports the listener, not the certificate, and the wording is careful about
// the difference: the certificate itself is fetched by the node on the first
// handshake for the name. What is proven at this moment is that both console
// prerequisites are met, which is the part an operator was waiting on.
func (c *tailnetCertificates) announce(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.announced[name] {
		return
	}
	c.announced[name] = true
	log.Printf("obelo: tailnet: HTTPS is serving on the Tailnet at https://%s — MagicDNS and HTTPS certificates are both enabled in the Tailscale console, and the node fetches and renews the certificate itself. Note that issuing it publishes %q in public Certificate Transparency logs, permanently.",
		name, name)
}

// complain reports an HTTPS listener that could not be bound, rate-limited to one
// line per complainEvery with a count of what was swallowed, and RETURNS the
// message it composed whether or not it logged it.
//
// The return value is what reaches the settings panel (tailscale/06). It is
// deliberately outside the rate limit: the log is a stream a person skims, so a
// paragraph repeated every retry buries the line that matters, whereas the panel
// is a snapshot of the current state and an operator opening it during a
// suppression window would otherwise be shown a failure with no reason attached.
// The suppressed-attempt count is the one part that stays log-only — it describes
// the log's own history, not the node's condition.
func (c *tailnetCertificates) complain(name string, err error) string {
	msg := c.failureMessage(name, err)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if now.Sub(c.lastGripe) < c.complainEvery {
		c.suppressed++
		return msg
	}
	c.lastGripe = now

	also := ""
	if c.suppressed > 0 {
		also = fmt.Sprintf(" (%d further attempts suppressed since the last message)", c.suppressed)
		c.suppressed = 0
	}
	log.Printf("obelo: tailnet: WARNING: %s%s", msg, also)
	return msg
}

// failureMessage composes the one paragraph both channels get.
//
// THE MESSAGE IS LONG ON PURPOSE. It arrives on a box nobody is watching, hours
// after the operator ticked a box, and its job is to answer "what do I go and look
// at" without a second round trip. Here that means naming BOTH console toggles —
// the node's own error names only whichever one it noticed first — saying plainly
// that plain HTTP on the Tailnet is unaffected, and saying that the fix takes
// effect on its own, because an operator who does not know that will restart the
// server and conclude the restart is what fixed it.
//
// ONE MESSAGE, TWO DESTINATIONS, and that is the point of it being a function
// rather than an inline Printf. The settings panel needs to say the same thing the
// log says; a second paragraph written for the UI would answer the same question
// in words that drift apart the first time either is edited.
//
// IT IS ALSO WRITTEN FOR BOTH, which costs a little emphasis the log alone would
// enjoy. It says what is still serving rather than that the server is still
// running, because "still running" answers a fear you only have at a log — on the
// settings panel of a server you are looking at, it reads like a non-sequitur. It
// names the setting to turn off rather than where to find it, for the same reason:
// the panel IS where to find it, and a log reader can search the label. Emphasis
// is carried by word order instead of capitals, which shout on a screen. Anything
// added here has to survive being read in both places.
func (c *tailnetCertificates) failureMessage(name string, err error) string {
	return fmt.Sprintf("There is no HTTPS on the Tailnet for %s: %v — plain HTTP on the Tailnet at http://%s is serving normally, and so is the LAN listener on %s (ADR-0043: a certificate problem costs HTTPS, not the Tailnet and not the LAN). Tailnet HTTPS needs two settings in the Tailscale admin console, neither of which this server can set or see in advance: DNS → MagicDNS enabled, and DNS → HTTPS Certificates enabled. Turn both on and this retries on its own every %s — no restart needed. Turning the \"Serve HTTPS on the Tailnet\" setting off stops the retries.",
		quotedName(name), err, name, c.plainAddr, c.retryEvery)
}

// quotedName renders the MagicDNS name for a message, or says it is not known yet.
// A node can be running with no FQDN — MagicDNS being off is exactly one of the
// two failures this message is about — and "no HTTPS for \"\"" reads like a bug in
// the logging rather than the state it is describing.
func quotedName(name string) string {
	if name == "" {
		return "this node (it has no MagicDNS name, which is itself the first thing to check)"
	}
	return fmt.Sprintf("%q", name)
}
