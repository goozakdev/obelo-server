package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Artwork for a mirrored Show / Artist / Album (.scratch/linked-servers issue 19,
// ADR-0056 §5). The byte relay already worked (issue 09); the bug was that a
// browse builder advertised an image URL only when a LOCAL entity_artwork row
// existed, and the mirror copied none — so the client was never told the image
// was there and the relay was never asked. The sharer's Export now carries
// `artworkRoles` + `artworkVersion`, the mirror lands them, and the read path
// unions them in so the decorators advertise a mirrored image on one code path.
//
// The claim is about the space between two machines, so this is two whole Servers
// over real HTTP with a RECORDER between them — relay_test.go's harness, widened
// to TV + music and with the artwork uploaded on the sharer BEFORE the link, so
// the very first pull carries the signal.

// artFixture is a sharer with scanned TV + music Libraries whose Show/Artist/Album
// carry uploaded artwork, granted to a `remote` User, and a home Server linked to
// it through a recorder.
type artFixture struct {
	sharer      *testharness.Server
	sharerAdmin string
	home        *testharness.Server
	homeAdmin   string
	remoteUser  string
	rec         *relayRecorder

	tvLib, musicLib, movieLib          string // sharer-side library ids
	mirrorTV, mirrorMusic, mirrorMovie string // home-side (mirror) library ids

	// The sharer-side ids and titles of the entities the test uploaded art to.
	showID, showTitle    string
	artistID, artistName string
	albumID, albumTitle  string
	movieID, movieTitle  string
	// The show whose Season 01 carries a local poster (issue 20), and that season's
	// number — the season artwork the mirror must advertise + relay.
	seasonShowTitle string
	seasonNumber    int
}

func linkArtworkFixture(t *testing.T) *artFixture {
	t.Helper()
	requireFixtures(t)

	f := &artFixture{rec: &relayRecorder{}}
	f.sharer = testharness.New(t)
	f.sharerAdmin = adminToken(t, f.sharer)

	// A MUTABLE tv tree so the seasons that carry a local `Season NN.jpg` poster
	// serve KNOWN bytes: "Double Show (2020)" already ships one, overwritten here so
	// assertRelayed can compare the sharer's exact image (issue 20 — Season art
	// folds into issue 19's entity_artwork machinery).
	tvDir := testharness.MutableLibraryDir(t, tvRoot(t))
	// The show FOLDER is "Double Show (2020)"; its parsed grid title drops the year.
	f.seasonShowTitle, f.seasonNumber = "Double Show", 1
	writeSeasonPoster(t, tvDir, "Double Show (2020)", "Season 01.jpg", pngImage("season-poster"))
	f.tvLib = createTVLibrary(t, f.sharer, f.sharerAdmin, tvDir)
	scanLib(t, f.sharer, f.sharerAdmin, f.tvLib, "")
	f.musicLib = createMusicLibrary(t, f.sharer, f.sharerAdmin, musicRoot(t))
	scanLib(t, f.sharer, f.sharerAdmin, f.musicLib, "")

	// A Movie library: its artwork lives in the `artwork` table (issue 20's extra
	// work), uploaded to the first movie before the link so the first pull carries
	// the poster/background/logo signal.
	f.movieLib = createMovieLibrary(t, f.sharer, f.sharerAdmin, fixtureRoot(t))
	scanLib(t, f.sharer, f.sharerAdmin, f.movieLib, "")
	f.movieID, f.movieTitle = firstRow(t, f.sharer, f.sharerAdmin, f.movieLib, "titles")
	uploadArtwork(t, f.sharer, f.sharerAdmin, "/api/v1/titles/"+f.movieID+"/artworkUpload?role=poster", "image/png", pngImage("movie-poster"), nil)
	uploadArtwork(t, f.sharer, f.sharerAdmin, "/api/v1/titles/"+f.movieID+"/artworkUpload?role=background", "image/png", pngImage("movie-background"), nil)
	uploadArtwork(t, f.sharer, f.sharerAdmin, "/api/v1/titles/"+f.movieID+"/artworkUpload?role=logo", "image/png", pngImage("movie-logo"), nil)

	// The Show gets a poster and a background but deliberately NO logo — a role the
	// sharer lacks, which the mirror must not advertise.
	f.showID, f.showTitle = firstRow(t, f.sharer, f.sharerAdmin, f.tvLib, "shows")
	uploadEntityArt(t, f.sharer, f.sharerAdmin, "shows", f.showID, "poster", pngImage("show-poster"))
	uploadEntityArt(t, f.sharer, f.sharerAdmin, "shows", f.showID, "background", pngImage("show-background"))

	f.artistID, f.artistName = firstRow(t, f.sharer, f.sharerAdmin, f.musicLib, "artists")
	uploadEntityArt(t, f.sharer, f.sharerAdmin, "artists", f.artistID, "poster", pngImage("artist-poster"))

	f.albumID, f.albumTitle = firstAlbum(t, f.sharer, f.sharerAdmin, f.artistID)
	uploadEntityArt(t, f.sharer, f.sharerAdmin, "albums", f.albumID, "cover", pngImage("album-cover"))

	f.remoteUser = createRemoteUser(t, f.sharer, f.sharerAdmin, "The other household")
	grantLibraries(t, f.sharer, f.sharerAdmin, f.remoteUser, f.tvLib, f.musicLib, f.movieLib)

	// The invite origin is the RECORDER, so every byte the home Server fetches is
	// inspectable — which is how "fetched once" becomes an assertion.
	proxy := httptest.NewServer(f.rec.wrap(f.sharer.Handler()))
	t.Cleanup(proxy.Close)

	f.home = testharness.New(t)
	f.homeAdmin = adminToken(t, f.home)
	invite := mintInvite(t, f.sharer, f.sharerAdmin, f.remoteUser, proxy.URL).Invite
	var l linkResp
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &l); status != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201; body: %s", status, body)
	}
	for _, lib := range l.Libraries {
		switch lib.Kind {
		case "tv":
			f.mirrorTV = lib.ID
		case "music":
			f.mirrorMusic = lib.ID
		case "movie":
			f.mirrorMovie = lib.ID
		}
	}
	if f.mirrorTV == "" || f.mirrorMusic == "" || f.mirrorMovie == "" {
		t.Fatalf("the link did not bring all libraries: %+v", l.Libraries)
	}
	return f
}

