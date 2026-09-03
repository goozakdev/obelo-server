package api_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Keeping a mirror fresh, over real HTTP between two real Servers
// (.scratch/linked-servers issue 08, ADR-0056 §4, §6).
//
// The unit-level proof — the incremental walk, the 410 restart, the backoff
// schedule, the state machine — is in internal/link, against a sharer that can be
// made to misbehave. What can only be proved HERE is that the two halves fit: the
// home Server's `since` is the cursor the sharer's export actually issued, the
// nudge it acts on is the event the sharer actually publishes, and the badge a
// client reads follows the Link's state.

// syncFixture is a sharer whose Library lives in a WRITABLE directory — so a
// Title can be added to it mid-test, which is the whole acceptance criterion —
// and a home Server linked to it.
type syncFixture struct {
	sharer      *testharness.Server
	sharerAdmin string
	sharerLib   string
	root        string
	home        *testharness.Server
	homeAdmin   string
	link        linkResp
	mirrorLib   string
}

func newLinkSyncFixture(t *testing.T, homeOpts ...testharness.Option) *syncFixture {
	t.Helper()

	f := &syncFixture{root: t.TempDir()}
	makeMovie(t, filepath.Join(f.root, "First Movie (2001)", "First Movie (2001).mp4"))

	f.sharer = testharness.New(t)
	f.sharerAdmin = adminToken(t, f.sharer)
	f.sharerLib = createMovieLibrary(t, f.sharer, f.sharerAdmin, f.root)
	scanLib(t, f.sharer, f.sharerAdmin, f.sharerLib, "")

	remoteID := createRemoteUser(t, f.sharer, f.sharerAdmin, "The other household")
	grantLibraries(t, f.sharer, f.sharerAdmin, remoteID, f.sharerLib)

	// The home Server is created SECOND on purpose: t.Cleanup is LIFO, so it shuts
	// down — releasing any subscription it holds — before the sharer's listener is
	// closed under it.
	f.home = testharness.New(t, homeOpts...)
	f.homeAdmin = adminToken(t, f.home)

	invite := mintInvite(t, f.sharer, f.sharerAdmin, remoteID, f.sharer.URL("")).Invite
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &f.link); status != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201; body: %s", status, body)
	}
	if len(f.link.Libraries) != 1 {
		t.Fatalf("the link brought %d libraries, want 1: %+v", len(f.link.Libraries), f.link.Libraries)
	}
	f.mirrorLib = f.link.Libraries[0].ID
	return f
}

// addMovie puts one more film in the sharer's folder and rescans it, which is
// what an evening's downloading looks like from the mirror's side.
func (f *syncFixture) addMovie(t *testing.T, name string) {
	t.Helper()
	makeMovie(t, filepath.Join(f.root, name, name+".mp4"))
	scanLib(t, f.sharer, f.sharerAdmin, f.sharerLib, "")
}

