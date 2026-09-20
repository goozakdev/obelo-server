package omdb

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// This provider's HTTP/parse layer, exercised against canned OMDb JSON served in
// memory by sdktest.Host — the port of internal/enrich/omdb_test.go, with every
// assertion intact. The httptest handler came across verbatim; what changed is
// that there is no server and no port, because the provider asks its Host rather
// than a net/http client. No live network is ever touched.

// apiBase is the operator's OMDb base URL as the host resolves it for a call. It
// carries no path, so the request this provider builds — base + "/?" + query —
// reads exactly as it did when these tests ran against an httptest.Server.
const apiBase = "https://www.omdbapi.com"

// settings is what the host publishes for one call.
func settings() pluginapi.Settings {
	return pluginapi.Settings{Enabled: true, Secret: "k", Language: "en-US", URL: apiBase}
}

const movieJSON = `{
  "Title": "The Shawshank Redemption",
  "Rated": "R",
  "Genre": "Drama, Crime",
  "Plot": "Two imprisoned men bond over a number of years.",
  "Response": "True"
}`

// stub serves the OMDb endpoint with canned JSON and hands back the Host, whose
// Requests() carry the raw query each call asked for — the `seen []string` the
// original tests kept by hand.
func stub(t *testing.T, body string) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}),
	)
	return New(host), host
}

// queries is the raw query string of every fetch, in order.
func queries(t *testing.T, host *sdktest.Host) []string {
	t.Helper()
	var out []string
	for _, req := range host.Requests() {
		u, err := url.Parse(req.URL)
		if err != nil {
			t.Fatalf("the provider built an unparseable URL %q: %v", req.URL, err)
		}
		out = append(out, u.RawQuery)
	}
	return out
}

// lookup runs one lookup and fails the test on a Go error, so each case below
// reads as it did when Lookup returned a record and an error.
func lookup(t *testing.T, p *Provider, ref pluginapi.MediaRef) pluginapi.LookupResponse {
	t.Helper()
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return resp
}

func TestOMDbResolvesByIMDbID(t *testing.T) {
	p, host := stub(t, movieJSON)

	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "Shawshank", IMDBID: "tt0111161"})
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	got := resp.Record
	if got.Source != Source || !got.Matched {
		t.Errorf("source/matched = %q/%v, want omdb/true", got.Source, got.Matched)
	}
	if got.Overview != "Two imprisoned men bond over a number of years." {
		t.Errorf("overview = %q", got.Overview)
	}
	if got.ContentRating != "R" {
		t.Errorf("content rating = %q, want R", got.ContentRating)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Drama" || got.Genres[1] != "Crime" {
		t.Errorf("genres = %v, want [Drama Crime]", got.Genres)
	}
	// It resolved by IMDb id (i=), not a title search.
	seen := queries(t, host)
	if len(seen) != 1 || seen[0] != "apikey=k&i=tt0111161" {
		t.Errorf("query = %v, want a single i=tt0111161 lookup", seen)
	}
}

func TestOMDbResolvesByTitleYearWhenNoIMDbID(t *testing.T) {
	p, host := stub(t, movieJSON)

	lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "The Shawshank Redemption", Year: 1994})
	seen := queries(t, host)
	if len(seen) != 1 || seen[0] != "apikey=k&t=The+Shawshank+Redemption&y=1994" {
		t.Errorf("query = %v, want a t=/y= title lookup", seen)
	}
}

func TestOMDbTreatsNAAsEmpty(t *testing.T) {
	// A record that resolves but carries only "N/A" fields contributes nothing —
	// the fill-only supplement reports it as a no-match.
	p, _ := stub(t, `{"Rated":"N/A","Genre":"N/A","Plot":"N/A","Response":"True"}`)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match (all fields N/A)", resp.Outcome)
	}
}

func TestOMDbSplitsGenresAndDropsNA(t *testing.T) {
	p, _ := stub(t, `{"Genre":"Action, N/A, Sci-Fi","Response":"True"}`)
	got := lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1"}).Record
	if len(got.Genres) != 2 || got.Genres[0] != "Action" || got.Genres[1] != "Sci-Fi" {
		t.Errorf("genres = %v, want [Action Sci-Fi] (N/A dropped)", got.Genres)
	}
}

func TestOMDbResponseFalseIsNoMatch(t *testing.T) {
	p, _ := stub(t, `{"Response":"False","Error":"Movie not found!"}`)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt404"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match for Response:False", resp.Outcome)
	}
}

