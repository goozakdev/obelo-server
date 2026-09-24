package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the Lyric provider Extension point: Installed Lyric
// providers placed under <dataDir>/plugins/<id>/, a Music Library whose tracks
// carry no lyrics, Plain ones or Synced ones, and GET /titles/{id}/lyrics — the
// request opening the lyrics view makes.
//
// Every guest is the suite's own module, which POSTs each question to its
// source and answers what the source says. The source is an httptest server per
// provider, so a test decides each answer — well timed, mistimed, for another
// recording, nothing — and counts each question asked.

// knownRecording is the MusicBrainz recording id the "Known Recording" fixture's
// tags assert.
const knownRecording = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"

// lyricQuestion is what a source is asked, as it arrives on the wire.
type lyricQuestion struct {
	Artist      string `json:"artist"`
	Title       string `json:"title"`
	Album       string `json:"album"`
	DurationMs  int64  `json:"durationMs"`
	RecordingID string `json:"recordingId"`
}

// lyricSource stands in for a lyrics service. answer builds its JSON reply to
// each question; nil answers nothing found.
type lyricSource struct {
	srv    *httptest.Server
	mu     sync.Mutex
	asked  []lyricQuestion
	answer func(q lyricQuestion) map[string]any
}

func newLyricSource(t *testing.T, answer func(q lyricQuestion) map[string]any) *lyricSource {
	t.Helper()
	s := &lyricSource{answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q lyricQuestion
		_ = json.NewDecoder(r.Body).Decode(&q)
		s.mu.Lock()
		s.asked = append(s.asked, q)
		s.mu.Unlock()
		reply := map[string]any{}
		if s.answer != nil {
			if a := s.answer(q); a != nil {
				reply = a
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// askedAbout is how many times the source was asked about title.
func (s *lyricSource) askedAbout(title string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, q := range s.asked {
		if q.Title == title {
			n++
		}
	}
	return n
}

// lastQuestion is the latest question the source was asked about title.
func (s *lyricSource) lastQuestion(t *testing.T, title string) lyricQuestion {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.asked) - 1; i >= 0; i-- {
		if s.asked[i].Title == title {
			return s.asked[i]
		}
	}
	t.Fatalf("the source was never asked about %q", title)
	return lyricQuestion{}
}

// syncedAnswer is a two-line Synced answer timed against a recording offMs
// longer than the track the question named.
func syncedAnswer(offMs int64, words string) func(q lyricQuestion) map[string]any {
	return func(q lyricQuestion) map[string]any {
		return map[string]any{
			"kind": "synced",
			"lines": []map[string]any{
				{"startMs": 100, "text": words + " one"},
				{"startMs": 600, "text": words + " two"},
			},
			"durationMs": q.DurationMs + offMs,
		}
	}
}

// fetchedLyricsFixture writes a Music Library with four tracks and returns its
// album folder: one with no lyrics, one whose only Local lyrics are Plain, one
// with Synced Local lyrics, and one whose tags assert knownRecording.
func fetchedLyricsFixture(t *testing.T) string {
	t.Helper()
	root := lyricsFixtureRoot(t)
	album := filepath.Join(root, "Lyric Band", "Fetched (2024)")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatalf("creating album folder: %v", err)
	}
	tags := func(title, track string) map[string]string {
		return map[string]string{
			"artist": "Lyric Band", "album_artist": "Lyric Band", "album": "Fetched",
			"title": title, "track": track, "date": "2024",
		}
	}
	encodeClip(t, filepath.Join(album, "01 - Nothing Local.flac"), tags("Nothing Local", "1"))

	plainTags := tags("Plain Local", "2")
	plainTags["LYRICS"] = "Local plain words"
	encodeClip(t, filepath.Join(album, "02 - Plain Local.flac"), plainTags)

	encodeClip(t, filepath.Join(album, "03 - Synced Local.flac"), tags("Synced Local", "3"))
	if err := os.WriteFile(filepath.Join(album, "03 - Synced Local.lrc"), []byte("[00:00.20]Local synced line\n"), 0o644); err != nil {
		t.Fatalf("writing sidecar lrc: %v", err)
	}

	known := tags("Known Recording", "4")
	known["MUSICBRAINZ_TRACKID"] = knownRecording
	encodeClip(t, filepath.Join(album, "04 - Known Recording.flac"), known)
	return album
}

// lyricsFixtureRoot is a temp library root, skipping without ffmpeg as every
// synthesized-fixture suite does.
func lyricsFixtureRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	return t.TempDir()
}

// lyricProviderServer installs one Lyric provider per source under the given
// ids, scans the fixture library, and returns the server, an Admin token, the
// Library, the album folder and the track ids keyed by title.
func lyricProviderServer(t *testing.T, providers map[string]*lyricSource) (*testharness.Server, string, string, string, map[string]string) {
	t.Helper()
	dataDir := t.TempDir()
	for id, src := range providers {
		plugintest.Install(t, dataDir, plugintest.LyricManifest(id, src.srv.URL))
	}
	album := fetchedLyricsFixture(t)
	root := filepath.Dir(filepath.Dir(album))

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	artistID := findArtist(t, listArtists(t, srv, token, libID), "Lyric Band")
	albums := artistAlbums(t, srv, token, artistID)
	if len(albums.Albums) != 1 {
		t.Fatalf("albums = %+v, want the one fixture album", albums.Albums)
	}
	tracks := map[string]string{}
	for _, tr := range albumTracks(t, srv, token, albums.Albums[0].ID).Tracks {
		tracks[tr.Title] = tr.ID
	}
	if len(tracks) != 4 {
		t.Fatalf("tracks = %v, want all four fixtures filed", tracks)
	}
	return srv, token, libID, album, tracks
}

func assertFetched(t *testing.T, got lyricsResp, kind string) {
	t.Helper()
	if got.Lyrics == nil {
		t.Fatalf("lyrics = null, want fetched %s lyrics", kind)
	}
	if got.Lyrics.Kind != kind || got.Lyrics.Source != "fetched" {
		t.Fatalf("lyrics = %+v, want kind/source %s/fetched", *got.Lyrics, kind)
	}
}

// folderListing is every file name in dir, sorted.
func folderListing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestAFetchedSyncedAnswerTimedTenSecondsOffIsServedAsPlain: the provider's
// lines were timed for a recording ten seconds longer than the track. The words
// reach the viewer as Plain lyrics — not Synced, and not nothing.
func TestAFetchedSyncedAnswerTimedTenSecondsOffIsServedAsPlain(t *testing.T) {
	src := newLyricSource(t, syncedAnswer(10000, "Mistimed"))
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})

	got := readLyrics(t, srv, token, tracks["Nothing Local"])
	assertFetched(t, got, "plain")
	if got.Lyrics.Text != "Mistimed one\nMistimed two" || len(got.Lyrics.Lines) != 0 {
		t.Fatalf("lyrics = %+v, want the two lines as Plain text", *got.Lyrics)
	}
	q := src.lastQuestion(t, "Nothing Local")
	if q.Artist != "Lyric Band" || q.Album != "Fetched" || q.DurationMs <= 0 {
		t.Fatalf("the source was asked %+v, want the artist, album and a duration", q)
	}
}

