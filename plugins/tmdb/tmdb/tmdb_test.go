package tmdb

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// This provider's HTTP/parse layer, exercised against canned TMDB JSON served
// in memory by sdktest.Host — the port of internal/enrich/tmdb_test.go, with
// every assertion intact. The httptest handlers came across verbatim; what
// changed is that there is no server and no port, because the provider asks its
// Host rather than a net/http client. No live network is ever touched.

// The operator's two URLs, as the host resolves them for a call. The API base
// carries no path so an asserted request path reads exactly as it did when these
// tests ran against an httptest.Server; the image base keeps its trailing slash,
// which is why every expected artwork URL below has two.
const (
	apiBase   = "https://api.themoviedb.org"
	imageBase = "https://img/"
)

// settings is what the host publishes for one call.
func settings() pluginapi.Settings {
	return pluginapi.Settings{Enabled: true, Secret: "k", Language: "en-US", URL: apiBase, URL2: imageBase}
}

const movieDetailsJSON = `{
  "id": 12345,
  "title": "Dune",
  "overview": "Paul Atreides leads a desert rebellion.",
  "tagline": "Fear is the mind-killer.",
  "release_date": "2021-10-22",
  "runtime": 155,
  "genres": [{"name": "Science Fiction"}, {"name": "Adventure"}],
  "production_companies": [{"name": "Legendary"}, {"name": "Warner Bros."}],
  "poster_path": "/poster.jpg",
  "backdrop_path": "/backdrop.jpg",
  "credits": {"cast": [
     {"name": "Timothée Chalamet", "character": "Paul Atreides"},
     {"name": "Rebecca Ferguson", "character": "Lady Jessica"}
  ]},
  "release_dates": {"results": [
     {"iso_3166_1": "GB", "release_dates": [{"certification": "12A"}]},
     {"iso_3166_1": "US", "release_dates": [{"certification": "PG-13"}]}
  ]},
  "images": {"logos": [
     {"file_path": "/logo.svg", "width": 800, "height": 310},
     {"file_path": "/logo.png", "width": 800, "height": 310},
     {"file_path": "/logo-alt.png", "width": 400, "height": 155}
  ]}
}`

// stub serves the movie-details endpoint and a search endpoint, and hands back
// the Host so a test can read the paths it asked for — the `seen []string` the
// original tests kept by hand.
func stub(t *testing.T, searchID string) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/movie/"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(movieDetailsJSON))
			case r.URL.Path == "/search/movie":
				w.Header().Set("Content-Type", "application/json")
				if searchID == "" {
					_, _ = w.Write([]byte(`{"results": []}`))
					return
				}
				_, _ = w.Write([]byte(`{"results": [{"id": ` + searchID + `}]}`))
			default:
				http.NotFound(w, r)
			}
		}),
	)
	return New(host), host
}

// lookup runs one lookup and fails the test on a transport error, so each case
// below reads as it did when Lookup returned a record and an error.
func lookup(t *testing.T, p *Provider, ref pluginapi.MediaRef) pluginapi.LookupResponse {
	t.Helper()
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return resp
}

