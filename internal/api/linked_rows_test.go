package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The linked mark on the row shapes issue 07 missed (.scratch/linked-servers
// issue 14, ADR-0056 §1 and §6): a Home row and an Album row.
//
// The claim under test is the client's, not the server's: an Apple client
// deliberately refuses to infer "this is somebody else's" from the screen the
// viewer arrived through, so a Continue Watching card and an Album row have to
// say it themselves. Which means these tests ask the two questions a client
// asks — "is this a mirror" and "is that Server answering" — of the documents a
// client actually reads, and ask them of a LOCAL row beside each mirrored one,
// because a decoration bolted on unconditionally would pass without the control.

// unreachable takes the sharer away and drives the Link into its `unreachable`
// state through the one route that does it on demand (issue 08's sync). It is
// the state the `available: false` half of the pair exists for.
func (f *linkedFixture) unreachable(t *testing.T) {
	t.Helper()
	var links []linkResp
	if status, body := f.home.AuthGET("/api/v1/links", f.homeAdmin, &links); status != http.StatusOK || len(links) != 1 {
		t.Fatalf("GET /links = %d with %d links; body: %s", status, len(links), body)
	}
	f.sharer.Close()
	if status, body := f.home.JSON(http.MethodPost, "/api/v1/links/"+links[0].ID+"/sync",
		f.homeAdmin, nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("syncing a closed sharer = %d, want 503; body: %s", status, body)
	}
}

// linkedMark is the pair as a client reads it: whether the fields were there at
// all, and what they said.
type linkedMark struct {
	Linked       bool   `json:"linked"`
	Available    *bool  `json:"available"`
	LinkedServer string `json:"linkedServer"`
}

// wantMirrored asserts a row is badged as a mirror whose Server is (or is not)
// answering.
func wantMirrored(t *testing.T, what string, m linkedMark, available bool) {
	t.Helper()
	if !m.Linked {
		t.Errorf("%s: linked = false, want true (it is somebody else's row)", what)
		return
	}
	if m.Available == nil {
		t.Errorf("%s: available is absent on a mirrored row", what)
		return
	}
	if *m.Available != available {
		t.Errorf("%s: available = %v, want %v", what, *m.Available, available)
	}
}

func wantLocal(t *testing.T, what string, m linkedMark) {
	t.Helper()
	if m.Linked || m.Available != nil || m.LinkedServer != "" {
		t.Errorf("%s: a local row carried the linked fields (linked=%v available=%v linkedServer=%q)",
			what, m.Linked, m.Available, m.LinkedServer)
	}
}

// --- Home rows ----------------------------------------------------------------

type homeRowsResp struct {
	ContinueWatching []homeRowResp `json:"continueWatching"`
	UpNext           []homeRowResp `json:"upNext"`
	RecentlyAdded    []homeRowResp `json:"recentlyAdded"`
}

type homeRowResp struct {
	ID string `json:"id"`
	linkedMark
}

func homeRows(t *testing.T, srv *testharness.Server, token string) homeRowsResp {
	t.Helper()
	var out homeRowsResp
	if status, body := srv.AuthGET("/api/v1/home", token, &out); status != http.StatusOK {
		t.Fatalf("GET /home = %d; body: %s", status, body)
	}
	return out
}

func rowFor(t *testing.T, what string, rows []homeRowResp, id string) linkedMark {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r.linkedMark
		}
	}
	t.Fatalf("%s: %s is not in the row", what, id)
	return linkedMark{}
}

