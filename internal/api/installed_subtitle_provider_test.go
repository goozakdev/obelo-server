package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for an INSTALLED Subtitle provider (ADR-0021 behind ADR-0057
// and ADR-0058, .scratch/plugin-system issue 12): a WebAssembly module and a
// manifest an Admin placed by hand under <dataDir>/plugins/<id>/, loaded at boot
// into the same registry the OpenSubtitles Built-in was registered into.
//
// Everything asserted here is what a person can observe — a settings screen, a
// player's "search online", a subtitle that plays, and a refusal an Admin can
// read. The module is compiled from source by the suite, so the whole path from
// the viewer's click to the guest and back runs across the real ABI in a real
// sandbox.
//
// The source these Plugins wrap is an httptest server the test configures as the
// provider's base URL, which is also what makes the fetch legal: it is the
// OPERATOR's target, and a target an operator typed is one this server will let
// a Plugin reach (ADR-0058 decision 5, as issue 09 amended it).

// --- the source a Plugin wraps -----------------------------------------------

// subtitleSourceServer is a stand-in for OpenSubtitles: a search that answers one
// candidate and a download that answers a SubRip file. Each instance answers with
// its OWN candidate id, so a test with two Installed providers can tell which one
// the host actually asked.
type subtitleSourceServer struct {
	srv        *httptest.Server
	candidate  string
	body       string
	mu         sync.Mutex
	searches   int
	downloads  int
	lastLang   string
	lastTitle  string
	lastYear   int
	lastHash   string
	lastAPIKey string
}

func newSubtitleSourceServer(t *testing.T, candidateID, cue string) *subtitleSourceServer {
	t.Helper()
	s := &subtitleSourceServer{
		candidate: candidateID,
		body:      "1\n00:00:01,000 --> 00:00:03,000\n" + cue + "\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		var got struct {
			Ref struct {
				Title     string `json:"title"`
				Year      int    `json:"year"`
				MovieHash string `json:"movieHash"`
			} `json:"ref"`
			Language string `json:"language"`
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		s.mu.Lock()
		s.searches++
		s.lastLang, s.lastHash, s.lastAPIKey = got.Language, got.Ref.MovieHash, r.Header.Get("Api-Key")
		s.lastTitle, s.lastYear = got.Ref.Title, got.Ref.Year
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subtitles":[{"id":"` + s.candidate + `","language":"` + got.Language +
			`","format":"srt","release":"Release.From.` + s.candidate + `","downloads":7}]}`))
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.downloads++
		s.mu.Unlock()
		_, _ = w.Write([]byte(s.body))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *subtitleSourceServer) seen() (searches, downloads int, lang, hash, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.searches, s.downloads, s.lastLang, s.lastHash, s.lastAPIKey
}

func (s *subtitleSourceServer) seenRef() (title string, year int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTitle, s.lastYear
}

// --- the settings view, with the Installed fields ----------------------------

// installedProviderResp is the subtitle-provider view with the four fields this
// slice adds. It is a separate decoding of the same response body, so the
// existing suite's shape is untouched and a client that has not heard of
// Installed plugins still reads it.
type installedProviderResp struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	RequiresKey bool   `json:"requiresKey"`
	Enabled     bool   `json:"enabled"`
	HasKey      bool   `json:"hasKey"`
	BaseURL     string `json:"baseURL"`
	Installed   bool   `json:"installed"`
	Disabled    bool   `json:"disabled"`
	LastError   string `json:"lastError"`
	Version     string `json:"version"`
}

type installedProvidersResp struct {
	Providers     []installedProviderResp `json:"providers"`
	AutoFetchLang string                  `json:"autoFetchLang"`
}

func readSubtitleProviders(t *testing.T, srv *testharness.Server, token string) installedProvidersResp {
	t.Helper()
	var resp installedProvidersResp
	status, body := srv.AuthGET("/api/v1/settings/subtitle-providers", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET subtitle-providers status = %d, want 200; body: %s", status, body)
	}
	return resp
}

func providerNamed(t *testing.T, resp installedProvidersResp, slug string) installedProviderResp {
	t.Helper()
	for _, p := range resp.Providers {
		if p.Slug == slug {
			return p
		}
	}
	t.Fatalf("no subtitle provider %q on the settings screen; got %+v", slug, resp.Providers)
	return installedProviderResp{}
}

// configureProvider PUTs one provider's settings through the ordinary Admin
// endpoint — the same endpoint, with the same body, an Admin uses for the
// OpenSubtitles Built-in.
func configureProvider(t *testing.T, srv *testharness.Server, token string, update map[string]any) installedProvidersResp {
	t.Helper()
	var resp installedProvidersResp
	body := map[string]any{"providers": []map[string]any{update}}
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/subtitle-providers", token, body, &resp)
	if status != http.StatusOK {
		t.Fatalf("PUT subtitle-providers status = %d, want 200; body: %s", status, raw)
	}
	return resp
}

// searchOnline is the player's "search online" for a language, which degrades to
// an empty list and NEVER to an error a viewer would see.
func searchOnline(t *testing.T, srv *testharness.Server, token, titleID, lang string) subtitleCandidatesResp {
	t.Helper()
	var resp subtitleCandidatesResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/subtitles/search", token,
		map[string]any{"language": lang}, &resp)
	if status != http.StatusOK {
		t.Fatalf("search-online status = %d, want 200; body: %s", status, body)
	}
	return resp
}

