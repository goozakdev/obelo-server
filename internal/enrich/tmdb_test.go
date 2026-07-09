package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real TMDBProvider's HTTP/parse layer, exercised against an httptest server
// serving canned TMDB JSON — the secondary, lower seam (the project's black-box
// tests use a fake provider instead). No live network is ever touched.

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
  ]}
}`

// tmdbStub serves the movie-details endpoint and a search endpoint. It records
// the last request path so a test can assert by-id vs. search resolution.
func tmdbStub(t *testing.T, searchID string) (*TMDBProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
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
	}))
	t.Cleanup(srv.Close)
	return NewTMDBProvider("k", "en-US", srv.URL, "https://img/"), &seen
}

func TestTMDBLookupByID(t *testing.T) {
	p, seen := tmdbStub(t, "")
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", Year: 2021, TMDBID: "12345"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched {
		t.Fatal("not matched")
	}
	if meta.Overview == "" || meta.Tagline != "Fear is the mind-killer." {
		t.Errorf("scalar fields wrong: %+v", meta)
	}
	if meta.RuntimeMinutes != 155 || meta.ReleaseDate != "2021-10-22" {
		t.Errorf("runtime/release wrong: %+v", meta)
	}
	// Canonical title + year are surfaced for by-id identity resolution.
	if meta.Name != "Dune" || meta.Year != 2021 {
		t.Errorf("canonical title/year = %q/%d, want Dune/2021", meta.Name, meta.Year)
	}
	if meta.ContentRating != "PG-13" {
		t.Errorf("content rating = %q, want PG-13 (US certification)", meta.ContentRating)
	}
	if meta.Studio != "Legendary" {
		t.Errorf("studio = %q, want Legendary (first production company)", meta.Studio)
	}
	if len(meta.Genres) != 2 || meta.Genres[0] != "Science Fiction" {
		t.Errorf("genres = %v", meta.Genres)
	}
	if len(meta.Cast) != 2 || meta.Cast[0].Character != "Paul Atreides" {
		t.Errorf("cast wrong: %+v", meta.Cast)
	}
	// Artwork URLs are the image base + path; poster + backdrop both present.
	roles := map[string]string{}
	for _, a := range meta.Artwork {
		roles[a.Role] = a.URL
	}
	if roles["poster"] != "https://img//poster.jpg" || roles["background"] != "https://img//backdrop.jpg" {
		t.Errorf("artwork urls wrong: %+v", meta.Artwork)
	}
	// By-id: it went straight to /movie/{id}, never searched.
	for _, path := range *seen {
		if strings.HasPrefix(path, "/search") {
			t.Errorf("searched despite an embedded id: %v", *seen)
		}
	}
}

func TestTMDBLookupBySearch(t *testing.T) {
	p, seen := tmdbStub(t, "12345")
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", Year: 2021})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched || meta.Overview == "" {
		t.Errorf("search lookup did not resolve: %+v", meta)
	}
	sawSearch, sawDetails := false, false
	for _, path := range *seen {
		if strings.HasPrefix(path, "/search") {
			sawSearch = true
		}
		if strings.HasPrefix(path, "/movie/") {
			sawDetails = true
		}
	}
	if !sawSearch || !sawDetails {
		t.Errorf("expected a search THEN a details fetch; paths=%v", *seen)
	}
}

func TestTMDBSearchNoResultsIsNoMatch(t *testing.T) {
	p, _ := tmdbStub(t, "") // empty search results
	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Nonexistent", Year: 1900})
	if err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestTMDBNonMovieKindIsNoMatch(t *testing.T) {
	p, _ := tmdbStub(t, "12345")
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Title: "x"}); err != ErrNoMatch {
		t.Errorf("track lookup err = %v, want ErrNoMatch (Movie-only slice)", err)
	}
}