func TestTMDBLookupByID(t *testing.T) {
	p, host := stub(t, "")
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021, TMDBID: "12345"})
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	rec := resp.Record
	if !rec.Matched {
		t.Fatal("not matched")
	}
	if rec.Overview == "" || rec.Tagline != "Fear is the mind-killer." {
		t.Errorf("scalar fields wrong: %+v", rec)
	}
	if rec.RuntimeMinutes != 155 || rec.ReleaseDate != "2021-10-22" {
		t.Errorf("runtime/release wrong: %+v", rec)
	}
	// Canonical title + year are surfaced for by-id identity resolution.
	if rec.Name != "Dune" || rec.Year != 2021 {
		t.Errorf("canonical title/year = %q/%d, want Dune/2021", rec.Name, rec.Year)
	}
	if rec.ContentRating != "PG-13" {
		t.Errorf("content rating = %q, want PG-13 (US certification)", rec.ContentRating)
	}
	if rec.Studio != "Legendary" {
		t.Errorf("studio = %q, want Legendary (first production company)", rec.Studio)
	}
	if len(rec.Genres) != 2 || rec.Genres[0] != "Science Fiction" {
		t.Errorf("genres = %v", rec.Genres)
	}
	if len(rec.Cast) != 2 || rec.Cast[0].Character != "Paul Atreides" {
		t.Errorf("cast wrong: %+v", rec.Cast)
	}
	if rec.Source != "tmdb" || rec.ExternalID != "12345" {
		t.Errorf("source/id = %q/%q, want tmdb/12345", rec.Source, rec.ExternalID)
	}
	// Artwork URLs are the image base + path; poster + backdrop both present, and
	// the first NON-SVG appended-images logo rides along as the auto-applied logo
	// (TMDB mixes SVG and PNG renditions; the pipeline is raster-only).
	roles := map[string]string{}
	for _, a := range rec.Artwork {
		roles[a.Role] = a.URL
	}
	if roles["poster"] != "https://img//poster.jpg" || roles["background"] != "https://img//backdrop.jpg" {
		t.Errorf("artwork urls wrong: %+v", rec.Artwork)
	}
	if roles["logo"] != "https://img//logo.png" {
		t.Errorf("logo url = %q, want https://img//logo.png", roles["logo"])
	}
	// By-id: it went straight to /movie/{id}, never searched.
	for _, path := range host.Paths() {
		if strings.HasPrefix(path, "/search") {
			t.Errorf("searched despite an embedded id: %v", host.Paths())
		}
	}
}

func TestTMDBLookupBySearch(t *testing.T) {
	p, host := stub(t, "12345")
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "Dune", Year: 2021})
	if resp.Outcome != pluginapi.OutcomeMatched || resp.Record.Overview == "" {
		t.Errorf("search lookup did not resolve: %+v", resp)
	}
	sawSearch, sawDetails := false, false
	for _, path := range host.Paths() {
		if strings.HasPrefix(path, "/search") {
			sawSearch = true
		}
		if strings.HasPrefix(path, "/movie/") {
			sawDetails = true
		}
	}
	if !sawSearch || !sawDetails {
		t.Errorf("expected a search THEN a details fetch; paths=%v", host.Paths())
	}
}

func TestTMDBSearchNoResultsIsNoMatch(t *testing.T) {
	p, _ := stub(t, "") // empty search results
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "Nonexistent", Year: 1900})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", resp.Outcome)
	}
}

func TestTMDBNonMovieKindIsNoMatch(t *testing.T) {
	p, _ := stub(t, "12345")
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "track", Title: "x"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("track lookup outcome = %q, want no-match (video-only source)", resp.Outcome)
	}
}

// The API key and the language the HOST resolved reach every request, and they
// are read PER CALL rather than held: a key rotation (ADR-0032) takes effect on
// the next lookup without the plugin being rebuilt.
func TestTMDBReadsItsSettingsOnEveryCall(t *testing.T) {
	p, host := stub(t, "")
	lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"})

	next := settings()
	next.Secret = "rotated"
	host.SetSettings(next)
	lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "12345"})

	reqs := host.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if !strings.Contains(reqs[0].URL, "api_key=k") || !strings.Contains(reqs[0].URL, "language=en-US") {
		t.Errorf("first request carried neither the key nor the language: %s", reqs[0].URL)
	}
	if !strings.Contains(reqs[1].URL, "api_key=rotated") {
		t.Errorf("second request did not carry the rotated key: %s", reqs[1].URL)
	}
}

func TestYearFromDate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"2021-10-22", 2021},
		{"1984", 1984},
		{"", 0},
		{"20", 0},
		{"not-a-date", 0},
	} {
		if got := YearFromDate(tc.in); got != tc.want {
			t.Errorf("YearFromDate(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
