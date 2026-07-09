package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// External-metadata-enrichment issue 01 black-box tests: scan a Movie library,
// drive POST /libraries/{id}/enrich with FAKE MetadataProvider + ArtworkFetcher
// (zero network), and assert the decorated catalog through the HTTP API. The
// fakes are injected via the testharness options (mirroring how the scanner
// fakes the Prober), so production wiring is unchanged.

// --- Fakes (the two network seams) ------------------------------------------

type fakeProvider struct {
	mu       sync.Mutex
	refs     []enrich.TitleRef
	searches []fakeSearch
	artworks []fakeArtworkQuery
	fn       func(enrich.TitleRef) (enrich.TitleMetadata, error)
	// searchFn answers Search (the Edit-item provider search seam, ADR-0019). Nil
	// means "no searchable source" — Search then reports ErrSearchUnavailable, so a
	// test that doesn't set it exercises the unconfigured/graceful path.
	searchFn func(kind, query string) ([]enrich.Candidate, error)
	// artworkFn answers ArtworkCandidates (the Edit-item image picker seam,
	// item-editing/03). Nil means "no images offered" — the picker gets an empty list.
	artworkFn func(ref enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error)
}

// fakeSearch records one Search call for assertions (which kind/query/opts were
// asked — opts carries the artist-narrowing + paging threaded from the picker,
// item-editing/search-improvements).
type fakeSearch struct {
	kind  string
	query string
	opts  enrich.SearchOptions
}

// fakeArtworkQuery records one ArtworkCandidates call for assertions.
type fakeArtworkQuery struct {
	ref  enrich.TitleRef
	role string
}

func (f *fakeProvider) Lookup(_ context.Context, ref enrich.TitleRef) (enrich.TitleMetadata, error) {
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.mu.Unlock()
	return f.fn(ref)
}

// Search records the call and delegates to searchFn (canned candidates, zero
// network). With no searchFn it reports the "unavailable" outcome.
func (f *fakeProvider) Search(_ context.Context, kind, query string, opts enrich.SearchOptions) ([]enrich.Candidate, error) {
	f.mu.Lock()
	f.searches = append(f.searches, fakeSearch{kind: kind, query: query, opts: opts})
	f.mu.Unlock()
	if f.searchFn == nil {
		return nil, enrich.ErrSearchUnavailable
	}
	return f.searchFn(kind, query)
}

// lastSearch returns the most recent recorded Search call (for asserting the
// artist-narrowing + paging opts the picker threaded). Fatal-safe: returns a zero
// value when none was recorded.
func (f *fakeProvider) lastSearch() fakeSearch {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.searches) == 0 {
		return fakeSearch{}
	}
	return f.searches[len(f.searches)-1]
}

// ArtworkCandidates records the call and delegates to artworkFn (canned images,
// zero network). With no artworkFn it offers no images (an empty list).
func (f *fakeProvider) ArtworkCandidates(_ context.Context, ref enrich.TitleRef, role string) ([]enrich.ArtworkCandidate, error) {
	f.mu.Lock()
	f.artworks = append(f.artworks, fakeArtworkQuery{ref: ref, role: role})
	f.mu.Unlock()
	if f.artworkFn == nil {
		return nil, nil
	}
	return f.artworkFn(ref, role)
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refs)
}

// artworkCalls reports how many times ArtworkCandidates was invoked — the
// provider-hit counter the candidate-cache tests assert against (a cache hit
// serves from the Service and never reaches here).
func (f *fakeProvider) artworkCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.artworks)
}

type fakeFetcher struct {
	mu          sync.Mutex
	urls        []string
	data        []byte
	contentType string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]byte, string, error) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	f.mu.Unlock()
	ct := f.contentType
	if ct == "" {
		ct = "image/jpeg"
	}
	return f.data, ct, nil
}

// richMeta is a fully-populated movie metadata result with a poster + backdrop.
func richMeta(genres ...string) enrich.TitleMetadata {
	if len(genres) == 0 {
		genres = []string{"Science Fiction", "Drama"}
	}
	return enrich.TitleMetadata{
		Matched:        true,
		Overview:       "An epic tale of dunes and destiny.",
		Tagline:        "Fear is the mind-killer.",
		ContentRating:  "PG-13",
		ReleaseDate:    "2021-10-22",
		RuntimeMinutes: 155,
		Studio:         "Legendary",
		Genres:         genres,
		Cast: []enrich.Credit{
			{Person: "Timothée Chalamet", Character: "Paul Atreides", Kind: "cast"},
			{Person: "Zendaya", Character: "Chani", Kind: "cast"},
		},
		Artwork: []enrich.ArtworkRef{
			{Role: "poster", URL: "https://img.example/poster.jpg"},
			{Role: "background", URL: "https://img.example/backdrop.jpg"},
		},
		ExternalID: "999",
		Source:     "tmdb",
	}
}

