package api_test

import (
	"io"
	"net/http"
	"reflect"
	"sync"
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
