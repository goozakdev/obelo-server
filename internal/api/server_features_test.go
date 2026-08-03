package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

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

// TestTranscodeFlagFollowsFFmpegAvailability is the counterpart of
// TestFeaturesMatchRoutes for the ONE flag that is deliberately not about routes.
// transcode advertises the transcode delivery TIER (ADR-0003), which depends on
// the host having a usable ffmpeg — so it is the only flag whose truth two
// otherwise-identical builds can disagree about, and the only one a probe of the
// route table cannot check.
//
// Both directions are asserted because both are real deployments and neither is
// reachable by accident: a developer box has ffmpeg (so `false` would never be
// exercised) and a CI box may not (so `true` would not be), which is exactly why
// the availability answer is injectable. Each direction also re-pins the
// not-route-existence property against /transcoding, the admin observability
// snapshot (ADR-0029) that is served either way — so a future reader cannot
// "fix" this flag by pattern-matching it onto TestFeaturesMatchRoutes.
func TestTranscodeFlagFollowsFFmpegAvailability(t *testing.T) {
	cases := []struct {
		name      string
		available bool
	}{
		{name: "ffmpeg resolved", available: true},
		{name: "no resolvable ffmpeg", available: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t, testharness.WithFFmpegAvailability(tc.available))

			var got serverInfo
			if status, body := srv.GET("/api/v1/server", &got); status != http.StatusOK {
				t.Fatalf("handshake status = %d, want 200; body: %s", status, body)
			}

			advertised, present := got.Features["transcode"]
			if !present {
				t.Fatal("features map has no \"transcode\" key")
			}
			if advertised != tc.available {
				t.Errorf("features[\"transcode\"] = %t, want %t — the flag must follow the "+
					"resolved ffmpeg backend, in both directions", advertised, tc.available)
			}

			// The admin snapshot is served regardless (unauthenticated → 401, which
			// proves the route exists). If the flag ever tracked THIS, it would be true
			// in both cases above — it does not, and that is the documented exception.
			resp := srv.Do(http.MethodGet, "/api/v1/transcoding", nil)
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				t.Fatal("GET /api/v1/transcoding 404s; the transcode flag's " +
					"not-route-existence caveat no longer has anything to say")
			}
		})
	}
}

// TestTranscodeUnavailableDegradesHonestly checks the promise the other half of
// the flag makes. Advertising `transcode: false` tells a client "do not come
// here" — but a client that ignores it, or one that negotiated before the server
// lost its ffmpeg, still arrives. What it must find is an ERROR, promptly: a
// server with no ffmpeg had never actually been exercised on this path, so
// "it degrades cleanly" was an assumption rather than a fact.
//
// The two failure modes that would be worse than an error are a HANG (the HLS
// runtime waits for a playlist ffmpeg is never going to write, tying up a
// connection and leaving the client on a spinner forever) and a PANIC (a nil job
// dereferenced downstream of a launch that failed). This asserts against both:
// a bounded, enveloped 5xx, well inside the deadline.
//
// Note that NEGOTIATION still succeeds and still says `transcode`. That is
// correct and deliberate: the tier is what this File plus this profile requires,
// and the decision reports the truth about the media rather than about the host.
// Whether the host can serve it is exactly what features.transcode is for.
func TestTranscodeUnavailableDegradesHonestly(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithFFmpegAvailability(false))
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	bladeID := findTitle(t, list, "Blade Runner")

	dec := negotiateTranscodeDecision(t, srv, token, bladeID, transcodeProfile())

	start := time.Now()
	status, body := srv.AuthGET(dec.StreamURL, token, nil)
	elapsed := time.Since(start)

	// An honest server error, not a lie and not a 200 with an empty playlist.
	if status < 500 || status > 599 {
		t.Errorf("GET %s = %d, want a 5xx (ffmpeg cannot be spawned); body: %s",
			dec.StreamURL, status, body)
	}
	// The error envelope holds even on the path nothing had walked before.
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		t.Errorf("response is not an error envelope (%v); body: %s", err, body)
	}
	// The runtime must fail on the failed LAUNCH, not sit in a playlist/segment
	// wait window for output that will never arrive. The waits are seconds-scale,
	// so anything near them means the launch error was swallowed.
	if elapsed > 3*time.Second {
		t.Errorf("GET %s took %s — a failed ffmpeg launch must surface immediately, "+
			"not wait out the playlist window", dec.StreamURL, elapsed)
	}
}