// --- Test wire shapes (enrichment fields the issue adds) --------------------

type enrichResultResp struct {
	LibraryID string `json:"libraryId"`
	Total     int    `json:"total"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Disabled  int    `json:"disabled"`
}

type enrichedArtworkResp struct {
	Role   string `json:"role"`
	URL    string `json:"url"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

type enrichedDetailResp struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Year             int      `json:"year"`
	TMDBID           string   `json:"tmdbId"`
	Overview         string   `json:"overview"`
	Tagline          string   `json:"tagline"`
	ContentRating    string   `json:"contentRating"`
	ReleaseDate      string   `json:"releaseDate"`
	RuntimeMinutes   int      `json:"runtimeMinutes"`
	Studio           string   `json:"studio"`
	Genres           []string `json:"genres"`
	DisplayTitle     string   `json:"displayTitle"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
	IdentityKey      string   `json:"identityKey"`
	LockedFields     []string `json:"lockedFields"`
	Watched          bool     `json:"watched"`
	Cast             []struct {
		Person    string `json:"person"`
		Character string `json:"character"`
		Kind      string `json:"kind"`
	} `json:"cast"`
	Artwork []enrichedArtworkResp `json:"artwork"`
}

type enrichedSummaryResp struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Genres           []string `json:"genres"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
}

type enrichedListResp struct {
	Titles []enrichedSummaryResp `json:"titles"`
}

// --- Helpers ----------------------------------------------------------------

func enrichLib(t *testing.T, srv *testharness.Server, token, libID, mode string) enrichResultResp {
	t.Helper()
	path := "/api/v1/libraries/" + libID + "/enrich"
	if mode != "" {
		path += "?mode=" + mode
	}
	var res enrichResultResp
	status, body := srv.JSON(http.MethodPost, path, token, nil, &res)
	if status != http.StatusOK {
		t.Fatalf("enrich = %d, want 200; body: %s", status, body)
	}
	return res
}

func getEnrichedDetail(t *testing.T, srv *testharness.Server, token, titleID string) enrichedDetailResp {
	t.Helper()
	var d enrichedDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title = %d, want 200; body: %s", status, body)
	}
	return d
}

// titleIDByName scans the listing and returns the id of the Title with the given
// display title (fatal if absent).
func titleIDByName(t *testing.T, srv *testharness.Server, token, libID, name string) string {
	t.Helper()
	for _, ts := range listAllTitles(t, srv, token, libID).Titles {
		if ts.Title == name {
			return ts.ID
		}
	}
	t.Fatalf("title %q not found in library %s", name, libID)
	return ""
}