// A 500 was "a real (non-ErrNoMatch) error" for the Go provider, and the pass
// above read enrich.ErrTransient off it to retry the item. A guest cannot wrap a
// server sentinel, so the same decision travels as the OUTCOME: unavailable, with
// no Go error for the host to count as a strike. See failure_test.go for the
// whole table.
func TestOMDbNon2xxIsNotAMatch(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "server on fire", http.StatusInternalServerError)
		}),
	)
	p := New(host)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1"},
	})
	if err != nil {
		t.Fatalf("a 500 became a Go error, which the host counts against the plugin: %v", err)
	}
	if resp.Outcome == pluginapi.OutcomeNoMatch || resp.Outcome == pluginapi.OutcomeMatched {
		t.Errorf("outcome = %q, want unavailable on a 500 (never a claim about the movie)", resp.Outcome)
	}
}

func TestOMDbNonMovieKindIsNoMatch(t *testing.T) {
	p, host := stub(t, movieJSON)
	for _, kind := range []string{"show", "season", "episode", "artist"} {
		resp := lookup(t, p, pluginapi.MediaRef{Kind: kind, IMDBID: "tt1"})
		if resp.Outcome != pluginapi.OutcomeNoMatch {
			t.Errorf("kind %q: outcome = %q, want no-match (OMDb serves movies only)", kind, resp.Outcome)
		}
	}
	if seen := host.Requests(); len(seen) != 0 {
		t.Errorf("OMDb hit for a non-movie kind (%v); want zero requests", seen)
	}
}

func TestOMDbCachesRepeatLookup(t *testing.T) {
	p, host := stub(t, movieJSON)
	ref := pluginapi.MediaRef{Kind: "movie", IMDBID: "tt0111161"}

	lookup(t, p, ref)
	lookup(t, p, ref)
	// The instance response cache means the repeat lookup does not re-hit the host.
	if got := len(host.Requests()); got != 1 {
		t.Errorf("the source was hit %d times, want 1 (repeat served from cache)", got)
	}
}

// A no-match is cached too — the Go provider stored the zero result for it, so a
// movie OMDb has never heard of costs one request per instance and not one per
// enrichment pass.
func TestOMDbCachesANoMatch(t *testing.T) {
	p, host := stub(t, `{"Response":"False","Error":"Movie not found!"}`)
	ref := pluginapi.MediaRef{Kind: "movie", IMDBID: "tt404"}

	for i := 0; i < 3; i++ {
		if resp := lookup(t, p, ref); resp.Outcome != pluginapi.OutcomeNoMatch {
			t.Fatalf("lookup %d: outcome = %q, want no-match", i, resp.Outcome)
		}
	}
	if got := len(host.Requests()); got != 1 {
		t.Errorf("the source was hit %d times for a known no-match, want 1", got)
	}
}

func TestOMDbNoKeyToResolveByIsNoMatch(t *testing.T) {
	p, host := stub(t, movieJSON)
	// A movie with neither IMDb id nor title has nothing to key a lookup by.
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match (nothing to key by)", resp.Outcome)
	}
	if seen := host.Requests(); len(seen) != 0 {
		t.Errorf("OMDb hit with no lookup key (%v); want zero requests", seen)
	}
}

// The secret and the base URL are read PER CALL, because that is where the host
// publishes them: a settings save between two lookups reaches the second one.
func TestOMDbReadsItsSettingsOnEveryCall(t *testing.T) {
	p, host := stub(t, movieJSON)

	lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1"})
	s := settings()
	s.Secret = "rotated"
	host.SetSettings(s)
	lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt2"})

	seen := queries(t, host)
	if len(seen) != 2 {
		t.Fatalf("queries = %v, want two lookups", seen)
	}
	if seen[0] != "apikey=k&i=tt1" {
		t.Errorf("first query = %q, want the original key", seen[0])
	}
	if seen[1] != "apikey=rotated&i=tt2" {
		t.Errorf("second query = %q, want the rotated key", seen[1])
	}
}

// The manifest declares neither capability, so the host never asks — and when
// something does ask anyway, the answer is the host's own "not now" rather than a
// claim that this source has no candidates or no images.
func TestOMDbAnswersNeitherSearchNorArtwork(t *testing.T) {
	p, host := stub(t, movieJSON)
	ctx := context.Background()

	if resp, err := p.Search(ctx, pluginapi.SearchRequest{Kind: "movie", Query: "Dune"}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("search = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1"}, Role: "poster",
	}); err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("artwork candidates = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if seen := host.Requests(); len(seen) != 0 {
		t.Errorf("a declined call still fetched something (%v); want zero requests", seen)
	}
}
