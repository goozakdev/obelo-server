package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real providers' Search HTTP/parse layer (the Edit-item Enrichment-override
// picker, ADR-0019), exercised against httptest serving canned JSON — the
// secondary, lower seam. The project's black-box tests use a fake provider's
// Search instead; no live network is ever touched here.

const tmdbMovieSearchJSON = `{"results": [
  {"id": 438631, "title": "Dune", "release_date": "2021-10-22",
   "overview": "Paul Atreides leads a desert rebellion.", "poster_path": "/dune21.jpg"},
  {"id": 841, "title": "Dune", "release_date": "1984-12-14",
   "overview": "A Duke's son leads desert warriors.", "poster_path": "/dune84.jpg"}
]}`

const tmdbTVSearchJSON = `{"results": [
  {"id": 1399, "name": "Game of Thrones", "first_air_date": "2011-04-17",
   "overview": "Noble families vie for the Iron Throne.", "poster_path": "/got.jpg"}
]}`

// TestTMDBSearchMovieParsesCandidates: a movie query hits /search/movie and maps
// each result into a Candidate with the id, title, year, poster thumbnail, and
// the overview as the disambiguation hint.
func TestTMDBSearchMovieParsesCandidates(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/search/movie" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tmdbMovieSearchJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	cands, err := p.Search(context.Background(), "movie", "Dune", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/search/movie" {
		t.Fatalf("expected one /search/movie call, saw %v", seen)
	}
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

// TestTMDBSearchTVForEpisodeKind: an Episode search targets /search/tv (an episode
// is corrected by re-pointing at its show) and parses the TV name/first_air_date.
func TestTMDBSearchTVForEpisodeKind(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/search/tv" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tmdbTVSearchJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	cands, err := p.Search(context.Background(), "episode", "Game of Thrones", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/search/tv" {
		t.Fatalf("expected one /search/tv call, saw %v", seen)
	}
	if len(cands) != 1 || cands[0].Title != "Game of Thrones" || cands[0].Year != 2011 || cands[0].Kind != "episode" {
		t.Fatalf("tv candidate unexpected: %+v", cands)
	}
}

// TestTMDBSearchEmptyQueryNoCall: a blank query short-circuits with no candidates
// and no HTTP call.
func TestTMDBSearchEmptyQueryNoCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")
	cands, err := p.Search(context.Background(), "movie", "   ", SearchOptions{})
	if err != nil || len(cands) != 0 {
		t.Fatalf("empty query = (%v, %v), want (nil, nil)", cands, err)
	}
	if called {
		t.Errorf("empty query issued an HTTP call")
	}
}

const mbRecordingSearchJSON = `{"recordings": [
  {"id": "rec-1", "title": "Come as You Are", "disambiguation": "album version",
   "first-release-date": "1991-09-24", "artist-credit": [{"name": "Nirvana"}]},
  {"id": "rec-2", "title": "Come as You Are", "artist-credit": [{"name": "Nirvana (60s band)"}]}
]}`