// --- criterion 1: end to end --------------------------------------------------

// TestAnInstalledSubtitleProviderServesSearchOnlineAndCaches is the acceptance
// criterion, whole: an Admin places two files, boots, sees the Plugin on the
// subtitle-providers screen beside OpenSubtitles, turns it on, and a viewer's
// "search online" is answered from inside the sandbox — then the picked candidate
// downloads, caches identity-keyed on the filesystem, records as `fetched` and
// plays as WebVTT, exactly as an OpenSubtitles one does.
func TestAnInstalledSubtitleProviderServesSearchOnlineAndCaches(t *testing.T) {
	requireFixtures(t)
	dataDir := t.TempDir()
	// The Admin's hand-placement, before the server ever starts. (Installing
	// through the API is issue 10's flow and its own tests; the loader reads a
	// directory either way.)
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("example-subs"))
	source := newSubtitleSourceServer(t, "9001", "Guten Tag")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	// On the screen, beside the Built-in, with the manifest's facts.
	view := readSubtitleProviders(t, srv, token)
	if len(view.Providers) != 2 {
		t.Fatalf("the screen lists %d providers, want OpenSubtitles and the Installed one: %+v", len(view.Providers), view.Providers)
	}
	if builtin := providerNamed(t, view, "opensubtitles"); builtin.Installed {
		t.Error("the OpenSubtitles Built-in is reported as an Installed plugin")
	}
	installed := providerNamed(t, view, "example-subs")
	switch {
	case !installed.Installed:
		t.Fatalf("the hand-placed Plugin is not reported as Installed: %+v", installed)
	case installed.Disabled || installed.LastError != "":
		t.Fatalf("a Plugin that loaded cleanly reads as %+v, want no error", installed)
	case installed.Name != "Test Subtitles (example-subs)" || installed.Version != "2.1.0":
		t.Fatalf("the screen shows %+v, want the manifest's name and version", installed)
	case !installed.RequiresKey:
		t.Fatalf("the manifest requires a secret but the screen does not say so: %+v", installed)
	case installed.BaseURL != "https://subs.example.test/v1":
		t.Fatalf("the screen shows base URL %q, want the manifest's default", installed.BaseURL)
	}

	// Before it is turned on, search-online degrades to nothing at all — and to no
	// outbound call (ADR-0001).
	if cands := searchOnline(t, srv, token, id, "de"); len(cands.Candidates) != 0 {
		t.Fatalf("a provider nobody enabled returned %d candidates", len(cands.Candidates))
	}
	if searches, _, _, _, _ := source.seen(); searches != 0 {
		t.Fatalf("the source was reached %d times before the provider was enabled", searches)
	}

	// Turned on through the SAME endpoint the Built-in uses, pointed at the
	// operator's own source.
	after := configureProvider(t, srv, token, map[string]any{
		"slug": "example-subs", "enabled": true, "apiKey": "sk-test", "baseURL": source.srv.URL,
	})
	if p := providerNamed(t, after, "example-subs"); !p.Enabled || !p.HasKey || p.BaseURL != source.srv.URL {
		t.Fatalf("after the save the screen shows %+v, want enabled with a key and the operator's URL", p)
	}

	// The viewer's search online, answered from inside the sandbox.
	cands := searchOnline(t, srv, token, id, "de")
	if len(cands.Candidates) != 1 {
		t.Fatalf("search-online returned %d candidates, want the Plugin's one: %+v", len(cands.Candidates), cands.Candidates)
	}
	c := cands.Candidates[0]
	if c.ID != "9001" || c.Language != "de" || c.Format != "srt" {
		t.Fatalf("candidate = %+v, want the source's 9001/de/srt", c)
	}
	// The match order, as this fixture can exercise it. The checked-in clip is
	// 18 KiB — below the 64 KiB floor the moviehash needs — and the Title is
	// un-enriched, so the host sends no hash and no imdb id and the Plugin says
	// which signal it fell back to. That the hash TRAVELS when there is one is
	// pinned exactly in internal/plugins/subtitle_test.go, which does not need a
	// media file to state it.
	if c.MatchedBy != "query" {
		t.Fatalf("matchedBy = %q, want query for a file too small to hash", c.MatchedBy)
	}
	_, _, lang, hash, key := source.seen()
	if lang != "de" {
		t.Fatalf("the source was asked for language %q, want the language the viewer typed", lang)
	}
	if hash != "" {
		t.Fatalf("the host sent a moviehash %q for a file it cannot hash", hash)
	}
	if title, year := source.seenRef(); title != "Dune" || year != 2021 {
		t.Fatalf("the parsed identity did not reach the source: %q (%d)", title, year)
	}
	if key != "sk-test" {
		t.Fatalf("the source saw api key %q, want the one the Admin saved — the settings ride with the call", key)
	}

	// The pick: downloaded, converted, cached, recorded as fetched.
	var fetched subtitleFetchResp
	pick := map[string]any{
		"language": "de",
		"candidate": map[string]any{
			"id": c.ID, "language": "de", "format": "srt",
		},
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/fetch", token, pick, &fetched)
	if status != http.StatusOK {
		t.Fatalf("fetch status = %d, want 200; body: %s", status, body)
	}
	if fetched.Subtitle.Source != "fetched" || fetched.Subtitle.Kind != "text" || fetched.Subtitle.Language != "de" {
		t.Fatalf("fetched subtitle = %+v, want fetched/text/de", fetched.Subtitle)
	}
	if _, downloads, _, _, _ := source.seen(); downloads != 1 {
		t.Fatalf("the source served %d downloads, want exactly one", downloads)
	}

	// It plays: valid WebVTT carrying the cue the source served.
	vttStatus, vtt := srv.AuthGET(fetched.Subtitle.URL, token, nil)
	if vttStatus != http.StatusOK {
		t.Fatalf("fetched .vtt status = %d, want 200", vttStatus)
	}
	if !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), "Guten Tag") {
		t.Fatalf("the fetched track is not the WebVTT the guest downloaded:\n%s", vtt)
	}

	// Cached identity-keyed on the FILESYSTEM under the data dir, named by the
	// Title id, the language and the provider's candidate id (ADR-0021/ADR-0007).
	cached := filepath.Join(dataDir, "subtitles", id+"-de-9001.srt")
	raw, err := os.ReadFile(cached)
	if err != nil {
		listing, _ := os.ReadDir(filepath.Join(dataDir, "subtitles"))
		t.Fatalf("no identity-keyed cache file at %s: %v (dir holds %v)", cached, err, listing)
	}
	if !strings.Contains(string(raw), "Guten Tag") {
		t.Fatalf("the cache holds something other than the downloaded SubRip:\n%s", raw)
	}

	// It is in the Title, and a rescan does not take it away.
	assertFetchedTrackListed(t, srv, token, id)
	scanLib(t, srv, token, libID, "")
	assertFetchedTrackListed(t, srv, token, id)
}