// mirroredTitles is what the home Server's browse says about the mirror.
func (f *syncFixture) mirroredTitles(t *testing.T) []string {
	t.Helper()
	var grid struct {
		Titles []struct {
			Title string `json:"title"`
		} `json:"titles"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirrorLib+"/titles", f.homeAdmin, &grid); status != http.StatusOK {
		t.Fatalf("browsing the mirror = %d; body: %s", status, body)
	}
	out := make([]string, 0, len(grid.Titles))
	for _, ti := range grid.Titles {
		out = append(out, ti.Title)
	}
	return out
}

func (f *syncFixture) syncNow(t *testing.T, out any) (int, []byte) {
	t.Helper()
	return f.home.JSON(http.MethodPost, "/api/v1/links/"+f.link.ID+"/sync", f.homeAdmin, nil, out)
}

func (f *syncFixture) currentLink(t *testing.T) linkResp {
	t.Helper()
	var list []linkResp
	if status, body := f.home.AuthGET("/api/v1/links", f.homeAdmin, &list); status != http.StatusOK || len(list) != 1 {
		t.Fatalf("GET /links = %d with %d links; body: %s", status, len(list), body)
	}
	return list[0]
}

// eventually polls until cond holds. Nothing here waits out a duration: the poll
// ends the moment the background pull has landed, and the deadline exists so a
// broken build fails instead of hanging.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSyncNowPullsWhatChangedOnTheSharer is the operator's "try again now", and
// the only end-to-end proof that the incremental pull speaks the sharer's own
// dialect: the `since` the home Server sends back is the opaque cursor the
// sharer's export issued, not something this side invented.
func TestSyncNowPullsWhatChangedOnTheSharer(t *testing.T) {
	f := newLinkSyncFixture(t)

	before := f.mirroredTitles(t)
	if len(before) != 1 || !contains(before, "First Movie") {
		t.Fatalf("the first pull mirrored %v, want the one film", before)
	}
	if f.link.LastSyncedAt == nil {
		t.Error("lastSyncedAt is null after the first pull; nothing recorded the sweep")
	}

	f.addMovie(t, "Second Movie (2002)")

	var synced linkResp
	if status, body := f.syncNow(t, &synced); status != http.StatusOK {
		t.Fatalf("POST /links/{id}/sync = %d, want 200; body: %s", status, body)
	}
	if synced.State != "connected" {
		t.Errorf("state after a good sweep = %q, want connected", synced.State)
	}

	after := f.mirroredTitles(t)
	if len(after) != 2 || !contains(after, "Second Movie") || !contains(after, "First Movie") {
		t.Fatalf("after the sync the mirror holds %v, want both films", after)
	}
}

// TestTheSharersNudgeRefreshesTheMirror is the acceptance criterion in one test:
// a Title added on the sharer is on the home Server WITHIN SECONDS, with nobody
// pressing anything and the timer an hour away. The mechanism is the `/events`
// subscription the home Server holds under the Link's token — the sharer's scan
// publishes libraryUpdated to the `remote` User, whose stream is audience-gated
// to exactly the Libraries it was granted.
func TestTheSharersNudgeRefreshesTheMirror(t *testing.T) {
	f := newLinkSyncFixture(t, testharness.WithLinkSyncInterval(time.Hour))

	f.addMovie(t, "Second Movie (2002)")

	eventually(t, "the nudged pull to land the new film", func() bool {
		return contains(f.mirroredTitles(t), "Second Movie")
	})
}

// TestAnUnreachableSharerLeavesTheMirrorInPlace is ADR-0056 §6's promise, which
// is mostly about what does NOT happen: the friend's server goes away, the Link
// says so, the badge says so — and the shelf, the Titles and this household's
// watch state are all exactly where they were.
func TestAnUnreachableSharerLeavesTheMirrorInPlace(t *testing.T) {
	f := newLinkSyncFixture(t)

	f.sharer.Close()

	var errBody errorEnvelope
	if status, body := f.syncNow(t, &errBody); status != http.StatusServiceUnavailable {
		t.Fatalf("syncing an unreachable sharer = %d, want 503; body: %s", status, body)
	}
	if errBody.Error.Code != "LINK_UNREACHABLE" {
		t.Errorf("error code = %q, want LINK_UNREACHABLE", errBody.Error.Code)
	}

	l := f.currentLink(t)
	if l.State != "unreachable" || l.LastError == "" {
		t.Fatalf("link = %+v, want unreachable with a reason on it", l)
	}
	if len(l.Libraries) != 1 {
		t.Fatalf("the mirror lost its shelf: %+v", l.Libraries)
	}

	// The badge follows the state, which is the only thing a client needs to grey
	// the shelf out — and the Titles are still there under it.
	var libs struct {
		Libraries []struct {
			ID        string `json:"id"`
			Linked    bool   `json:"linked"`
			Available *bool  `json:"available"`
		} `json:"libraries"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries", f.homeAdmin, &libs); status != http.StatusOK {
		t.Fatalf("GET /libraries = %d; body: %s", status, body)
	}
	found := false
	for _, lib := range libs.Libraries {
		if lib.ID != f.mirrorLib {
			continue
		}
		found = true
		if !lib.Linked || lib.Available == nil || *lib.Available {
			t.Errorf("mirror badge = linked:%v available:%v, want linked and NOT available",
				lib.Linked, lib.Available)
		}
	}
	if !found {
		t.Fatal("the linked Library vanished from GET /libraries when its sharer went away")
	}
	if titles := f.mirroredTitles(t); len(titles) != 1 {
		t.Errorf("the mirror browses to %v, want the film that was already here", titles)
	}
}

