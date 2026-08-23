package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box integration tests for the Music vertical slice (issue tv-music/03):
// generate tagged audio fixtures (tags written via `ffmpeg -metadata`), scan
// them via a Music Library, then assert the Artist → Album → Track hierarchy +
// Album-Artist grouping + disc/track ordering + path-fallback + access control
// through the browse API — the primary (highest) test seam. Fixtures are
// generated lazily under testdata/music/; if ffmpeg is absent the tests skip (as
// the Movie/TV integration tests do).
//
// The tree exercises every music identity branch:
//   Radiohead / OK Computer  → a normal single-artist album, disc/track tags
//   Various Artists / Summer  → a COMPILATION: two tracks with DIFFERENT artist
//                               tags but a shared album_artist → ONE Album
//   (tagless) Pink Floyd/The Wall (1979)/05 - Brick.flac → PATH FALLBACK
//   Radiohead FLAC track      → transcoded FLAC→AAC (codec the test profile lacks)
//   Ben Folds LP + single     → SAME album title, different MusicBrainz release
//                               groups → TWO Albums, badged by releaseType (ADR-0038)

const musicRootRel = "music"

// musicClip is one fixture audio file: its relative path, container ext, and the
// embedded tags to write (empty map = tagless, exercising the path fallback).
type musicClip struct {
	relPath string
	tags    map[string]string
}

var musicClips = []musicClip{
	{
		relPath: filepath.Join("Radiohead", "OK Computer (1997)", "01 - Airbag.m4a"),
		tags: map[string]string{
			"artist": "Radiohead", "album_artist": "Radiohead", "album": "OK Computer",
			"title": "Airbag", "track": "1/12", "disc": "1/1", "date": "1997", "genre": "Alternative",
		},
	},
	{
		relPath: filepath.Join("Radiohead", "OK Computer (1997)", "02 - Paranoid Android.m4a"),
		tags: map[string]string{
			"artist": "Radiohead", "album_artist": "Radiohead", "album": "OK Computer",
			"title": "Paranoid Android", "track": "2/12", "disc": "1/1", "date": "1997",
		},
	},
	{
		// A FLAC the test transcode profile cannot decode → FLAC→AAC transcode case.
		relPath: filepath.Join("Radiohead", "Lossless Single", "01 - No Surprises.flac"),
		tags: map[string]string{
			"artist": "Radiohead", "album_artist": "Radiohead", "album": "Lossless Single",
			"title": "No Surprises", "track": "1", "date": "1997",
		},
	},
	{
		// Compilation track 1: artist "Artist A" but album_artist "Various Artists".
		relPath: filepath.Join("Compilations", "Summer Hits Disc 1.mp3"),
		tags: map[string]string{
			"artist": "Artist A", "album_artist": "Various Artists", "album": "Summer Hits",
			"title": "Beach Day", "track": "1", "date": "2020",
		},
	},
	{
		// Compilation track 2: DIFFERENT artist "Artist B", SAME album_artist.
		relPath: filepath.Join("Compilations", "Summer Hits Disc 2.mp3"),
		tags: map[string]string{
			"artist": "Artist B", "album_artist": "Various Artists", "album": "Summer Hits",
			"title": "Sunset Drive", "track": "2", "date": "2020",
		},
	},
	{
		// Tagless: identity must fall back to the Artist/Album (Year)/NN - Title path.
		relPath: filepath.Join("Pink Floyd", "The Wall (1979)", "05 - Another Brick.mp3"),
		tags:    nil,
	},
	{
		// LP whose title single shares its (normalized) album title — the curly vs
		// straight apostrophe is exactly the punctuation normalizeTitle collapses.
		// The MusicBrainz release-group id must split the two (ADR-0038). FLAC so
		// ffmpeg preserves the arbitrary Vorbis comment keys.
		relPath: filepath.Join("Ben Folds", "Rockin' the Suburbs", "01 - Annie Waits.flac"),
		tags: map[string]string{
			"artist": "Ben Folds", "album_artist": "Ben Folds", "album": "Rockin’ the Suburbs",
			"title": "Annie Waits", "track": "1/12", "date": "2001",
			"musicbrainz_releasegroupid": "b110c311-9cd1-3a5e-82e2-6ab07a1665c7",
			"releasetype":                "album",
		},
	},
	{
		// The title single: same normalized album title, different release group.
		relPath: filepath.Join("Ben Folds", "Rockin' the Suburbs Single", "01 - Rockin' the Suburbs.flac"),
		tags: map[string]string{
			"artist": "Ben Folds", "album_artist": "Ben Folds", "album": "Rockin' the Suburbs",
			"title": "Rockin' the Suburbs (radio edit)", "track": "1/3", "date": "2001",
			"musicbrainz_releasegroupid": "1c45b7fd-f0fd-3a0d-87b0-8f1e4f5445b9",
			"releasetype":                "single",
		},
	},
}