// --- criterion 2: the byte cap ------------------------------------------------

// TestAnInstalledSubtitleProviderOverTheByteCapIsRefused: the cap is the HOST's,
// so a guest that answers with more than it was told it could gets its download
// refused whole — nothing is cached, nothing is recorded as fetched, the failure
// reaches the Admin's screen, and the Title's own sidecar and embedded subtitles
// are exactly as they were.
func TestAnInstalledSubtitleProviderOverTheByteCapIsRefused(t *testing.T) {
	requireSubtitleFixtures(t)
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("greedy-subs"))
	source := newSubtitleSourceServer(t, "7777", "Guten Tag")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	root, err := filepath.Abs(filepath.Join("testdata", subtitlesRootRel))
	if err != nil {
		t.Fatalf("resolving the subtitle fixture root: %v", err)
	}
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Sub Movie")

	before := titleSubtitleSources(t, srv, token, id)
	if before["embedded"] == 0 || before["sidecar"] == 0 {
		t.Fatalf("the fixture has no local subtitles to defend: %v", before)
	}

	// The secret carries the mode marker that makes this guest answer with one
	// byte more than the host said it would take. A well-behaved Plugin cannot
	// demonstrate this: the host's fetch cap already refuses an oversize body on
	// the way IN, so the bytes have to be invented inside the guest.
	configureProvider(t, srv, token, map[string]any{
		"slug": "greedy-subs", "enabled": true,
		"apiKey": "sk-test;obelo-mode=oversize", "baseURL": source.srv.URL,
	})

	cands := searchOnline(t, srv, token, id, "ja")
	if len(cands.Candidates) != 1 {
		t.Fatalf("search-online returned %d candidates, want one to pick", len(cands.Candidates))
	}

	var fetched subtitleFetchResp
	pick := map[string]any{
		"language":  "ja",
		"candidate": map[string]any{"id": cands.Candidates[0].ID, "language": "ja", "format": "srt"},
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/subtitles/fetch", token, pick, &fetched)
	if status == http.StatusOK {
		t.Fatalf("an oversize download was accepted: %+v", fetched.Subtitle)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("oversize fetch status = %d, want 500 (a refusal, not a not-found); body: %s", status, body)
	}

	// Nothing was stored. Not a truncated file, not a row, not a cache entry — a
	// truncated subtitle is one the host would serve as though it were whole.
	after := titleSubtitleSources(t, srv, token, id)
	if after["fetched"] != 0 {
		t.Fatalf("a refused download still produced %d fetched tracks", after["fetched"])
	}
	if after["embedded"] != before["embedded"] || after["sidecar"] != before["sidecar"] {
		t.Fatalf("the local subtitles changed under a refused fetch: before %v, after %v", before, after)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "subtitles")); err == nil && len(entries) != 0 {
		t.Fatalf("the refused download left %d files in the subtitle cache", len(entries))
	}

	// And the Admin is told, on the screen they turned it on from.
	p := providerNamed(t, readSubtitleProviders(t, srv, token), "greedy-subs")
	if !strings.Contains(p.LastError, "more than the") || !strings.Contains(p.LastError, "bytes") {
		t.Fatalf("the settings screen does not say the answer was over the cap: lastError = %q", p.LastError)
	}
	if !p.Enabled {
		t.Errorf("the Admin's enabled toggle was flipped by a plugin failure: %+v", p)
	}
}