// TestHomeRowsCarryTheLinkedMark is the issue's first acceptance criterion. A
// mirrored Title in Continue Watching and in Recently Added says so; the local
// Title beside it in the same row says nothing; and when the friend's Server
// goes away the rows STAY, saying `available: false` (ADR-0056 §6 — badged
// unavailable, never dropped).
func TestHomeRowsCarryTheLinkedMark(t *testing.T) {
	f := linkFixtures(t, "movie")
	localLib := createMovieLibrary(t, f.home, f.homeAdmin, fixtureRoot(t))
	scanLib(t, f.home, f.homeAdmin, localLib, "")

	mirrored := firstMirroredTitleID(t, f)
	local := firstGridID(t, f.home, f.homeAdmin, localLib, "titles")
	setWatchStateResume(t, f.home, f.homeAdmin, mirrored, 60000)
	setWatchStateResume(t, f.home, f.homeAdmin, local, 60000)

	rows := homeRows(t, f.home, f.homeAdmin)
	wantMirrored(t, "continue watching (mirrored)", rowFor(t, "continue watching", rows.ContinueWatching, mirrored), true)
	wantLocal(t, "continue watching (local)", rowFor(t, "continue watching", rows.ContinueWatching, local))
	wantMirrored(t, "recently added (mirrored)", rowFor(t, "recently added", rows.RecentlyAdded, mirrored), true)
	wantLocal(t, "recently added (local)", rowFor(t, "recently added", rows.RecentlyAdded, local))

	f.unreachable(t)

	rows = homeRows(t, f.home, f.homeAdmin)
	wantMirrored(t, "continue watching, sharer away", rowFor(t, "continue watching", rows.ContinueWatching, mirrored), false)
	wantMirrored(t, "recently added, sharer away", rowFor(t, "recently added", rows.RecentlyAdded, mirrored), false)
	wantLocal(t, "continue watching (local), sharer away", rowFor(t, "continue watching", rows.ContinueWatching, local))
}

