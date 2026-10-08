package api_test

import (
	"bytes"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Online items stay out of everything the catalog feeds (ADR-0068 decisions 5 and
// 6, .scratch/online-sources issue 07): they are visible to an Admin on the
// sessions page, emit no Event sink events, leave no per-User state, and appear in
// no search, Collection, Playlist, home row or linked-server feed.

type onlineSessionsView struct {
	OnlineSessions []struct {
		SessionID string `json:"sessionId"`
		Source    string `json:"source"`
		Title     string `json:"title"`
		Label     string `json:"label"`
		Mode      string `json:"mode"`
	} `json:"onlineSessions"`
}

func getOnlineSessions(t *testing.T, srv *testharness.Server, token string) onlineSessionsView {
	t.Helper()
	var view onlineSessionsView
	if st, body := srv.AuthGET(transcodingPath, token, &view); st != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", transcodingPath, st, body)
	}
	return view
}

// TestActiveOnlineSessionsAppearOnTheSessionsPageWithSourceAndTitle: a relayed play
// and an ffmpeg-encoded play each show as "{source} — {item title}", say which path
// carries them, and go when they end. The page is Admin-only like the rest of it.
func TestActiveOnlineSessionsAppearOnTheSessionsPageWithSourceAndTitle(t *testing.T) {
	t.Parallel()
	srv, admin, src, _ := onlineServer(t)
	baseRows := src.rows
	src.rows = func() []map[string]any {
		rows := baseRows()
		items := rows[0]["items"].([]map[string]any)
		rows[0]["items"] = append(items, map[string]any{
			"id": "split", "title": "Split clip", "thumbnailUrl": "https://127.0.0.1/thumb/split.png", "durationMs": 1000,
		})
		return rows
	}
	// A client names the item it plays from the page it just read, so the page is
	// what tells the Server the title.
	srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil)

	if got := getOnlineSessions(t, srv, admin); len(got.OnlineSessions) != 0 {
		t.Fatalf("sessions before any play = %+v, want none", got.OnlineSessions)
	}

	st, relay, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil))
	if st != http.StatusOK || relay.Format != "progressive" {
		t.Fatalf("relay start = %d %s", st, raw)
	}
	st, encoded, raw := playOnline(t, srv, admin, "split", canPlayMP4(nil))
	if st != http.StatusOK || encoded.Format != "hls" {
		t.Fatalf("ffmpeg start = %d %s", st, raw)
	}

	got := getOnlineSessions(t, srv, admin)
	if len(got.OnlineSessions) != 2 {
		t.Fatalf("sessions = %+v, want the relay and the ffmpeg play", got.OnlineSessions)
	}
	byID := map[string]int{}
	for i, s := range got.OnlineSessions {
		byID[s.SessionID] = i
	}
	for _, c := range []struct{ id, label, title, mode string }{
		{relay.SessionID, onlineName + " — A talk", "A talk", "relay"},
		{encoded.SessionID, onlineName + " — Split clip", "Split clip", "ffmpeg"},
	} {
		i, ok := byID[c.id]
		if !ok {
			t.Fatalf("session %s missing from %+v", c.id, got.OnlineSessions)
		}
		s := got.OnlineSessions[i]
		if s.Label != c.label || s.Source != onlineName || s.Title != c.title || s.Mode != c.mode {
			t.Errorf("session = %+v, want label %q title %q mode %q", s, c.label, c.title, c.mode)
		}
	}

	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+relay.SessionID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("ending the relay = %d", st)
	}
	if got = getOnlineSessions(t, srv, admin); len(got.OnlineSessions) != 1 || got.OnlineSessions[0].SessionID != encoded.SessionID {
		t.Fatalf("sessions after the relay ended = %+v, want only the ffmpeg play", got.OnlineSessions)
	}

	srv.CreateUser(admin, "kid", "memberpass123", "member")
	member := srv.LoginAs("kid", "memberpass123")
	if st, _ := srv.AuthGET(transcodingPath, member, nil); st != http.StatusForbidden && st != http.StatusNotFound {
		t.Fatalf("a Member reading the sessions page = %d, want it refused", st)
	}
}

