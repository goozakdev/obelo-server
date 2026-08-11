// Command obelo is the single-process entry point for the media server
// (the modular monolith of ADR-0006). It boots the app from environment-derived
// config and serves the HTTP API until interrupted.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/marioquake/obelo-server/internal/api"
	"github.com/marioquake/obelo-server/internal/app"
	"github.com/marioquake/obelo-server/internal/config"
	"github.com/marioquake/obelo-server/internal/discovery"
)

func main() {
	// Diagnostic subcommands (e.g. debug-hls-boundaries) run and exit without
	// booting the server.
	if maybeRunDebugCommand() {
		return
	}
	if err := run(); err != nil {
		log.Fatalf("obelo: fatal: %v", err)
	}
}

func run() error {
	cfg := config.FromEnv()

	// Validate the full serve configuration (including ListenAddr) before binding
	// a listener. app.New only checks what the handler wiring needs.
	if err := cfg.Validate(); err != nil {
		return err
	}

	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = application.Close() }()

	// Announce on the local link so Apple clients can find us without anyone
	// typing an IP (ADR-0005, implemented by ADR-0034). Wired here rather than in
	// app.New because only this layer knows the listen address — app.New is also
	// used by the test harness, which drives the handler via httptest and has no
	// listener at all, and must not start a responder.
	//
	// Best-effort by design: a failure to register logs and is ignored. Discovery
	// is a convenience, and an unadvertised server is still fully usable via
	// manual address entry — refusing to boot over it would trade a working
	// server for a cosmetic one.
	advOpts := discovery.Options{IPs: cfg.AdvertiseIPs, Interface: cfg.MDNSInterface}
	if adv, advErr := discovery.Advertise(application.Identity, cfg.ListenAddr, advOpts); advErr != nil {
		log.Printf("obelo: mDNS advertisement unavailable (%v) — clients must enter the address manually", advErr)
	} else {
		// The advertised addresses are logged because they are the thing that goes
		// wrong: a record pointing at a bridge address a client cannot route to
		// fails exactly like a server that never advertised, and this line is the
		// only place to catch it without a packet capture.
		log.Printf("obelo: advertising %q as %s on %s port %d at %s%s (id: %s)",
			adv.Instance, adv.Host, discovery.ServiceType, adv.Port,
			formatIPs(adv.IPs), viaInterface(adv.Interface), application.Identity.ID)
		defer func() { _ = adv.Close() }()
	}

	// The access log lives here, around the fully composed handler, rather than
	// inside app.New: the test harness serves application.Handler directly, so the
	// suite is not drowned in a line per HLS segment. api.LogRequests redacts the
	// two URLs that carry a credential — the stream token in
	// /stream/{streamToken}/… and the ?token= on the direct-file download — because
	// a request logger is the most likely way either reaches a file on disk.
	servers, err := newServers(cfg, api.LogRequests(application.Handler), application.Events.Close)
	if err != nil {
		return err
	}

	// Serve in the background so we can react to OS signals for graceful stop.
	// The channel is buffered per server so that a second listener failing after
	// the first cannot block its goroutine forever on a send nobody will receive —
	// run() reads exactly one error and returns, and a blocked send would leak the
	// goroutine and hide whichever failure arrived second.
	serveErr := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			log.Printf("obelo: listening on %s%s (data dir: %s)", srv.Addr, schemeNote(srv), cfg.DataDir)
			if err := serve(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// Name the listener: with two of them, a bare "address already in use"
				// does not say which port the operator has to go and look at.
				serveErr <- fmt.Errorf("serving %s%s: %w", srv.Addr, schemeNote(srv), err)
				return
			}
			serveErr <- nil
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		return err
	case <-stop:
		log.Printf("obelo: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return shutdownAll(ctx, servers)
	}
}

