package api_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// TestLibraryScopedEventsGatedByGrant: the realtime stream honors real grants. A
// Member granted only Library A receives no library-scoped events for a scan of
// Library B, while an Admin subscribed at the same time does. This proves the
// Broker's accessible-Library set is now seeded from the resolved access Scope
// (issue 05) — with no change to the Broker itself.
func TestLibraryScopedEventsGatedByGrant(t *testing.T) {
	rootA := t.TempDir()
	makeMovie(t, filepath.Join(rootA, "Alpha Movie (2001)", "Alpha Movie (2001).mp4"))
	rootB := t.TempDir()
	makeMovie(t, filepath.Join(rootB, "Beta Movie (2002)", "Beta Movie (2002).mp4"))

	srv := testharness.New(t)
	adminTok := adminToken(t, srv)
	libA := createMovieLibrary(t, srv, adminTok, rootA)
	libB := createMovieLibrary(t, srv, adminTok, rootB)

	// A Member granted only Library A (Library B is ungranted).
	memberID := srv.CreateUser(adminTok, "kid", "memberpass123", "member")
	grantLibraries(t, srv, adminTok, memberID, libA)
	memberTok := srv.LoginAs("kid", "memberpass123")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adminLines := openEventStream(t, ctx, srv, adminTok)
	memberLines := openEventStream(t, ctx, srv, memberTok)

	// Scan Library B (ungranted to the Member). The Admin's stream sees B's
	// library-scoped events (the data line carries B's id); wait for one so B's
	// events are published (and gated) before we trigger the Member's sentinel.
	scanLib(t, srv, adminTok, libB, "")
	waitForLine(t, adminLines, func(s string) bool {
		return strings.Contains(s, `"libraryId":"`+libB+`"`)
	})

	// Now scan Library A (granted). The Member receives A's events — and must never
	// have received any event carrying Library B's id ahead of this sentinel.
	scanLib(t, srv, adminTok, libA, "")
	waitForLineForbidding(t, memberLines,
		func(s string) bool { return strings.Contains(s, `"libraryId":"`+libA+`"`) },
		`"libraryId":"`+libB+`"`,
	)
}
