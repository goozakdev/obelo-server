package webui_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
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