// newServers builds the listeners this configuration asks for: always the
// plain-HTTP one, plus the HTTPS one when the operator opted into native TLS
// (ADR-0041, OBELO_TLS_MODE=files or =acme). With TLS off it returns exactly the
// one server this command has always had — same struct, same handler, and no
// certificate machinery constructed or goroutine started.
//
// THE TWO TLS MODES FAIL IN OPPOSITE DIRECTIONS HERE, ON PURPOSE. `files`
// returns an error and stops the boot when the certificate will not load;
// `acme` cannot return an error at all. See the switch below and
// config.validateTLS, which carries the same reasoning from the config side.
//
// onShutdown is registered on EVERY server returned, and that is load-bearing
// rather than tidy. It releases the long-lived SSE subscribers of
// GET /api/v1/events: Shutdown waits for in-flight requests to drain, but an SSE
// handler only returns when its broker channel closes, and the broker is
// otherwise not closed until run()'s deferred application.Close() — which runs
// AFTER Shutdown has already returned. Without the hook, Shutdown blocks for the
// full 10s and exits "context deadline exceeded".
//
// With two listeners, a subscriber can be on either one, and Shutdown only runs
// the hooks of the server it was called on. So registering this on the HTTP
// server alone would leave a browser's EventSource on the HTTPS listener holding
// its handler open, and shutdownAll would wait out the entire timeout — the same
// hang the hook was added to fix, moved one listener sideways where the existing
// test would not see it. events.Broker.Close is idempotent (it returns
// immediately when already closed), so N registrations plus the deferred
// application.Close() amount to one real close and N no-ops.
func newServers(cfg config.Config, h http.Handler, onShutdown func()) ([]*http.Server, error) {
	srv := newHTTPServer(cfg, h)
	srv.RegisterOnShutdown(onShutdown)
	servers := []*http.Server{srv}

	switch cfg.TLSMode {
	case config.TLSModeFiles:
		// cfg.Validate has already loaded this pair once, so a failure here is a
		// file that changed in the moments since — rare, and still a refusal,
		// because the operator asked for HTTPS and must not get a quietly
		// plain-HTTP server instead.
		reloader, err := newCertReloader(cfg.TLSCertFile, cfg.TLSKeyFile, defaultCertCheckInterval)
		if err != nil {
			return nil, err
		}
		tlsSrv := newTLSServer(cfg, h, reloader.GetCertificate)
		tlsSrv.RegisterOnShutdown(onShutdown)
		servers = append(servers, tlsSrv)

	case config.TLSModeACME:
		// Note what is missing: an error path. Nothing between here and a serving
		// listener can fail, because nothing here talks to the CA — the first
		// handshake for a whitelisted name does. If the CA is down, if DNS is not
		// pointed at the house yet, if the router forwards nothing, or if the
		// account is rate-limited, this server still boots and still serves the LAN
		// on ListenAddr; only HTTPS is missing, and it is logged every time someone
		// asks for it (acmeCertificates.complain).
		//
		// That is the deliberate opposite of the `files` branch above, and the
		// reasoning is ADR-0041's: `files` failing means the operator mistyped a
		// path they control, so a loud refusal is a favour. `acme` failing means
		// the world is temporarily unavailable, and taking the household's media
		// server down over a third party's outage would be a bad trade. Anyone
		// tempted to "make these consistent" should change neither.
		//
		// Also absent, and equally deliberate: any use of Manager.HTTPHandler.
		// HTTP-01 would need port 80 reachable from the internet — a second
		// listener and a second port-forward for a household that only wanted one,
		// which is precisely the usability cost TLS-ALPN-01 exists to avoid.
		tlsSrv := newACMEServer(cfg, h, newACMECertificates(cfg))
		tlsSrv.RegisterOnShutdown(onShutdown)
		servers = append(servers, tlsSrv)
	}
	return servers, nil
}

// serve runs one listener, choosing the transport from the server itself: a
// TLSConfig means HTTPS. The empty file names in ListenAndServeTLS are not an
// oversight — net/http skips its own one-shot PEM load when the TLS config
// already supplies a certificate (here, GetCertificate on the reloader), and
// passing the paths instead would reintroduce exactly the read-once behaviour
// certReloader exists to avoid.
func serve(srv *http.Server) error {
	if srv.TLSConfig != nil {
		return srv.ListenAndServeTLS("", "")
	}
	return srv.ListenAndServe()
}