var musicFixturesAvailable bool

func requireMusicFixtures(t *testing.T) {
	t.Helper()
	if !musicFixturesAvailable {
		t.Skip("music fixtures unavailable (ffmpeg not on PATH)")
	}
}

func init() {
	musicFixturesAvailable = ensureMusicFixtures()
}

func ensureMusicFixtures() bool {
	root := filepath.Join("testdata", musicRootRel)
	for _, c := range musicClips {
		out := filepath.Join(root, c.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		args := []string{
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		}
		for k, v := range c.tags {
			args = append(args, "-metadata", k+"="+v)
		}
		args = append(args, out)
		if exec.Command("ffmpeg", args...).Run() != nil {
			return false
		}
	}
	return true
}

func musicRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", musicRootRel))
	if err != nil {
		t.Fatalf("resolving music root: %v", err)
	}
	return abs
}

// --- wire shapes ------------------------------------------------------------

type artistSummaryResp struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type artistsListResp struct {
	Artists    []artistSummaryResp `json:"artists"`
	NextCursor string              `json:"nextCursor"`
}

type albumResp struct {
	ID          string `json:"id"`
	ArtistID    string `json:"artistId"`
	Title       string `json:"title"`
	Year        int    `json:"year"`
	HasArtwork  bool   `json:"hasArtwork"`
	ReleaseType string `json:"releaseType"`
	TrackCount  int    `json:"trackCount"`
}

type albumsListResp struct {
	Artist artistSummaryResp `json:"artist"`
	Albums []albumResp       `json:"albums"`
}

type trackSummaryResp struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	DiscNumber  int    `json:"discNumber"`
	TrackNumber int    `json:"trackNumber"`
	NeedsReview bool   `json:"needsReview"`
	Watched     bool   `json:"watched"`
}

type tracksListResp struct {
	Album  albumResp          `json:"album"`
	Tracks []trackSummaryResp `json:"tracks"`
}

type trackContextResp struct {
	ArtistID    string `json:"artistId"`
	ArtistName  string `json:"artistName"`
	AlbumID     string `json:"albumId"`
	AlbumTitle  string `json:"albumTitle"`
	AlbumYear   int    `json:"albumYear"`
	DiscNumber  int    `json:"discNumber"`
	TrackNumber int    `json:"trackNumber"`
}

type trackDetailResp struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Title    string            `json:"title"`
	Editions []editionResp     `json:"editions"`
	Track    *trackContextResp `json:"track"`
}

// --- helpers ----------------------------------------------------------------

func createMusicLibrary(t *testing.T, srv *testharness.Server, token, root string) string {
	t.Helper()
	_, lib, raw := createLibrary(t, srv, token, map[string]any{
		"name":        "Music",
		"kind":        "music",
		"rootFolders": []string{root},
	})
	if lib.ID == "" {
		t.Fatalf("music library not created; body: %s", raw)
	}
	return lib.ID
}

