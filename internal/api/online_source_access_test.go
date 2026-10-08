package api_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// Black-box tests for who may see an Online source (ADR-0068, issue 03): the
// per-User grant, the Rating-ceiling rules, the Remote-role exclusion, and the live
// session a grant keeps alive. Two sources are installed (both answered by the one
// fake source) so "the same User's other source" has something to be.

const (
	otherSlug = "tube2"
	otherName = "Test Tube Two"
)

// trackedFFmpeg is the fake ffmpeg that also remembers its jobs, so a test can ask
// whether a run was stopped.
type trackedFFmpeg struct {
	*fakeOnlineFFmpeg
	jmu  sync.Mutex
	jobs []*fakeOnlineJob
}

func (f *trackedFFmpeg) Start(ctx context.Context, args []string) (transcode.Job, error) {
	j, err := f.fakeOnlineFFmpeg.Start(ctx, args)
	if err == nil {
		f.jmu.Lock()
		f.jobs = append(f.jobs, j.(*fakeOnlineJob))
		f.jmu.Unlock()
	}
	return j, err
}

// stopped reports whether the i-th ffmpeg run has been killed.
func (f *trackedFFmpeg) stopped(i int) bool {
	f.jmu.Lock()
	defer f.jmu.Unlock()
	if i >= len(f.jobs) {
		return false
	}
	select {
	case <-f.jobs[i].done:
		return true
	default:
		return false
	}
}

// onlineAccessServer is onlineServer with two sources installed, a runner that
// remembers its jobs, and (extra) a chance to install more Plugins.
func onlineAccessServer(t *testing.T, extra func(dataDir string), opts ...testharness.Option) (*testharness.Server, string, *trackedFFmpeg) {
	t.Helper()
	srv, admin, runner, _, _ := onlineAccessServerFull(t, extra, opts...)
	return srv, admin, runner
}

// onlineAccessServerFull is onlineAccessServer, also returning the fake source and
// media host so a test can hold a resolve() open or watch an upstream response.
func onlineAccessServerFull(t *testing.T, extra func(dataDir string), opts ...testharness.Option) (*testharness.Server, string, *trackedFFmpeg, *onlineSource, *onlineMedia) {
	t.Helper()
	media := newOnlineMedia(t)
	src := newOnlineSource(t, media)
	runner := &trackedFFmpeg{fakeOnlineFFmpeg: &fakeOnlineFFmpeg{}}
	dataDir := t.TempDir()
	for slug, name := range map[string]string{onlineSlug: onlineName, otherSlug: otherName} {
		m := plugintest.OnlineSourceManifest(slug, name, src.srv.URL)
		m.Network.Hosts = []string{"127.0.0.1"}
		plugintest.Install(t, dataDir, m)
	}
	if extra != nil {
		extra(dataDir)
	}
	all := append([]testharness.Option{
		testharness.WithDataDir(dataDir),
		testharness.WithPluginFetchesExemptAt(src.srv.Listener.Addr().String()),
		testharness.WithOnlineSourceClient(media.srv.Client()),
		testharness.WithOnlineSourceRunner(runner),
		testharness.WithFFmpegAvailability(true),
		testharness.WithOnlineSourceMediaExemptAt(media.srv.Listener.Addr().String()),
	}, opts...)
	srv := testharness.New(t, all...)
	return srv, adminToken(t, srv), runner, src, media
}

func putSourceAccess(srv *testharness.Server, admin, userID string, ids ...string) (int, errorEnvelope) {
	if ids == nil {
		ids = []string{}
	}
	var env errorEnvelope
	status, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+userID+"/onlineSourceAccess", admin,
		map[string]any{"sourceIds": ids}, &env)
	return status, env
}

func mustGrantSources(t *testing.T, srv *testharness.Server, admin, userID string, ids ...string) {
	t.Helper()
	if status, env := putSourceAccess(srv, admin, userID, ids...); status != http.StatusNoContent {
		t.Fatalf("granting %v = %d %+v, want 204", ids, status, env.Error)
	}
}

type userSourceDetail struct {
	OnlineSourceIDs []string `json:"onlineSourceIds"`
}

func grantedSources(t *testing.T, srv *testharness.Server, admin, userID string) []string {
	t.Helper()
	var d userSourceDetail
	if status, body := srv.AuthGET("/api/v1/users/"+userID, admin, &d); status != http.StatusOK {
		t.Fatalf("GET /users/%s = %d; body: %s", userID, status, body)
	}
	if d.OnlineSourceIDs == nil {
		d.OnlineSourceIDs = []string{}
	}
	return d.OnlineSourceIDs
}