// authBytes does an authenticated raw GET, returning the status and body bytes
// (used for artwork, which serves image bytes rather than JSON).
func authBytes(t *testing.T, srv *testharness.Server, token, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL(path), nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// --- Tests ------------------------------------------------------------------

// TestEnrichMovieAppliesMetadataAndArtwork: a pass with a rich fake decorates a
// Movie's detail with overview/genres/cast/contentRating/releaseDate/runtime/
// studio + a FETCHED poster whose URL serves the fake's image bytes.
func TestEnrichMovieAppliesMetadataAndArtwork(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	fetch := &fakeFetcher{data: []byte("FAKEPOSTERBYTES"), contentType: "image/jpeg"}

	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	res := enrichLib(t, srv, token, libID, "")
	if res.Matched != 3 || res.Total != 3 {
		t.Fatalf("enrich result = %+v, want 3 matched / 3 total", res)
	}

	id := titleIDByName(t, srv, token, libID, "Dune")
	d := getEnrichedDetail(t, srv, token, id)
	if d.EnrichmentStatus != "matched" {
		t.Errorf("status = %q, want matched", d.EnrichmentStatus)
	}
	if d.Overview == "" || d.Tagline == "" {
		t.Errorf("overview/tagline empty: %+v", d)
	}
	if d.ContentRating != "PG-13" || d.ReleaseDate != "2021-10-22" || d.RuntimeMinutes != 155 || d.Studio != "Legendary" {
		t.Errorf("scalar fields wrong: %+v", d)
	}
	if len(d.Genres) != 2 || d.Genres[0] != "Science Fiction" {
		t.Errorf("genres = %v, want [Science Fiction Drama]", d.Genres)
	}
	if len(d.Cast) != 2 || d.Cast[0].Person != "Timothée Chalamet" || d.Cast[0].Character != "Paul Atreides" {
		t.Errorf("cast wrong: %+v", d.Cast)
	}

	// A fetched poster is present and its URL serves the fake's bytes.
	var posterURL, posterSource string
	for _, a := range d.Artwork {
		if a.Role == "poster" {
			posterURL, posterSource = a.URL, a.Source
		}
	}
	if posterURL == "" {
		t.Fatalf("no poster artwork on enriched detail: %+v", d.Artwork)
	}
	if posterSource != "fetched" {
		t.Errorf("poster source = %q, want fetched (Dune has no local poster)", posterSource)
	}
	status, body := authBytes(t, srv, token, posterURL)
	if status != http.StatusOK {
		t.Fatalf("artwork GET = %d, want 200", status)
	}
	if string(body) != "FAKEPOSTERBYTES" {
		t.Errorf("artwork bytes = %q, want the fetched fake bytes", body)
	}

	// The summary carries genres + enrichmentStatus too.
	var list enrichedListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
	for _, ts := range list.Titles {
		if ts.ID == id {
			if ts.EnrichmentStatus != "matched" || len(ts.Genres) != 2 {
				t.Errorf("summary not decorated: %+v", ts)
			}
		}
	}
}

// TestEnrichByEmbeddedID: a Title with a {tmdb-…} token enriches BY id (no fuzzy
// search), and its identity (year, tmdbId) + watch state survive the pass.
func TestEnrichByEmbeddedID(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")

	id := titleIDByName(t, srv, token, libID, "Pinned Movie")
	before := getEnrichedDetail(t, srv, token, id)
	if before.TMDBID != "12345" {
		t.Fatalf("pinned movie tmdbId = %q, want 12345 (parsed from token)", before.TMDBID)
	}

	// Set a watch state BEFORE enrichment; if enrichment touched identity the
	// per-Title watch state would be lost.
	srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/watchState", token, map[string]any{"watched": true}, nil)

	enrichLib(t, srv, token, libID, "")

	// The provider was asked by id (TMDBID set on the ref), never a blind search.
	prov.mu.Lock()
	sawByID := false
	for _, r := range prov.refs {
		if strings.Contains(r.Title, "Pinned") && r.TMDBID == "12345" {
			sawByID = true
		}
	}
	prov.mu.Unlock()
	if !sawByID {
		t.Errorf("provider not called by embedded id for Pinned Movie; refs=%+v", prov.refs)
	}

	after := getEnrichedDetail(t, srv, token, id)
	if after.ID != before.ID || after.Year != before.Year || after.TMDBID != "12345" {
		t.Errorf("identity changed by enrichment: before=%+v after=%+v", before, after)
	}
	if !after.Watched {
		t.Errorf("watch state lost across enrichment (identity not preserved)")
	}
}

// TestEnrichLocalArtworkWins: a Title with a local poster.jpg keeps serving the
// LOCAL bytes after enrichment fetches a remote poster (local wins, CONTEXT.md).
func TestEnrichLocalArtworkWins(t *testing.T) {
	requireNamingFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	fetch := &fakeFetcher{data: []byte("REMOTEPOSTER"), contentType: "image/jpeg"}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, namingRoot(t))
	scanLib(t, srv, token, libID, "")

	// "Extras Movie (2013)" ships a local poster.jpg + fanart.jpg.
	id := titleIDByName(t, srv, token, libID, "Extras Movie")
	enrichLib(t, srv, token, libID, "")

	d := getEnrichedDetail(t, srv, token, id)
	var posterSource string
	for _, a := range d.Artwork {
		if a.Role == "poster" {
			posterSource = a.Source
		}
	}
	if posterSource != "local" {
		t.Errorf("poster source = %q, want local (local wins over fetched)", posterSource)
	}
	status, body := authBytes(t, srv, token, "/api/v1/titles/"+id+"/artwork/poster")
	if status != http.StatusOK {
		t.Fatalf("artwork GET = %d, want 200", status)
	}
	if string(body) == "REMOTEPOSTER" {
		t.Errorf("served the fetched poster; local artwork should win")
	}
}

