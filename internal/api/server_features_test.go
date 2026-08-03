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
	//
	// method is the verb the probe uses, defaulting to GET. It exists for exactly
	// one flag — see streamToken below, whose routes answer 404 to a GET on purpose.
	probes := []struct {
		flag   string
		path   string
		method string
	}{
		{flag: "libraries", path: "/api/v1/libraries"},
		{flag: "home", path: "/api/v1/home"},
		{flag: "watchState", path: "/api/v1/titles/nonexistent/watchState"},
		{flag: "search", path: "/api/v1/search?q=x"},
		{flag: "collections", path: "/api/v1/collections"},
		{flag: "playlists", path: "/api/v1/playlists"},
		{flag: "realtimeEvents", path: "/api/v1/events"},
		// POST-only, so this GET probe draws a 405 — which is exactly what the
		// probe wants: a 405 proves the route exists, and existence is all the flag
		// claims (ADR-0036).
		{flag: "deviceAuth", path: "/api/v1/auth/device/code"},
		// remuxSelectedOnly rides the POST-only playback route (the flag advertises a
		// request FIELD on it, PRD remux-selected). A GET draws a 405 — the route
		// exists, which is what the flag claims; the field's behaviour is pinned by the
		// playback tests, not by route existence.
		{flag: "remuxSelectedOnly", path: "/api/v1/titles/nonexistent/playback"},
		// POST-only, so this GET probe draws a 405 — which proves the route exists,
		// and existence is all the flag claims. mediaCookieRefresh advertises
		// POST /auth/media-cookie (appletv-parity/12).
		{flag: "mediaCookieRefresh", path: "/api/v1/auth/media-cookie"},
		// The ONE probe that cannot be a GET. The token-carried media routes
		// (ADR-0039) answer every credential failure with an existence-hiding 404 —
		// that is the posture, not an accident — so a GET with a made-up token is
		// indistinguishable from the route not existing, which is precisely what this
		// test measures. A non-GET is refused BEFORE the token is examined (the 405
		// gate runs first, so the answer cannot become a token-validity oracle), so it
		// draws a 405 from a server that serves the subtree and a 404 from one that
		// does not. That is the distinction the flag claims, and the only verb that
		// can observe it.
		{flag: "streamToken", path: "/api/v1/stream/not-a-real-token/stream", method: http.MethodPost},
	}

	for _, p := range probes {
		t.Run(p.flag, func(t *testing.T) {
			method := p.method
			if method == "" {
				method = http.MethodGet
			}
			resp := srv.Do(method, p.path, nil)
			defer resp.Body.Close()

			served := resp.StatusCode != http.StatusNotFound
			advertised, present := got.Features[p.flag]
			if !present {
				t.Fatalf("features map has no %q key, but %s is a route this server serves",
					p.flag, p.path)
			}

			if served && !advertised {
				t.Errorf("features[%q] = false, but %s %s is served (status %d). "+
					"Clients branch on flags, so this hides a working feature.",
					p.flag, method, p.path, resp.StatusCode)
			}
			if !served && advertised {
				t.Errorf("features[%q] = true, but %s %s 404s. "+
					"Clients will call a route that does not exist.",
					p.flag, method, p.path)
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
