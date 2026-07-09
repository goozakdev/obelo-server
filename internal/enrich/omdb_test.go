package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The real OMDbProvider's HTTP/parse layer, exercised against an httptest server
// serving canned OMDb JSON — the secondary, lower seam (the project's black-box
// tests use a fake provider instead). No live network is ever touched.

const omdbMovieJSON = `{
  "Title": "The Shawshank Redemption",
  "Rated": "R",
  "Genre": "Drama, Crime",
  "Plot": "Two imprisoned men bond over a number of years.",
  "Response": "True"
}`

// omdbStub serves the OMDb endpoint with canned JSON, recording each request's
// raw query so a test can assert id-vs-title resolution and count the hits.
func omdbStub(t *testing.T, body string) (*OMDbProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewOMDbProvider("k", srv.URL)
	p.minInterval = 0 // no throttle wait in tests
	return p, &seen
}

func TestOMDbResolvesByIMDbID(t *testing.T) {
	p, seen := omdbStub(t, omdbMovieJSON)

	got, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Shawshank", IMDBID: "tt0111161"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Source != "omdb" || !got.Matched {
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
	if len(*seen) != 1 || (*seen)[0] != "apikey=k&i=tt0111161" {
		t.Errorf("query = %v, want a single i=tt0111161 lookup", *seen)
	}
}

func TestOMDbResolvesByTitleYearWhenNoIMDbID(t *testing.T) {
	p, seen := omdbStub(t, omdbMovieJSON)

	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "The Shawshank Redemption", Year: 1994}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != "apikey=k&t=The+Shawshank+Redemption&y=1994" {
		t.Errorf("query = %v, want a t=/y= title lookup", *seen)
	}
}

func TestOMDbTreatsNAAsEmpty(t *testing.T) {
	// A record that resolves but carries only "N/A" fields contributes nothing —
	// the fill-only supplement reports it as a no-match.
	p, _ := omdbStub(t, `{"Rated":"N/A","Genre":"N/A","Plot":"N/A","Response":"True"}`)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", IMDBID: "tt1"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch (all fields N/A)", err)
	}
}

func TestOMDbSplitsGenresAndDropsNA(t *testing.T) {
	p, _ := omdbStub(t, `{"Genre":"Action, N/A, Sci-Fi","Response":"True"}`)
	got, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", IMDBID: "tt1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Action" || got.Genres[1] != "Sci-Fi" {
		t.Errorf("genres = %v, want [Action Sci-Fi] (N/A dropped)", got.Genres)
	}
}

func TestOMDbResponseFalseIsNoMatch(t *testing.T) {
	p, _ := omdbStub(t, `{"Response":"False","Error":"Movie not found!"}`)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", IMDBID: "tt404"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch for Response:False", err)
	}
}

func TestOMDbNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server on fire", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	p := NewOMDbProvider("k", srv.URL)
	p.minInterval = 0

	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", IMDBID: "tt1"})
	if err == nil || err == ErrNoMatch {
		t.Errorf("err = %v, want a real (non-ErrNoMatch) error on 500", err)
	}
}

func TestOMDbNonMovieKindIsNoMatch(t *testing.T) {
	p, seen := omdbStub(t, omdbMovieJSON)
	for _, kind := range []string{"show", "season", "episode", "artist"} {
		if _, err := p.Lookup(context.Background(), TitleRef{Kind: kind, IMDBID: "tt1"}); err != ErrNoMatch {
			t.Errorf("kind %q: err = %v, want ErrNoMatch (OMDb serves movies only)", kind, err)
		}
	}
	if len(*seen) != 0 {
		t.Errorf("OMDb hit for a non-movie kind (%v); want zero requests", *seen)
	}
}

func TestOMDbCachesRepeatLookup(t *testing.T) {
	p, seen := omdbStub(t, omdbMovieJSON)
	ref := TitleRef{Kind: "movie", IMDBID: "tt0111161"}

	if _, err := p.Lookup(context.Background(), ref); err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	if _, err := p.Lookup(context.Background(), ref); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	// The response cache means the repeat lookup does not re-hit the server.
	if len(*seen) != 1 {
		t.Errorf("server hit %d times, want 1 (repeat served from cache)", len(*seen))
	}
}

func TestOMDbNoKeyToResolveByIsNoMatch(t *testing.T) {
	p, seen := omdbStub(t, omdbMovieJSON)
	// A movie with neither IMDb id nor title has nothing to key a lookup by.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch (nothing to key by)", err)
	}
	if len(*seen) != 0 {
		t.Errorf("OMDb hit with no lookup key (%v); want zero requests", *seen)
	}
}