// TestRelayArtworkForMirroredShowArtistAlbum is the issue's acceptance: a mirrored
// Show advertises the poster/background URLs the sharer has (and NOT the logo it
// lacks), a mirrored Artist its image, a mirrored Album its cover — and GETting
// each URL relays the exact bytes from the sharer and serves them from cache
// afterwards, the sharer asked once.
func TestRelayArtworkForMirroredShowArtistAlbum(t *testing.T) {
	f := linkArtworkFixture(t)

	// --- Show -------------------------------------------------------------------
	show := mirroredRow(t, f.home, f.homeAdmin, f.mirrorTV, "shows", "title", f.showTitle)
	poster := str(show, "posterUrl")
	background := str(show, "backgroundUrl")
	if poster == "" || background == "" {
		t.Fatalf("mirrored show advertised no poster/background: posterUrl=%q backgroundUrl=%q", poster, background)
	}
	if logo := str(show, "logoUrl"); logo != "" {
		t.Errorf("mirrored show advertised a logoUrl=%q, but the sharer has no logo", logo)
	}
	if !strings.Contains(poster, "?v=") {
		t.Errorf("mirrored show poster carries no cache-bust version: %q", poster)
	}
	assertRelayedOnce(t, f, poster, pngImage("show-poster"), "/api/v1/shows/")
	assertRelayed(t, f, background, pngImage("show-background"))

	// --- Artist -----------------------------------------------------------------
	artist := mirroredRow(t, f.home, f.homeAdmin, f.mirrorMusic, "artists", "name", f.artistName)
	artURL := str(artist, "artworkUrl")
	if artURL == "" {
		t.Fatalf("mirrored artist advertised no artworkUrl")
	}
	if bg := str(artist, "backgroundUrl"); bg != "" {
		t.Errorf("mirrored artist advertised a backgroundUrl=%q the sharer lacks", bg)
	}
	assertRelayedOnce(t, f, artURL, pngImage("artist-poster"), "/api/v1/artists/")

	// --- Album ------------------------------------------------------------------
	// The mirrored artist on the HOME side, to reach its albums.
	mirroredArtistID := str(artist, "id")
	var albums map[string]any
	if st, body := f.home.AuthGET("/api/v1/artists/"+mirroredArtistID+"/albums", f.homeAdmin, &albums); st != http.StatusOK {
		t.Fatalf("GET mirrored artist albums = %d; body: %s", st, body)
	}
	album := findRow(t, rawList(albums, "albums"), "title", f.albumTitle)
	if has, _ := album["hasArtwork"].(bool); !has {
		t.Fatalf("mirrored album does not advertise hasArtwork: %+v", album)
	}
	if str(album, "artworkVersion") == "" {
		t.Errorf("mirrored album carries no artworkVersion")
	}
	// The album cover route is role-less: /albums/{id}/artwork.
	coverURL := "/api/v1/albums/" + str(album, "id") + "/artwork?v=" + str(album, "artworkVersion")
	assertRelayedOnce(t, f, coverURL, pngImage("album-cover"), "/api/v1/albums/")
}

