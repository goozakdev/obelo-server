package api_test

import (
	"io"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// TestUninstallingTheirSignInProviderEndsADeletedUsersOpenRelay: a User the uninstall
// of their only sign-in path deletes loses the Online response already open.
func TestUninstallingTheirSignInProviderEndsADeletedUsersOpenRelay(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, media := onlineAccessServerFull(t, func(dataDir string) {
		plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::"))
	})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	mustGrantSources(t, srv, admin, ada.User.ID, onlineSlug)
	st, play, raw := playOn(t, srv, ada.Token, onlineSlug, "slow", canPlayMP4(nil))
	if st != http.StatusOK {
		t.Fatalf("play = %d %s", st, raw)
	}
	resp, err := http.Get(srv.URL(play.StreamURL))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("opening the stream: %v %v", err, resp)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(ended) }()
	<-media.slowStarted

	preview := uninstallPreview(t, srv, admin, "directory")
	if status, body := uninstallConfirming(t, srv, admin, "directory", previewIDs(preview)); status != http.StatusOK {
		t.Fatalf("confirmed uninstall = %d %s", status, body)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the deleted User's open response kept flowing")
	}
}

// More black-box tests for who may see an Online source (issue 03): what a revocation
// does to a response already open, a disabled source's grant, a play racing a
// revocation, and a deleted User.

// TestRevokingAGrantCutsTheRelayAlreadyOpen: a response mid-flight from the media host
// stops when the grant goes (directly, or by a Rating ceiling), not merely the next
// request, and the upstream request is dropped with it.
func TestRevokingAGrantCutsTheRelayAlreadyOpen(t *testing.T) {
	t.Parallel()
	for name, revoke := range map[string]func(t *testing.T, srv *testharness.Server, admin, kidID string){
		"direct removal": func(t *testing.T, srv *testharness.Server, admin, kidID string) {
			mustGrantSources(t, srv, admin, kidID, otherSlug)
		},
		"a rating ceiling": func(t *testing.T, srv *testharness.Server, admin, kidID string) {
			if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+kidID+"/ratingCeiling", admin, map[string]any{"rating": "PG"}, nil); st != http.StatusOK {
				t.Fatalf("setting the ceiling = %d %s", st, body)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, admin, _, _, media := onlineAccessServerFull(t, nil)
			kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
			mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
			kid := srv.LoginAs("kid", "memberpass123")
			st, play, raw := playOn(t, srv, kid, onlineSlug, "slow", canPlayMP4(nil))
			if st != http.StatusOK || play.Format != "progressive" {
				t.Fatalf("play = %d %s", st, raw)
			}

			resp, err := http.Get(srv.URL(play.StreamURL))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("opening the stream: %v %v", err, resp)
			}
			defer resp.Body.Close()
			ended := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, resp.Body); close(ended) }()
			select {
			case <-media.slowStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("the upstream never started")
			}
			select {
			case <-ended:
				t.Fatal("the response ended before any revocation")
			case <-time.After(150 * time.Millisecond):
			}

			revoke(t, srv, admin, kidID)
			select {
			case <-ended:
			case <-time.After(3 * time.Second):
				t.Fatal("the open response kept flowing after the grant was revoked")
			}
			select {
			case <-media.slowEnded:
			case <-time.After(3 * time.Second):
				t.Fatal("the upstream request was not dropped")
			}
		})
	}
}

// TestRevokingAGrantCutsALargeRelayFarShortOfTheWholeBody: a 128 MiB upstream
// (generated as it is sent, so the test holds none of it) read slowly by the client
// is cut mid-copy by a revocation: the client gets a small fraction of the body, and
// the upstream request is dropped promptly rather than drained.
func TestRevokingAGrantCutsALargeRelayFarShortOfTheWholeBody(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, media := onlineAccessServerFull(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	st, play, raw := playOn(t, srv, kid, onlineSlug, "big", canPlayMP4(nil))
	if st != http.StatusOK || play.Format != "progressive" {
		t.Fatalf("play = %d %s", st, raw)
	}
	resp, err := http.Get(srv.URL(play.StreamURL))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("opening the stream: %v %v", err, resp)
	}
	defer resp.Body.Close()

	// A slow reader: 16 KiB every 10ms, about 1.6 MB/s.
	var received atomic.Int64
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		buf := make([]byte, 16<<10)
		for {
			n, err := resp.Body.Read(buf)
			received.Add(int64(n))
			if err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-media.bigStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream never started")
	}
	time.Sleep(300 * time.Millisecond)
	if got := received.Load(); got == 0 || got >= bigBody {
		t.Fatalf("received %d bytes before the revocation, want some but not all", got)
	}

	mustGrantSources(t, srv, admin, kidID, otherSlug)
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the client kept receiving after the grant was revoked")
	}
	select {
	case <-media.bigEnded:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
	if got := received.Load(); got > bigBody/8 {
		t.Fatalf("client received %d of %d bytes, want far less after a mid-copy revocation", got, bigBody)
	}
	if got := media.bigSent.Load(); got > bigBody/2 {
		t.Fatalf("upstream wrote %d of %d bytes, want the copy cut well short", got, bigBody)
	}
}