func scanMusicLibrary(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

func listArtists(t *testing.T, srv *testharness.Server, token, libID string) artistsListResp {
	t.Helper()
	var list artistsListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
	if status != http.StatusOK {
		t.Fatalf("list artists status = %d, want 200; body: %s", status, body)
	}
	return list
}

func findArtist(t *testing.T, list artistsListResp, name string) string {
	t.Helper()
	for _, a := range list.Artists {
		if a.Name == name {
			return a.ID
		}
	}
	t.Fatalf("artist %q not found; have: %+v", name, list.Artists)
	return ""
}

func artistAlbums(t *testing.T, srv *testharness.Server, token, artistID string) albumsListResp {
	t.Helper()
	var resp albumsListResp
	status, body := srv.AuthGET("/api/v1/artists/"+artistID+"/albums", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("albums status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func albumTracks(t *testing.T, srv *testharness.Server, token, albumID string) tracksListResp {
	t.Helper()
	var resp tracksListResp
	status, body := srv.AuthGET("/api/v1/albums/"+albumID+"/tracks", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("tracks status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func getTrackDetail(t *testing.T, srv *testharness.Server, token, id string) trackDetailResp {
	t.Helper()
	var d trackDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("track detail status = %d, want 200; body: %s", status, body)
	}
	return d
}

// --- tests ------------------------------------------------------------------

// TestMusicScanBuildsHierarchy is the central tracer bullet: scan the tagged
// music fixtures and assert the Artist → Album → Track hierarchy via the API.
func TestMusicScanBuildsHierarchy(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)

	artists := listArtists(t, srv, token, libID)
	// Four artists: Radiohead, Various Artists (compilation album_artist),
	// Pink Floyd, Ben Folds (release-group split fixtures).
	if len(artists.Artists) != 4 {
		t.Fatalf("artists = %d, want 4; have: %+v", len(artists.Artists), artists.Artists)
	}
	for _, a := range artists.Artists {
		if a.Kind != "artist" {
			t.Errorf("artist %q kind = %q, want artist", a.Name, a.Kind)
		}
	}

	// Radiohead: two albums (OK Computer + Lossless Single).
	radioID := findArtist(t, artists, "Radiohead")
	radio := artistAlbums(t, srv, token, radioID)
	if len(radio.Albums) != 2 {
		t.Fatalf("Radiohead albums = %d, want 2; have: %+v", len(radio.Albums), radio.Albums)
	}
	var okc albumResp
	for _, a := range radio.Albums {
		if a.Title == "OK Computer" {
			okc = a
		}
	}
	if okc.ID == "" {
		t.Fatalf("OK Computer album not found; have: %+v", radio.Albums)
	}
	if okc.Year != 1997 {
		t.Errorf("OK Computer year = %d, want 1997", okc.Year)
	}
	if okc.TrackCount != 2 {
		t.Errorf("OK Computer trackCount = %d, want 2", okc.TrackCount)
	}

	// Tracks in disc/track order: Airbag (1) then Paranoid Android (2).
	tracks := albumTracks(t, srv, token, okc.ID)
	if len(tracks.Tracks) != 2 {
		t.Fatalf("OK Computer tracks = %d, want 2", len(tracks.Tracks))
	}
	if tracks.Tracks[0].Title != "Airbag" || tracks.Tracks[1].Title != "Paranoid Android" {
		t.Errorf("track order = %q,%q, want Airbag,Paranoid Android",
			tracks.Tracks[0].Title, tracks.Tracks[1].Title)
	}
	if tracks.Tracks[0].TrackNumber != 1 || tracks.Tracks[1].TrackNumber != 2 {
		t.Errorf("track numbers = %d,%d, want 1,2",
			tracks.Tracks[0].TrackNumber, tracks.Tracks[1].TrackNumber)
	}
	for _, tr := range tracks.Tracks {
		if tr.Kind != "track" {
			t.Errorf("track %q kind = %q, want track", tr.Title, tr.Kind)
		}
		if tr.NeedsReview {
			t.Errorf("tagged track %q should not need review", tr.Title)
		}
	}
}

// TestMusicCompilationGrouping: a compilation whose two tracks carry DIFFERENT
// artist tags but a shared album_artist ("Various Artists") files as ONE Album
// under one Artist (Album-Artist grouping — the headline behavior).
func TestMusicCompilationGrouping(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)

	artists := listArtists(t, srv, token, libID)
	vaID := findArtist(t, artists, "Various Artists")
	va := artistAlbums(t, srv, token, vaID)
	if len(va.Albums) != 1 {
		t.Fatalf("Various Artists albums = %d, want 1 (compilation stays ONE album); have: %+v",
			len(va.Albums), va.Albums)
	}
	if va.Albums[0].Title != "Summer Hits" {
		t.Errorf("compilation album = %q, want Summer Hits", va.Albums[0].Title)
	}
	if va.Albums[0].TrackCount != 2 {
		t.Errorf("Summer Hits trackCount = %d, want 2 (both varied-artist tracks)", va.Albums[0].TrackCount)
	}
	tracks := albumTracks(t, srv, token, va.Albums[0].ID)
	if len(tracks.Tracks) != 2 {
		t.Fatalf("Summer Hits tracks = %d, want 2", len(tracks.Tracks))
	}
}

// TestMusicSameTitleReleasesSplit (album-release-identity, ADR-0038): an LP and
// its title single share a normalized album title (apostrophe styles collapse)
// but carry distinct MusicBrainz release-group ids — they must file as TWO
// Albums with disjoint track lists, each labeled with its releaseType so the
// client can badge the single.
func TestMusicSameTitleReleasesSplit(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)

	artists := listArtists(t, srv, token, libID)
	bf := artistAlbums(t, srv, token, findArtist(t, artists, "Ben Folds"))
	if len(bf.Albums) != 2 {
		t.Fatalf("Ben Folds albums = %d, want 2 (LP + single split); have: %+v",
			len(bf.Albums), bf.Albums)
	}
	var lp, single albumResp
	for _, a := range bf.Albums {
		switch a.ReleaseType {
		case "album":
			lp = a
		case "single":
			single = a
		}
	}
	if lp.ID == "" || single.ID == "" {
		t.Fatalf("expected one album + one single; have: %+v", bf.Albums)
	}
	if lp.TrackCount != 1 || single.TrackCount != 1 {
		t.Errorf("trackCounts = %d/%d, want 1/1 (disjoint track lists)",
			lp.TrackCount, single.TrackCount)
	}
	lpTracks := albumTracks(t, srv, token, lp.ID)
	singleTracks := albumTracks(t, srv, token, single.ID)
	if len(lpTracks.Tracks) != 1 || lpTracks.Tracks[0].Title != "Annie Waits" {
		t.Errorf("LP tracks = %+v, want [Annie Waits]", lpTracks.Tracks)
	}
	if len(singleTracks.Tracks) != 1 ||
		singleTracks.Tracks[0].Title != "Rockin' the Suburbs (radio edit)" {
		t.Errorf("single tracks = %+v, want [Rockin' the Suburbs (radio edit)]", singleTracks.Tracks)
	}
}

// TestMusicPathFallback: a tagless audio file files by its
// Artist/Album (Year)/NN - Title path and is flagged needs-review.
func TestMusicPathFallback(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)

	artists := listArtists(t, srv, token, libID)
	pfID := findArtist(t, artists, "Pink Floyd")
	pf := artistAlbums(t, srv, token, pfID)
	if len(pf.Albums) != 1 {
		t.Fatalf("Pink Floyd albums = %d, want 1; have: %+v", len(pf.Albums), pf.Albums)
	}
	if pf.Albums[0].Title != "The Wall" || pf.Albums[0].Year != 1979 {
		t.Errorf("path-fallback album = %q (%d), want The Wall (1979)",
			pf.Albums[0].Title, pf.Albums[0].Year)
	}
	tracks := albumTracks(t, srv, token, pf.Albums[0].ID)
	if len(tracks.Tracks) != 1 {
		t.Fatalf("The Wall tracks = %d, want 1", len(tracks.Tracks))
	}
	tr := tracks.Tracks[0]
	if tr.Title != "Another Brick" {
		t.Errorf("path-fallback track title = %q, want Another Brick", tr.Title)
	}
	if tr.TrackNumber != 5 {
		t.Errorf("path-fallback track number = %d, want 5", tr.TrackNumber)
	}
	if !tr.NeedsReview {
		t.Errorf("path-fallback (tagless) track should be flagged needs-review")
	}
}