func tileIDs(t *testing.T, srv *testharness.Server, token string) []string {
	t.Helper()
	var list onlineSourcesResp
	if status, body := srv.AuthGET(onlineBase, token, &list); status != http.StatusOK {
		t.Fatalf("GET /onlineSources = %d; body: %s", status, body)
	}
	ids := []string{}
	for _, s := range list.Sources {
		ids = append(ids, s.ID)
	}
	return ids
}

func playOn(t *testing.T, srv *testharness.Server, token, source, item string, body map[string]any) (int, onlinePlayResp, []byte) {
	t.Helper()
	var out onlinePlayResp
	status, raw := srv.JSON(http.MethodPost, onlineBase+"/"+source+"/items/"+item+"/playback", token, body, &out)
	return status, out, raw
}

// TestAGrantedMemberSeesTheTileAndAnUngrantedOneGetsA404: a Member holding the grant
// sees the tile and can open the page, a thumbnail and play; one without it sees no
// tile and a 404 (not a 403) from every Online endpoint; an Admin sees every enabled
// source with no grant at all.
func TestAGrantedMemberSeesTheTileAndAnUngrantedOneGetsA404(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	granted := srv.CreateUser(admin, "kid", "memberpass123", "member")
	srv.CreateUser(admin, "other", "memberpass123", "member")
	mustGrantSources(t, srv, admin, granted, onlineSlug)
	kid, other := srv.LoginAs("kid", "memberpass123"), srv.LoginAs("other", "memberpass123")

	if got := tileIDs(t, srv, kid); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("granted Member's tiles = %v, want only %s", got, onlineSlug)
	}
	if st, body := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", kid, nil); st != http.StatusOK {
		t.Fatalf("granted Member's rows = %d %s, want 200", st, body)
	}
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/items/"+onlineItemMP4+"/thumbnail", kid, nil); st != http.StatusOK {
		t.Fatalf("granted Member's thumbnail = %d, want 200", st)
	}
	if st, _, body := playOn(t, srv, kid, onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK {
		t.Fatalf("granted Member's playback = %d %s, want 200", st, body)
	}
	// The grant is per source: the other one is still hidden from the same Member.
	if st, _ := srv.AuthGET(onlineBase+"/"+otherSlug+"/rows", kid, nil); st != http.StatusNotFound {
		t.Fatalf("a source the Member holds no grant for = %d, want 404", st)
	}

	if got := tileIDs(t, srv, other); len(got) != 0 {
		t.Fatalf("ungranted Member's tiles = %v, want none", got)
	}
	for _, path := range []string{
		onlineBase + "/" + onlineSlug + "/rows",
		onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail",
	} {
		if st, _ := srv.AuthGET(path, other, nil); st != http.StatusNotFound {
			t.Fatalf("ungranted Member GET %s = %d, want 404", path, st)
		}
	}
	if st, _, _ := playOn(t, srv, other, onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusNotFound {
		t.Fatalf("ungranted Member's playback = %d, want 404", st)
	}

	if got := tileIDs(t, srv, admin); !reflect.DeepEqual(got, []string{otherSlug, onlineSlug}) && !reflect.DeepEqual(got, []string{onlineSlug, otherSlug}) {
		t.Fatalf("Admin's tiles = %v, want both enabled sources", got)
	}
}

// TestAGrantIsRefusedForAUserWithARatingCeiling: the "unrated is visible" rule would
// expose everything to a capped User, so the grant is refused, saying why, and the
// prior set stands. An Admin, a linked Server and a source that does not exist are
// refused too.
func TestAGrantIsRefusedForAUserWithARatingCeiling(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+kidID+"/ratingCeiling", admin, map[string]any{"rating": nil}, nil); st != http.StatusNoContent {
		t.Fatalf("clearing a ceiling that is not set = %d %s, want 204", st, body)
	}
	// A ceiling stored behind the API's back is the state the check must still see.
	srv.Exec(`UPDATE users SET rating_ceiling = 'PG' WHERE id = ?`, kidID)

	status, env := putSourceAccess(srv, admin, kidID, onlineSlug, otherSlug)
	if status != http.StatusUnprocessableEntity || env.Error.Code != "RATING_CEILING_SET" ||
		!strings.Contains(env.Error.Message, "rating ceiling") {
		t.Fatalf("granting to a capped User = %d %+v, want 422 RATING_CEILING_SET naming the ceiling", status, env.Error)
	}
	if got := grantedSources(t, srv, admin, kidID); !reflect.DeepEqual(got, []string{onlineSlug}) {
		t.Fatalf("after the refusal the grants are %v, want the prior set", got)
	}

	other := srv.CreateUser(admin, "free", "memberpass123", "member")
	if status, env = putSourceAccess(srv, admin, other, "nonesuch"); status != http.StatusUnprocessableEntity || env.Error.Code != "UNKNOWN_SOURCE" {
		t.Fatalf("granting an unknown source = %d %+v, want 422 UNKNOWN_SOURCE", status, env.Error)
	}
	adminID := adminUserID(t, srv, admin)
	if status, env = putSourceAccess(srv, admin, adminID, onlineSlug); status != http.StatusUnprocessableEntity || env.Error.Code != "ADMIN_GRANT" {
		t.Fatalf("granting to an Admin = %d %+v, want 422 ADMIN_GRANT", status, env.Error)
	}
	remoteID := createRemoteUser(t, srv, admin, "peer")
	if status, env = putSourceAccess(srv, admin, remoteID, onlineSlug); status != http.StatusUnprocessableEntity || env.Error.Code != "REMOTE_GRANT" {
		t.Fatalf("granting to a linked Server = %d %+v, want 422 REMOTE_GRANT", status, env.Error)
	}
	if status, _ = putSourceAccess(srv, admin, "no-such-user", onlineSlug); status != http.StatusNotFound {
		t.Fatalf("granting to an unknown User = %d, want 404", status)
	}
}