// TestAWellTimedFetchedSyncedAnswerIsServedSynced is the other side of the same
// check: timed for this track's length, the lines are followed.
func TestAWellTimedFetchedSyncedAnswerIsServedSynced(t *testing.T) {
	src := newLyricSource(t, syncedAnswer(0, "Timed"))
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})

	got := readLyrics(t, srv, token, tracks["Nothing Local"])
	assertFetched(t, got, "synced")
	want := []lyricLineResp{{StartMs: 100, Text: "Timed one"}, {StartMs: 600, Text: "Timed two"}}
	if !reflect.DeepEqual(got.Lyrics.Lines, want) {
		t.Fatalf("lines = %+v, want %+v", got.Lyrics.Lines, want)
	}
}

// TestAnAnswerForAnotherRecordingIsNeverStored: the track's tags assert a
// recording, the provider is asked with it, and answers — well timed — for a
// different recording. Nothing from it is stored or shown, not even as Plain.
func TestAnAnswerForAnotherRecordingIsNeverStored(t *testing.T) {
	src := newLyricSource(t, func(q lyricQuestion) map[string]any {
		a := syncedAnswer(0, "Wrong song")(q)
		a["recordingId"] = "00000000-0000-0000-0000-000000000000"
		return a
	})
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})

	if got := readLyrics(t, srv, token, tracks["Known Recording"]); got.Lyrics != nil {
		t.Fatalf("lyrics = %+v, want null: the only answer named another recording", *got.Lyrics)
	}
	if q := src.lastQuestion(t, "Known Recording"); q.RecordingID != knownRecording {
		t.Fatalf("the source was asked for recording %q, want %q", q.RecordingID, knownRecording)
	}
	if got := readLyrics(t, srv, token, tracks["Known Recording"]); got.Lyrics != nil {
		t.Fatalf("lyrics on a second open = %+v, want still null", *got.Lyrics)
	}
}

