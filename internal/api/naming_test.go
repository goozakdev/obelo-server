package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue-05 integration tests: the full Movie naming convention asserted through
// the browse + attention API. Fixtures live under testdata/naming/ in canonical
// layout (a separate root from the issue-04 testdata/movies/ tree, so the core
// three-Title regression is untouched). ensureNamingFixtures regenerates any
// missing clip/poster with ffmpeg; if ffmpeg is absent the tests skip.
//
// What the tree exercises (one Library, one root):
//   - Edition Movie (2010): "- 1080p.mkv" + "- 2160p.mkv"  → 2 Editions
//   - Cut Movie (2011): "{edition-Director's Cut}.mp4"      → named Edition
//   - Split Movie (2012): "- part1" + "- part2"             → 1 Edition, 2 Files
//   - Extras Movie (2013): main + "-trailer" + Trailers/teaser + sample(junk)
//                          + poster.jpg + fanart.jpg        → extras hidden,
//                                                             junk ignored, art
//   - Pinned Movie (2014) {tmdb-12345}: main                → embedded id
//   - Yearless Movie: main (no year)                        → needs-review
//   - 1080p.mkv (bare file, name is only a quality token)   → Unmatched

const namingRootRel = "naming"

// namingFixtureClip is a video clip to (re)generate under the naming root.
type namingFixtureClip struct {
	relPath string
	size    string
}

// namingPoster is an image fixture (solid-color still).
type namingPoster struct {
	relPath string
	color   string
}

var namingClips = []namingFixtureClip{
	{filepath.Join("Edition Movie (2010)", "Edition Movie (2010) - 1080p.mkv"), "192x108"},
	{filepath.Join("Edition Movie (2010)", "Edition Movie (2010) - 2160p.mkv"), "256x144"},
	{filepath.Join("Cut Movie (2011)", "Cut Movie (2011) {edition-Director's Cut}.mp4"), "160x120"},
	{filepath.Join("Split Movie (2012)", "Split Movie (2012) - part1.mp4"), "160x120"},
	{filepath.Join("Split Movie (2012)", "Split Movie (2012) - part2.mp4"), "160x120"},
	{filepath.Join("Extras Movie (2013)", "Extras Movie (2013).mp4"), "160x120"},
	{filepath.Join("Extras Movie (2013)", "Extras Movie (2013)-trailer.mp4"), "160x120"},
	{filepath.Join("Extras Movie (2013)", "Trailers", "teaser.mp4"), "160x120"},
	{filepath.Join("Extras Movie (2013)", "sample.mp4"), "160x120"},
	{filepath.Join("Pinned Movie (2014) {tmdb-12345}", "Pinned Movie (2014).mp4"), "160x120"},
	{filepath.Join("Yearless Movie", "Yearless Movie.mp4"), "160x120"},
	// A bare yearless movie dropped LOOSE at the root (no folder): parseable
	// identity but no year → a needs-review Title whose fix-match anchor is the
	// FILE itself, not the shared root.
	{filepath.Join("Loose Yearless.mp4"), "160x120"},
	// A bare recognized file whose name is only a quality token carries no
	// minimal identity → Unmatched (never auto-guessed into a Title).
	{filepath.Join("1080p.mkv"), "160x120"},
}

var namingPosters = []namingPoster{
	{filepath.Join("Extras Movie (2013)", "poster.jpg"), "blue"},
	{filepath.Join("Extras Movie (2013)", "fanart.jpg"), "red"},
}

var namingFixturesAvailable bool

func requireNamingFixtures(t *testing.T) {
	t.Helper()
	if !namingFixturesAvailable {
		t.Skip("naming fixtures unavailable (ffmpeg not on PATH)")
	}
}

// ensureNamingFixtures is invoked lazily (not in TestMain) so issue-04 fixture
// handling is untouched. It generates any missing clip/poster.
func ensureNamingFixtures() bool {
	root := filepath.Join("testdata", namingRootRel)
	for _, c := range namingClips {
		out := filepath.Join(root, c.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		if !generateNamingClip(c, out) {
			return false
		}
	}
	for _, p := range namingPosters {
		out := filepath.Join(root, p.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		if !generatePoster(p, out) {
			return false
		}
	}
	return true
}

func generateNamingClip(c namingFixtureClip, out string) bool {
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size="+c.size+":rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", out)
	return cmd.Run() == nil
}

func generatePoster(p namingPoster, out string) bool {
	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c="+p.color+":s=64x96",
		"-frames:v", "1", out)
	return cmd.Run() == nil
}

func namingRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", namingRootRel))
	if err != nil {
		t.Fatalf("resolving naming root: %v", err)
	}
	return abs
}