// TestEnrichGracefulDegradation: a no-match Title → unmatched, a provider error →
// failed, and neither aborts the pass (the rest still match). All stay browsable.
func TestEnrichGracefulDegradation(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		switch ref.Title {
		case "Blade Runner":
			return enrich.TitleMetadata{}, enrich.ErrNoMatch
		case "Sample Movie":
			return enrich.TitleMetadata{}, errors.New("provider exploded")
		default:
			return richMeta(), nil
		}
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	res := enrichLib(t, srv, token, libID, "")
	if res.Matched != 1 || res.Unmatched != 1 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 1 matched / 1 unmatched / 1 failed", res)
	}

	statuses := map[string]string{}
	for _, name := range []string{"Dune", "Blade Runner", "Sample Movie"} {
		statuses[name] = getEnrichedDetail(t, srv, token, titleIDByName(t, srv, token, libID, name)).EnrichmentStatus
	}
	if statuses["Dune"] != "matched" || statuses["Blade Runner"] != "unmatched" || statuses["Sample Movie"] != "failed" {
		t.Errorf("statuses = %+v, want matched/unmatched/failed", statuses)
	}
	// All three remain browsable regardless of enrichment outcome.
	if got := len(listAllTitles(t, srv, token, libID).Titles); got != 3 {
		t.Errorf("titles after enrich = %d, want 3 (all browsable)", got)
	}
}

// TestEnrichDisabledByDefault: with no provider key, the pass is a no-op that
// records candidates 'disabled' and makes NO outbound calls.
func TestEnrichDisabledByDefault(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	// Provider injected but NO WithEnrichmentKey → enrichment disabled.
	srv := testharness.New(t,
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	res := enrichLib(t, srv, token, libID, "")
	if res.Disabled != 3 || res.Matched != 0 {
		t.Fatalf("result = %+v, want 3 disabled / 0 matched", res)
	}
	if prov.calls() != 0 {
		t.Errorf("provider called %d times while disabled, want 0 (no outbound calls)", prov.calls())
	}
	d := getEnrichedDetail(t, srv, token, titleIDByName(t, srv, token, libID, "Dune"))
	if d.EnrichmentStatus != "disabled" {
		t.Errorf("status = %q, want disabled", d.EnrichmentStatus)
	}
}

// TestEnrichIdempotent: a second full pass produces the same result — no churned
// or duplicated genres/cast/artwork.
func TestEnrichIdempotent(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	enrichLib(t, srv, token, libID, "full")
	enrichLib(t, srv, token, libID, "full")

	d := getEnrichedDetail(t, srv, token, titleIDByName(t, srv, token, libID, "Dune"))
	if len(d.Genres) != 2 {
		t.Errorf("genres after re-enrich = %d, want 2 (no dup)", len(d.Genres))
	}
	if len(d.Cast) != 2 {
		t.Errorf("cast after re-enrich = %d, want 2 (no dup)", len(d.Cast))
	}
	posters := 0
	for _, a := range d.Artwork {
		if a.Role == "poster" {
			posters++
		}
	}
	if posters != 1 {
		t.Errorf("poster entries after re-enrich = %d, want 1 (no dup)", posters)
	}
}

// TestEnrichGenreFilter: filter[genre]= returns only Titles carrying that genre.
func TestEnrichGenreFilter(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		switch ref.Title {
		case "Sample Movie":
			return richMeta("Comedy"), nil
		default:
			return richMeta("Science Fiction"), nil
		}
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

	var list enrichedListResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?filter[genre]=Comedy&limit=100", token, &list)
	if status != http.StatusOK {
		t.Fatalf("filtered list = %d; body: %s", status, body)
	}
	if len(list.Titles) != 1 || list.Titles[0].Title != "Sample Movie" {
		t.Errorf("filter[genre]=Comedy = %+v, want only Sample Movie", list.Titles)
	}

	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?filter[genre]=Science+Fiction&limit=100", token, &list)
	if len(list.Titles) != 2 {
		t.Errorf("filter[genre]=Science Fiction = %d, want 2", len(list.Titles))
	}
}

// TestEnrichRequiresAdmin: a Member cannot trigger an enrichment pass.
func TestEnrichRequiresAdmin(t *testing.T) {
	srv := testharness.New(t, testharness.WithEnrichmentKey("test-key"))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	srv.CreateMember("m", "memberpass123")
	mTok := login(t, srv, "m", "memberpass123", "P", "ios", "mc").Token

	status, _ := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/enrich", mTok, nil, nil)
	if status != http.StatusForbidden {
		t.Errorf("member enrich = %d, want 403", status)
	}
}