// TestTheAdminsOrderDecidesWhoseSyncedAnswerWins: the Admin orders provider A
// ("zeta-lyrics") before B ("alpha-lyrics"). A's Synced answer is mistimed and
// B's is good, so the track shows B's Synced lyrics — and A was asked first.
func TestTheAdminsOrderDecidesWhoseSyncedAnswerWins(t *testing.T) {
	a := newLyricSource(t, syncedAnswer(10000, "A's"))
	b := newLyricSource(t, syncedAnswer(0, "B's"))
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"zeta-lyrics": a, "alpha-lyrics": b})

	var order struct {
		Providers []struct {
			Slug string `json:"slug"`
		} `json:"providers"`
	}
	status, body := srv.JSON(http.MethodPut, "/api/v1/settings/lyric-providers", token,
		map[string]any{"order": []string{"zeta-lyrics", "alpha-lyrics"}}, &order)
	if status != http.StatusOK || len(order.Providers) != 2 ||
		order.Providers[0].Slug != "zeta-lyrics" || order.Providers[1].Slug != "alpha-lyrics" {
		t.Fatalf("PUT lyric-providers = %d %s, want zeta-lyrics then alpha-lyrics", status, body)
	}

	got := readLyrics(t, srv, token, tracks["Nothing Local"])
	assertFetched(t, got, "synced")
	if got.Lyrics.Lines[0].Text != "B's one" {
		t.Fatalf("lines = %+v, want B's", got.Lyrics.Lines)
	}
	if a.askedAbout("Nothing Local") != 1 {
		t.Fatalf("A was asked %d times, want once, before B", a.askedAbout("Nothing Local"))
	}
}

// TestLyricsAreFetchedOnFirstOpenOnlyAndThenReadFromTheCache: nothing is asked
// by the scan; the first open asks; a second open — by anyone — asks nobody. A
// track with Synced Local lyrics is never asked about at all.
func TestLyricsAreFetchedOnFirstOpenOnlyAndThenReadFromTheCache(t *testing.T) {
	src := newLyricSource(t, syncedAnswer(0, "Cached"))
	srv, admin, libID, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	if n := src.askedAbout("Nothing Local"); n != 0 {
		t.Fatalf("the source was asked %d times before anyone opened the lyrics view, want 0", n)
	}

	first := readLyrics(t, srv, admin, tracks["Nothing Local"])
	assertFetched(t, first, "synced")

	memberID := srv.CreateUser(admin, "listener", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, libID)
	member := srv.LoginAs("listener", "memberpass123")
	second := readLyrics(t, srv, member, tracks["Nothing Local"])
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second open = %+v, want the first's %+v", second.Lyrics, first.Lyrics)
	}
	if n := src.askedAbout("Nothing Local"); n != 1 {
		t.Fatalf("the source was asked %d times over two opens, want 1", n)
	}

	local := readLyrics(t, srv, admin, tracks["Synced Local"])
	if local.Lyrics == nil || local.Lyrics.Source != "local" || src.askedAbout("Synced Local") != 0 {
		t.Fatalf("Synced Local lyrics = %+v, asked %d times; want local and never asked",
			local.Lyrics, src.askedAbout("Synced Local"))
	}
}

