package api_test

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Library browse filters studio / network / decade / person (home-discovery-rows 02).

func cast(refs ...string) []enrich.Credit {
	var out []enrich.Credit
	for _, r := range refs {
		out = append(out, enrich.Credit{Person: r, PersonRef: r, Kind: "cast"})
	}
	return out
}

func browseMovieServer(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		m := richMeta("Drama")
		m.Cast = nil
		switch ref.Title {
		case "Dune":
			m.Studios = []string{"Legendary", "Warner"}
			m.Cast = cast("a", "b", "c", "d", "star") // billed 5th
		case "Blade Runner":
			m.Studios = []string{"Ladd"}
			m.Genres = []string{"Drama", "Sci-Fi"}
			m.Cast = cast("a", "b", "c", "d", "e", "star") // billed 6th
		default:
			m.Studios = []string{"Legendary"}
			m.Cast = cast("star")
		}
		return m, nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")
	return srv, token, libID
}

func movieNames(t *testing.T, srv *testharness.Server, token, libID, query string) []string {
	t.Helper()
	var list enrichedListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100&"+query, token, &list)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", query, status, body)
	}
	out := []string{}
	for _, ti := range list.Titles {
		out = append(out, ti.Title)
	}
	sort.Strings(out)
	return out
}

func TestMovieBrowseFilters(t *testing.T) {
	t.Parallel()
	srv, token, lib := browseMovieServer(t)
	cases := []struct {
		name, query string
		want        []string
	}{
		{"studio on its second studio", "filter[studio]=Warner", []string{"Dune"}},
		{"studio on its first studio", "filter[studio]=Legendary", []string{"Dune", "Sample Movie"}},
		{"decade", "filter[decade]=1980", []string{"Blade Runner"}},
		{"person billed 5th kept, 6th excluded", "filter[person]=star", []string{"Dune", "Sample Movie"}},
		{"genre and decade intersect", "filter[genre]=Drama&filter[decade]=1980", []string{"Blade Runner"}},
		{"genre and decade intersect (none)", "filter[genre]=Sci-Fi&filter[decade]=2020", []string{}},
		{"studio and person AND", "filter[studio]=Legendary&filter[person]=star", []string{"Dune", "Sample Movie"}},
		{"network on a movie library matches nothing", "filter[network]=HBO", []string{}},
	}
	for _, c := range cases {
		if got := movieNames(t, srv, token, lib, c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (%s) = %v, want %v", c.name, c.query, got, c.want)
		}
	}
}

func TestBrowseDecadeMustBeAMultipleOfTen(t *testing.T) {
	t.Parallel()
	srv, token, lib := browseMovieServer(t)
	for _, v := range []string{"1985", "abc", "-10", "0"} {
		var out map[string]any
		status, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?filter[decade]="+v, token, &out)
		if status != http.StatusBadRequest {
			t.Errorf("filter[decade]=%s = %d, want 400; body: %s", v, status, body)
		}
	}
}

func TestMovieBrowseFilteredPaginationReturnsEveryMatchOnce(t *testing.T) {
	t.Parallel()
	srv, token, lib := browseMovieServer(t)
	var seen []string
	cursor := ""
	for i := 0; i < 5; i++ {
		var page struct {
			Titles     []enrichedSummaryResp `json:"titles"`
			NextCursor string                `json:"nextCursor"`
		}
		path := fmt.Sprintf("/api/v1/libraries/%s/titles?limit=1&filter[studio]=Legendary&cursor=%s", lib, cursor)
		if status, body := srv.AuthGET(path, token, &page); status != http.StatusOK {
			t.Fatalf("page = %d; body: %s", status, body)
		}
		for _, ti := range page.Titles {
			seen = append(seen, ti.Title)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if want := []string{"Dune", "Sample Movie"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("paged = %v, want %v", seen, want)
	}
}

func TestShowBrowseFilters(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		if ref.Kind != "show" {
			return enrich.TitleMetadata{Matched: true, Source: "tmdb"}, nil
		}
		m := enrich.TitleMetadata{Matched: true, ExternalID: "1", Source: "tmdb", Genres: []string{"Drama"}}
		switch ref.Title {
		case "The Bear":
			m.Studios = []string{"FX", "Hulu"}
			m.Cast = cast("a", "b", "c", "d", "star")
		case "Double Show":
			m.Studios = []string{"HBO"}
			m.Cast = cast("a", "b", "c", "d", "e", "star")
		default:
			m.Studios = []string{"HBO"}
		}
		return m, nil
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	lib := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, lib, "")
	enrichLib(t, srv, token, lib, "")

	names := func(query string) []string {
		var list enrichedShowsListResp
		status, body := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=100&"+query, token, &list)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d; body: %s", query, status, body)
		}
		out := []string{}
		for _, s := range list.Shows {
			out = append(out, s.Title)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name, query string
		want        []string
	}{
		{"network on its second network", "filter[network]=Hulu", []string{"The Bear"}},
		{"decade by first-air year", "filter[decade]=2020", []string{"Double Show", "The Bear", "The Daily"}},
		{"person billed 5th kept, 6th excluded", "filter[person]=star", []string{"The Bear"}},
		{"genre and decade intersect", "filter[genre]=Drama&filter[decade]=2010", []string{}},
		{"studio on a tv library matches nothing", "filter[studio]=HBO", []string{}},
	}
	for _, c := range cases {
		if got := names(c.query); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s (%s) = %v, want %v", c.name, c.query, got, c.want)
		}
	}
	var out map[string]any
	if status, _ := srv.AuthGET("/api/v1/libraries/"+lib+"/titles?filter[decade]=2025", token, &out); status != http.StatusBadRequest {
		t.Errorf("tv filter[decade]=2025 = %d, want 400", status)
	}
}
