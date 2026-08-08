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

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		// The access log lives here, around the fully composed handler, rather than
		// inside app.New: the test harness serves application.Handler directly, so the
		// suite is not drowned in a line per HLS segment. api.LogRequests redacts the
		// two URLs that carry a credential — the stream token in
		// /stream/{streamToken}/… and the ?token= on the direct-file download — because
		// a request logger is the most likely way either reaches a file on disk.
		Handler:           api.LogRequests(application.Handler),
		ReadHeaderTimeout: 10 * time.Second,
	}

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