// TestAMemberWithNoCeilingSendingTheWebPlayersBitrateStillGetsTheRelay: the web player
// always asks for 100 Mbit/s; for a muxed variant it can play that is not a bitrate
// ceiling, so the bytes are relayed and ffmpeg is not started.
func TestAMemberWithNoCeilingSendingTheWebPlayersBitrateStillGetsTheRelay(t *testing.T) {
	t.Parallel()
	srv, admin, runner, _, _ := onlineAccessServerFull(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	st, play, raw := playOn(t, srv, kid, onlineSlug, onlineItemMP4,
		canPlayMP4(map[string]any{"maxBitrate": 100000000, "maxResolution": "1080p"}))
	if st != http.StatusOK || play.Format != "progressive" {
		t.Fatalf("play = %d %s, want a progressive relay", st, raw)
	}
	runner.jmu.Lock()
	defer runner.jmu.Unlock()
	if len(runner.jobs) != 0 {
		t.Fatalf("ffmpeg started %d times for a relayed play, want 0", len(runner.jobs))
	}
}

// TestRevokingAGrantTellsThePlayerOnItsNextKeepalive: the stream just stops when the
// grant goes, so the keepalive after it says why (410 SOURCE_GONE with an
// access-removed message), to the session's own User only.
func TestRevokingAGrantTellsThePlayerOnItsNextKeepalive(t *testing.T) {
	t.Parallel()
	srv, admin, _, _, _ := onlineAccessServerFull(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	st, play, raw := playOn(t, srv, kid, onlineSlug, onlineItemMP4, canPlayMP4(nil))
	if st != http.StatusOK {
		t.Fatalf("play = %d %s", st, raw)
	}
	mustGrantSources(t, srv, admin, kidID, otherSlug)

	var env errorEnvelope
	st, body := srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", kid,
		map[string]any{"positionMs": 1, "state": "playing"}, &env)
	if st != http.StatusGone || env.Error.Code != "SOURCE_GONE" || env.Error.Message != "You no longer have access to "+onlineName {
		t.Fatalf("keepalive after the revocation = %d %s, want 410 SOURCE_GONE with the access-removed message", st, body)
	}
	if st, body = srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
		map[string]any{"positionMs": 1, "state": "playing"}, nil); st != http.StatusNotFound {
		t.Fatalf("another User's keepalive = %d %s, want 404", st, body)
	}
}

// TestAGrantToADisabledSourceIsKeptUntilTheAdminLeavesItOut: switching a source off does
// not take grants; a replace-set may still name it while the User holds it, leaving it
// out removes it, and an id the User never held that is not enabled is still refused.
func TestAGrantToADisabledSourceIsKeptUntilTheAdminLeavesItOut(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/disable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("disabling = %d %s", st, body)
	}

	// Unticking the enabled source while the disabled one stays in the set.
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	if got := grantedSources(t, srv, admin, kidID); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("grants after unticking %s = %v, want the disabled %s kept", otherSlug, got, onlineSlug)
	}
	// Never held and not enabled: refused.
	freeID := srv.CreateUser(admin, "free", "memberpass123", "member")
	if st, env := putSourceAccess(srv, admin, freeID, onlineSlug); st != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_SOURCE" {
		t.Fatalf("granting a disabled source never held = %d %+v, want 422 UNKNOWN_SOURCE", st, env.Error)
	}
	if st, env := putSourceAccess(srv, admin, kidID, "nonesuch"); st != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_SOURCE" {
		t.Fatalf("granting an unknown source = %d %+v, want 422", st, env.Error)
	}
	// Leaving it out is the explicit untick.
	mustGrantSources(t, srv, admin, kidID)
	if got := grantedSources(t, srv, admin, kidID); len(got) != 0 {
		t.Fatalf("grants after leaving the disabled source out = %v, want none", got)
	}
}

// TestAGrantRemovedWhileResolveRunsRefusesThePlay: access lost between the check at the
// start of a play and the session existing leaves no session for the revocation to
// end, so the play re-checks once it has registered one and answers the 404 an
// ungranted caller gets.
func TestAGrantRemovedWhileResolveRunsRefusesThePlay(t *testing.T) {
	t.Parallel()
	srv, admin, _, src, _ := onlineAccessServerFull(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	kid := srv.LoginAs("kid", "memberpass123")

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	orig := src.variants
	src.variants = func(itemID string) []map[string]any {
		once.Do(func() { close(entered) })
		<-release
		return orig(itemID)
	}
	result := make(chan int, 1)
	go func() {
		st, _, _ := playOn(t, srv, kid, onlineSlug, onlineItemMP4, canPlayMP4(nil))
		result <- st
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("resolve() was never reached")
	}
	mustGrantSources(t, srv, admin, kidID, otherSlug)
	close(release)
	select {
	case st := <-result:
		if st != http.StatusNotFound {
			t.Fatalf("a play whose grant was removed during resolve = %d, want 404", st)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the play never answered")
	}
}

// TestDeletingAUserEndsTheirLiveOnlineSessions: the ffmpeg run of a deleted User's
// session is stopped with them.
func TestDeletingAUserEndsTheirLiveOnlineSessions(t *testing.T) {
	t.Parallel()
	srv, admin, runner := onlineAccessServer(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	if st, play, raw := playOn(t, srv, kid, onlineSlug, "split", canPlayMP4(nil)); st != http.StatusOK || play.Format != "hls" {
		t.Fatalf("ffmpeg play = %d %s", st, raw)
	}
	if runner.stopped(0) {
		t.Fatal("the run was already stopped")
	}
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/users/"+kidID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("deleting the User = %d %s", st, body)
	}
	if !runner.stopped(0) {
		t.Fatal("the deleted User's ffmpeg run is still running")
	}
}
