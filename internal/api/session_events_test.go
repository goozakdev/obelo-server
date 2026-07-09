package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Admin-only session events (realtime-events issue 03). The session lifecycle
// (sessionStarted / nowPlaying / sessionEnded) is AudienceAdmin: the Admin's
// /events stream sees all three, a Member's stream sees NONE — while the Member
// still receives the non-session events it is entitled to. These are black-box
// tests over GET /api/v1/events, mirroring TestScanProgressSSE's handshake +
// cancel-on-return discipline (openEventStream/waitForLine).

const (
	sessionStartedEventName = "sessionStarted"
	nowPlayingEventName     = "nowPlaying"
	sessionEndedEventName   = "sessionEnded"
)

// waitForLineForbidding consumes SSE lines until one satisfies want, failing the
// test if any line along the way contains a forbidden substring. It is the
// absence assertion for the gating test: a Member stream must reach a sentinel
// event (scanProgress) WITHOUT ever carrying a session event in between. Because
// the session events are published before the sentinel, a leak would surface in
// the buffer ahead of the sentinel.
func waitForLineForbidding(t *testing.T, ch <-chan string, want func(string) bool, forbidden ...string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-ch:
			for _, f := range forbidden {
				if strings.Contains(line, f) {
					t.Fatalf("forbidden substring %q appeared on stream: %q", f, line)
				}
			}
			if want(line) {
				return
			}
		case <-timeout:
			t.Fatal("timed out waiting for a matching SSE line")
		}
	}
}

// endSession ends a Playback session over HTTP (DELETE /sessions/{id}), asserting
// 204 No Content.
func endSession(t *testing.T, srv *testharness.Server, token, sessionID string) {
	t.Helper()
	status, body := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+sessionID, token, nil, nil)
	if status != http.StatusNoContent {
		t.Fatalf("end session status = %d, want 204; body: %s", status, body)
	}
}

// TestSessionEventsAdminOnlySSE is the headline gate test: across a full session
// lifecycle over HTTP (create → progress → end), the Admin stream observes
// sessionStarted / nowPlaying (with position) / sessionEnded, and the Member
// stream observes NONE of them — while still receiving a non-session event
// (scanProgress), proving the gate filters by type rather than muting the Member.
func TestSessionEventsAdminOnlySSE(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	adminTok := adminToken(t, srv)

	// A Member, created via the admin API, so its /events stream carries a
	// non-Admin identity. It is GRANTED the library below so it is entitled to that
	// library's scanProgress sentinel (the gate filters by event TYPE, not by
	// muting the Member) while still being gated out of the admin-only session
	// events.
	memberID := srv.CreateUser(adminTok, "member", "correct horse battery staple", "member")

	// One library we control end to end, so we can both negotiate a session
	// against a Title in it AND re-scan it later to produce the Member's sentinel
	// scanProgress (a second library over the same root would overlap-reject).
	libID := createMovieLibrary(t, srv, adminTok, fixtureRoot(t))
	grantLibraries(t, srv, adminTok, memberID, libID)
	memberTok := srv.LoginAs("member", "correct horse battery staple")
	scanLib(t, srv, adminTok, libID, "")
	duneID := findTitle(t, listAllTitles(t, srv, adminTok, libID), "Dune")

	// Open both streams; cancel before return so the long-lived connections close
	// (else the harness's ts.Close blocks on them).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adminLines := openEventStream(t, ctx, srv, adminTok)
	memberLines := openEventStream(t, ctx, srv, memberTok)

	// Full lifecycle over HTTP: create the session, report progress, end it.
	dec := negotiateDune(t, srv, adminTok, duneID)
	const reportPos = 1234
	postProgress(t, srv, adminTok, dec.SessionID, reportPos, http.StatusOK)
	endSession(t, srv, adminTok, dec.SessionID)

	// Admin sees all three, correlated by the session id; nowPlaying carries the
	// reported position.
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+sessionStartedEventName) })
	waitForLine(t, adminLines, func(s string) bool {
		return strings.Contains(s, "event: "+nowPlayingEventName) || strings.Contains(s, `"positionMs":1234`)
	})
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, `"positionMs":1234`) })
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+sessionEndedEventName) })
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, `"sessionId":"`+dec.SessionID+`"`) })

	// Now trigger a non-session, library-scoped event the Member IS entitled to.
	// The session events were published first, so if the gate leaked they would
	// surface on the Member stream BEFORE this scanProgress sentinel.
	scanLib(t, srv, adminTok, libID, "")
	waitForLineForbidding(t, memberLines,
		func(s string) bool { return strings.Contains(s, "event: "+scanProgressEventName) },
		"event: "+sessionStartedEventName,
		"event: "+nowPlayingEventName,
		"event: "+sessionEndedEventName,
	)
}

// TestSessionEndedOnReapSSE: a session reaped for idleness (not cleanly stopped)
// still emits sessionEnded on the Admin stream — the reap path is the headline
// acceptance criterion. A short SessionIdleTimeout lets the reaper sweep the
// session without a clean DELETE, the same way the reaper tests drive it.
func TestSessionEndedOnReapSSE(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithSessionIdleTimeout(150*time.Millisecond))
	adminTok := adminToken(t, srv)

	list := scanFixtureLibrary(t, srv, adminTok)
	duneID := findTitle(t, list, "Dune")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adminLines := openEventStream(t, ctx, srv, adminTok)

	// Create the session and then stay silent — no progress reports — so the idle
	// reaper sweeps it and emits sessionEnded.
	dec := negotiateDune(t, srv, adminTok, duneID)
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+sessionStartedEventName) })
	// The reaped session ends without a clean DELETE: the event: name and its
	// data: line arrive separately, so assert each in turn.
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+sessionEndedEventName) })
	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, `"sessionId":"`+dec.SessionID+`"`) })
}
