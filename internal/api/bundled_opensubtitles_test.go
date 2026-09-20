package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// OpenSubtitles as a Bundled plugin (.scratch/bundled-plugins issue 09): the REAL
// module, built from plugins/opensubtitles, through the real sandbox, against a
// stand-in for OpenSubtitles' REST v1 API on loopback. Nothing here is a fake of
// the plugin; the stand-in is the only thing that is not production code.
//
// The stand-in is the operator's configured base URL, and its download links
// point back at the same host. That is load-bearing: a fetch to the host the
// operator typed is permitted whatever the manifest allowlists, which is what
// lets a loopback stand-in exist at all (internal/plugins/hostfuncs.go).

// openSubtitlesStandIn answers /api/v1/subtitles with one candidate, POST
// /api/v1/download with a link on itself (or with the status a test set), and the
// link with body.
type openSubtitlesStandIn struct {
	srv *httptest.Server

	mu          sync.Mutex
	body        string
	dlStatus    int    // non-zero: POST /download answers this instead of a link
	linkPath    string // what the link points at; "/files/sub.srt" unless set
	searches    int
	downloads   int
	fileHits    int
	lastAPIKey  string
	lastAgent   string
	lastLangArg string
}

func newOpenSubtitlesStandIn(t *testing.T, body string) *openSubtitlesStandIn {
	t.Helper()
	s := &openSubtitlesStandIn{body: body, linkPath: "/files/sub.srt"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/subtitles", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.searches++
		s.lastAPIKey, s.lastAgent = r.Header.Get("Api-Key"), r.Header.Get("User-Agent")
		s.lastLangArg = r.URL.Query().Get("languages")
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"attributes":{"language":"pt-BR","download_count":12,
"release":"Dune.2021.1080p","files":[{"file_id":4242,"file_name":"Dune.2021.srt"}]}}]}`))
	})
	mux.HandleFunc("/api/v1/download", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.downloads++
		status, link := s.dlStatus, s.srv.URL+s.linkPath
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"You have downloaded your allowed 5 subtitles for 24h."}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"link":%q,"file_name":"Dune.2021.srt"}`, link)
	})
	mux.HandleFunc("/files/sub.srt", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.fileHits++
		body := s.body
		s.mu.Unlock()
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/files/inward.srt", func(w http.ResponseWriter, r *http.Request) {
		// A link that bounces to loopback. A hostile provider would aim it at
		// 127.0.0.1:<obelo>, a LAN NAS or 169.254.169.254; the Built-in refused
		// this hop through safefetch, and the host's fetch policy must still.
		http.Redirect(w, r, s.srv.URL+"/files/sub.srt", http.StatusFound)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *openSubtitlesStandIn) set(fn func(s *openSubtitlesStandIn)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *openSubtitlesStandIn) snapshot() openSubtitlesStandIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return openSubtitlesStandIn{
		searches: s.searches, downloads: s.downloads, fileHits: s.fileHits,
		lastAPIKey: s.lastAPIKey, lastAgent: s.lastAgent, lastLangArg: s.lastLangArg,
	}
}

// bundledOpenSubtitlesServer boots a server, scans the movie fixtures, points the
// bundled OpenSubtitles plugin at the stand-in exactly as an Admin would, and
// returns the Title to search for.
func bundledOpenSubtitlesServer(t *testing.T, stand *openSubtitlesStandIn) (*testharness.Server, string, string) {
	t.Helper()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	id := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	after := configureProvider(t, srv, token, map[string]any{
		"slug": "opensubtitles", "enabled": true, "apiKey": "os-key", "baseURL": stand.srv.URL + "/api/v1",
	})
	if p := providerNamed(t, after, "opensubtitles"); !p.Enabled || !p.HasKey || !p.Installed {
		t.Fatalf("after the save the screen shows %+v, want the bundled plugin enabled with a key", p)
	}
	return srv, token, id
}

func pickOpenSubtitlesCandidate(t *testing.T, srv *testharness.Server, token, titleID string) (int, subtitleFetchResp, []byte) {
	t.Helper()
	var fetched subtitleFetchResp
	pick := map[string]any{
		"language":  "pt",
		"candidate": map[string]any{"id": "4242", "language": "pt", "format": "srt"},
	}
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/subtitles/fetch", token, pick, &fetched)
	return status, fetched, body
}

// bigSRT is a valid SubRip document of at least n bytes.
func bigSRT(n int) string {
	var b strings.Builder
	for i := 1; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%d\n00:%02d:%02d,000 --> 00:%02d:%02d,500\nOlá, mundo — linha %d de uma legenda muito longa.\n\n",
			i, (i/60)%60, i%60, (i/60)%60, i%60, i)
	}
	return b.String()
}