// TestLinkedRowsNameTheSharingServer is issue 18: the sharing Server's display
// name now rides the wire (`linkedServer`), not the Admin-only `GET /links`
// join, so a member-facing surface can name whose shelf a mirror is. It is on
// the linked libraryJSON and on a linked Home row, and it equals the name the
// home Server recorded on the Link. A local Library and a local Home row omit it.
func TestLinkedRowsNameTheSharingServer(t *testing.T) {
	f := linkFixtures(t, "movie")
	localLib := createMovieLibrary(t, f.home, f.homeAdmin, fixtureRoot(t))
	scanLib(t, f.home, f.homeAdmin, localLib, "")

	// The name of record: what the home Server stored for this Link.
	var links []linkResp
	if status, body := f.home.AuthGET("/api/v1/links", f.homeAdmin, &links); status != http.StatusOK || len(links) != 1 {
		t.Fatalf("GET /links = %d with %d links; body: %s", status, len(links), body)
	}
	want := links[0].ServerName
	if want == "" {
		t.Fatal("the fixture Link recorded no server name to assert against")
	}

	// libraryJSON: the mirror names its Server; the local shelf beside it omits it.
	type libRow struct {
		ID string `json:"id"`
		linkedMark
	}
	var libraries struct {
		Libraries []libRow `json:"libraries"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries", f.homeAdmin, &libraries); status != http.StatusOK {
		t.Fatalf("GET /libraries = %d; body: %s", status, body)
	}
	var sawMirror, sawLocal bool
	for _, l := range libraries.Libraries {
		switch l.ID {
		case f.mirror["movie"]:
			sawMirror = true
			wantMirrored(t, "linked libraryJSON", l.linkedMark, true)
			if l.LinkedServer != want {
				t.Errorf("linked libraryJSON linkedServer = %q, want %q", l.LinkedServer, want)
			}
		case localLib:
			sawLocal = true
			wantLocal(t, "local libraryJSON", l.linkedMark)
		}
	}
	if !sawMirror || !sawLocal {
		t.Fatalf("GET /libraries did not return both shelves (mirror=%v local=%v)", sawMirror, sawLocal)
	}

	// A Home row: the mirror names its Server there too.
	mirrored := firstMirroredTitleID(t, f)
	local := firstGridID(t, f.home, f.homeAdmin, localLib, "titles")
	setWatchStateResume(t, f.home, f.homeAdmin, mirrored, 60000)
	setWatchStateResume(t, f.home, f.homeAdmin, local, 60000)

	rows := homeRows(t, f.home, f.homeAdmin)
	if got := rowFor(t, "continue watching", rows.ContinueWatching, mirrored).LinkedServer; got != want {
		t.Errorf("linked Home row linkedServer = %q, want %q", got, want)
	}
	if got := rowFor(t, "continue watching", rows.ContinueWatching, local).LinkedServer; got != "" {
		t.Errorf("local Home row carried linkedServer = %q, want empty", got)
	}
}

// --- Album rows ---------------------------------------------------------------

type albumsRowsResp struct {
	Albums []struct {
		ID string `json:"id"`
		linkedMark
	} `json:"albums"`
}

type tracksRowsResp struct {
	Album struct {
		ID string `json:"id"`
		linkedMark
	} `json:"album"`
	Tracks []struct {
		ID string `json:"id"`
		linkedMark
	} `json:"tracks"`
}

// TestAlbumRowsCarryTheLinkedMark is the same criterion for music: the Album
// entries of GET /artists/{id}/albums, the `album` object of
// GET /albums/{id}/tracks, and the Track rows under it. A local music Library
// beside the mirror is the control, and an unreachable sharer flips `available`
// without taking one row away.
func TestAlbumRowsCarryTheLinkedMark(t *testing.T) {
	f := linkFixtures(t, "music")
	localLib := createMusicLibrary(t, f.home, f.homeAdmin, musicRoot(t))
	scanLib(t, f.home, f.homeAdmin, localLib, "")

	check := func(stage string, artistID string, mirrored bool, available bool) {
		t.Helper()
		var albums albumsRowsResp
		path := "/api/v1/artists/" + artistID + "/albums"
		if status, body := f.home.AuthGET(path, f.homeAdmin, &albums); status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", path, status, body)
		}
		if len(albums.Albums) == 0 {
			t.Fatalf("%s: the Artist listed no Albums", stage)
		}
		for _, a := range albums.Albums {
			if mirrored {
				wantMirrored(t, stage+": album row", a.linkedMark, available)
			} else {
				wantLocal(t, stage+": album row", a.linkedMark)
			}
		}

		var tracks tracksRowsResp
		path = "/api/v1/albums/" + albums.Albums[0].ID + "/tracks"
		if status, body := f.home.AuthGET(path, f.homeAdmin, &tracks); status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", path, status, body)
		}
		if len(tracks.Tracks) == 0 {
			t.Fatalf("%s: the Album listed no Tracks", stage)
		}
		if mirrored {
			wantMirrored(t, stage+": album detail", tracks.Album.linkedMark, available)
		} else {
			wantLocal(t, stage+": album detail", tracks.Album.linkedMark)
		}
		for _, tr := range tracks.Tracks {
			if mirrored {
				wantMirrored(t, stage+": track row", tr.linkedMark, available)
			} else {
				wantLocal(t, stage+": track row", tr.linkedMark)
			}
		}
	}

	mirroredArtist := firstMirroredID(t, f, "music", "artists")
	localArtist := firstGridID(t, f.home, f.homeAdmin, localLib, "artists")
	check("connected", mirroredArtist, true, true)
	check("connected", localArtist, false, false)

	f.unreachable(t)

	check("sharer away", mirroredArtist, true, false)
	check("sharer away", localArtist, false, false)
}

// TestAMirroredAlbumIsBadgedInSearch: an Album reaches a client from three
// places and the search group is the one with no Artist beside it to inherit the
// mark from, which is why store.Album learned its Library.
func TestAMirroredAlbumIsBadgedInSearch(t *testing.T) {
	f := linkFixtures(t, "music")

	var albums albumsRowsResp
	path := "/api/v1/artists/" + firstMirroredID(t, f, "music", "artists") + "/albums"
	if status, body := f.home.AuthGET(path, f.homeAdmin, &albums); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", path, status, body)
	}
	var titled struct {
		Albums []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"albums"`
	}
	f.home.AuthGET(path, f.homeAdmin, &titled)
	if len(titled.Albums) == 0 {
		t.Fatal("the mirrored Artist has no Albums")
	}

	var found bool
	var search struct {
		Albums []struct {
			ID string `json:"id"`
			linkedMark
		} `json:"albums"`
	}
	q := "/api/v1/search?q=" + url.QueryEscape(titled.Albums[0].Title)
	if status, body := f.home.AuthGET(q, f.homeAdmin, &search); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", q, status, body)
	}
	for _, a := range search.Albums {
		if a.ID == titled.Albums[0].ID {
			found = true
			wantMirrored(t, "search album hit", a.linkedMark, true)
		}
	}
	if !found {
		t.Errorf("searching for %q did not find the mirrored Album", titled.Albums[0].Title)
	}
}