// TestMusicTrackDetailContext: a Track's GET /titles/{id} carries its nested
// Editions plus the Artist/Album/disc/track parent context.
func TestMusicTrackDetailContext(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)
	artists := listArtists(t, srv, token, libID)
	radio := artistAlbums(t, srv, token, findArtist(t, artists, "Radiohead"))
	var okc albumResp
	for _, a := range radio.Albums {
		if a.Title == "OK Computer" {
			okc = a
		}
	}
	tracks := albumTracks(t, srv, token, okc.ID)
	d := getTrackDetail(t, srv, token, tracks.Tracks[1].ID) // Paranoid Android
	if d.Kind != "track" {
		t.Errorf("detail kind = %q, want track", d.Kind)
	}
	if len(d.Editions) == 0 || len(d.Editions[0].Files) == 0 {
		t.Fatalf("track detail has no Editions/Files")
	}
	if d.Track == nil {
		t.Fatalf("track detail missing parent context")
	}
	if d.Track.ArtistName != "Radiohead" || d.Track.AlbumTitle != "OK Computer" || d.Track.TrackNumber != 2 {
		t.Errorf("track context = %+v, want Radiohead / OK Computer / 2", d.Track)
	}
}

// TestMusicAccessControl404: an unknown Artist/Album returns 404, not 403
// (hide-existence, api-contract.md).
func TestMusicAccessControl404(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, _ := scanMusicLibrary(t)
	for _, path := range []string{
		"/api/v1/artists/no-such-artist/albums",
		"/api/v1/albums/no-such-album/tracks",
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

// TestMusicScanIdempotent: a rescan re-resolves to the same Artists/Albums/Tracks.
//
// It compares the post-rescan catalog against what the FIRST scan produced, and
// compares IDENTITIES rather than totals. Both choices are deliberate.
//
// Identities, because "the same artists" and "the same NUMBER of artists" are
// different claims and only the first is idempotence: a rescan that dropped one
// artist and minted another would hold the count and still be the bug this test
// exists to catch.
//
// A captured baseline rather than literal counts, because the literals rotted.
// This test asserted `want 3` from the initial commit until 720e17f added the Ben
// Folds release-group fixtures (ADR-0038), bumped its sibling
// TestMusicScanBuildsHierarchy from 3 to 4 in the same diff, and left this one
// behind — so it then failed on the FIRST scan's count, before the rescan it
// guards was even reached, and sat red while the scan itself was provably stable.
// Reading the baseline from the first scan cannot rot that way: add a fixture
// artist tomorrow and this test keeps testing rescan stability instead of
// demanding an edit. Do not "simplify" it back to a literal count.
func TestMusicScanIdempotent(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)

	before := listArtists(t, srv, token, libID)
	beforeRadiohead := artistAlbums(t, srv, token, findArtist(t, before, "Radiohead"))

	scanLib(t, srv, token, libID, "")

	after := listArtists(t, srv, token, libID)
	if got, want := artistIDs(after), artistIDs(before); !slices.Equal(got, want) {
		t.Errorf("after rescan artists = %v, want %v (no duplicates, no churn)", got, want)
	}
	afterRadiohead := artistAlbums(t, srv, token, findArtist(t, after, "Radiohead"))
	if got, want := albumIDs(afterRadiohead), albumIDs(beforeRadiohead); !slices.Equal(got, want) {
		t.Errorf("after rescan Radiohead albums = %v, want %v", got, want)
	}
}

// artistIDs and albumIDs project a listing down to its sorted ids, so the
// idempotence comparison is about membership and not about the order a listing
// happened to come back in — browse ordering is its own property with its own
// tests, and letting it fail this one would be a false alarm.
func artistIDs(list artistsListResp) []string {
	ids := make([]string, 0, len(list.Artists))
	for _, a := range list.Artists {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	return ids
}

func albumIDs(list albumsListResp) []string {
	ids := make([]string, 0, len(list.Albums))
	for _, a := range list.Albums {
		ids = append(ids, a.ID)
	}
	slices.Sort(ids)
	return ids
}

// TestMusicTrackDirectPlay: a Track whose audio codec the client supports
// direct-plays (audio-only negotiation, no video stream) — reusing the existing
// tier machinery additively.
func TestMusicTrackDirectPlay(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)
	artists := listArtists(t, srv, token, libID)
	radio := artistAlbums(t, srv, token, findArtist(t, artists, "Radiohead"))
	var okc albumResp
	for _, a := range radio.Albums {
		if a.Title == "OK Computer" {
			okc = a
		}
	}
	tracks := albumTracks(t, srv, token, okc.ID) // m4a/aac tracks

	// A profile that supports the m4a (mp4) container + aac → directPlay.
	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4", "m4a", "mov"},
			"audioCodecs":      []string{"aac", "flac", "alac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+tracks.Tracks[0].ID+"/playback", token, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("track playback status = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "directPlay" {
		t.Errorf("aac track tier = %q, want directPlay; body: %s", dec.Tier, body)
	}
	if dec.SessionID == "" {
		t.Errorf("sessionId empty; body: %s", body)
	}
	// "no video stream" above is a wire claim, so assert it on the wire. This went
	// unasserted while the field marshalled as {"index":0,"codec":""} on every Track
	// — present, empty, and true under `if (d.videoStream)`.
	if dec.VideoStream != nil {
		t.Errorf("track decision carries videoStream %+v, want it OMITTED (ADR-0017); body: %s",
			*dec.VideoStream, body)
	}
	if dec.AudioStream == nil {
		t.Errorf("track decision has no audioStream; body: %s", body)
	}
}

// TestMusicTrackDecisionOmitsVideoStreamKey: the stronger form of the assertion
// above — the `videoStream` KEY is absent from the JSON, not merely null. A client
// distinguishes audio-only from video by the field's presence, so "null" and
// "omitted" must not diverge (Swift decodes both to nil, but JS `"videoStream" in d`
// does not). Paired with the Movie case, which must still carry it: the guard has to
// omit for Tracks WITHOUT silently dropping the field for real video.
func TestMusicTrackDecisionOmitsVideoStreamKey(t *testing.T) {
	requireMusicFixtures(t)
	srv, token, libID := scanMusicLibrary(t)
	artists := listArtists(t, srv, token, libID)
	radio := artistAlbums(t, srv, token, findArtist(t, artists, "Radiohead"))
	var okc albumResp
	for _, a := range radio.Albums {
		if a.Title == "OK Computer" {
			okc = a
		}
	}
	tracks := albumTracks(t, srv, token, okc.ID)
	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4", "m4a", "mov"},
			"audioCodecs":      []string{"aac", "flac", "alac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var raw map[string]any
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+tracks.Tracks[0].ID+"/playback", token, profile, &raw)
	if status != http.StatusOK {
		t.Fatalf("track playback status = %d, want 200; body: %s", status, body)
	}
	if _, present := raw["videoStream"]; present {
		t.Errorf("track decision JSON has a videoStream key (= %v), want it absent; body: %s",
			raw["videoStream"], body)
	}
	// The empty list is the plural's contract and is unrelated to the singular's
	// omission — it was already correct, and must stay a [] rather than disappear.
	if vs, present := raw["videoStreams"]; !present || vs == nil {
		t.Errorf("videoStreams must stay a present empty list, got %v; body: %s", vs, body)
	}
}

// TestMusicTrackTranscodeFLACtoAAC: a FLAC Track under a profile that supports
// only aac transcodes to an audio-only AAC HLS rendition — the FLAC→AAC case,
// reusing the existing transcode tier additively. The fetched segment is a valid
// HLS .ts that ffprobes as aac.
func TestMusicTrackTranscodeFLACtoAAC(t *testing.T) {
	requireMusicFixtures(t)
	requireFFmpeg(t)
	srv, token, libID := scanMusicLibrary(t)
	artists := listArtists(t, srv, token, libID)
	radio := artistAlbums(t, srv, token, findArtist(t, artists, "Radiohead"))
	var lossless albumResp
	for _, a := range radio.Albums {
		if a.Title == "Lossless Single" {
			lossless = a
		}
	}
	if lossless.ID == "" {
		t.Fatalf("Lossless Single album not found; have: %+v", radio.Albums)
	}
	tracks := albumTracks(t, srv, token, lossless.ID) // the .flac track

	// A profile that supports aac (not flac) → the FLAC must transcode to AAC.
	profile := map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mp4", "fmp4"},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
		"constraints": map[string]any{"maxBitrate": 100000000},
	}
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+tracks.Tracks[0].ID+"/playback", token, profile, &dec)
	if status != http.StatusOK {
		t.Fatalf("FLAC playback status = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "transcode" {
		t.Fatalf("FLAC track tier = %q, want transcode; body: %s", dec.Tier, body)
	}
	if !strings.Contains(dec.StreamURL, "/hls/") {
		t.Fatalf("FLAC streamUrl = %q, want an HLS URL", dec.StreamURL)
	}

	// Fetch the HLS playlist + first segment and confirm it re-encoded to aac.
	seg := fetchFirstSegment(t, srv, dec.StreamURL, token)
	vCodec, aCodec, _ := ffprobeSegment(t, seg)
	if aCodec != "aac" {
		t.Errorf("transcoded segment audio codec = %q, want aac (FLAC→AAC)", aCodec)
	}
	if vCodec != "" {
		t.Errorf("audio-only transcode segment has video codec %q, want none", vCodec)
	}
}
