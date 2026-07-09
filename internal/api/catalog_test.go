package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Integration tests for the scan + browse tracer bullet: point a Library at the
// checked-in fixture tree, scan it, and assert the catalog via the browse API
// (PRD testing contract — observable HTTP behavior, real ffprobe).
//
// Fixtures live under testdata/movies/ in the canonical layout and are checked
// in. ensureFixtures (TestMain) regenerates any that are missing with ffmpeg, so
// a fresh checkout without the binaries still passes as long as ffmpeg is on
// PATH; if neither the files nor ffmpeg are present the integration test skips
// rather than fails.

// --- wire shapes -----------------------------------------------------------

type streamResp struct {
	Index     int    `json:"index"`
	Kind      string `json:"kind"`
	Codec     string `json:"codec"`
	Language  string `json:"language"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Channels  int    `json:"channels"`
	IsDefault bool   `json:"isDefault"`
}

type fileResp struct {
	ID         string       `json:"id"`
	Path       string       `json:"path"`
	Container  string       `json:"container"`
	VideoCodec string       `json:"videoCodec"`
	AudioCodec string       `json:"audioCodec"`
	Width      int          `json:"width"`
	Height     int          `json:"height"`
	Bitrate    int64        `json:"bitrate"`
	DurationMs int64        `json:"durationMs"`
	SizeBytes  int64        `json:"sizeBytes"`
	Streams    []streamResp `json:"streams"`
}

type editionResp struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Files []fileResp `json:"files"`
}

type titleSummaryResp struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
	NeedsReview bool   `json:"needsReview"`
	Ambiguous   bool   `json:"ambiguous"`
	TMDBID      string `json:"tmdbId"`
	IMDBID      string `json:"imdbId"`
	AddedAt     string `json:"addedAt"`
}

type titlesListResp struct {
	Titles     []titleSummaryResp `json:"titles"`
	NextCursor string             `json:"nextCursor"`
}

type titleDetailResp struct {
	ID       string        `json:"id"`
	Kind     string        `json:"kind"`
	Title    string        `json:"title"`
	Year     int           `json:"year"`
	AddedAt  string        `json:"addedAt"`
	Editions []editionResp `json:"editions"`
}

type scanStatusResp struct {
	LibraryID    string `json:"libraryId"`
	State        string `json:"state"`
	TitlesFound  int    `json:"titlesFound"`
	FilesFound   int    `json:"filesFound"`
	ErrorMessage string `json:"errorMessage"`
	StartedAt    string `json:"startedAt"`
	FinishedAt   string `json:"finishedAt"`
}

// scanFixtureDir is the checked-in fixture root, resolved absolute (the library
// service requires absolute root paths).
func fixtureRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "movies"))
	if err != nil {
		t.Fatalf("resolving fixture root: %v", err)
	}
	return abs
}

// createMovieLibrary POSTs a Movie Library pointing at root and returns its id.
func createMovieLibrary(t *testing.T, srv *testharness.Server, token, root string) string {
	t.Helper()
	_, lib, raw := createLibrary(t, srv, token, map[string]any{
		"name":        "Movies",
		"kind":        "movie",
		"rootFolders": []string{root},
	})
	if lib.ID == "" {
		t.Fatalf("library not created; body: %s", raw)
	}
	return lib.ID
}

// TestScanAndBrowse is the central tracer bullet: scan the fixture library, then
// assert the catalog through the browse API.
func TestScanAndBrowse(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// Trigger the scan (Admin). Asynchronous: 202 with state "running"; the scan
	// runs in the background and is observed via the pollable GET.
	var accepted scanStatusResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/scan", token, nil, &accepted)
	if status != http.StatusAccepted {
		t.Fatalf("scan trigger = %d, want 202; body: %s", status, body)
	}
	if accepted.State != "running" {
		t.Errorf("accepted state = %q, want running; body: %s", accepted.State, body)
	}

	// Pollable scan status settles to the completed scan.
	scanned := waitScanSettled(t, srv, token, libID)
	if scanned.State != "idle" {
		t.Errorf("post-scan state = %q, want idle", scanned.State)
	}
	// Three fixtures: Dune, Blade Runner, Sample Movie (bare-file fallback).
	if scanned.TitlesFound != 3 {
		t.Errorf("titlesFound = %d, want 3", scanned.TitlesFound)
	}
	if scanned.FilesFound != 3 {
		t.Errorf("filesFound = %d, want 3", scanned.FilesFound)
	}
	if scanned.FinishedAt == "" {
		t.Errorf("finishedAt empty after a completed scan")
	}

	// List titles: default sort (by title, A→Z).
	var list titlesListResp
	status, body = srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body: %s", status, body)
	}
	if len(list.Titles) != 3 {
		t.Fatalf("title count = %d, want 3; body: %s", len(list.Titles), body)
	}
	wantOrder := []string{"Blade Runner", "Dune", "Sample Movie"}
	for i, want := range wantOrder {
		if list.Titles[i].Title != want {
			t.Errorf("title[%d] = %q, want %q (sort=title)", i, list.Titles[i].Title, want)
		}
	}
	// Year identity parsed from the folder/bare-file name.
	years := map[string]int{"Blade Runner": 1982, "Dune": 2021, "Sample Movie": 2000}
	for _, ts := range list.Titles {
		if years[ts.Title] != ts.Year {
			t.Errorf("title %q year = %d, want %d", ts.Title, ts.Year, years[ts.Title])
		}
		if ts.Kind != "movie" {
			t.Errorf("title %q kind = %q, want movie", ts.Title, ts.Kind)
		}
	}

	// Get one Title: nested Editions → Files → Streams with ffprobed attributes.
	var dune titleDetailResp
	duneID := findTitle(t, list, "Dune")
	status, body = srv.AuthGET("/api/v1/titles/"+duneID, token, &dune)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	if dune.Title != "Dune" || dune.Year != 2021 {
		t.Errorf("Dune detail = %q (%d), want Dune (2021)", dune.Title, dune.Year)
	}
	if len(dune.Editions) != 1 {
		t.Fatalf("Dune editions = %d, want 1 (one File → one Edition); body: %s", len(dune.Editions), body)
	}
	if len(dune.Editions[0].Files) != 1 {
		t.Fatalf("Dune files = %d, want 1; body: %s", len(dune.Editions[0].Files), body)
	}
	f := dune.Editions[0].Files[0]
	if f.Container == "" {
		t.Errorf("Dune file container empty (ffprobe attrs missing); body: %s", body)
	}
	if f.VideoCodec != "h264" {
		t.Errorf("Dune videoCodec = %q, want h264", f.VideoCodec)
	}
	if f.AudioCodec != "aac" {
		t.Errorf("Dune audioCodec = %q, want aac", f.AudioCodec)
	}
	if f.Width != 320 || f.Height != 240 {
		t.Errorf("Dune resolution = %dx%d, want 320x240", f.Width, f.Height)
	}
	if f.DurationMs <= 0 {
		t.Errorf("Dune durationMs = %d, want > 0", f.DurationMs)
	}
	// Elementary streams: at least one video + one audio.
	var nVideo, nAudio int
	for _, s := range f.Streams {
		switch s.Kind {
		case "video":
			nVideo++
			if s.Codec != "h264" {
				t.Errorf("video stream codec = %q, want h264", s.Codec)
			}
		case "audio":
			nAudio++
		}
	}
	if nVideo < 1 || nAudio < 1 {
		t.Errorf("Dune streams: video=%d audio=%d, want >=1 each; body: %s", nVideo, nAudio, body)
	}

	// Blade Runner exercises a different container/codec mix (mkv, mpeg4/mp3).
	var blade titleDetailResp
	bladeID := findTitle(t, list, "Blade Runner")
	srv.AuthGET("/api/v1/titles/"+bladeID, token, &blade)
	bf := blade.Editions[0].Files[0]
	if bf.VideoCodec != "mpeg4" {
		t.Errorf("Blade Runner videoCodec = %q, want mpeg4", bf.VideoCodec)
	}
	if bf.AudioCodec != "mp3" {
		t.Errorf("Blade Runner audioCodec = %q, want mp3", bf.AudioCodec)
	}
}

// TestScanIsIdempotent: a second scan re-resolves to the same Titles (identity
// stability) rather than duplicating them.
func TestScanIsIdempotent(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	// Two sequential scans (each waited to completion): the second must re-resolve
	// to the same Titles, not duplicate them.
	scanLib(t, srv, token, libID, "")
	scanLib(t, srv, token, libID, "")

	var list titlesListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles", token, &list)
	if len(list.Titles) != 3 {
		t.Errorf("after rescan title count = %d, want 3 (no duplicates)", len(list.Titles))
	}
}

// TestTitlesPaginationAndSort: cursor pagination returns every Title exactly
// once across pages, and sort=dateAdded is honored.
func TestTitlesPaginationAndSort(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Page through with limit=1; collect all titles, assert no dupes/skips.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/libraries/" + libID + "/titles?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var page titlesListResp
		status, body := srv.AuthGET(path, token, &page)
		if status != http.StatusOK {
			t.Fatalf("page status = %d; body: %s", status, body)
		}
		if len(page.Titles) > 1 {
			t.Fatalf("limit=1 returned %d titles", len(page.Titles))
		}
		for _, ts := range page.Titles {
			if seen[ts.ID] {
				t.Errorf("title %q (%s) returned twice across pages", ts.Title, ts.ID)
			}
			seen[ts.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 3 {
		t.Errorf("paged %d distinct titles, want 3", len(seen))
	}

	// sort=dateAdded returns a full page; just assert it succeeds and is complete.
	var byDate titlesListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?sort=dateAdded&limit=50", token, &byDate)
	if status != http.StatusOK {
		t.Fatalf("sort=dateAdded status = %d; body: %s", status, body)
	}
	if len(byDate.Titles) != 3 {
		t.Errorf("sort=dateAdded count = %d, want 3", len(byDate.Titles))
	}
}

// TestBrowseRequiresAuth: unauthenticated browse/detail are refused (401).
func TestBrowseRequiresAuth(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))

	status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/titles", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unauth list status = %d, want 401", status)
	}
	status, _ = srv.AuthGET("/api/v1/titles/anything", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("unauth get status = %d, want 401", status)
	}
	// Scan trigger requires Admin specifically — a Member is forbidden.
	srv.CreateMember("member", "memberpass123")
	memberToken := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	status, _ = srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/scan", memberToken, nil, nil)
	if status != http.StatusForbidden {
		t.Errorf("member scan status = %d, want 403", status)
	}
}

// TestGetMissingTitle returns 404 (not 403) — hide existence (api-contract.md).
func TestGetMissingTitle(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	var env errorEnvelope
	status, body := srv.AuthGET("/api/v1/titles/no-such-title", token, &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, body)
	}
	if env.Error.Code != "NOT_FOUND" {
		t.Errorf("code = %q, want NOT_FOUND", env.Error.Code)
	}
}

// TestScanMissingLibrary: scanning an unknown Library is 404.
func TestScanMissingLibrary(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	var env errorEnvelope
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/no-such-lib/scan", token, nil, &env)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", status, body)
	}
}

// findTitle returns the id of the Title with the given display title from a list.
func findTitle(t *testing.T, list titlesListResp, title string) string {
	t.Helper()
	for _, ts := range list.Titles {
		if ts.Title == title {
			return ts.ID
		}
	}
	t.Fatalf("title %q not found in list", title)
	return ""
}

// --- fixtures --------------------------------------------------------------

// fixtureSpec describes one fixture to (re)generate with ffmpeg if absent.
type fixtureSpec struct {
	relPath    string
	size       string
	audioFreq  string
	videoCodec string // libx264 | mpeg4
	audioCodec string // aac | libmp3lame
}

var fixtureSpecs = []fixtureSpec{
	{filepath.Join("Dune (2021)", "Dune (2021).mp4"), "320x240", "440", "libx264", "aac"},
	{filepath.Join("Blade Runner (1982)", "Blade Runner (1982).mkv"), "256x144", "330", "mpeg4", "libmp3lame"},
	{filepath.Join("Sample Movie (2000).mp4"), "160x120", "220", "libx264", "aac"},
}

// fixturesAvailable is set by TestMain: true when every fixture file exists on
// disk (checked in or freshly generated). When false the integration tests skip.
var fixturesAvailable bool

func requireFixtures(t *testing.T) {
	t.Helper()
	if !fixturesAvailable {
		t.Skip("media fixtures unavailable (no checked-in clips and ffmpeg not on PATH)")
	}
}

// TestMain ensures the checked-in fixtures exist, regenerating any missing one
// with ffmpeg. Checking the clips in keeps tests fast and offline; regeneration
// is the fallback for a fresh tree. If a fixture is missing and ffmpeg can't
// produce it, the integration tests skip rather than fail.
func TestMain(m *testing.M) {
	fixturesAvailable = ensureFixtures()
	os.Exit(m.Run())
}

func ensureFixtures() bool {
	root := filepath.Join("testdata", "movies")
	for _, spec := range fixtureSpecs {
		out := filepath.Join(root, spec.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		if !generateClip(spec, out) {
			return false
		}
	}
	// Final check: every fixture must now exist.
	for _, spec := range fixtureSpecs {
		if !fileExists(filepath.Join(root, spec.relPath)) {
			return false
		}
	}
	return true
}

// generateClip shells out to ffmpeg to synthesize one ~1s test clip. Returns
// false if ffmpeg is unavailable or fails.
func generateClip(spec fixtureSpec, out string) bool {
	args := []string{
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=" + spec.size + ":rate=24",
		"-f", "lavfi", "-i", "sine=frequency=" + spec.audioFreq + ":duration=1",
		"-c:v", spec.videoCodec,
	}
	if spec.videoCodec == "libx264" {
		args = append(args, "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:a", spec.audioCodec, "-shortest", out)
	cmd := exec.Command("ffmpeg", args...)
	return cmd.Run() == nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Size() > 0
}