// scanNamingLibrary boots a server, creates a Movie Library at the naming root,
// scans it, and returns the server, admin token, and library id.
func scanNamingLibrary(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

// listAllTitles fetches every Title summary (one page; the fixture set is small).
func listAllTitles(t *testing.T, srv *testharness.Server, token, libID string) titlesListResp {
	t.Helper()
	var list titlesListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", status, body)
	}
	return list
}

func getDetail(t *testing.T, srv *testharness.Server, token, id string) namingTitleDetailResp {
	t.Helper()
	var d namingTitleDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	return d
}

// --- richer wire shapes (issue-05 fields) ----------------------------------

type namingExtraResp struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	DurationMs int64  `json:"durationMs"`
}

type namingArtworkResp struct {
	Role string `json:"role"`
	URL  string `json:"url"`
	Path string `json:"path"`
}

type namingTitleDetailResp struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	Year        int                 `json:"year"`
	NeedsReview bool                `json:"needsReview"`
	Ambiguous   bool                `json:"ambiguous"`
	TMDBID      string              `json:"tmdbId"`
	IMDBID      string              `json:"imdbId"`
	Editions    []editionResp       `json:"editions"`
	Extras      []namingExtraResp   `json:"extras"`
	Artwork     []namingArtworkResp `json:"artwork"`
}

type unmatchedFileResp struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type unmatchedListResp struct {
	Files []unmatchedFileResp `json:"files"`
}

// TestMainSetsNamingFixtures runs first (alphabetically) to populate the package
// flag; Go runs tests in source order within a file but TestMain in catalog_test
// owns process setup, so we lazily ensure here via init-style helper called by
// each test. We set the flag once in this no-op test for clarity.
func init() {
	namingFixturesAvailable = ensureNamingFixtures()
}

// TestNamingTwoEditions: distinct quality tokens in one folder → two Editions.
func TestNamingTwoEditions(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	id := findNamingTitle(t, list, "Edition Movie")
	d := getDetail(t, srv, token, id)
	if len(d.Editions) != 2 {
		t.Fatalf("Edition Movie editions = %d, want 2", len(d.Editions))
	}
	names := map[string]bool{}
	for _, e := range d.Editions {
		names[e.Name] = true
		if len(e.Files) != 1 {
			t.Errorf("edition %q files = %d, want 1", e.Name, len(e.Files))
		}
	}
	if !names["1080p"] || !names["2160p"] {
		t.Errorf("edition names = %v, want 1080p + 2160p", names)
	}
	if d.Ambiguous {
		t.Errorf("two distinct-quality editions should NOT be ambiguous")
	}
}

// TestNamingNamedEdition: an explicit {edition-…} tag → a named Edition.
func TestNamingNamedEdition(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	id := findNamingTitle(t, list, "Cut Movie")
	d := getDetail(t, srv, token, id)
	if len(d.Editions) != 1 {
		t.Fatalf("Cut Movie editions = %d, want 1", len(d.Editions))
	}
	if d.Editions[0].Name != "Director's Cut" {
		t.Errorf("edition name = %q, want Director's Cut", d.Editions[0].Name)
	}
}

// TestNamingMultiPart: part1/part2 join into one Edition with two Files.
func TestNamingMultiPart(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	id := findNamingTitle(t, list, "Split Movie")
	d := getDetail(t, srv, token, id)
	if len(d.Editions) != 1 {
		t.Fatalf("Split Movie editions = %d, want 1 (parts join); detail: %+v", len(d.Editions), d)
	}
	if len(d.Editions[0].Files) != 2 {
		t.Fatalf("Split Movie files = %d, want 2", len(d.Editions[0].Files))
	}
	if d.Ambiguous {
		t.Errorf("multi-part should NOT be ambiguous")
	}
}

// TestNamingExtrasAndArtworkAndJunk: extras attach + hidden from list; junk
// ignored; poster/fanart associated and servable.
func TestNamingExtrasAndArtworkAndJunk(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	// "teaser" / "trailer" / "sample" never appear as Titles.
	for _, ts := range list.Titles {
		switch ts.Title {
		case "teaser", "trailer", "sample", "Trailers":
			t.Errorf("extra/junk %q leaked into the titles list", ts.Title)
		}
	}

	id := findNamingTitle(t, list, "Extras Movie")
	d := getDetail(t, srv, token, id)

	// One main Edition with one File (the sample.mp4 junk is ignored).
	if len(d.Editions) != 1 || len(d.Editions[0].Files) != 1 {
		t.Fatalf("Extras Movie editions/files = %d/%v, want 1 edition / 1 file",
			len(d.Editions), d.Editions)
	}

	// Two extras: the -trailer suffix file and Trailers/teaser.
	if len(d.Extras) != 2 {
		t.Fatalf("Extras Movie extras = %d, want 2; extras: %+v", len(d.Extras), d.Extras)
	}
	var trailerCount int
	for _, ex := range d.Extras {
		if ex.Type == "trailer" {
			trailerCount++
		}
	}
	if trailerCount != 2 {
		t.Errorf("trailer-type extras = %d, want 2 (suffix + Trailers/ folder)", trailerCount)
	}

	// Artwork: poster + background, each servable.
	roles := map[string]string{}
	for _, a := range d.Artwork {
		roles[a.Role] = a.URL
	}
	if roles["poster"] == "" || roles["background"] == "" {
		t.Fatalf("artwork roles = %+v, want poster + background", roles)
	}
	// The artwork-serving endpoint returns the local image bytes (auth required).
	status, _ := srv.AuthGET(roles["poster"], token, nil)
	if status != http.StatusOK {
		t.Errorf("GET poster artwork = %d, want 200", status)
	}
}

