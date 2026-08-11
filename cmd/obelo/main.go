// Command obelo is the single-process entry point for the media server
// (the modular monolith of ADR-0006). It boots the app from environment-derived
// config and serves the HTTP API until interrupted.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	srv := newHTTPServer(cfg, api.LogRequests(application.Handler))

	// Release long-lived SSE subscribers (GET /api/v1/events) as part of graceful
	// shutdown. srv.Shutdown waits for in-flight requests to drain, but an SSE
	// handler only returns when its broker channel closes — and the broker is
	// otherwise not closed until the deferred application.Close() runs, AFTER
	// Shutdown has already returned. Without this hook Shutdown blocks the full
	// timeout and exits with "context deadline exceeded". Broker.Close is
	// idempotent, so the later application.Close() re-closing it is a no-op.
	srv.RegisterOnShutdown(application.Events.Close)

	// Serve in the background so we can react to OS signals for graceful stop.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("obelo: listening on %s (data dir: %s)", cfg.ListenAddr, cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		return err
	case <-stop:
		log.Printf("obelo: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
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