// TestSettingARatingCeilingRemovesTheGrantsAndNamesThem: the ceiling is accepted, the
// grants go in the same step so no capped User ever holds one, the response names each
// source removed, and clearing the ceiling later restores nothing.
func TestSettingARatingCeilingRemovesTheGrantsAndNamesThem(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	if got := tileIDs(t, srv, kid); len(got) != 2 {
		t.Fatalf("before the ceiling the Member's tiles = %v, want both", got)
	}

	var out struct {
		Removed []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"removedOnlineSources"`
	}
	status, body := srv.JSON(http.MethodPut, "/api/v1/users/"+kidID+"/ratingCeiling", admin, map[string]any{"rating": "PG"}, &out)
	if status != http.StatusOK {
		t.Fatalf("setting a ceiling on a User holding grants = %d %s, want 200 naming the sources removed", status, body)
	}
	names := []string{}
	for _, r := range out.Removed {
		names = append(names, r.Name)
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), onlineName) || !strings.Contains(strings.Join(names, ","), otherName) {
		t.Fatalf("removed sources = %v, want %q and %q", names, onlineName, otherName)
	}
	if got := grantedSources(t, srv, admin, kidID); len(got) != 0 {
		t.Fatalf("a capped User still holds grants %v", got)
	}
	if got := tileIDs(t, srv, kid); len(got) != 0 {
		t.Fatalf("the capped Member still sees tiles %v", got)
	}

	// Clearing the ceiling gives nothing back, and says nothing was removed.
	if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+kidID+"/ratingCeiling", admin, map[string]any{"rating": nil}, nil); st != http.StatusNoContent {
		t.Fatalf("clearing the ceiling = %d %s, want 204", st, body)
	}
	if got := grantedSources(t, srv, admin, kidID); len(got) != 0 {
		t.Fatalf("clearing the ceiling restored grants %v", got)
	}
	if got := tileIDs(t, srv, kid); len(got) != 0 {
		t.Fatalf("after clearing the ceiling the Member sees tiles %v, want none until re-granted", got)
	}
}

// TestALinkedServerNeverReceivesASource: a Remote-role session gets no tile, page,
// thumbnail or playback for any source, enabled or granted — even with a grant row
// planted behind the API's back — and the Export carries nothing of sources.
func TestALinkedServerNeverReceivesASource(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	remoteID := createRemoteUser(t, srv, admin, "peer")
	srv.Exec(`INSERT INTO user_online_source_access (user_id, source_id) VALUES (?, ?)`, remoteID, onlineSlug)
	remote := srv.IssueTokenForUser(remoteID, "peer-server-id")

	if got := tileIDs(t, srv, remote); len(got) != 0 {
		t.Fatalf("Remote role's tiles = %v, want none", got)
	}
	for _, path := range []string{
		onlineBase + "/" + onlineSlug + "/rows",
		onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail",
	} {
		if st, _ := srv.AuthGET(path, remote, nil); st != http.StatusNotFound {
			t.Fatalf("Remote role GET %s = %d, want 404", path, st)
		}
	}
	if st, _, _ := playOn(t, srv, remote, onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusNotFound {
		t.Fatalf("Remote role's playback = %d, want 404", st)
	}
}

// TestTheExportCarriesNoSourceGrantOrOnlineItem: a granted Member and a played Online
// item change nothing the Export, the one feed a linked Server pulls, says.
func TestTheExportCarriesNoSourceGrantOrOnlineItem(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	lib := createMovieLibrary(t, srv, admin, t.TempDir())
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	if st, _, body := playOn(t, srv, kid, onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK {
		t.Fatalf("playback = %d %s", st, body)
	}
	if st, _ := srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", kid, nil); st != http.StatusOK {
		t.Fatalf("rows = %d", st)
	}

	remote := linkedServerToken(t, srv, admin, lib)
	page, raw := exportPage(t, srv, remote, lib, "", http.StatusOK)
	for _, e := range page.Entities {
		t.Errorf("export carries an entity: %+v", e)
	}
	for _, leak := range []string{onlineSlug, onlineName, otherSlug, "A talk", "onlineSource", "sourceId"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("export mentions %q: %s", leak, raw)
		}
	}
}

// TestADisabledSourceHasNoTileForAnyoneAdminsIncluded: switching a source off
// server-wide hides it from the Admin's tiles and from a Member who holds the grant.
func TestADisabledSourceHasNoTileForAnyoneAdminsIncluded(t *testing.T) {
	t.Parallel()
	srv, admin, _ := onlineAccessServer(t, nil)
	kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
	kid := srv.LoginAs("kid", "memberpass123")
	if st, body := srv.JSON(http.MethodPost, pluginsPath+"/"+onlineSlug+"/disable", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("disabling = %d %s", st, body)
	}
	for name, token := range map[string]string{"admin": admin, "member": kid} {
		if got := tileIDs(t, srv, token); !reflect.DeepEqual(got, []string{otherSlug}) {
			t.Errorf("%s's tiles after disabling %s = %v, want only %s", name, onlineSlug, got, otherSlug)
		}
	}
}

// liveSessions starts the sessions of the Q19 scenario and returns what a test needs
// to ask whether each still serves: A is a relayed play of tube, B an ffmpeg play of
// tube (the first run), C a relayed play of tube2.
type liveSessions struct{ a, b, c onlinePlayResp }

func startSessions(t *testing.T, srv *testharness.Server, token string) liveSessions {
	t.Helper()
	var s liveSessions
	var st int
	var raw []byte
	if st, s.a, raw = playOn(t, srv, token, onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK || s.a.Format != "progressive" {
		t.Fatalf("relayed play on %s = %d %s", onlineSlug, st, raw)
	}
	if st, s.b, raw = playOn(t, srv, token, onlineSlug, "split", canPlayMP4(nil)); st != http.StatusOK || s.b.Format != "hls" {
		t.Fatalf("ffmpeg play on %s = %d %s", onlineSlug, st, raw)
	}
	if st, s.c, raw = playOn(t, srv, token, otherSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK || s.c.Format != "progressive" {
		t.Fatalf("relayed play on %s = %d %s", otherSlug, st, raw)
	}
	return s
}

// serves reports whether a stream token URL still serves bytes.
func serves(t *testing.T, srv *testharness.Server, p onlinePlayResp) bool {
	t.Helper()
	resp, _ := getBytes(t, srv, p.StreamURL, nil)
	return resp.StatusCode == http.StatusOK
}

// TestLosingAGrantEndsTheLiveSession (Q19): removing a grant directly, and setting a
// Rating ceiling (which removes them), each end the User's live sessions on the
// affected sources at once: the stream token stops serving and the ffmpeg run is
// stopped. Another User's session and the same User's session on a source they keep
// are untouched.
func TestLosingAGrantEndsTheLiveSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		revoke func(srv *testharness.Server, admin, kidID string)
		// which of A, B, C the revocation ends.
		endsA, endsB, endsC bool
	}{
		"direct removal": {
			revoke: func(srv *testharness.Server, admin, kidID string) { mustGrantSources(t, srv, admin, kidID, otherSlug) },
			endsA:  true, endsB: true, endsC: false,
		},
		"a rating ceiling": {
			revoke: func(srv *testharness.Server, admin, kidID string) {
				if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+kidID+"/ratingCeiling", admin, map[string]any{"rating": "PG"}, nil); st != http.StatusOK {
					t.Fatalf("setting the ceiling = %d %s", st, body)
				}
			},
			endsA: true, endsB: true, endsC: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, admin, runner := onlineAccessServer(t, nil)
			kidID := srv.CreateUser(admin, "kid", "memberpass123", "member")
			otherID := srv.CreateUser(admin, "other", "memberpass123", "member")
			mustGrantSources(t, srv, admin, kidID, onlineSlug, otherSlug)
			mustGrantSources(t, srv, admin, otherID, onlineSlug)
			kid, other := srv.LoginAs("kid", "memberpass123"), srv.LoginAs("other", "memberpass123")

			kids := startSessions(t, srv, kid)
			st, theirs, raw := playOn(t, srv, other, onlineSlug, onlineItemMP4, canPlayMP4(nil))
			if st != http.StatusOK {
				t.Fatalf("the other Member's play = %d %s", st, raw)
			}
			for label, p := range map[string]onlinePlayResp{"A": kids.a, "B": kids.b, "C": kids.c, "theirs": theirs} {
				if !serves(t, srv, p) {
					t.Fatalf("session %s does not serve before the revocation", label)
				}
			}

			tc.revoke(srv, admin, kidID)

			for label, c := range map[string]struct {
				p     onlinePlayResp
				ended bool
			}{"A": {kids.a, tc.endsA}, "B": {kids.b, tc.endsB}, "C": {kids.c, tc.endsC}, "theirs": {theirs, false}} {
				if got := !serves(t, srv, c.p); got != c.ended {
					t.Errorf("session %s: ended = %v, want %v", label, got, c.ended)
				}
			}
			if got := runner.stopped(0); got != tc.endsB {
				t.Errorf("the ffmpeg run of session B stopped = %v, want %v", got, tc.endsB)
			}
			// The ended session is gone for the player's keepalive too.
			if tc.endsA {
				if st, _ := srv.JSON(http.MethodPost, "/api/v1/sessions/"+kids.a.SessionID+"/progress", kid, map[string]any{"positionMs": 1}, nil); st == http.StatusOK {
					t.Errorf("the keepalive of an ended session answered 200")
				}
			}
		})
	}
}

// TestAnAdminDemotedMidSessionLosesTheLiveSession: a Group mapping that makes an
// Admin a Member, with no grant to hold, ends the live Online sessions the Admin had
// the way losing a grant does.
func TestAnAdminDemotedMidSessionLosesTheLiveSession(t *testing.T) {
	t.Parallel()
	srv, admin, runner := onlineAccessServer(t, func(dataDir string) {
		plugintest.Install(t, dataDir, plugintest.SignInManifest("directory", "ada:ada-pw:subject-ada::admins"))
	})
	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{{"group": "admins", "role": "admin"}}})
	ada := login(t, srv, "ada", "ada-pw", "Laptop", "test", "ada-client")
	if ada.User.Role != "admin" {
		t.Fatalf("ada = %+v, want an admin by mapping", ada.User)
	}
	live := startSessions(t, srv, ada.Token)
	if !serves(t, srv, live.a) || !serves(t, srv, live.b) {
		t.Fatalf("the Admin's sessions do not serve before the demotion")
	}

	putGroupMapping(t, srv, admin, "directory", map[string]any{"rules": []map[string]any{{"group": "admins", "role": "member"}}})
	if res := resyncNow(t, srv, admin, "directory"); res.Remapped != 1 {
		t.Fatalf("re-sync now = %+v, want ada re-mapped", res)
	}
	if d := userDetail(t, srv, admin, ada.User.ID); d.Role != "member" {
		t.Fatalf("ada is %q after the re-map, want member", d.Role)
	}

	for label, p := range map[string]onlinePlayResp{"relayed": live.a, "encoded": live.b, "other source": live.c} {
		if serves(t, srv, p) {
			t.Errorf("the demoted Admin's %s session still serves", label)
		}
	}
	if !runner.stopped(0) {
		t.Errorf("the ffmpeg run of the demoted Admin's session is still running")
	}
}

// TestTheBitrateCeilingHoldsAnOnlineItemToItsCap: variants carry no bitrate, so the
// relay cannot be shown to be under a bitrate cap. A User with one is therefore
// never relayed: the item goes through ffmpeg held to that bitrate, or, with no
// ffmpeg, is refused as a Title that needs a transcode is.
func TestTheBitrateCeilingHoldsAnOnlineItemToItsCap(t *testing.T) {
	t.Parallel()
	setCap := func(srv *testharness.Server, admin, id string) {
		if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+id+"/playbackCeiling", admin, map[string]any{"maxBitrate": 2_000_000}, nil); st != http.StatusNoContent {
			t.Fatalf("setting the playback ceiling = %d %s", st, body)
		}
	}

	t.Run("with ffmpeg it is encoded at the cap", func(t *testing.T) {
		t.Parallel()
		srv, admin, runner := onlineAccessServer(t, nil)
		id := srv.CreateUser(admin, "kid", "memberpass123", "member")
		mustGrantSources(t, srv, admin, id, onlineSlug)
		setCap(srv, admin, id)
		kid := srv.LoginAs("kid", "memberpass123")

		st, play, raw := playOn(t, srv, kid, onlineSlug, onlineItemMP4, canPlayMP4(nil))
		if st != http.StatusOK || play.Format != "hls" {
			t.Fatalf("a bitrate-capped Member's play = %d format %q; body: %s, want an encode", st, play.Format, raw)
		}
		runs := runner.argv()
		if len(runs) != 1 || !strings.Contains(strings.Join(runs[0], " "), "-maxrate 2000000") {
			t.Fatalf("ffmpeg runs = %v, want one held to -maxrate 2000000", runs)
		}
		// An uncapped User still gets the untouched relay.
		other := srv.CreateUser(admin, "free", "memberpass123", "member")
		mustGrantSources(t, srv, admin, other, onlineSlug)
		if st, play, raw = playOn(t, srv, srv.LoginAs("free", "memberpass123"), onlineSlug, onlineItemMP4, canPlayMP4(nil)); st != http.StatusOK || play.Format != "progressive" {
			t.Fatalf("an uncapped Member's play = %d format %q; body: %s, want the relay", st, play.Format, raw)
		}
	})

	t.Run("without ffmpeg it is refused", func(t *testing.T) {
		t.Parallel()
		srv, admin, _ := onlineAccessServer(t, nil, testharness.WithFFmpegAvailability(false))
		id := srv.CreateUser(admin, "kid", "memberpass123", "member")
		mustGrantSources(t, srv, admin, id, onlineSlug)
		setCap(srv, admin, id)
		kid := srv.LoginAs("kid", "memberpass123")

		var env errorEnvelope
		st, _ := srv.JSON(http.MethodPost, onlineBase+"/"+onlineSlug+"/items/"+onlineItemMP4+"/playback", kid, canPlayMP4(nil), &env)
		if st != http.StatusNotImplemented || env.Error.Code != "TRANSCODE_REQUIRED" || env.Error.Details["reason"] != "bitrate" {
			t.Fatalf("a bitrate-capped play without ffmpeg = %d %+v, want 501 TRANSCODE_REQUIRED (bitrate)", st, env.Error)
		}
	})
}

// TestTheUsersPlaybackCeilingIsAppliedToTheOnlinePlaybackStart: the Member's own
// resolution ceiling holds even when the client asks for more or for nothing. A play
// that skipped the clamp would relay the 1080p variant untouched.
func TestTheUsersPlaybackCeilingIsAppliedToTheOnlinePlaybackStart(t *testing.T) {
	t.Parallel()
	srv, admin, runner := onlineAccessServer(t, nil)
	id := srv.CreateUser(admin, "kid", "memberpass123", "member")
	mustGrantSources(t, srv, admin, id, onlineSlug)
	if st, body := srv.JSON(http.MethodPut, "/api/v1/users/"+id+"/playbackCeiling", admin, map[string]any{"maxResolution": "720p"}, nil); st != http.StatusNoContent {
		t.Fatalf("setting the playback ceiling = %d %s", st, body)
	}
	kid := srv.LoginAs("kid", "memberpass123")

	for name, constraints := range map[string]map[string]any{
		"the client asks for nothing": nil,
		"the client asks for more":    {"maxResolution": "2160p"},
	} {
		st, play, raw := playOn(t, srv, kid, onlineSlug, "tall", canPlayMP4(constraints))
		if st != http.StatusOK || play.Format != "hls" {
			t.Fatalf("%s: a 1080p item under a 720p ceiling = %d format %q; body: %s, want an encode", name, st, play.Format, raw)
		}
	}
	for i, run := range runner.argv() {
		if !strings.Contains(strings.Join(run, " "), "min(720,ih)") {
			t.Errorf("run %d is not scaled to the ceiling: %v", i, run)
		}
	}
}
