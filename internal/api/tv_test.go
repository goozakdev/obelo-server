package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box integration tests for the TV vertical slice (issue tv-music/01):
// generate a Show/Season/Episode folder tree, scan it via a TV Library, then
// assert the Show → Season → Episode hierarchy + identities + access control
// through the browse API — the primary (highest) test seam (PRD testing
// contract). Fixtures are generated lazily with ffmpeg under testdata/tv/; if
// ffmpeg is absent the tests skip (as the Movie integration tests do).
//
// The tree exercises every TV grammar branch:
//   The Bear (2022)/Season 01/ - S01E01 / S01E02          → canonical SxxExx
//   The Bear (2022)/Season 00/ - S00E01 (Specials)        → Season 00 = Specials
//   The Bear (2022)/Specials/  - "Specials" alias         → also season 0
//   Double Show (2020)/Season 01/ - S01E05-E06            → one File → TWO Episodes
//   The Daily (2024)/Season 01/ - 2024-01-15              → date-based (raw token)
//   Anime Show/Season 01/ - 135                           → absolute number (raw token)

const tvRootRel = "tv"

type tvClip struct {
	relPath string
	size    string
}

var tvClips = []tvClip{
	{filepath.Join("The Bear (2022)", "Season 01", "The Bear (2022) - S01E01 - System.mkv"), "160x120"},
	{filepath.Join("The Bear (2022)", "Season 01", "The Bear (2022) - S01E02 - Hands.mkv"), "160x120"},
	{filepath.Join("The Bear (2022)", "Season 00", "The Bear (2022) - S00E01 - Special.mkv"), "160x120"},
	{filepath.Join("Double Show (2020)", "Season 01", "Double Show (2020) - S01E05-E06 - Twofer.mkv"), "160x120"},
	{filepath.Join("The Daily (2024)", "Season 01", "The Daily (2024) - 2024-01-15 - Guest.mkv"), "160x120"},
	{filepath.Join("Anime Show", "Season 01", "Anime Show - 135 - The Battle.mkv"), "160x120"},
}

var tvFixturesAvailable bool

func requireTVFixtures(t *testing.T) {
	t.Helper()
	if !tvFixturesAvailable {
		t.Skip("tv fixtures unavailable (ffmpeg not on PATH)")
	}
}

func init() {
	tvFixturesAvailable = ensureTVFixtures()
}

func ensureTVFixtures() bool {
	root := filepath.Join("testdata", tvRootRel)
	for _, c := range tvClips {
		out := filepath.Join(root, c.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		cmd := exec.Command("ffmpeg",
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=1:size="+c.size+":rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-shortest", out)
		if cmd.Run() != nil {
			return false
		}
	}
	return true
}

func tvRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", tvRootRel))
	if err != nil {
		t.Fatalf("resolving tv root: %v", err)
	}
	return abs
}

// --- wire shapes ------------------------------------------------------------

type showSummaryResp struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
	NeedsReview bool   `json:"needsReview"`
}

type showsListResp struct {
	Shows      []showSummaryResp `json:"shows"`
	NextCursor string            `json:"nextCursor"`
}

type seasonResp struct {
	ID           string `json:"id"`
	ShowID       string `json:"showId"`
	SeasonNumber int    `json:"seasonNumber"`
	Specials     bool   `json:"specials"`
	EpisodeCount int    `json:"episodeCount"`
}

type seasonsListResp struct {
	Show    showSummaryResp `json:"show"`
	Seasons []seasonResp    `json:"seasons"`
}

type episodeSummaryResp struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeLabel  string `json:"episodeLabel"`
	NeedsReview   bool   `json:"needsReview"`
	Watched       bool   `json:"watched"`
}

type episodesListResp struct {
	Season   seasonResp           `json:"season"`
	Episodes []episodeSummaryResp `json:"episodes"`
}