// TestRelayArtworkVersionReflectsASharerChange is the cache-bust half: swapping
// the sharer's poster changes the advertised `?v=` token after a full pull, so a
// stale image reloads. (An artwork-only change does not move the entity's
// updated_at, so this is the FULL pull the initial link and a RESYNC also do; the
// incremental gap is issue 09 deviation 5's, unchanged.)
func TestRelayArtworkVersionReflectsASharerChange(t *testing.T) {
	f := linkArtworkFixture(t)

	show := mirroredRow(t, f.home, f.homeAdmin, f.mirrorTV, "shows", "title", f.showTitle)
	before := versionOf(str(show, "posterUrl"))
	if before == "" {
		t.Fatal("the mirrored show poster carried no version to change")
	}

	// The sharer's poster changes: a distinctly newer artwork timestamp.
	f.sharer.Exec(
		`UPDATE entity_artwork SET added_at = '2031-02-03 04:05:06'
		  WHERE entity_type = 'show' AND entity_id = ? AND role = 'poster'`, f.showID)
	forceFullPull(t, f.home, f.homeAdmin)

	show = mirroredRow(t, f.home, f.homeAdmin, f.mirrorTV, "shows", "title", f.showTitle)
	after := versionOf(str(show, "posterUrl"))
	if after == "" {
		t.Fatal("the mirrored show poster lost its version after the sharer's change")
	}
	if after == before {
		t.Errorf("the poster version did not change after the sharer swapped it: still %q", after)
	}
}

// TestRelayArtworkForMirroredMovie is issue 20's movie half: a mirrored Movie
// advertises its poster in the GRID (a non-empty artworkVersion, the signal that
// makes the client request the poster) and poster/background/logo on the DETAIL —
// and each URL relays the sharer's exact bytes and serves them from cache after,
// the sharer asked once. A Movie's artwork lives in the `artwork` table, so this
// exercises both read paths (ArtworkVersionsForTitles for the grid, the detail
// artwork[] for the hero).
func TestRelayArtworkForMirroredMovie(t *testing.T) {
	f := linkArtworkFixture(t)

	// --- Grid: the version signal that makes the client ask for the poster ------
	movie := mirroredRow(t, f.home, f.homeAdmin, f.mirrorMovie, "titles", "title", f.movieTitle)
	movieID := str(movie, "id")
	if str(movie, "artworkVersion") == "" {
		t.Fatalf("mirrored movie grid row carries no artworkVersion, so the client never asks for its poster: %+v", movie)
	}
	posterURL := "/api/v1/titles/" + movieID + "/artwork/poster"
	assertRelayedOnce(t, f, posterURL, pngImage("movie-poster"), "/api/v1/titles/")

	// --- Detail: the hero advertises every mirrored role ------------------------
	var detail map[string]any
	if st, body := f.home.AuthGET("/api/v1/titles/"+movieID, f.homeAdmin, &detail); st != http.StatusOK {
		t.Fatalf("GET mirrored movie detail = %d; body: %s", st, body)
	}
	art := artworkURLsByRole(detail)
	for _, role := range []string{"poster", "background", "logo"} {
		if art[role] == "" {
			t.Fatalf("mirrored movie detail did not advertise the %s the sharer has: %+v", role, detail["artwork"])
		}
	}
	assertRelayed(t, f, art["background"], pngImage("movie-background"))
	assertRelayed(t, f, art["logo"], pngImage("movie-logo"))
}