// TestLinkStateEventIsAdminOnly: the state nudge reaches an Admin's stream — so
// the Linked servers page moves without a reload — and reaches a Member's stream
// NEVER. A Link is the household's relationship with another household, which is
// the operator's business; the Member still sees the consequence on the badge.
//
// Modelled on TestTailnetStateEventAdminOnly, sentinel and all: the Member is
// granted a Library so it is entitled to that Library's scanProgress, which is
// what proves the gate filters by TYPE rather than muting the Member wholesale.
func TestLinkStateEventIsAdminOnly(t *testing.T) {
	const linkEventName = "linkState"
	f := newLinkSyncFixture(t)

	memberID := f.home.CreateUser(f.homeAdmin, "member", "correct horse battery staple", "member")
	localLib := createMovieLibrary(t, f.home, f.homeAdmin, f.root)
	grantLibraries(t, f.home, f.homeAdmin, memberID, localLib)
	memberTok := f.home.LoginAs("member", "correct horse battery staple")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	adminLines := openEventStream(t, ctx, f.home, f.homeAdmin)
	memberLines := openEventStream(t, ctx, f.home, memberTok)

	// Their server goes away, and the forced sweep discovers it: connected →
	// unreachable, which is a transition and therefore a nudge.
	f.sharer.Close()
	if status, body := f.syncNow(t, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("syncing an unreachable sharer = %d, want 503; body: %s", status, body)
	}

	waitForLine(t, adminLines, func(s string) bool { return strings.Contains(s, "event: "+linkEventName) })

	// The link event was published BEFORE this scan, so a leak would sit in the
	// Member's buffer ahead of the sentinel.
	scanLib(t, f.home, f.homeAdmin, localLib, "")
	waitForLineForbidding(t, memberLines,
		func(s string) bool { return strings.Contains(s, "event: "+scanProgressEventName) },
		"event: "+linkEventName,
	)
}

// TestSyncRouteIsAdminOnly: linking is not a per-User act, so neither is asking
// for a pull. A Member gets the same 403 every other /links route gives it.
func TestSyncRouteIsAdminOnly(t *testing.T) {
	f := newLinkSyncFixture(t)
	f.home.CreateMember("member", "correct horse battery staple")
	memberTok := f.home.LoginAs("member", "correct horse battery staple")

	path := "/api/v1/links/" + f.link.ID + "/sync"
	if status, body := f.home.JSON(http.MethodPost, path, memberTok, nil, nil); status != http.StatusForbidden {
		t.Errorf("POST %s as a Member = %d, want 403; body: %s", path, status, body)
	}
	if status, body := f.home.JSON(http.MethodPost, path, "", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("POST %s anonymously = %d, want 401; body: %s", path, status, body)
	}
	if status, body := f.home.JSON(http.MethodPost, "/api/v1/links/nope/sync", f.homeAdmin, nil, nil); status != http.StatusNotFound {
		t.Errorf("syncing a link that does not exist = %d, want 404; body: %s", status, body)
	}
	if status, body := f.home.JSON(http.MethodGet, path, f.homeAdmin, nil, nil); status != http.StatusMethodNotAllowed {
		t.Errorf("GET %s = %d, want 405; body: %s", path, status, body)
	}
}