// TestPlayingAnOnlineItemEmitsNoEventSinkEvents: a Webhook subscribed to the
// playback events hears nothing while an Online item is played to completion and
// stopped, by relay and by ffmpeg — and does hear a Title's play on the same
// Server, so the silence is the Online path's and not a dead sink's.
func TestPlayingAnOnlineItemEmitsNoEventSinkEvents(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	receiver := newSinkReceiver(t)
	srv, admin, _, media := onlineServer(t)
	list := scanFixtureLibrary(t, srv, admin)
	subscribeSink(t, srv, admin, receiver.srv.URL, "playback.started", "playback.stopped")

	for _, item := range []string{onlineItemMP4, "tall"} {
		// "tall" is 1080p under a 720p cap: ffmpeg encodes it.
		var constraints map[string]any
		if item == "tall" {
			constraints = map[string]any{"maxResolution": "720p"}
		}
		st, play, raw := playOnline(t, srv, admin, item, canPlayMP4(constraints))
		if st != http.StatusOK {
			t.Fatalf("%s: start = %d %s", item, st, raw)
		}
		if play.Format == "progressive" {
			if resp, b := getBytes(t, srv, play.StreamURL, nil); resp.StatusCode != http.StatusOK || !bytes.Equal(b, media.body) {
				t.Fatalf("%s: stream = %d", item, resp.StatusCode)
			}
		}
		if st, _ = srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
			map[string]any{"positionMs": 61000, "state": "playing"}, nil); st != http.StatusOK {
			t.Fatalf("%s: progress = %d", item, st)
		}
		if st, _ = srv.JSON(http.MethodDelete, "/api/v1/sessions/"+play.SessionID, admin, nil, nil); st != http.StatusNoContent {
			t.Fatalf("%s: end = %d", item, st)
		}
	}
	settleSinkDeliveries()
	if posts := receiver.received(); len(posts) != 0 {
		t.Fatalf("an Online play reached the Event sink as %d document(s): %s", len(posts), posts[0].Body)
	}

	// The control: the same sink hears a Title.
	var dec decisionResp
	if st, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+findTitle(t, list, "Dune")+"/playback",
		admin, mp4Profile(), &dec); st != http.StatusOK {
		t.Fatalf("title negotiate = %d %s", st, raw)
	}
	if docs := docsOfType(parseSinkDocs(t, receiver.waitForPosts(t, 1)), "playback.started"); len(docs) != 1 {
		t.Fatalf("the control Title play produced %d playback.started, want 1", len(docs))
	}
}

// TestAnOnlinePlayLeavesNoPerUserStateAndNoCatalogTrace: after playing to the end
// and stopping, no per-User table holds a row it did not before (watch state,
// resume, memory — anything keyed by user), and the item is in none of the home
// rows (Continue Watching, Up Next, Recently Added).
func TestAnOnlinePlayLeavesNoPerUserStateAndNoCatalogTrace(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv, admin, _, media := onlineServer(t)
	scanFixtureLibrary(t, srv, admin)
	srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil)
	before := srv.UserOwnedRowCounts()

	st, play, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil))
	if st != http.StatusOK {
		t.Fatalf("start = %d %s", st, raw)
	}
	if _, b := getBytes(t, srv, play.StreamURL, nil); !bytes.Equal(b, media.body) {
		t.Fatal("the whole file was not streamed")
	}
	srv.JSON(http.MethodPost, "/api/v1/sessions/"+play.SessionID+"/progress", admin,
		map[string]any{"positionMs": 61000, "state": "playing"}, nil)
	if st, _ = srv.JSON(http.MethodDelete, "/api/v1/sessions/"+play.SessionID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("end = %d", st)
	}

	after := srv.UserOwnedRowCounts()
	if _, ok := after["watch_state"]; !ok {
		t.Fatalf("the per-User snapshot does not cover watch_state: %v", after)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("per-User rows changed by an Online play: before %v, after %v", before, after)
	}

	home := mustGet(t, srv, "/api/v1/home", admin)
	if !bytes.Contains(home, []byte("Dune")) {
		t.Fatalf("home lacks the control Title, so its absence of the Online item proves nothing: %s", home)
	}
	for _, needle := range []string{"A talk", "Test Tube", onlineSlug, `"` + onlineItemMP4 + `"`, "online:"} {
		if bytes.Contains(home, []byte(needle)) {
			t.Errorf("home mentions %q after an Online play: %s", needle, home)
		}
	}
}