// shutdownAll gracefully stops every listener and joins whatever they report.
//
// Concurrently, not in sequence: the caller gives all of this one 10s budget, so
// draining the listeners one after another would hand the second whatever the
// first left over — and the slow case (a client mid-download) is precisely when
// both are busy at once.
func shutdownAll(ctx context.Context, servers []*http.Server) error {
	errs := make([]error, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = srv.Shutdown(ctx)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// schemeNote labels the HTTPS listener in log lines and errors. Two listeners
// on two ports produce two of everything, and "listening on :8443" alone does
// not say which of them it is.
func schemeNote(srv *http.Server) string {
	if srv.TLSConfig != nil {
		return " (HTTPS)"
	}
	return ""
}

// newHTTPServer builds the listening server with the connection timeouts we
// intend. It is a function rather than a literal inside run() so main_test.go can
// assert those timeouts without binding a port — in particular that WriteTimeout
// is still zero. It does no wiring beyond the struct: run() owns
// RegisterOnShutdown, ListenAndServe, and the signal handling.
//
// ADR-0005 puts an operator's reverse proxy in front of this in the expected
// deployment, and a proxy absorbs most of what these bound — but the proxy is the
// operator's to configure, not ours to assume, and the server also gets pointed
// straight at a LAN (and, increasingly, at a port-forward) with nothing in front
// of it. These are the direct-exposure defaults; a proxy in front only makes them
// redundant, never wrong.
func newHTTPServer(cfg config.Config, h http.Handler) *http.Server {
	return &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: h,

		// Headers must arrive promptly. Nothing legitimate takes ten seconds to send
		// a request line and a few KB of headers, and this is the slowloris bound.
		ReadHeaderTimeout: 10 * time.Second,

		// The whole request, body included — ReadHeaderTimeout stops at the blank
		// line, so without this a client can send valid headers and then dribble a
		// body one byte at a time forever, holding a connection for free.
		//
		// Sized against the largest legitimate body we accept: the artwork upload,
		// capped at 16 MiB (maxArtworkUploadBytes) plus 1 MiB of multipart framing
		// slack (uploadSlack) = 17 MiB ≈ 17.8 MB on the wire. Five minutes means a
		// sustained ~59 KiB/s (~490 kbit/s) uplink completes it; a 1 Mbit/s domestic
		// uplink — the slow end of what anyone still has — finishes in ~2.5 minutes,
		// so there is roughly 2× headroom for a bad afternoon on the DSL. Shrink this
		// and the first symptom is an admin on a poor connection getting a truncated
		// upload on a large poster, which reads as a mystery server bug, not a
		// timeout. Every other request we serve has a body of at most a few KB.
		//
		// Safe for the streaming endpoints despite covering the request: for a
		// request with no body left to read (every GET, so every HLS segment, the
		// direct-file download, and the SSE stream), net/http clears the connection's
		// read deadline before it calls the handler. The read deadline therefore
		// never fires under a long-running response. Verified against Go 1.26
		// (connReader.startBackgroundRead).
		ReadTimeout: 5 * time.Minute,

		// WriteTimeout MUST stay zero. Do not "finish the set" by filling this in.
		//
		// It is an absolute deadline on the whole response, armed when the request is
		// read, not an idle timer — so any non-zero value is a hard cap on how long a
		// response may take, and this server exists to send responses that are
		// deliberately longer than any number you would pick:
		//   - progressive direct-play and HLS segment delivery (ADR-0004): a movie is
		//     served over a connection for as long as the client plays it;
		//   - the direct-file download, which is however long the file is over
		//     however slow the link is;
		//   - GET /api/v1/events, the SSE stream (ADR-0016), which by design never
		//     finishes — it lives as long as the browser tab does.
		// Setting it to, say, 30s does not "harden" anything; it makes playback and
		// live updates cut out mid-response for every user, intermittently, in a way
		// that looks like a network fault rather than a config change. The
		// asymmetry with ReadTimeout above is intentional and load-bearing.
		//
		// The correct way to bound a genuinely slow response is a per-handler write
		// deadline via http.ResponseController on the handlers that are NOT supposed
		// to stream — the JSON API — which leaves the streaming ones alone. That is a
		// change to individual handlers, not to this struct. TestNewHTTPServerTimeouts
		// asserts this field is zero so the shortcut fails loudly instead.
		WriteTimeout: 0,

		// The keep-alive bound, and the main fix here: with both this and ReadTimeout
		// at zero, net/http applies no idle deadline whatsoever and idle keep-alive
		// connections accumulate until something else runs out of file descriptors.
		// Two minutes comfortably outlives the gap between a client's polling calls
		// and between HLS segment fetches, so it costs a reconnect only on
		// connections that were doing nothing anyway.
		IdleTimeout: 2 * time.Minute,

		// MaxHeaderBytes is deliberately left at Go's 1 MB default. It is already a
		// bound, so there is nothing to fix, and lowering it is not free: requests
		// arrive through the operator's reverse proxy (ADR-0005) carrying whatever
		// X-Forwarded-* and auth headers that proxy adds, and a too-tight cap turns
		// into a 431 that is very hard to attribute from the client side.
	}
}

// newTLSServer builds the HTTPS listener (ADR-0041). It starts from
// newHTTPServer and changes only the address and the TLS config, so the two
// listeners CANNOT drift: the timeout reasoning above — including the zero
// WriteTimeout that keeps media and the SSE stream from being cut off — applies
// identically to a request that arrived over TLS, and a copy of that struct here
// would eventually be updated in one place only.
//
// getCert is certReloader.GetCertificate. It is a parameter rather than
// something this function constructs so the timeout/TLS assertions in the tests
// need no files on disk.
func newTLSServer(cfg config.Config, h http.Handler, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *http.Server {
	srv := newHTTPServer(cfg, h)
	srv.Addr = cfg.TLSListenAddr
	srv.TLSConfig = &tls.Config{
		// TLS 1.2 floor, stated explicitly rather than left to whatever the Go
		// runtime's default happens to be. Apple's App Transport Security requires
		// 1.2 or better, and the clients this product exists for are Apple ones
		// (ADR-0004) — so a lower floor would not "support older clients", it would
		// make an iOS or tvOS client fail its own connection check in a way that
		// surfaces as an unexplained network error and reads as a server bug.
		MinVersion: tls.VersionTLS12,

		// The certificate comes from the reloader on every handshake, which is the
		// whole reason this server is built by hand instead of by
		// ListenAndServeTLS(certFile, keyFile): that helper reads the PEM files
		// once, at boot, and a renewal then leaves us serving a dead chain until
		// somebody restarts the process. See certReloader.
		GetCertificate: getCert,

		// NextProtos IS DELIBERATELY UNSET HERE. Do not fill it in.
		//
		// http.Server.ServeTLS negotiates HTTP/2 for us — it appends "h2" to a copy
		// of this config when TLSNextProto is nil, which it is. Setting NextProtos
		// here, to anything not containing "h2", silently turns HTTP/2 OFF. Nothing
		// fails; every request still works over HTTP/1.1. What you lose is the one
		// thing this listener gains for free and that HLS benefits from most:
		// dozens of small segment fetches multiplexing over a single connection
		// instead of queueing against the browser's per-origin connection limit.
		// The symptom would be "playback got choppier after some TLS change", months
		// later, with no error anywhere.
		//
		// newACMEServer below IS the one caller that sets it, and it takes the whole
		// list from autocert rather than writing one out, so "h2" cannot go missing
		// there either. See its comment for why acme mode has no choice.
	}
	return srv
}

// newACMEServer builds the HTTPS listener for OBELO_TLS_MODE=acme (ADR-0041
// phase 2). It is newTLSServer plus the two things autocert has to supply, so
// the timeouts, the TLS 1.2 floor, and the rest of the posture cannot drift
// between the two TLS modes.
//
// NextProtos MUST be set in this mode, which is the one exception to the rule
// stated above, and it is copied from autocert.Manager.TLSConfig() rather than
// written out here. Two things depend on the exact contents:
//
//   - "acme-tls/1" (acme.ALPNProto) must be present or the TLS-ALPN-01 challenge
//     CANNOT COMPLETE. The CA connects with that protocol and nothing else; a
//     server that does not offer it fails ALPN, the challenge is never answered,
//     and the certificate never arrives. The failure surfaces as a CA-side
//     authorization error that reads like a problem at Let's Encrypt, so it is
//     very easy to spend an afternoon on — and, worse, each attempt spends one of
//     the five failed authorizations per hostname per hour.
//   - "h2" must survive, for the HLS multiplexing reason in newTLSServer. Setting
//     NextProtos by hand is exactly how it gets dropped, which is why the list
//     comes from the manager (it is ["h2", "http/1.1", "acme-tls/1"]) instead of
//     being retyped. Order matters too: Go's TLS server picks the first entry the
//     client also offers, so h2 leading keeps ordinary traffic on HTTP/2 while a
//     CA offering only acme-tls/1 still matches.
func newACMEServer(cfg config.Config, h http.Handler, certs *acmeCertificates) *http.Server {
	fromManager := certs.manager.TLSConfig()
	srv := newTLSServer(cfg, h, certs.GetCertificate)
	srv.TLSConfig.NextProtos = fromManager.NextProtos
	return srv
}

// formatIPs renders the advertised addresses for the boot log.
func formatIPs(ips []net.IP) string {
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		parts = append(parts, ip.String())
	}
	return strings.Join(parts, ", ")
}

// viaInterface names the pinned mDNS interface, or says nothing when the responder
// is on the system default.
func viaInterface(name string) string {
	if name == "" {
		return ""
	}
	return " via " + name
}