// titleSubtitleSources counts a Title's subtitle tracks by source, which is how a
// viewer's captions menu is built — and therefore what "the sidecar and embedded
// subtitles still win at serve time" means from the outside.
func titleSubtitleSources(t *testing.T, srv *testharness.Server, token, id string) map[string]int {
	t.Helper()
	var detail subtitleDetailResp
	status, body := srv.JSON(http.MethodGet, "/api/v1/titles/"+id, token, nil, &detail)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	out := map[string]int{}
	for _, s := range detail.Subtitles {
		out[s.Source]++
	}
	return out
}

// --- criterion 3: disabling degrades silently ---------------------------------

// TestDisablingAnInstalledSubtitleProviderFallsBackToTheRest: the Admin's enabled
// toggle removes the Plugin from the provider the Manager composes on Reload, so
// search-online falls through to whatever else is configured — and when nothing
// is, it answers an empty list with a 200 rather than an error a viewer would be
// shown (ADR-0001).
func TestDisablingAnInstalledSubtitleProviderFallsBackToTheRest(t *testing.T) {
	requireFixtures(t)
	dataDir := t.TempDir()
	// Two Installed providers, from ONE module: the manifest is what says which
	// seam a Plugin fills, so the same wasm serves as both. "alpha" sorts before
	// "beta", and the composed provider takes the first enabled row.
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("alpha-subs"))
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("beta-subs"))
	alpha := newSubtitleSourceServer(t, "alpha-1", "Von Alpha")
	beta := newSubtitleSourceServer(t, "beta-1", "Von Beta")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	configureProvider(t, srv, token, map[string]any{
		"slug": "alpha-subs", "enabled": true, "apiKey": "sk-a", "baseURL": alpha.srv.URL,
	})
	configureProvider(t, srv, token, map[string]any{
		"slug": "beta-subs", "enabled": true, "apiKey": "sk-b", "baseURL": beta.srv.URL,
	})

	if cands := searchOnline(t, srv, token, id, "de"); len(cands.Candidates) != 1 || cands.Candidates[0].ID != "alpha-1" {
		t.Fatalf("search-online answered %+v, want the first enabled provider's candidate", cands.Candidates)
	}

	// Turn the first one off. The Manager rebuilds on the save, so the next search
	// reaches the one that is left — with no restart and nothing surfaced.
	configureProvider(t, srv, token, map[string]any{"slug": "alpha-subs", "enabled": false})
	cands := searchOnline(t, srv, token, id, "de")
	if len(cands.Candidates) != 1 || cands.Candidates[0].ID != "beta-1" {
		t.Fatalf("after disabling alpha, search-online answered %+v, want beta's candidate", cands.Candidates)
	}
	if searches, _, _, _, _ := beta.seen(); searches == 0 {
		t.Fatal("the remaining provider was never asked")
	}

	// Turn the last one off too: nothing answers, and a viewer sees an empty
	// picker rather than a failure.
	configureProvider(t, srv, token, map[string]any{"slug": "beta-subs", "enabled": false})
	if cands := searchOnline(t, srv, token, id, "de"); len(cands.Candidates) != 0 {
		t.Fatalf("with every provider disabled search-online returned %+v, want nothing", cands.Candidates)
	}

	// A disabled Plugin is still LISTED, with its settings intact: switching a
	// provider off is not uninstalling it.
	p := providerNamed(t, readSubtitleProviders(t, srv, token), "alpha-subs")
	if !p.Installed || p.Enabled || !p.HasKey || p.Disabled || p.LastError != "" {
		t.Fatalf("a provider an Admin switched off reads as %+v, want listed, off, with its key and no error", p)
	}
}
