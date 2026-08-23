package webui_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The webui package composes the top-level routing: /api/v1 stays the API's,
// everything else is the embedded SPA with an index.html fallback. These tests
// drive the real wired server (app.New → webui.Handler) through the harness, so
// they assert the actual composition production uses.

// The handshake still works through the wrapped handler — /api/v1 is untouched.
func TestAPIHandshakeUnaffected(t *testing.T) {
	srv := testharness.New(t)

	var info struct {
		Version           string          `json:"version"`
		SupportedVersions []int           `json:"supportedVersions"`
		Features          map[string]bool `json:"features"`
		SetupRequired     bool            `json:"setupRequired"`
	}
	status, body := srv.GET("/api/v1/server", &info)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/server: status = %d, want 200\nbody: %s", status, body)
	}
	if info.Version == "" {
		t.Errorf("handshake version empty\nbody: %s", body)
	}
	if !info.SetupRequired {
		t.Errorf("fresh server should report setupRequired=true\nbody: %s", body)
	}
}

// An unknown path UNDER /api/v1 must still return the JSON NOT_FOUND envelope,
// never the SPA index.html. This is the key regression guard.
func TestUnknownAPIPathReturnsEnvelope(t *testing.T) {
	srv := testharness.New(t)

	resp := srv.Do(http.MethodGet, "/api/v1/does-not-exist", nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\nbody: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON (the error envelope, not index.html)", ct)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not the JSON error envelope: %v\nbody: %s", err, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("error code = %q, want NOT_FOUND\nbody: %s", env.Error.Code, body)
	}
	if strings.Contains(string(body), "<html") {
		t.Errorf("unknown API path served HTML; must serve the envelope\nbody: %s", body)
	}
}

// The root path serves the SPA shell (index.html), not an API envelope.
func TestRootServesSPA(t *testing.T) {
	srv := testharness.New(t)

	resp := srv.Do(http.MethodGet, "/", nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200\nbody: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /: Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(string(body), "<html") {
		t.Errorf("GET / did not serve HTML\nbody: %s", body)
	}
}

// Every response leaves this process through webui.Handler's securityHeaders
// wrapper, so the baseline headers must be on the SPA shell AND on the API — one
// uniform set, deliberately (see securityHeaders). A header that only lands on
// one of the two subtrees is the exact regression these two cases catch.
func TestSecurityHeadersOnSPAAndAPI(t *testing.T) {
	srv := testharness.New(t)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}

	for _, tc := range []struct {
		name string
		path string
	}{
		{"SPA shell", "/"},
		{"API", "/api/v1/server"},
	} {
		resp := srv.Do(http.MethodGet, tc.path, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		for h, v := range want {
			if got := resp.Header.Get(h); got != v {
				t.Errorf("%s (GET %s): %s = %q, want %q\nbody: %s", tc.name, tc.path, h, got, v, body)
			}
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s (GET %s): no Content-Security-Policy header", tc.name, tc.path)
		}
		// Strict-Transport-Security is deliberately never sent: this server
		// speaks plain HTTP and the operator's TLS proxy owns that decision
		// (ADR-0005), and HSTS is sticky per hostname — emitting it here can
		// lock a household out of the plain-HTTP LAN path for the whole
		// max-age. See securityHeaders for the full reasoning before "fixing"
		// this.
		if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("%s (GET %s): Strict-Transport-Security = %q, want it ABSENT", tc.name, tc.path, got)
		}
	}
}

// The CSP directives that video playback depends on.
//
// THIS TEST EXISTS SO A FUTURE CSP TIGHTENING FAILS HERE INSTEAD OF IN A USER'S
// BROWSER, HALFWAY THROUGH A MOVIE. Both blob: allowances read like leftovers to
// anyone tidying the policy, and neither failure is visible from the server:
//
//   - media-src blob: — the hls.js MSE path attaches the stream by setting
//     video.src to a blob: object URL wrapping the MediaSource. Without it Chrome
//     refuses the attach and <video> dies with "Media load rejected by URL safety
//     check" on every non-Safari browser.
//   - worker-src blob: — hls.js builds its demuxer worker from a blob: URL
//     (no workerPath, so nothing same-origin could be allowed instead). It is
//     dormant only because Vite resolves hls.js's ESM dist, which omits the
//     inlined worker bundle; an hls.js repackaging re-arms it silently.
//
// connect-src 'self' is asserted too: it carries the API, hls.js's segment
// fetches, and the never-ending SSE stream at GET /api/v1/events.
//
// The assertions are substring checks on purpose — they pin the source
// expressions that matter, not the directive order or the rest of the policy, so
// tightening an unrelated directive does not fail here.
func TestCSPAllowsWhatPlaybackNeeds(t *testing.T) {
	srv := testharness.New(t)

	resp := srv.Do(http.MethodGet, "/", nil)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("GET /: no Content-Security-Policy header")
	}

	for _, want := range []string{
		"default-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"media-src 'self' blob:",
		"worker-src 'self' blob:",
		"connect-src 'self'",
		"img-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q — playback and/or the baseline hardening depends on it\nCSP: %s", want, csp)
		}
	}
}

// img-src permits NOTHING but this origin, and that is a property worth its own
// test because the substring check above cannot see the difference: "img-src
// 'self'" matches "img-src 'self' https:" happily.
//
// The directive carried `https:` until the metadata-provider image proxy landed
// (internal/api/provider_image.go). Every image the app renders — library artwork,
// cast headshots, and the admin Edit-item pickers' candidate thumbnails — is now
// served from this origin, so `https:` would buy nothing except an outbound
// channel an injected <img> could signal on, and would quietly re-permit a future
// screen to point a household browser at a third party (ADR-0001).
//
// If this fails, the question to answer is WHICH image needs a foreign host, and
// the answer is almost certainly "proxy it too" rather than "widen the policy".
func TestCSPImgSrcIsSameOriginOnly(t *testing.T) {
	srv := testharness.New(t)

	resp := srv.Do(http.MethodGet, "/", nil)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")

	var imgSrc string
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "img-src ") {
			imgSrc = d
		}
	}
	if imgSrc == "" {
		t.Fatalf("CSP has no img-src directive\nCSP: %s", csp)
	}
	if imgSrc != "img-src 'self'" {
		t.Errorf("img-src = %q, want exactly \"img-src 'self'\" — provider thumbnails are proxied through this origin now (ADR-0001)\nCSP: %s", imgSrc, csp)
	}
}

// A deep client-side route (no matching asset, not under /api/v1) falls back to
// index.html with a 200, so deep links and refreshes load the app.
func TestClientRouteFallback(t *testing.T) {
	srv := testharness.New(t)

	for _, path := range []string{"/login", "/libraries/abc/titles", "/some/deep/route"} {
		resp := srv.Do(http.MethodGet, path, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200 (SPA fallback)\nbody: %s", path, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "<html") {
			t.Errorf("GET %s did not fall back to index.html\nbody: %s", path, body)
		}
	}
}