// TestSeasonEpisodeRowsCarryTheLinkedMark is the audit's one other gap (issue 14
// Comments): GET /seasons/{id}/episodes is the only browse document in which
// nothing else could carry the mark — a Season has no Library of its own and the
// marked Show is a screen back — so the Episode rows carry it.
func TestSeasonEpisodeRowsCarryTheLinkedMark(t *testing.T) {
	f := linkFixtures(t, "tv")

	showID := firstMirroredID(t, f, "tv", "shows")
	var seasons struct {
		Seasons []struct {
			ID string `json:"id"`
		} `json:"seasons"`
	}
	if status, body := f.home.AuthGET("/api/v1/shows/"+showID+"/seasons", f.homeAdmin, &seasons); status != http.StatusOK {
		t.Fatalf("GET /shows/{id}/seasons = %d; body: %s", status, body)
	}
	if len(seasons.Seasons) == 0 {
		t.Fatal("the mirrored Show has no Seasons")
	}

	episodes := func() []struct {
		ID string `json:"id"`
		linkedMark
	} {
		t.Helper()
		var out struct {
			Episodes []struct {
				ID string `json:"id"`
				linkedMark
			} `json:"episodes"`
		}
		path := "/api/v1/seasons/" + seasons.Seasons[0].ID + "/episodes"
		if status, body := f.home.AuthGET(path, f.homeAdmin, &out); status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", path, status, body)
		}
		if len(out.Episodes) == 0 {
			t.Fatal("the mirrored Season has no Episodes")
		}
		return out.Episodes
	}

	for _, e := range episodes() {
		wantMirrored(t, "episode row", e.linkedMark, true)
	}

	f.unreachable(t)

	for _, e := range episodes() {
		wantMirrored(t, "episode row, sharer away", e.linkedMark, false)
	}
}

// --- the wire a household that never linked sends ------------------------------

// TestNeverLinkedRowsCarryNoMark is the issue's second acceptance criterion, and
// the reason both fields are omitempty and `available` is a pointer: a Server
// with no Links sends byte-for-byte the documents it always sent. Asserted as
// field ABSENCE over the whole decoded document, not as a false value, because
// `"linked": false` on every row of every Home payload would itself be a wire
// change.
func TestNeverLinkedRowsCarryNoMark(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	movieLib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, movieLib, "")
	musicLib := createMusicLibrary(t, srv, admin, musicRoot(t))
	scanLib(t, srv, admin, musicLib, "")
	tvLib := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, tvLib, "")

	setWatchStateResume(t, srv, admin, firstGridID(t, srv, admin, movieLib, "titles"), 60000)

	artistID := firstGridID(t, srv, admin, musicLib, "artists")
	var albums albumsRowsResp
	if status, body := srv.AuthGET("/api/v1/artists/"+artistID+"/albums", admin, &albums); status != http.StatusOK {
		t.Fatalf("GET /artists/{id}/albums = %d; body: %s", status, body)
	}
	if len(albums.Albums) == 0 {
		t.Fatal("the music fixture produced no Albums")
	}

	showID := firstGridID(t, srv, admin, tvLib, "shows")
	var seasons struct {
		Seasons []struct {
			ID string `json:"id"`
		} `json:"seasons"`
	}
	if status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", admin, &seasons); status != http.StatusOK {
		t.Fatalf("GET /shows/{id}/seasons = %d; body: %s", status, body)
	}
	if len(seasons.Seasons) == 0 {
		t.Fatal("the tv fixture produced no Seasons")
	}

	for _, path := range []string{
		"/api/v1/home",
		"/api/v1/artists/" + artistID + "/albums",
		"/api/v1/albums/" + albums.Albums[0].ID + "/tracks",
		"/api/v1/shows/" + showID + "/seasons",
		"/api/v1/seasons/" + seasons.Seasons[0].ID + "/episodes",
		"/api/v1/libraries/" + movieLib + "/titles",
		"/api/v1/libraries/" + musicLib + "/titles",
		"/api/v1/libraries/" + tvLib + "/titles",
	} {
		var raw json.RawMessage
		if status, body := srv.AuthGET(path, admin, &raw); status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", path, status, body)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if key := findKey(doc, "linked", "available", "linkedServer"); key != "" {
			t.Errorf("GET %s carried %q on a Server that has never linked: %s", path, key, raw)
		}
	}
}

// findKey returns the first of the named keys found anywhere in a decoded JSON
// document, or "".
func findKey(v any, names ...string) string {
	switch t := v.(type) {
	case map[string]any:
		for _, n := range names {
			if _, ok := t[n]; ok {
				return n
			}
		}
		for _, val := range t {
			if got := findKey(val, names...); got != "" {
				return got
			}
		}
	case []any:
		for _, val := range t {
			if got := findKey(val, names...); got != "" {
				return got
			}
		}
	}
	return ""
}
