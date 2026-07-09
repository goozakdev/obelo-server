package api_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box API tests for cross-kind search (issue tv-music/04): GET /search
// returns Movies, Shows, Artists/Albums, and (drilling in) Episodes/Tracks in one
// grouped response, and access-filters results the same way browse does (hidden
// entities excluded — the existence-hiding the catalog enforces today).

type searchResp struct {
	Movies   []titleSummaryResp  `json:"movies"`
	Shows    []showSummaryResp   `json:"shows"`
	Artists  []artistSummaryResp `json:"artists"`
	Albums   []albumResp         `json:"albums"`
	Episodes []titleSummaryResp  `json:"episodes"`
	Tracks   []titleSummaryResp  `json:"tracks"`
}

func search(t *testing.T, srv *testharness.Server, token, q string) searchResp {
	t.Helper()
	var out searchResp
	status, body := srv.AuthGET("/api/v1/search?q="+url.QueryEscape(q), token, &out)
	if status != http.StatusOK {
		t.Fatalf("search %q = %d, want 200; body: %s", q, status, body)
	}
	return out
}

func hasTitle(list []titleSummaryResp, title string) bool {
	for _, t := range list {
		if t.Title == title {
			return true
		}
	}
	return false
}

// TestCrossKindSearch: one server with a Movie, a TV, and a Music Library; a
// query against /search resolves into the right kind group across all three.
func TestCrossKindSearch(t *testing.T) {
	requireFixtures(t)
	requireTVFixtures(t)
	requireMusicFixtures(t)

	srv := testharness.New(t)
	token := adminToken(t, srv)

	movieLib := createMovieLibrary(t, srv, token, fixtureRoot(t))
	tvLib := createTVLibrary(t, srv, token, tvRoot(t))
	musicLib := createMusicLibrary(t, srv, token, musicRoot(t))
	for _, id := range []string{movieLib, tvLib, musicLib} {
		scanLib(t, srv, token, id, "")
	}

	// Movie: "Dune (2021)" is one of the core movie fixtures.
	if r := search(t, srv, token, "Dune"); !hasTitle(r.Movies, "Dune") {
		t.Errorf("search Dune: movies = %+v, want a Dune movie", r.Movies)
	}
	// Show: "The Bear".
	if r := search(t, srv, token, "Bear"); len(r.Shows) == 0 || r.Shows[0].Title != "The Bear" {
		t.Errorf("search Bear: shows = %+v, want The Bear", r.Shows)
	}
	// Artist + Album: "Radiohead" / "OK Computer".
	rad := search(t, srv, token, "Radiohead")
	if len(rad.Artists) == 0 || rad.Artists[0].Name != "Radiohead" {
		t.Errorf("search Radiohead: artists = %+v, want Radiohead", rad.Artists)
	}
	okc := search(t, srv, token, "OK Computer")
	foundAlbum := false
	for _, a := range okc.Albums {
		if a.Title == "OK Computer" {
			foundAlbum = true
		}
	}
	if !foundAlbum {
		t.Errorf("search OK Computer: albums = %+v, want OK Computer", okc.Albums)
	}
	// Episode (drill-in): "System" is The Bear S01E01's title.
	if r := search(t, srv, token, "System"); !hasTitle(r.Episodes, "System") {
		t.Errorf("search System: episodes = %+v, want the System episode", r.Episodes)
	}
	// Track (drill-in): "Airbag".
	if r := search(t, srv, token, "Airbag"); !hasTitle(r.Tracks, "Airbag") {
		t.Errorf("search Airbag: tracks = %+v, want the Airbag track", r.Tracks)
	}
}

// TestSearchRequiresAuth: /search is authenticated (no token → 401).
func TestSearchRequiresAuth(t *testing.T) {
	srv := testharness.New(t)
	resp := srv.Do(http.MethodGet, "/api/v1/search?q=anything", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated search = %d, want 401", resp.StatusCode)
	}
}

// TestSearchAccessFiltering: a soft-deleted (hidden) Title is excluded from
// search results, the same existence-hiding the browse list enforces — so search
// never reveals content the catalog hides.
func TestSearchAccessFiltering(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "Searchable Gem (2099)", "Searchable Gem (2099).mp4")
	makeMovie(t, file)

	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")

	// Present → found.
	if r := search(t, srv, token, "Searchable"); !hasTitle(r.Movies, "Searchable Gem") {
		t.Fatalf("present movie not found in search; movies: %+v", r.Movies)
	}

	// Remove the only file and rescan → the Title goes hidden (all Files Missing).
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	scanLib(t, srv, token, libID, "")

	if r := search(t, srv, token, "Searchable"); hasTitle(r.Movies, "Searchable Gem") {
		t.Errorf("hidden (soft-deleted) movie still returned by search; movies: %+v", r.Movies)
	}
}