// TestOnlineItemsAreOutsideSearchCollectionsAndPlaylists: with the source's page
// read (so the Server knows the item), search finds neither the item nor the
// source, and an Online item id is refused by Collections, Playlists and the
// Watchlist — none of them gains a member.
func TestOnlineItemsAreOutsideSearchCollectionsAndPlaylists(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv, admin, _, _ := onlineServer(t)
	list := scanFixtureLibrary(t, srv, admin)
	srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil)

	for _, q := range []string{"A talk", "talk", "Test Tube", "Recently added"} {
		_, body := srv.AuthGET("/api/v1/search?q="+url.QueryEscape(q), admin, nil)
		for _, needle := range []string{"A talk", onlineSlug, "online:"} {
			if bytes.Contains(body, []byte(needle)) {
				t.Errorf("search %q mentions %q: %s", q, needle, body)
			}
		}
	}
	if r := search(t, srv, admin, "Dune"); !hasTitle(r.Movies, "Dune") {
		t.Fatalf("search control: Dune not found, so the sweep above proves little: %+v", r)
	}

	ids := []string{onlineItemMP4, "online:" + onlineSlug + ":" + onlineItemMP4}
	colID := createCollection(t, srv, admin, "Mixed", "")
	plID := createPlaylist(t, srv, admin, "Mixed")
	for _, id := range ids {
		if st, _ := srv.JSON(http.MethodPost, "/api/v1/collections/"+colID+"/items", admin,
			map[string]any{"titleIds": []string{id}}, nil); st < 400 || st >= 500 {
			t.Errorf("collection add %q = %d, want a 4xx refusal", id, st)
		}
		if st, _ := appendPlaylistItem(t, srv, admin, plID, id); st < 400 || st >= 500 {
			t.Errorf("playlist add %q = %d, want a 4xx refusal", id, st)
		}
		if st, _ := srv.JSON(http.MethodPost, "/api/v1/watchlist/items", admin,
			map[string]any{"titleId": id}, nil); st < 400 || st >= 500 {
			t.Errorf("watchlist add %q = %d, want a 4xx refusal", id, st)
		}
	}
	if c := getCollectionDetail(t, srv, admin, colID).MemberCount; c != 0 {
		t.Errorf("collection holds %d members after refused adds, want 0", c)
	}
	if n := len(playlistMemberIDs(getPlaylistDetail(t, srv, admin, plID))); n != 0 {
		t.Errorf("playlist holds %d members after refused adds, want 0", n)
	}
	// A real Title still goes in: the refusals are about the id, not the endpoint.
	addCollectionItems(t, srv, admin, colID, findTitle(t, list, "Dune"))
}

// TestALinkedServersFeedNeverReferencesOnlineSources: the Export a linked server
// pulls and mirrors carries nothing of a source or an Online item even after one is
// played, and the linked server's own token is a 404 on every source endpoint.
func TestALinkedServersFeedNeverReferencesOnlineSources(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv, admin, _, _ := onlineServer(t)
	libID := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, libID, "")
	srv.AuthGET(onlineBase+"/"+onlineSlug+"/rows", admin, nil)
	st, play, raw := playOnline(t, srv, admin, onlineItemMP4, canPlayMP4(nil))
	if st != http.StatusOK {
		t.Fatalf("start = %d %s", st, raw)
	}
	defer srv.JSON(http.MethodDelete, "/api/v1/sessions/"+play.SessionID, admin, nil, nil)

	peer := linkedServerToken(t, srv, admin, libID)
	entities, _, body := fullExport(t, srv, peer, libID, "")
	if len(entities) == 0 {
		t.Fatal("the export is empty, so its silence about sources proves nothing")
	}
	for _, needle := range []string{"A talk", "Test Tube", onlineSlug, "onlineSource", "online:"} {
		if strings.Contains(string(body), needle) {
			t.Errorf("the export mentions %q: %s", needle, body)
		}
	}
	for _, path := range []string{
		onlineBase,
		onlineBase + "/" + onlineSlug + "/rows",
		onlineBase + "/" + onlineSlug + "/items/" + onlineItemMP4 + "/thumbnail",
	} {
		st, body := srv.AuthGET(path, peer, nil)
		if path == onlineBase {
			if st != http.StatusOK || !bytes.Contains(body, []byte(`"sources":[]`)) {
				t.Errorf("peer GET %s = %d %s, want an empty list", path, st, body)
			}
		} else if st != http.StatusNotFound {
			t.Errorf("peer GET %s = %d, want 404", path, st)
		}
	}
}