// TestRelayArtworkForMirroredSeason is issue 20's season half: a mirrored Season
// with a local poster on the sharer advertises a posterUrl, which relays the
// sharer's exact bytes. Seasons fold into issue 19's entity_artwork machinery, so
// this proves the fold works end to end.
func TestRelayArtworkForMirroredSeason(t *testing.T) {
	f := linkArtworkFixture(t)

	show := mirroredRow(t, f.home, f.homeAdmin, f.mirrorTV, "shows", "title", f.seasonShowTitle)
	var seasons map[string]any
	if st, body := f.home.AuthGET("/api/v1/shows/"+str(show, "id")+"/seasons", f.homeAdmin, &seasons); st != http.StatusOK {
		t.Fatalf("GET mirrored show seasons = %d; body: %s", st, body)
	}
	var poster string
	for _, s := range rawList(seasons, "seasons") {
		if int(numOf(s, "seasonNumber")) == f.seasonNumber {
			poster = str(s, "posterUrl")
		}
	}
	if poster == "" {
		t.Fatalf("mirrored season %d advertised no posterUrl: %+v", f.seasonNumber, rawList(seasons, "seasons"))
	}
	if !strings.Contains(poster, "?v=") {
		t.Errorf("mirrored season poster carries no cache-bust version: %q", poster)
	}
	assertRelayedOnce(t, f, poster, pngImage("season-poster"), "/api/v1/seasons/")
}

// --- helpers ------------------------------------------------------------------

// writeSeasonPoster overwrites (or creates) a `Season NN.jpg` poster in a show
// folder of a mutable library tree, so the sharer serves KNOWN bytes for it.
func writeSeasonPoster(t *testing.T, root, showFolder, name string, img []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, showFolder, name), img, 0o644); err != nil {
		t.Fatalf("writing season poster: %v", err)
	}
}

// artworkURLsByRole maps a Title detail's artwork[] to role -> url.
func artworkURLsByRole(detail map[string]any) map[string]string {
	out := map[string]string{}
	raw, _ := detail["artwork"].([]any)
	for _, a := range raw {
		if m, ok := a.(map[string]any); ok {
			out[str(m, "role")] = str(m, "url")
		}
	}
	return out
}

// numOf reads a JSON number field (they decode as float64).
func numOf(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// uploadEntityArt uploads one image to a Show/Artist/Album role on a Server.
func uploadEntityArt(t *testing.T, srv *testharness.Server, token, plural, id, role string, img []byte) {
	t.Helper()
	path := "/api/v1/" + plural + "/" + id + "/artworkUpload?role=" + role
	if st := uploadArtwork(t, srv, token, path, "image/png", img, nil); st != http.StatusOK {
		t.Fatalf("uploading %s %s artwork = %d, want 200", plural, role, st)
	}
}

// firstRow returns the id and display field (title for shows, name for artists) of
// the first grid row of a Library.
func firstRow(t *testing.T, srv *testharness.Server, token, libID, key string) (id, label string) {
	t.Helper()
	var raw map[string]any
	if st, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &raw); st != http.StatusOK {
		t.Fatalf("listing %s = %d; body: %s", key, st, body)
	}
	rows := rawList(raw, key)
	if len(rows) == 0 {
		t.Fatalf("%s has no %s", libID, key)
	}
	label = str(rows[0], "title")
	if label == "" {
		label = str(rows[0], "name")
	}
	return str(rows[0], "id"), label
}

