package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box API tests for issue tv-music/04: needs-review / Unmatched bucketing
// made kind-aware for TV & Music, the Music Album-folder Match override, and the
// Show-poster watched affordance. Everything is asserted through the HTTP API
// over crafted on-disk fixture trees (the primary test seam), never on internal
// scanner shapes. Trees are built in throwaway temp dirs (these mutate between
// scans), so the checked-in fixtures are never touched.

// makeAudio writes a ~1s audio clip at dst with the given embedded tags (empty
// map = tagless, exercising the path fallback). Skips if ffmpeg is unavailable.
func makeAudio(t *testing.T, dst string, tags map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	args := []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1"}
	for k, v := range tags {
		args = append(args, "-metadata", k+"="+v)
	}
	args = append(args, dst)
	if exec.Command("ffmpeg", args...).Run() != nil {
		t.Fatalf("ffmpeg failed to generate %q", dst)
	}
}

// --- TV bucketing -----------------------------------------------------------

// TestTVNeedsReviewAndUnmatched: a yearless Show files as a browsable
// needs-review Show; a recognized media file with no episode token goes to the
// Unmatched bucket (never auto-guessed), while a well-named Show files cleanly.
func TestTVNeedsReviewAndUnmatched(t *testing.T) {
	root := t.TempDir()
	// Yearless Show → needs-review (browsable), with a valid episode so it files.
	makeMovie(t, filepath.Join(root, "Mystery Show", "Season 01", "Mystery Show - S01E01 - Pilot.mkv"))
	// A clean Show with a valid episode AND a recognized media file with NO episode
	// token (→ Unmatched, not silently guessed into the Show).
	makeMovie(t, filepath.Join(root, "Good Show (2020)", "Season 01", "Good Show (2020) - S01E01 - Real.mkv"))
	makeMovie(t, filepath.Join(root, "Good Show (2020)", "Season 01", "bonus_clip.mkv"))

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	byTitle := map[string]showSummaryResp{}
	for _, s := range shows.Shows {
		byTitle[s.Title] = s
	}
	mystery, ok := byTitle["Mystery Show"]
	if !ok {
		t.Fatalf("yearless Show not browsable; shows: %+v", shows.Shows)
	}
	if !mystery.NeedsReview {
		t.Errorf("yearless Show needsReview = false, want true")
	}
	good, ok := byTitle["Good Show"]
	if !ok {
		t.Fatalf("well-named Show missing; shows: %+v", shows.Shows)
	}
	if good.NeedsReview {
		t.Errorf("well-named Show flagged needs-review, want clean")
	}

	// The token-less file is Unmatched (Admin attention list), never a Title.
	unm := listUnmatched(t, srv, token, libID)
	foundBonus := false
	for _, f := range unm.Files {
		if filepath.Base(f.Path) == "bonus_clip.mkv" {
			foundBonus = true
		}
	}
	if !foundBonus {
		t.Errorf("token-less media file not in Unmatched bucket; have: %+v", unm.Files)
	}
}

// --- Music bucketing --------------------------------------------------------

// TestMusicNoIdentityUnmatched: an audio file from which no minimal identity can
// be extracted (no tags, and a bare path with no Artist/Album folders) is NOT
// silently guessed into a Title — but a bare file still files by filename, so to
// truly trip Unmatched we use a tagless file whose name is junk-only. Here we
// assert the partial-parse (path fallback) lands needs-review and an unprobeable
// file lands Unmatched is covered by music_test.go; this asserts the API
// surfaces both buckets together.
func TestMusicNeedsReviewBucket(t *testing.T) {
	root := t.TempDir()
	// Tagless track filed by path layout → needs-review (best-effort parse).
	makeAudio(t, filepath.Join(root, "Nirvana", "Nevermind (1991)", "01 - Come As You Are.mp3"), nil)
	// Fully-tagged track → clean.
	makeAudio(t, filepath.Join(root, "tagged.m4a"), map[string]string{
		"artist": "Daft Punk", "album_artist": "Daft Punk", "album": "Discovery",
		"title": "One More Time", "track": "1", "date": "2001",
	})

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	artists := listArtists(t, srv, token, libID)
	nirvana := artistAlbums(t, srv, token, findArtist(t, artists, "Nirvana"))
	if len(nirvana.Albums) != 1 {
		t.Fatalf("Nirvana albums = %d, want 1; have %+v", len(nirvana.Albums), nirvana.Albums)
	}
	tracks := albumTracks(t, srv, token, nirvana.Albums[0].ID)
	if len(tracks.Tracks) != 1 || !tracks.Tracks[0].NeedsReview {
		t.Errorf("path-fallback track should be needs-review; got %+v", tracks.Tracks)
	}
	daft := artistAlbums(t, srv, token, findArtist(t, artists, "Daft Punk"))
	dt := albumTracks(t, srv, token, daft.Albums[0].ID)
	if len(dt.Tracks) != 1 || dt.Tracks[0].NeedsReview {
		t.Errorf("tagged track should be clean; got %+v", dt.Tracks)
	}
}

// --- Music Album-folder Match override --------------------------------------

