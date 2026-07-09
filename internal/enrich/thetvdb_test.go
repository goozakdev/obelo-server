package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real TheTVDBProvider's HTTP/parse layer, exercised against an httptest server
// serving canned TheTVDB v4-shaped JSON — the secondary, lower seam (the project's
// black-box tests use a fake provider instead). No live network is ever touched.

// tvdbStub serves a TheTVDB-shaped API: a /login that mints a token and data
// endpoints that require the bearer token. It records the path of every DATA
// request (login excluded) so a test can assert the token flow, id-vs-name
// resolution, and the response cache. Handlers is a per-path map; a missing path
// 404s (TheTVDB's "no record" answer).
type tvdbStub struct {
	handlers  map[string]http.HandlerFunc
	dataReqs  []string // data-endpoint paths hit (login excluded)
	logins    int
	lastToken string // last Authorization bearer seen on a data request
}

func newTVDBProvider(t *testing.T, stub *tvdbStub) *TheTVDBProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			stub.logins++
			var body struct {
				APIKey string `json:"apikey"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok-` + body.APIKey + `"}}`))
			return
		}
		// Data endpoints require the bearer token minted by /login.
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		stub.dataReqs = append(stub.dataReqs, r.URL.Path)
		stub.lastToken = strings.TrimPrefix(auth, "Bearer ")
		h, ok := stub.handlers[r.URL.Path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	p := NewTheTVDBProvider("k", srv.URL)
	p.minInterval = 0 // no throttle wait in tests
	return p
}

func json200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

const tvdbSeriesJSON = `{"status":"success","data":{
  "id":121361,
  "name":"Game of Thrones",
  "overview":"Seven noble families fight for control of Westeros.",
  "image":"https://artworks.thetvdb.com/series/got.jpg",
  "genres":[{"name":"Drama"},{"name":"Fantasy"}]
}}`

const tvdbEpisodesJSON = `{"status":"success","data":{"episodes":[
  {"seasonNumber":1,"number":4,"name":"Cripples, Bastards","overview":"Ep four.","image":"https://artworks.thetvdb.com/e4.jpg"},
  {"seasonNumber":1,"number":5,"name":"The Wolf and the Lion","overview":"Ned uncovers the truth.","image":"https://artworks.thetvdb.com/e5.jpg"}
]}}`

func TestTheTVDBShowByIDLoginThenReuse(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/series/121361": json200(tvdbSeriesJSON),
		"/series/999":    json200(strings.Replace(tvdbSeriesJSON, "121361", "999", 1)),
	}}
	p := newTVDBProvider(t, stub)

	got, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "GoT", TheTVDBID: "121361"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Source != "thetvdb" || !got.Matched {
		t.Errorf("source/matched = %q/%v, want thetvdb/true", got.Source, got.Matched)
	}
	if got.Name != "Game of Thrones" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Overview != "Seven noble families fight for control of Westeros." {
		t.Errorf("overview = %q", got.Overview)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Drama" || got.Genres[1] != "Fantasy" {
		t.Errorf("genres = %v, want [Drama Fantasy]", got.Genres)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].Role != "poster" || got.Artwork[0].URL != "https://artworks.thetvdb.com/series/got.jpg" {
		t.Errorf("artwork = %+v, want a poster ref", got.Artwork)
	}
	// It resolved directly by id (no /search), and logged in exactly once.
	if len(stub.dataReqs) != 1 || stub.dataReqs[0] != "/series/121361" {
		t.Errorf("data reqs = %v, want a single /series/121361 lookup", stub.dataReqs)
	}
	if stub.logins != 1 || stub.lastToken != "tok-k" {
		t.Errorf("logins=%d token=%q, want 1 login + minted token", stub.logins, stub.lastToken)
	}

	// A second, uncached lookup reuses the token — no second login.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", TheTVDBID: "999"}); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	if stub.logins != 1 {
		t.Errorf("logins = %d after a second lookup, want still 1 (token reused)", stub.logins)
	}
}

func TestTheTVDBShowByNameSearchesFirst(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/search":        json200(`{"data":[{"tvdb_id":"121361","name":"Game of Thrones"}]}`),
		"/series/121361": json200(tvdbSeriesJSON),
	}}
	p := newTVDBProvider(t, stub)

	got, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "Game of Thrones"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "Game of Thrones" {
		t.Errorf("name = %q", got.Name)
	}
	// By-name resolution is a /search then a /series fetch (two data calls).
	if len(stub.dataReqs) != 2 || stub.dataReqs[0] != "/search" || stub.dataReqs[1] != "/series/121361" {
		t.Errorf("data reqs = %v, want /search then /series/121361", stub.dataReqs)
	}
}