// firstAlbum returns the id and title of an Artist's first Album.
func firstAlbum(t *testing.T, srv *testharness.Server, token, artistID string) (id, title string) {
	t.Helper()
	var raw map[string]any
	if st, body := srv.AuthGET("/api/v1/artists/"+artistID+"/albums", token, &raw); st != http.StatusOK {
		t.Fatalf("listing albums = %d; body: %s", st, body)
	}
	albums := rawList(raw, "albums")
	if len(albums) == 0 {
		t.Fatal("the artist has no albums")
	}
	return str(albums[0], "id"), str(albums[0], "title")
}

// mirroredRow finds a mirrored grid row by a matching field (the mirror copies
// titles verbatim, so the sharer's title finds the home Server's row).
func mirroredRow(t *testing.T, srv *testharness.Server, token, libID, key, field, want string) map[string]any {
	t.Helper()
	var raw map[string]any
	if st, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &raw); st != http.StatusOK {
		t.Fatalf("listing mirrored %s = %d; body: %s", key, st, body)
	}
	return findRow(t, rawList(raw, key), field, want)
}

func findRow(t *testing.T, rows []map[string]any, field, want string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if str(r, field) == want {
			return r
		}
	}
	t.Fatalf("no row with %s = %q among %d rows", field, want, len(rows))
	return nil
}

// versionOf returns the ?v= token of an artwork URL, or "".
func versionOf(rawURL string) string {
	_, v, ok := strings.Cut(rawURL, "?v=")
	if !ok {
		return ""
	}
	return v
}

// assertRelayed GETs an advertised artwork URL and asserts the sharer's exact
// bytes came through.
func assertRelayed(t *testing.T, f *artFixture, url string, want []byte) {
	t.Helper()
	resp := authStream(t, f.home, url, f.homeAdmin, "")
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("GET %s returned %d bytes, want the sharer's %d", url, len(got), len(want))
	}
}

// assertRelayedOnce is assertRelayed plus the fetch-once claim: a second GET is
// served from this Server's cache and the sharer is asked exactly once.
func assertRelayedOnce(t *testing.T, f *artFixture, url string, want []byte, sharerPrefix string) {
	t.Helper()
	before := f.rec.countPath(sharerPrefix)
	assertRelayed(t, f, url, want)
	afterFirst := f.rec.countPath(sharerPrefix)
	if afterFirst != before+1 {
		t.Errorf("GET %s hit the sharer %d times, want exactly one fetch", url, afterFirst-before)
	}
	assertRelayed(t, f, url, want)
	if now := f.rec.countPath(sharerPrefix); now != afterFirst {
		t.Errorf("a second GET %s hit the sharer again (%d→%d), want it served from cache",
			url, afterFirst, now)
	}
}

// forceFullPull clears the mirror's export checkpoint and syncs, which is a FULL
// pull — the path that carries an artwork-only change (which does not move an
// entity's updated_at, so an incremental pull would miss it).
func forceFullPull(t *testing.T, home *testharness.Server, admin string) {
	t.Helper()
	home.Exec(`UPDATE libraries SET remote_checkpoint = '' WHERE source = 'linked'`)
	var links []linkResp
	if st, body := home.AuthGET("/api/v1/links", admin, &links); st != http.StatusOK || len(links) == 0 {
		t.Fatalf("GET /links = %d with %d links; body: %s", st, len(links), body)
	}
	if st, raw := home.JSON(http.MethodPost, "/api/v1/links/"+links[0].ID+"/sync", admin, nil, nil); st != http.StatusOK {
		t.Fatalf("full pull sync = %d; body: %s", st, string(raw))
	}
}