// TestMusicBrainzSearchTrackParsesCandidates: a track query hits /recording and
// maps recordings into Candidates carrying the MBID, title, year, and an
// artist-credit + disambiguation hint (the "wrong Nirvana" tell).
func TestMusicBrainzSearchTrackParsesCandidates(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/recording" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mbRecordingSearchJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "track", "Come as You Are", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/recording" {
		t.Fatalf("expected one /recording call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].ExternalID != "rec-1" || cands[0].Title != "Come as You Are" ||
		cands[0].Year != 1991 || cands[0].Kind != "track" {
		t.Errorf("candidate[0] = %+v", cands[0])
	}
	if !strings.Contains(cands[0].Disambiguation, "Nirvana") ||
		!strings.Contains(cands[0].Disambiguation, "album version") {
		t.Errorf("disambiguation = %q", cands[0].Disambiguation)
	}
	if !strings.Contains(cands[1].Disambiguation, "60s band") {
		t.Errorf("candidate[1] disambiguation = %q", cands[1].Disambiguation)
	}
}

// TestMusicBrainzTrackLookupByPinnedMBID: a pinned recording MBID resolves BY id
// (/recording/{mbid}) — the durable Enrichment override path — instead of a
// name search.
func TestMusicBrainzTrackLookupByPinnedMBID(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/recording/rec-42" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "rec-42", "title": "Corrected Title"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", MusicbrainzID: "rec-42", Track: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/recording/rec-42" {
		t.Fatalf("expected a by-id /recording/rec-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.Name != "Corrected Title" || meta.ExternalID != "rec-42" {
		t.Errorf("meta = %+v", meta)
	}
}

// TestMusicBrainzSearchEscapesLuceneQuery: a title with Lucene metacharacters
// (e.g. AC/DC) is still escaped, so it can't produce a malformed query the server
// rejects (which would surface as a false SEARCH_UNAVAILABLE) — even though the terms
// are no longer wrapped in a recording:"…" exact phrase (item-editing/search-
// improvements). The stub captures the outgoing query param.
func TestMusicBrainzSearchEscapesLuceneQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recording" {
			gotQuery = r.URL.Query().Get("query")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"recordings": [{"id": "rec-1", "title": "Back in Black"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "track", `AC/DC "Heroes"`, SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	// Every metacharacter is backslash-escaped, but NOT phrase-wrapped: the query is
	// relevance-ranked terms, not recording:"…".
	want := `AC\/DC \"Heroes\"`
	if gotQuery != want {
		t.Errorf("escaped query = %q, want %q", gotQuery, want)
	}
	if strings.Contains(gotQuery, `recording:"`) {
		t.Errorf("query %q is still an exact-phrase recording:\"…\" — the phrase fix regressed", gotQuery)
	}
}

// TestMusicBrainzSearchYearFromReleases: a recording hit carries no top-level
// first-release-date, so the disambiguating year is derived from its releases /
// release-groups — the earliest (original) year across them.
func TestMusicBrainzSearchYearFromReleases(t *testing.T) {
	const body = `{"recordings": [{
	  "id": "rec-1", "title": "Star Wars (Main Title)",
	  "releases": [
	    {"date": "1997-03-01", "release-group": {"first-release-date": "1997-03-01"}},
	    {"date": "1977-05-25", "release-group": {"first-release-date": "1977-05-01"}}
	  ]
	}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recording" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "track", "Star Wars", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 || cands[0].Year != 1977 {
		t.Fatalf("candidate year = %+v, want earliest 1977", cands)
	}
}

// TestMusicBrainzSearchUnsupportedKindUnavailable: a kind MusicBrainz does not
// own (e.g. a video kind, or a season) reports ErrSearchUnavailable — only
// track/artist/album are searchable music kinds (item-editing/02).
func TestMusicBrainzSearchUnsupportedKindUnavailable(t *testing.T) {
	p := NewMusicBrainzProvider("http://unused", "http://unused", "en-US")
	if _, err := p.Search(context.Background(), "movie", "Dune", SearchOptions{}); err != ErrSearchUnavailable {
		t.Errorf("movie search err = %v, want ErrSearchUnavailable", err)
	}
}

const mbArtistSearchJSON = `{"artists": [
  {"id": "art-1", "name": "Nirvana", "type": "Group", "disambiguation": "90s grunge band",
   "area": {"name": "United States"}},
  {"id": "art-2", "name": "Nirvana", "type": "Group", "disambiguation": "60s UK band",
   "area": {"name": "United Kingdom"}}
]}`

// TestMusicBrainzSearchArtistParsesCandidates: an artist query hits /artist and
// maps artists into Candidates carrying the MBID, name, and a type/area/
// disambiguation hint (the "wrong Nirvana" tell).
func TestMusicBrainzSearchArtistParsesCandidates(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/artist" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mbArtistSearchJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "artist", "Nirvana", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/artist" {
		t.Fatalf("expected one /artist call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].ExternalID != "art-1" || cands[0].Title != "Nirvana" || cands[0].Kind != "artist" {
		t.Errorf("candidate[0] = %+v", cands[0])
	}
	if !strings.Contains(cands[0].Disambiguation, "90s grunge") ||
		!strings.Contains(cands[0].Disambiguation, "United States") {
		t.Errorf("disambiguation = %q", cands[0].Disambiguation)
	}
	if !strings.Contains(cands[1].Disambiguation, "60s UK") {
		t.Errorf("candidate[1] disambiguation = %q", cands[1].Disambiguation)
	}
}

// TestMusicBrainzSearchAlbumCarriesTracklist: an album (release-group) query hits
// /release-group for candidates and /release (inc=recordings) for each candidate's
// tracklist preview — the ordered disc/position/title an Admin confirms before
// applying (ADR-0019; slice 05 consumes it for the positional cascade).
func TestMusicBrainzSearchAlbumCarriesTracklist(t *testing.T) {
	const rgJSON = `{"release-groups": [
	  {"id": "rg-1", "title": "OK Computer", "first-release-date": "1997-05-21",
	   "artist-credit": [{"name": "Radiohead"}]}
	]}`
	const releaseJSON = `{"releases": [
	  {"media": [{"position": 1, "tracks": [
	    {"position": 1, "title": "Airbag"},
	    {"position": 2, "title": "Paranoid Android"}
	  ]}]}
	]}`
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/release-group":
			_, _ = w.Write([]byte(rgJSON))
		case "/release":
			_, _ = w.Write([]byte(releaseJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	cands, err := p.Search(context.Background(), "album", "OK Computer", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	c := cands[0]
	if c.ExternalID != "rg-1" || c.Title != "OK Computer" || c.Year != 1997 || c.Kind != "album" {
		t.Errorf("candidate = %+v", c)
	}
	if len(c.Tracklist) != 2 || c.Tracklist[0].Title != "Airbag" || c.Tracklist[1].Position != 2 {
		t.Errorf("tracklist = %+v", c.Tracklist)
	}
	if c.Tracklist[0].Disc != 1 {
		t.Errorf("tracklist disc = %d, want 1", c.Tracklist[0].Disc)
	}
}

// TestMusicBrainzArtistLookupByPinnedMBID: a pinned artist MBID resolves BY id
// (/artist/{mbid}) — the durable artist Enrichment override path — instead of a
// name search.
func TestMusicBrainzArtistLookupByPinnedMBID(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/artist/art-42" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "art-42", "name": "Corrected Artist", "type": "Group",
			  "tags": [{"name": "grunge"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: "art-42", Title: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/artist/art-42" {
		t.Fatalf("expected a by-id /artist/art-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.Name != "Corrected Artist" || meta.ExternalID != "art-42" {
		t.Errorf("meta = %+v", meta)
	}
	if len(meta.Genres) != 1 || meta.Genres[0] != "grunge" {
		t.Errorf("genres = %v", meta.Genres)
	}
}

// TestMusicBrainzAlbumLookupByPinnedMBID: a pinned release-group MBID resolves BY
// id (/release-group/{mbid}) — the durable album Enrichment override path.
func TestMusicBrainzAlbumLookupByPinnedMBID(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/release-group/rg-42" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": "rg-42", "title": "Corrected Album",
			  "first-release-date": "2000-01-01", "tags": [{"name": "rock"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewMusicBrainzProvider(srv.URL, "https://coverart/", "en-US")
	p.MinInterval = 0

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: "rg-42", Album: "ignored"})
	if err != nil {
		t.Fatalf("Lookup by MBID: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/release-group/rg-42" {
		t.Fatalf("expected a by-id /release-group/rg-42 fetch, saw %v", seen)
	}
	if !meta.Matched || meta.ExternalID != "rg-42" {
		t.Errorf("meta = %+v", meta)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "cover" {
		t.Errorf("expected a cover artwork ref, got %+v", meta.Artwork)
	}
}
