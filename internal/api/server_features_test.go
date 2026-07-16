package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// TestFeaturesMatchRoutes ties each route-existence feature flag to whether the
// route is actually served. The flags were hardcoded once and then rotted: four
// of them advertised false for slices that had long since shipped, and because
// clients are told to branch on flags rather than version strings, that lie made
// every client hide a working feature. Nothing caught it, because no test tied
// the advertisement to reality. This one does.
//
// A route is "served" if it does not 404. These probes are unauthenticated, so a
// live route answers 401 (or 400/405) — every one of those proves the route
// exists, which is the only thing the flag claims. Asserting on 200 would mean
// building a fixture per feature and would test the handler, not the flag.
func TestFeaturesMatchRoutes(t *testing.T) {
	srv := testharness.New(t)

	var got serverInfo
	status, body := srv.GET("/api/v1/server", &got)
	if status != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200; body: %s", status, body)
	}

	// Each flag paired with a path that only its slice serves. Keep this table in
	// step with Metadata.Features: a flag here that has no route, or a route that
	// no flag advertises, is the bug this test exists to catch.
	probes := []struct {
		flag string
		path string
	}{
		{"libraries", "/api/v1/libraries"},
		{"home", "/api/v1/home"},
		{"watchState", "/api/v1/titles/nonexistent/watchState"},
		{"search", "/api/v1/search?q=x"},
		{"collections", "/api/v1/collections"},
		{"playlists", "/api/v1/playlists"},
		{"realtimeEvents", "/api/v1/events"},
		// POST-only, so this GET probe draws a 405 — which is exactly what the
		// probe wants: a 405 proves the route exists, and existence is all the flag
		// claims (ADR-0036).
		{"deviceAuth", "/api/v1/auth/device/code"},
	}

	for _, p := range probes {
		t.Run(p.flag, func(t *testing.T) {
			resp := srv.Do(http.MethodGet, p.path, nil)
			defer resp.Body.Close()

			served := resp.StatusCode != http.StatusNotFound
			advertised, present := got.Features[p.flag]
			if !present {
				t.Fatalf("features map has no %q key, but %s is a route this server serves",
					p.flag, p.path)
			}

			if served && !advertised {
				t.Errorf("features[%q] = false, but GET %s is served (status %d). "+
					"Clients branch on flags, so this hides a working feature.",
					p.flag, p.path, resp.StatusCode)
			}
			if !served && advertised {
				t.Errorf("features[%q] = true, but GET %s 404s. "+
					"Clients will call a route that does not exist.",
					p.flag, p.path)
			}
		})
	}
}

// TestTranscodeFlagIsNotRouteExistence pins the one flag that deliberately reads
// false while a related route is served, so a future reader does not "fix" it by
// pattern-matching against TestFeaturesMatchRoutes. transcode advertises the
// transcode delivery tier, not the /transcoding admin snapshot (ADR-0029); it
// should flip only when it is computed from a resolved ffmpeg backend.
func TestTranscodeFlagIsNotRouteExistence(t *testing.T) {
	srv := testharness.New(t)

	var got serverInfo
	if status, body := srv.GET("/api/v1/server", &got); status != http.StatusOK {
		t.Fatalf("handshake status = %d, want 200; body: %s", status, body)
	}

	if got.Features["transcode"] {
		t.Error("features[\"transcode\"] = true; if it is now computed from the " +
			"ffmpeg backend rather than hardcoded, delete this test")
	}
}