// TestMusicAlbumOverridePersistsAcrossRescan: an Admin fix-match on an Album
// folder re-points the Album the tracks group under (overruling a mis-tag),
// persists across an incremental AND a full rescan, and surfaces as orphaned once
// the folder is renamed away — the Music interpretation of the folder-keyed
// override the Movie/TV paths already use.
func TestMusicAlbumOverridePersistsAcrossRescan(t *testing.T) {
	root := t.TempDir()
	albumDir := filepath.Join(root, "The Beatles", "Abbey Road (1969)")
	// Mis-tagged album: the file lives in the right folder but the album tag is wrong.
	makeAudio(t, filepath.Join(albumDir, "01 - Come Together.m4a"), map[string]string{
		"artist": "The Beatles", "album_artist": "The Beatles", "album": "Wrong Album",
		"title": "Come Together", "track": "1", "date": "1969",
	})

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	// Before the override the album reflects the (wrong) tag.
	artists := listArtists(t, srv, token, libID)
	before := artistAlbums(t, srv, token, findArtist(t, artists, "The Beatles"))
	if len(before.Albums) != 1 || before.Albums[0].Title != "Wrong Album" {
		t.Fatalf("pre-override album = %+v, want one 'Wrong Album'", before.Albums)
	}

	// Admin corrects the Album, keyed to the album folder path.
	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/fix-match", token, map[string]any{
		"folderPath": albumDir,
		"title":      "Abbey Road",
		"year":       1969,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("fix-match = %d, want 200; body: %s", status, body)
	}

	assertCorrected := func(stage string) {
		artists := listArtists(t, srv, token, libID)
		ab := artistAlbums(t, srv, token, findArtist(t, artists, "The Beatles"))
		if len(ab.Albums) != 1 {
			t.Fatalf("%s: Beatles albums = %d, want 1; have %+v", stage, len(ab.Albums), ab.Albums)
		}
		if ab.Albums[0].Title != "Abbey Road" || ab.Albums[0].Year != 1969 {
			t.Errorf("%s: album = %q (%d), want Abbey Road (1969)", stage, ab.Albums[0].Title, ab.Albums[0].Year)
		}
		if ab.Albums[0].TrackCount != 1 {
			t.Errorf("%s: trackCount = %d, want 1", stage, ab.Albums[0].TrackCount)
		}
		tr := albumTracks(t, srv, token, ab.Albums[0].ID)
		if len(tr.Tracks) != 1 || tr.Tracks[0].NeedsReview {
			t.Errorf("%s: overridden track should be a confirmed (non-needs-review) match; got %+v", stage, tr.Tracks)
		}
	}

	scanLib(t, srv, token, libID, "") // incremental rescan
	assertCorrected("after incremental rescan")
	scanLib(t, srv, token, libID, "full") // full rescan
	assertCorrected("after full rescan")

	// The override persists and is not orphaned while its folder exists.
	ovs := listOverrides(t, srv, token, libID)
	if len(ovs.Overrides) != 1 || ovs.Overrides[0].Orphaned {
		t.Fatalf("override = %+v, want 1 non-orphaned", ovs.Overrides)
	}

	// Renaming the album folder orphans the override (surfaced, not lost).
	if err := os.Rename(albumDir, filepath.Join(root, "The Beatles", "Renamed (1969)")); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")
	ovs = listOverrides(t, srv, token, libID)
	if len(ovs.Overrides) != 1 || !ovs.Overrides[0].Orphaned {
		t.Errorf("override not flagged orphaned after its album folder was renamed; have %+v", ovs.Overrides)
	}
}

// --- Show-poster watched affordance -----------------------------------------

// showSummaryWithCountResp extends the Show summary with the unwatched-episode
// count (issue tv-music/04 web affordance), asserted here at the API boundary.
type showSummaryWithCountResp struct {
	ID                    string `json:"id"`
	Title                 string `json:"title"`
	UnwatchedEpisodeCount int    `json:"unwatchedEpisodeCount"`
}

type showsWithCountResp struct {
	Shows []showSummaryWithCountResp `json:"shows"`
}

// TestShowPosterUnwatchedCount: a TV Library's Show grid reports a per-User
// unwatched-Episode count on each Show, and the count drops when an Episode is
// marked watched — the Show analogue of a Movie's resume marker.
func TestShowPosterUnwatchedCount(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	countFor := func(title string) (string, int) {
		var list showsWithCountResp
		status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
		if status != http.StatusOK {
			t.Fatalf("list shows = %d; body: %s", status, body)
		}
		for _, s := range list.Shows {
			if s.Title == title {
				return s.ID, s.UnwatchedEpisodeCount
			}
		}
		t.Fatalf("show %q not found; have %+v", title, list.Shows)
		return "", 0
	}

	// The Bear: Season 01 (System, Hands) + a Special → 3 unwatched initially.
	showID, initial := countFor("The Bear")
	if initial != 3 {
		t.Fatalf("The Bear unwatched = %d, want 3", initial)
	}

	// Mark one Episode watched and confirm the count drops by one.
	bear := showSeasons(t, srv, token, showID)
	var s1 seasonResp
	for _, s := range bear.Seasons {
		if s.SeasonNumber == 1 {
			s1 = s
		}
	}
	eps := seasonEpisodes(t, srv, token, s1.ID)
	status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+eps.Episodes[0].ID+"/watchState",
		token, map[string]any{"watched": true}, nil)
	if status != http.StatusOK {
		t.Fatalf("watchState = %d; body: %s", status, body)
	}
	if _, after := countFor("The Bear"); after != initial-1 {
		t.Errorf("after watching one episode unwatched = %d, want %d", after, initial-1)
	}
}

// listUnmatched fetches a Library's Unmatched files (Admin attention surface).
// The unmatchedFileResp / unmatchedListResp wire shapes live in naming_test.go.
func listUnmatched(t *testing.T, srv *testharness.Server, token, libID string) unmatchedListResp {
	t.Helper()
	var out unmatchedListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/unmatched", token, &out)
	if status != http.StatusOK {
		t.Fatalf("list unmatched = %d, want 200; body: %s", status, body)
	}
	return out
}