// TestAPlainOnlyLocalLyricStillAsksEveryProvider: Plain Local lyrics are not an
// answer, so both enabled providers are asked, and the fetched Synced answer is
// what the track shows.
func TestAPlainOnlyLocalLyricStillAsksEveryProvider(t *testing.T) {
	a := newLyricSource(t, nil)
	b := newLyricSource(t, syncedAnswer(0, "Fetched"))
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"alpha-lyrics": a, "beta-lyrics": b})
	status, body := srv.JSON(http.MethodPut, "/api/v1/settings/lyric-providers", token,
		map[string]any{"order": []string{"alpha-lyrics", "beta-lyrics"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT lyric-providers = %d %s", status, body)
	}

	got := readLyrics(t, srv, token, tracks["Plain Local"])
	if a.askedAbout("Plain Local") != 1 || b.askedAbout("Plain Local") != 1 {
		t.Fatalf("asked alpha %d and beta %d times, want each once",
			a.askedAbout("Plain Local"), b.askedAbout("Plain Local"))
	}
	assertFetched(t, got, "synced")
}

// TestEveryProviderMissingIsCachedUntilTheQuestionChanges: with only A
// enabled, A misses, and re-opening asks nobody. Enabling B changes the
// question, so the next open asks again — once.
func TestEveryProviderMissingIsCachedUntilTheQuestionChanges(t *testing.T) {
	a := newLyricSource(t, nil)
	b := newLyricSource(t, nil)
	srv, token, _, _, tracks := lyricProviderServer(t, map[string]*lyricSource{"alpha-lyrics": a, "beta-lyrics": b})
	if status, body := srv.JSON(http.MethodPost, "/api/v1/settings/plugins/beta-lyrics/disable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("disabling beta-lyrics = %d %s", status, body)
	}
	id := tracks["Nothing Local"]

	for i := 0; i < 2; i++ {
		if got := readLyrics(t, srv, token, id); got.Lyrics != nil {
			t.Fatalf("open %d: lyrics = %+v, want null", i+1, *got.Lyrics)
		}
	}
	if a.askedAbout("Nothing Local") != 1 || b.askedAbout("Nothing Local") != 0 {
		t.Fatalf("asked alpha %d and beta %d times over two opens, want 1 and 0",
			a.askedAbout("Nothing Local"), b.askedAbout("Nothing Local"))
	}

	if status, body := srv.JSON(http.MethodPost, "/api/v1/settings/plugins/beta-lyrics/enable", token, nil, nil); status != http.StatusOK {
		t.Fatalf("enabling beta-lyrics = %d %s", status, body)
	}
	for i := 0; i < 2; i++ {
		readLyrics(t, srv, token, id)
	}
	if a.askedAbout("Nothing Local") != 2 || b.askedAbout("Nothing Local") != 1 {
		t.Fatalf("after enabling beta: asked alpha %d and beta %d times, want 2 and 1",
			a.askedAbout("Nothing Local"), b.askedAbout("Nothing Local"))
	}
}

// TestAFetchedAnswerNeverLandsInTheLibraryFolder: fetching writes nothing
// beside the track, and a full rescan of the folder does not turn the fetched
// answer into a Local lyric — it is still the fetched one, and nobody is asked
// again.
func TestAFetchedAnswerNeverLandsInTheLibraryFolder(t *testing.T) {
	src := newLyricSource(t, syncedAnswer(0, "Fetched"))
	srv, token, libID, album, tracks := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	id := tracks["Nothing Local"]
	before := folderListing(t, album)

	assertFetched(t, readLyrics(t, srv, token, id), "synced")
	if after := folderListing(t, album); !reflect.DeepEqual(before, after) {
		t.Fatalf("album folder after fetching = %v, want unchanged %v", after, before)
	}

	scanLib(t, srv, token, libID, "full")
	got := readLyrics(t, srv, token, id)
	assertFetched(t, got, "synced")
	if n := src.askedAbout("Nothing Local"); n != 1 {
		t.Fatalf("the source was asked %d times, want 1", n)
	}
}

// TestLyricProviderSettingsRefuseAnUnknownSlug: the order names only registered
// Lyric providers.
func TestLyricProviderSettingsRefuseAnUnknownSlug(t *testing.T) {
	src := newLyricSource(t, nil)
	srv, token, _, _, _ := lyricProviderServer(t, map[string]*lyricSource{"example-lyrics": src})
	status, body := srv.JSON(http.MethodPut, "/api/v1/settings/lyric-providers", token,
		map[string]any{"order": []string{"nobody"}}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with an unknown slug = %d %s, want 422", status, body)
	}
}