func TestTheTVDBEpisodeByIDResolvesBySeasonNumber(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/series/121361/episodes/default": json200(tvdbEpisodesJSON),
	}}
	p := newTVDBProvider(t, stub)

	got, err := p.Lookup(context.Background(), TitleRef{
		Kind: "episode", TheTVDBID: "121361", SeasonNumber: 1, EpisodeNumber: 5,
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Name != "The Wolf and the Lion" {
		t.Errorf("name = %q, want the S1E5 title", got.Name)
	}
	if got.Overview != "Ned uncovers the truth." {
		t.Errorf("overview = %q", got.Overview)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://artworks.thetvdb.com/e5.jpg" {
		t.Errorf("artwork = %+v, want the S1E5 still", got.Artwork)
	}
}

func TestTheTVDBEpisodeUnknownNumberIsNoMatch(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/series/121361/episodes/default": json200(tvdbEpisodesJSON),
	}}
	p := newTVDBProvider(t, stub)

	if _, err := p.Lookup(context.Background(), TitleRef{
		Kind: "episode", TheTVDBID: "121361", SeasonNumber: 9, EpisodeNumber: 99,
	}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch (no such episode)", err)
	}
}

func TestTheTVDBTreatsNAAndEmptyAsEmpty(t *testing.T) {
	// A series that resolves but carries only "N/A"/empty fields contributes nothing —
	// the fill-only supplement reports it as a no-match.
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/series/1": json200(`{"data":{"name":"N/A","overview":"","image":"N/A","genres":[{"name":"N/A"}]}}`),
	}}
	p := newTVDBProvider(t, stub)

	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", TheTVDBID: "1"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch (all fields N/A/empty)", err)
	}
}

func TestTheTVDBUnknownIDIsNoMatch(t *testing.T) {
	// The series path 404s (no handler registered) — TheTVDB's "no record" answer.
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{}}
	p := newTVDBProvider(t, stub)

	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", TheTVDBID: "404"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch for a 404", err)
	}
}

func TestTheTVDBSearchNoResultsIsNoMatch(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/search": json200(`{"data":[]}`),
	}}
	p := newTVDBProvider(t, stub)

	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "Nope"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch (search empty)", err)
	}
}

func TestTheTVDBNon2xxIsError(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{
		"/series/1": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	}}
	p := newTVDBProvider(t, stub)

	_, err := p.Lookup(context.Background(), TitleRef{Kind: "show", TheTVDBID: "1"})
	if err == nil || err == ErrNoMatch {
		t.Errorf("err = %v, want a real (non-ErrNoMatch) error on 500", err)
	}
}

func TestTheTVDBNonTVKindIsNoMatch(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{"/series/1": json200(tvdbSeriesJSON)}}
	p := newTVDBProvider(t, stub)

	for _, kind := range []string{"movie", "artist", "album", "track"} {
		if _, err := p.Lookup(context.Background(), TitleRef{Kind: kind, TheTVDBID: "1", Title: "x"}); err != ErrNoMatch {
			t.Errorf("kind %q: err = %v, want ErrNoMatch (TheTVDB serves TV only)", kind, err)
		}
	}
	// A non-TV kind must not touch the network at all — not even a login.
	if stub.logins != 0 || len(stub.dataReqs) != 0 {
		t.Errorf("non-TV kind hit the network (logins=%d reqs=%v); want zero", stub.logins, stub.dataReqs)
	}
}

func TestTheTVDBCachesRepeatLookup(t *testing.T) {
	stub := &tvdbStub{handlers: map[string]http.HandlerFunc{"/series/121361": json200(tvdbSeriesJSON)}}
	p := newTVDBProvider(t, stub)
	ref := TitleRef{Kind: "show", TheTVDBID: "121361"}

	if _, err := p.Lookup(context.Background(), ref); err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	if _, err := p.Lookup(context.Background(), ref); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	// The response cache means the repeat lookup does not re-hit the data endpoint.
	if len(stub.dataReqs) != 1 {
		t.Errorf("data endpoint hit %d times, want 1 (repeat served from cache)", len(stub.dataReqs))
	}
}
