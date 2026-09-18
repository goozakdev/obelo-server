package tmdb

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// Search — the Edit-item Enrichment-override picker's candidate list (ADR-0019).
// Ported from internal/enrich/search_test.go's TMDB half, assertions intact.

const movieSearchJSON = `{"results": [
  {"id": 438631, "title": "Dune", "release_date": "2021-10-22",
   "overview": "Paul Atreides leads a desert rebellion.", "poster_path": "/dune21.jpg"},
  {"id": 841, "title": "Dune", "release_date": "1984-12-14",
   "overview": "A Duke's son leads desert warriors.", "poster_path": "/dune84.jpg"}
]}`

const tvSearchJSON = `{"results": [
  {"id": 1399, "name": "Game of Thrones", "first_air_date": "2011-04-17",
   "overview": "Noble families vie for the Iron Throne.", "poster_path": "/got.jpg"}
]}`

// searchStub answers one search path with one document and 404s everything else.
func searchStub(t *testing.T, path, body string) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == path {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
				return
			}
			http.NotFound(w, r)
		}),
	)
	return New(host), host
}

// TestTMDBSearchMovieParsesCandidates: a movie query hits /search/movie and maps
// each result into a candidate with the id, title, year, poster thumbnail, and
// the overview as the disambiguation hint.
func TestTMDBSearchMovieParsesCandidates(t *testing.T) {
	p, host := searchStub(t, "/search/movie", movieSearchJSON)

	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "Dune"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if seen := host.Paths(); len(seen) != 1 || seen[0] != "/search/movie" {
		t.Fatalf("expected one /search/movie call, saw %v", seen)
	}
	cands := resp.Candidates
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	c := cands[0]
	if c.ExternalID != "438631" || c.Title != "Dune" || c.Year != 2021 || c.Kind != "movie" {
		t.Errorf("candidate[0] = %+v", c)
	}
	if c.ThumbnailURL != "https://img//dune21.jpg" {
		t.Errorf("thumbnail = %q", c.ThumbnailURL)
	}
	if !strings.Contains(c.Disambiguation, "desert rebellion") {
		t.Errorf("disambiguation = %q", c.Disambiguation)
	}
	if cands[1].Year != 1984 {
		t.Errorf("candidate[1] year = %d, want 1984", cands[1].Year)
	}
}

// TestTMDBSearchTVForEpisodeKind: an Episode search targets /search/tv (an
// episode is corrected by re-pointing at its show) and parses the TV
// name/first_air_date.
func TestTMDBSearchTVForEpisodeKind(t *testing.T) {
	p, host := searchStub(t, "/search/tv", tvSearchJSON)

	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "episode", Query: "Game of Thrones"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if seen := host.Paths(); len(seen) != 1 || seen[0] != "/search/tv" {
		t.Fatalf("expected one /search/tv call, saw %v", seen)
	}
	cands := resp.Candidates
	if len(cands) != 1 || cands[0].Title != "Game of Thrones" || cands[0].Year != 2011 || cands[0].Kind != "episode" {
		t.Fatalf("tv candidate unexpected: %+v", cands)
	}
}

// TestTMDBSearchEmptyQueryNoCall: a blank query short-circuits with no candidates
// and no fetch.
func TestTMDBSearchEmptyQueryNoCall(t *testing.T) {
	p, host := searchStub(t, "/search/movie", movieSearchJSON)

	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "   "})
	if err != nil || len(resp.Candidates) != 0 {
		t.Fatalf("empty query = (%+v, %v), want no candidates and no error", resp, err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Errorf("outcome = %q, want matched (nothing found is not unavailable)", resp.Outcome)
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("empty query issued a fetch: %v", seen)
	}
}

// A kind this source does not own is the Edit-item box's "this kind cannot be
// searched right now", produced with no fetch.
func TestTMDBSearchUnsupportedKindIsUnavailable(t *testing.T) {
	p, host := searchStub(t, "/search/movie", movieSearchJSON)

	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "album", Query: "Nevermind"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", resp.Outcome)
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("an unsupported kind issued a fetch: %v", seen)
	}
}

// The picker's "show more": an offset becomes TMDB's 1-based page, and the first
// page carries no page parameter at all.
func TestTMDBSearchOffsetBecomesAPage(t *testing.T) {
	p, host := searchStub(t, "/search/movie", movieSearchJSON)

	if _, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "movie", Query: "Dune"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := host.Requests()[0].URL; strings.Contains(got, "page=") {
		t.Errorf("the first page carried a page parameter: %s", got)
	}
	req := pluginapi.SearchRequest{Kind: "movie", Query: "Dune"}
	req.Offset = 40
	if _, err := p.Search(context.Background(), req); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := host.Requests()[1].URL; !strings.Contains(got, "page=3") {
		t.Errorf("offset 40 asked for %s, want page=3", got)
	}
}