// TestNamingEmbeddedID: a {tmdb-…} folder records the id on the Title.
func TestNamingEmbeddedID(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	id := findNamingTitle(t, list, "Pinned Movie")
	d := getDetail(t, srv, token, id)
	if d.TMDBID != "12345" {
		t.Errorf("Pinned Movie tmdbId = %q, want 12345", d.TMDBID)
	}
	if d.Year != 2014 {
		t.Errorf("Pinned Movie year = %d, want 2014", d.Year)
	}
}

// TestNamingNeedsReview: a yearless movie is filed, browsable, and flagged.
func TestNamingNeedsReview(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	var found *titleSummaryResp
	for i := range list.Titles {
		if list.Titles[i].Title == "Yearless Movie" {
			found = &list.Titles[i]
		}
	}
	if found == nil {
		t.Fatalf("Yearless Movie not in the titles list (it must be filed + browsable)")
	}
	if !found.NeedsReview {
		t.Errorf("Yearless Movie needsReview = false, want true")
	}
	if found.Year != 0 {
		t.Errorf("Yearless Movie year = %d, want 0", found.Year)
	}
}

// TestNamingUnmatched: a recognized media file with no extractable identity
// appears in the Unmatched list and is NOT a browsable Title.
func TestNamingUnmatched(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	// The quality-token-only file is never a browsable Title.
	for _, ts := range list.Titles {
		if ts.Title == "1080p" {
			t.Errorf("unmatched file leaked into titles as %q", ts.Title)
		}
	}

	var um unmatchedListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/unmatched", token, &um)
	if status != http.StatusOK {
		t.Fatalf("unmatched status = %d, want 200; body: %s", status, body)
	}
	var found bool
	for _, f := range um.Files {
		if filepath.Base(f.Path) == "1080p.mkv" {
			found = true
		}
	}
	if !found {
		t.Errorf("1080p.mkv not in Unmatched list; files: %+v", um.Files)
	}
}

// TestNamingUnmatchedRequiresAdmin: the Unmatched attention surface is Admin-only.
func TestNamingUnmatchedRequiresAdmin(t *testing.T) {
	requireNamingFixtures(t)
	srv, _, libID := scanNamingLibrary(t)

	srv.CreateMember("member2", "memberpass123")
	memberToken := login(t, srv, "member2", "memberpass123", "Phone", "ios", "member2-client").Token
	status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/unmatched", memberToken, nil)
	if status != http.StatusForbidden {
		t.Errorf("member unmatched status = %d, want 403", status)
	}
}

// TestNamingCleanMovieStaysOneEdition is the issue-04 regression at the unit of
// this test root: each clean single-file movie (Cut/Pinned/Yearless) yields
// exactly one Edition.
func TestNamingCleanMovieStaysOneEdition(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)
	list := listAllTitles(t, srv, token, libID)

	for _, name := range []string{"Pinned Movie", "Yearless Movie"} {
		id := findNamingTitle(t, list, name)
		d := getDetail(t, srv, token, id)
		if len(d.Editions) != 1 {
			t.Errorf("%s editions = %d, want 1", name, len(d.Editions))
		}
		if len(d.Editions[0].Files) != 1 {
			t.Errorf("%s files = %d, want 1", name, len(d.Editions[0].Files))
		}
	}
}

// findNamingTitle returns the id of the Title with the given display title.
func findNamingTitle(t *testing.T, list titlesListResp, title string) string {
	t.Helper()
	for _, ts := range list.Titles {
		if ts.Title == title {
			return ts.ID
		}
	}
	t.Fatalf("title %q not found in list (titles: %v)", title, titleNames(list))
	return ""
}

func titleNames(list titlesListResp) []string {
	var out []string
	for _, ts := range list.Titles {
		out = append(out, ts.Title)
	}
	return out
}