type episodeContextResp struct {
	ShowID        string `json:"showId"`
	ShowTitle     string `json:"showTitle"`
	ShowYear      int    `json:"showYear"`
	SeasonID      string `json:"seasonId"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeLabel  string `json:"episodeLabel"`
}

type episodeDetailResp struct {
	ID       string              `json:"id"`
	Kind     string              `json:"kind"`
	Title    string              `json:"title"`
	Editions []editionResp       `json:"editions"`
	Episode  *episodeContextResp `json:"episode"`
}

// createTVLibrary POSTs a TV Library at root and returns its id.
func createTVLibrary(t *testing.T, srv *testharness.Server, token, root string) string {
	t.Helper()
	_, lib, raw := createLibrary(t, srv, token, map[string]any{
		"name":        "Shows",
		"kind":        "tv",
		"rootFolders": []string{root},
	})
	if lib.ID == "" {
		t.Fatalf("tv library not created; body: %s", raw)
	}
	return lib.ID
}

// scanTVLibrary boots a server, creates+scans a TV Library, returns server/token/libID.
func scanTVLibrary(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

func listShows(t *testing.T, srv *testharness.Server, token, libID string) showsListResp {
	t.Helper()
	var list showsListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list shows status = %d, want 200; body: %s", status, body)
	}
	return list
}

func findShow(t *testing.T, list showsListResp, title string) string {
	t.Helper()
	for _, s := range list.Shows {
		if s.Title == title {
			return s.ID
		}
	}
	t.Fatalf("show %q not found; have: %+v", title, list.Shows)
	return ""
}

func showSeasons(t *testing.T, srv *testharness.Server, token, showID string) seasonsListResp {
	t.Helper()
	var resp seasonsListResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("seasons status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func seasonEpisodes(t *testing.T, srv *testharness.Server, token, seasonID string) episodesListResp {
	t.Helper()
	var resp episodesListResp
	status, body := srv.AuthGET("/api/v1/seasons/"+seasonID+"/episodes", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("episodes status = %d, want 200; body: %s", status, body)
	}
	return resp
}

// TestTVScanBuildsHierarchy is the central tracer bullet: scan the TV fixture
// tree and assert the Show → Season → Episode hierarchy via the browse API.
func TestTVScanBuildsHierarchy(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	shows := listShows(t, srv, token, libID)
	// Four shows: The Bear, Double Show, The Daily, Anime Show.
	if len(shows.Shows) != 4 {
		t.Fatalf("shows = %d, want 4; have: %+v", len(shows.Shows), shows.Shows)
	}
	for _, s := range shows.Shows {
		if s.Kind != "show" {
			t.Errorf("show %q kind = %q, want show", s.Title, s.Kind)
		}
	}

	// The Bear (2022): a regular Season 01 (2 episodes) + a Specials (Season 00).
	bearID := findShow(t, shows, "The Bear")
	bear := showSeasons(t, srv, token, bearID)
	if bear.Show.Year != 2022 {
		t.Errorf("The Bear year = %d, want 2022", bear.Show.Year)
	}
	seasonByNum := map[int]seasonResp{}
	for _, s := range bear.Seasons {
		seasonByNum[s.SeasonNumber] = s
	}
	s1, ok := seasonByNum[1]
	if !ok {
		t.Fatalf("The Bear missing Season 01; seasons: %+v", bear.Seasons)
	}
	if s1.EpisodeCount != 2 {
		t.Errorf("Season 01 episodeCount = %d, want 2", s1.EpisodeCount)
	}
	s0, ok := seasonByNum[0]
	if !ok {
		t.Fatalf("The Bear missing Season 00 (Specials); seasons: %+v", bear.Seasons)
	}
	if !s0.Specials {
		t.Errorf("Season 00 specials flag = false, want true")
	}

	// Season 01 episodes in order, canonical SxxExx identities.
	eps := seasonEpisodes(t, srv, token, s1.ID)
	if len(eps.Episodes) != 2 {
		t.Fatalf("Season 01 episodes = %d, want 2", len(eps.Episodes))
	}
	if eps.Episodes[0].EpisodeNumber != 1 || eps.Episodes[1].EpisodeNumber != 2 {
		t.Errorf("episode order = %d,%d, want 1,2", eps.Episodes[0].EpisodeNumber, eps.Episodes[1].EpisodeNumber)
	}
	if eps.Episodes[0].Title != "System" {
		t.Errorf("episode 1 title = %q, want System", eps.Episodes[0].Title)
	}
	for _, e := range eps.Episodes {
		if e.Kind != "episode" {
			t.Errorf("episode kind = %q, want episode", e.Kind)
		}
		if e.NeedsReview {
			t.Errorf("canonical episode %q should not need review", e.Title)
		}
	}
}

// TestTVMultiEpisodeFile: S01E05-E06 maps one File to TWO Episode Titles.
func TestTVMultiEpisodeFile(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	shows := listShows(t, srv, token, libID)
	id := findShow(t, shows, "Double Show")
	seasons := showSeasons(t, srv, token, id)
	if len(seasons.Seasons) != 1 {
		t.Fatalf("Double Show seasons = %d, want 1", len(seasons.Seasons))
	}
	eps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	if len(eps.Episodes) != 2 {
		t.Fatalf("multi-episode file should yield 2 Episodes, got %d: %+v", len(eps.Episodes), eps.Episodes)
	}
	if eps.Episodes[0].EpisodeNumber != 5 || eps.Episodes[1].EpisodeNumber != 6 {
		t.Errorf("episode numbers = %d,%d, want 5,6", eps.Episodes[0].EpisodeNumber, eps.Episodes[1].EpisodeNumber)
	}
	// Both Episodes reference the same single physical File (it plays once).
	d5 := getEpisodeDetail(t, srv, token, eps.Episodes[0].ID)
	d6 := getEpisodeDetail(t, srv, token, eps.Episodes[1].ID)
	p5 := d5.Editions[0].Files[0].Path
	p6 := d6.Editions[0].Files[0].Path
	if p5 != p6 {
		t.Errorf("multi-episode Titles reference different files: %q vs %q", p5, p6)
	}

	// Watching one marks BOTH watched (per the multi-episode rule).
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+eps.Episodes[0].ID+"/watchState",
		token, map[string]any{"watched": true}, nil)
	if status != http.StatusOK {
		t.Fatalf("watchState status = %d; body: %s", status, body)
	}
	eps2 := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	for _, e := range eps2.Episodes {
		if !e.Watched {
			t.Errorf("episode E%02d watched = false, want true (multi-episode marks both)", e.EpisodeNumber)
		}
	}
}

// TestTVDateAndAbsolute: date-based and absolute-numbered episodes are filed by
// their raw token, remain browsable/playable, and are labeled.
func TestTVDateAndAbsolute(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	shows := listShows(t, srv, token, libID)

	daily := showSeasons(t, srv, token, findShow(t, shows, "The Daily"))
	dEps := seasonEpisodes(t, srv, token, daily.Seasons[0].ID)
	if len(dEps.Episodes) != 1 {
		t.Fatalf("The Daily episodes = %d, want 1", len(dEps.Episodes))
	}
	if dEps.Episodes[0].EpisodeLabel != "2024-01-15" {
		t.Errorf("date episode label = %q, want 2024-01-15", dEps.Episodes[0].EpisodeLabel)
	}
	// Playable: it carries Editions/Files.
	dd := getEpisodeDetail(t, srv, token, dEps.Episodes[0].ID)
	if len(dd.Editions) == 0 || len(dd.Editions[0].Files) == 0 {
		t.Errorf("date episode has no playable file")
	}

	anime := showSeasons(t, srv, token, findShow(t, shows, "Anime Show"))
	aEps := seasonEpisodes(t, srv, token, anime.Seasons[0].ID)
	if len(aEps.Episodes) != 1 {
		t.Fatalf("Anime Show episodes = %d, want 1", len(aEps.Episodes))
	}
	if aEps.Episodes[0].EpisodeLabel != "Episode 135" {
		t.Errorf("absolute episode label = %q, want 'Episode 135'", aEps.Episodes[0].EpisodeLabel)
	}
}

// TestTVEpisodeDetailContext: an Episode's GET /titles/{id} carries its nested
// Editions plus the Show/Season/episode parent context.
func TestTVEpisodeDetailContext(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	shows := listShows(t, srv, token, libID)
	bear := showSeasons(t, srv, token, findShow(t, shows, "The Bear"))
	var s1 seasonResp
	for _, s := range bear.Seasons {
		if s.SeasonNumber == 1 {
			s1 = s
		}
	}
	eps := seasonEpisodes(t, srv, token, s1.ID)
	d := getEpisodeDetail(t, srv, token, eps.Episodes[0].ID)
	if d.Kind != "episode" {
		t.Errorf("detail kind = %q, want episode", d.Kind)
	}
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 {
		t.Fatalf("episode detail has no Editions/Files")
	}
	if d.Episode == nil {
		t.Fatalf("episode detail missing parent context")
	}
	if d.Episode.ShowTitle != "The Bear" || d.Episode.SeasonNumber != 1 || d.Episode.EpisodeNumber != 1 {
		t.Errorf("episode context = %+v, want The Bear S01E01", d.Episode)
	}
}

// TestTVAccessControl404: an unknown Show/Season returns 404, not 403
// (hide-existence, api-contract.md). The Episode /titles/{id} is unchanged
// (already 404 for unknown ids).
func TestTVAccessControl404(t *testing.T) {
	requireTVFixtures(t)
	srv, token, _ := scanTVLibrary(t)
	for _, path := range []string{
		"/api/v1/shows/no-such-show/seasons",
		"/api/v1/seasons/no-such-season/episodes",
	} {
		var env errorEnvelope
		status, body := srv.AuthGET(path, token, &env)
		if status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404; body: %s", path, status, body)
		}
		if env.Error.Code != "NOT_FOUND" {
			t.Errorf("GET %s code = %q, want NOT_FOUND", path, env.Error.Code)
		}
	}
}

// TestTVScanIdempotent: a rescan re-resolves to the same Shows/Seasons/Episodes.
func TestTVScanIdempotent(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	scanLib(t, srv, token, libID, "")
	shows := listShows(t, srv, token, libID)
	if len(shows.Shows) != 4 {
		t.Errorf("after rescan shows = %d, want 4 (no duplicates)", len(shows.Shows))
	}
	bear := showSeasons(t, srv, token, findShow(t, shows, "The Bear"))
	for _, s := range bear.Seasons {
		if s.SeasonNumber == 1 && s.EpisodeCount != 2 {
			t.Errorf("after rescan Season 01 episodeCount = %d, want 2", s.EpisodeCount)
		}
	}
}

// TestTVMovieLibraryUnaffected: a Movie library still returns Titles (not shows)
// from /libraries/{id}/titles — the additive TV branch did not regress Movies.
func TestTVMovieLibraryUnaffected(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	var list titlesListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list)
	if status != http.StatusOK {
		t.Fatalf("movie titles status = %d; body: %s", status, body)
	}
	if len(list.Titles) != 3 {
		t.Errorf("movie titles = %d, want 3 (movie path unchanged)", len(list.Titles))
	}
	for _, ts := range list.Titles {
		if ts.Kind != "movie" {
			t.Errorf("movie title %q kind = %q, want movie", ts.Title, ts.Kind)
		}
	}
}

func getEpisodeDetail(t *testing.T, srv *testharness.Server, token, id string) episodeDetailResp {
	t.Helper()
	var d episodeDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("episode detail status = %d, want 200; body: %s", status, body)
	}
	return d
}
