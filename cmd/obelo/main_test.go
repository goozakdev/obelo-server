package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/marioquake/obelo-server/internal/config"
)

// TestNewHTTPServerTimeouts pins the connection timeouts the server binds with.
// They are not arbitrary — each one is a decision recorded in newHTTPServer — and
// none of them is observable from any other test, since nothing else in the suite
// constructs a real *http.Server (the API tests drive the handler via httptest,
// which has its own zero-valued server).
func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer(config.Config{ListenAddr: ":8080"}, http.NotFoundHandler())

	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q — the server must bind the configured address", srv.Addr, ":8080")
	}
	if srv.Handler == nil {
		t.Error("Handler is nil — newHTTPServer dropped the handler it was given")
	}

	// Headers: the slowloris bound. Loosening this re-opens the cheapest way to
	// pin a connection open with no intent to send a request.
	if got, want := srv.ReadHeaderTimeout, 10*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got, want)
	}

	// Whole request including body, sized against the 16 MiB artwork upload plus
	// multipart slack over a slow domestic uplink. Shortening it truncates large
	// poster uploads before it inconveniences an attacker.
	if got, want := srv.ReadTimeout, 5*time.Minute; got != want {
		t.Errorf("ReadTimeout = %v, want %v (see the 17 MiB upload math in newHTTPServer)", got, want)
	}

	// Keep-alive idle bound — the reason this hardening happened. Zero here means
	// no idle deadline at all and connections that never get reaped.
	if got, want := srv.IdleTimeout, 2*time.Minute; got != want {
		t.Errorf("IdleTimeout = %v, want %v", got, want)
	}

	// This assertion exists to make a well-meaning future change fail loudly rather
	// than silently break streaming. WriteTimeout is an absolute deadline on the
	// entire response, so ANY non-zero value truncates progressive/HLS media
	// delivery (ADR-0004), the direct-file download, and the SSE stream at
	// GET /api/v1/events (ADR-0016), which is designed never to end. Someone will
	// eventually notice that ReadTimeout is set and WriteTimeout is not, read it as
	// an oversight, and "fix" it — the resulting breakage is intermittent, looks
	// like a flaky network, and would not otherwise be caught by any test here.
	// If this test brought you here: bound slow responses with a per-handler
	// http.ResponseController deadline on the non-streaming handlers instead.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a non-zero WriteTimeout cuts off media streaming and the SSE stream mid-response; see the comment in newHTTPServer", srv.WriteTimeout)
	}
}