// TestBundledOpenSubtitlesSearchesAndFetchesThroughTheSandbox is the port's
// acceptance test: an Admin's saved key and URL reach the guest, the viewer's
// search online is answered from inside the sandbox with the Built-in's request
// and the Built-in's candidate, and a pick downloads, converts and plays.
//
// The subtitle is bigger than a MEBIBYTE on purpose. A plugin's default fetch
// limit is 1 MiB, and the Built-in took up to 8; the manifest asks for 8
// (maxFetchBytes), and until issue 09 the host honoured that only on a Metadata
// provider's entry. A 1.5 MiB subtitle downloading at all is the proof it now
// reaches the subtitle seam.
func TestBundledOpenSubtitlesSearchesAndFetchesThroughTheSandbox(t *testing.T) {
	stand := newOpenSubtitlesStandIn(t, bigSRT(1536<<10))
	srv, token, id := bundledOpenSubtitlesServer(t, stand)

	cands := searchOnline(t, srv, token, id, "pt")
	if len(cands.Candidates) != 1 {
		t.Fatalf("search-online returned %d candidates, want the stand-in's one: %+v", len(cands.Candidates), cands.Candidates)
	}
	c := cands.Candidates[0]
	// "pt-BR" from the source, "pt" on the wire: the host normalizes, as the
	// Built-in did for itself.
	if c.ID != "4242" || c.Language != "pt" || c.Format != "srt" || c.MatchedBy != "query" {
		t.Fatalf("candidate = %+v, want 4242/pt/srt matched by query", c)
	}
	seen := stand.snapshot()
	if seen.lastAPIKey != "os-key" {
		t.Errorf("the source saw Api-Key %q, want the one the Admin saved", seen.lastAPIKey)
	}
	if seen.lastLangArg != "pt" {
		t.Errorf("the source was asked for languages=%q, want pt", seen.lastLangArg)
	}
	if !strings.Contains(seen.lastAgent, "plugin/opensubtitles/") {
		t.Errorf("User-Agent = %q, want the host's, naming the plugin (ADR-0059 decision 7)", seen.lastAgent)
	}

	status, fetched, body := pickOpenSubtitlesCandidate(t, srv, token, id)
	if status != http.StatusOK {
		t.Fatalf("fetch status = %d, want 200 for a 1.5 MiB subtitle under the manifest's 8 MiB; body: %s", status, body)
	}
	if fetched.Subtitle.Source != "fetched" || fetched.Subtitle.Language != "pt" {
		t.Fatalf("fetched subtitle = %+v, want fetched/pt", fetched.Subtitle)
	}
	vttStatus, vtt := srv.AuthGET(fetched.Subtitle.URL, token, nil)
	if vttStatus != http.StatusOK || !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), "linha 1 de") {
		t.Fatalf("the fetched track (%d) is not the WebVTT the plugin downloaded:\n%.200s", vttStatus, vtt)
	}

	// And Test connection reaches the stand-in through the same guest.
	var probe struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	status, body = srv.JSON(http.MethodPost, "/api/v1/settings/subtitle-providers/opensubtitles/test", token, nil, &probe)
	if status != http.StatusOK || !probe.OK {
		t.Fatalf("Test connection = %d %+v, want ok; body: %s", status, probe, body)
	}
}

// TestBundledOpenSubtitlesSpentQuotaDoesNotDisableThePlugin: OpenSubtitles meters
// downloads per account per day and answers the one-too-many with a 406. Under
// the plain rule a 4xx is a Go error, a Go error from a Subtitle provider is a
// strike, and three strikes disable the plugin — so three viewers pressing
// "download" on a busy evening would take subtitles off the server until an
// Admin noticed. The plugin answers it as unavailable instead, which costs
// nothing; the Built-in, which had no strikes, simply failed the request.
func TestBundledOpenSubtitlesSpentQuotaDoesNotDisableThePlugin(t *testing.T) {
	stand := newOpenSubtitlesStandIn(t, "1\n00:00:01,000 --> 00:00:02,000\nOlá\n")
	stand.set(func(s *openSubtitlesStandIn) { s.dlStatus = 406 })
	srv, token, id := bundledOpenSubtitlesServer(t, stand)

	for i := 0; i < 4; i++ {
		if status, _, body := pickOpenSubtitlesCandidate(t, srv, token, id); status == http.StatusOK {
			t.Fatalf("download %d succeeded against a spent quota; body: %s", i+1, body)
		}
	}
	if seen := stand.snapshot(); seen.downloads != 4 {
		t.Fatalf("the source saw %d download requests, want 4 — the plugin stopped being asked", seen.downloads)
	}
	if p := providerNamed(t, readSubtitleProviders(t, srv, token), "opensubtitles"); p.Disabled {
		t.Fatalf("a spent download quota DISABLED the plugin: %+v", p)
	}

	// Tomorrow: the quota is back and the same plugin serves the download.
	stand.set(func(s *openSubtitlesStandIn) { s.dlStatus = 0 })
	if status, _, body := pickOpenSubtitlesCandidate(t, srv, token, id); status != http.StatusOK {
		t.Fatalf("fetch after the quota reset = %d, want 200; body: %s", status, body)
	}
}

// TestBundledOpenSubtitlesRefusesAnInwardRedirect: the Built-in's safefetch
// guarantee, kept by the host now. The download link is the PROVIDER'S string;
// a redirect off it into private address space is refused, and nothing it would
// have served reaches the subtitle cache.
func TestBundledOpenSubtitlesRefusesAnInwardRedirect(t *testing.T) {
	stand := newOpenSubtitlesStandIn(t, "1\n00:00:01,000 --> 00:00:02,000\nOlá\n")
	stand.set(func(s *openSubtitlesStandIn) { s.linkPath = "/files/inward.srt" })
	srv, token, id := bundledOpenSubtitlesServer(t, stand)

	if status, _, body := pickOpenSubtitlesCandidate(t, srv, token, id); status == http.StatusOK {
		t.Fatalf("a download that redirected inward succeeded; body: %s", body)
	}
	if seen := stand.snapshot(); seen.fileHits != 0 {
		t.Fatalf("the redirect was followed: the inward target was reached %d times", seen.fileHits)
	}
}
