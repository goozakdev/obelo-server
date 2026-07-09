package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Cast-photos/02 black-box tests: scan + enrich a TV library with a FAKE
// MetadataProvider whose TV details include series credits (some members with
// photos, one without) and a FAKE ArtworkFetcher (image bytes, plus a URL that
// 404s), then assert the decorated Show cast + served headshots through the HTTP
// API — zero network. TV cast reuses issue-01's person storage + /people route
// verbatim; these tests mirror cast_photos_test.go for the Show detail. They also
// prove cross-KIND dedupe: an actor in both a movie and a show shares one row.

// tvCastMetaFor is the faked provider for the TV cast tests. It answers every kind
// the TV pass looks up: a Show carries the series main cast (The Bear), a Season/
// Episode matches with no cast (so the pass succeeds), and a Movie reuses issue
// 01's castMetaFor so a shared actor (tmdb:1) lets the cross-kind dedupe test run
// one movie + one TV library off a single provider + fetcher.
func tvCastMetaFor(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
	switch ref.Kind {
	case "movie":
		return castMetaFor(ref.Title), nil
	case "show":
		m := enrich.TitleMetadata{
			Matched: true, Overview: "A restaurant drama.", ContentRating: "TV-MA",
			Studio: "FX", Genres: []string{"Comedy", "Drama"}, ExternalID: "9001", Source: "tmdb",
			Artwork: []enrich.ArtworkRef{{Role: "poster", URL: "https://img.example/show-poster.jpg"}},
		}
		if ref.Title == "The Bear" {
			m.Cast = []enrich.Credit{
				{Person: "Jeremy Allen White", Character: "Carmy", Kind: "cast",
					PersonRef: "tmdb:10", ImageURL: "https://img.example/white.jpg"}, // Show-only actor
				{Person: "Ayo Edebiri", Character: "Sydney", Kind: "cast",
					PersonRef: "tmdb:11"}, // no headshot
				{Person: "Ebon Moss-Bachrach", Character: "Richie", Kind: "cast",
					PersonRef: "tmdb:12", ImageURL: "https://img.example/moss.jpg"}, // download fails
				{Person: "Timothée Chalamet", Character: "Guest Chef", Kind: "cast",
					PersonRef: "tmdb:1", ImageURL: "https://img.example/chalamet.jpg"}, // shared w/ Dune (movie)
			}
		}
		return m, nil
	case "season", "episode":
		return enrich.TitleMetadata{Matched: true, Source: "tmdb"}, nil
	default:
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}
}

// --- Test wire shapes -------------------------------------------------------

type showCastDetailResp struct {
	Show struct {
		ID          string `json:"id"`
		IdentityKey string `json:"identityKey"`
		Cast        []struct {
			Person       string `json:"person"`
			Character    string `json:"character"`
			Kind         string `json:"kind"`
			PersonID     string `json:"personId"`
			PhotoVersion string `json:"photoVersion"`
		} `json:"cast"`
	} `json:"show"`
	Seasons []struct {
		ID string `json:"id"`
	} `json:"seasons"`
}

func getShowCast(t *testing.T, srv *testharness.Server, token, showID string) showCastDetailResp {
	t.Helper()
	var d showCastDetailResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &d)
	if status != http.StatusOK {
		t.Fatalf("get show = %d, want 200; body: %s", status, body)
	}
	return d
}

// showIDByName finds a Show's id via the (enriched) TV grid.
func showIDByName(t *testing.T, srv *testharness.Server, token, libID, name string) string {
	t.Helper()
	var grid enrichedShowsListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &grid)
	for _, s := range grid.Shows {
		if s.Title == name {
			return s.ID
		}
	}
	t.Fatalf("show %q not found in grid: %+v", name, grid.Shows)
	return ""
}

// findShowCredit returns the Show cast entry for a person (fatal if absent).
func findShowCredit(t *testing.T, d showCastDetailResp, person string) struct {
	Person       string `json:"person"`
	Character    string `json:"character"`
	Kind         string `json:"kind"`
	PersonID     string `json:"personId"`
	PhotoVersion string `json:"photoVersion"`
} {
	t.Helper()
	for _, c := range d.Show.Cast {
		if c.Person == person {
			return c
		}
	}
	t.Fatalf("show cast member %q not found in %+v", person, d.Show.Cast)
	return d.Show.Cast[0]
}

func tvCastSetup(t *testing.T) (*testharness.Server, *castFetcher, string, string) {
	t.Helper()
	requireTVFixtures(t)
	prov := &fakeProvider{fn: tvCastMetaFor}
	fetch := &castFetcher{fail: map[string]error{
		"https://img.example/moss.jpg": enrich.ErrArtworkNotFound,
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, fetch, token, libID
}

// --- Tests ------------------------------------------------------------------

// TestTVCastPhotosCaptureAndServe: after enrichment, the Show detail cast carries
// personIds and a Show-only actor's /people/{ref}/artwork/profile serves the cached
// bytes via the media cookie; a member with no headshot still appears (name +
// character) and its photo route 404s. Capturing TV cast disturbs neither the
// Show's identity nor an Episode's watch state, and leaves the season/episode
// structure intact.
func TestTVCastPhotosCaptureAndServe(t *testing.T) {
	srv, _, token, libID := tvCastSetup(t)
	showID := showIDByName(t, srv, token, libID, "The Bear")

	// Show identity + the season/episode structure BEFORE enrichment; capturing cast
	// must not disturb either (ADR-0002/0014). Mark an Episode watched too.
	before := getShowCast(t, srv, token, showID)
	seasonID := before.Seasons[0].ID
	var eps enrichedEpisodesResp
	srv.AuthGET("/api/v1/seasons/"+seasonID+"/episodes", token, &eps)
	if len(eps.Episodes) == 0 {
		t.Fatalf("no episodes in first season before enrich")
	}
	epID := eps.Episodes[0].ID
	epsBefore := len(eps.Episodes)
	srv.JSON(http.MethodPut, "/api/v1/titles/"+epID+"/watchState", token, map[string]any{"watched": true}, nil)

	enrichLib(t, srv, token, libID, "")

	d := getShowCast(t, srv, token, showID)
	if len(d.Show.Cast) != 4 {
		t.Fatalf("show cast = %d members, want 4: %+v", len(d.Show.Cast), d.Show.Cast)
	}
	if before.Show.IdentityKey != "" && d.Show.IdentityKey != before.Show.IdentityKey {
		t.Errorf("show identity changed by cast capture: before=%q after=%q", before.Show.IdentityKey, d.Show.IdentityKey)
	}

	// Requesting TV credits does not alter which episodes exist.
	var epsAfter enrichedEpisodesResp
	srv.AuthGET("/api/v1/seasons/"+seasonID+"/episodes", token, &epsAfter)
	if len(epsAfter.Episodes) != epsBefore {
		t.Errorf("episode count changed by cast capture: before=%d after=%d", epsBefore, len(epsAfter.Episodes))
	}
	// Watch state survives (a lost identity would drop it).
	var epDetail castDetailResp
	srv.AuthGET("/api/v1/titles/"+epID, token, &epDetail)
	if !epDetail.Watched {
		t.Errorf("episode watch state lost across cast capture")
	}

	// White (a Show-only actor) carries a personId + photo version and serves his
	// headshot bytes — proving a Show-only cast member's photo serves to a viewer
	// who can see the Show (access follows the crediting Library via entity_credits).
	white := findShowCredit(t, d, "Jeremy Allen White")
	if white.PersonID != "tmdb:10" || white.Character != "Carmy" {
		t.Errorf("White credit = %+v, want personId tmdb:10 + character", white)
	}
	if white.PhotoVersion == "" {
		t.Errorf("White has a cached photo but no photoVersion cache-bust token: %+v", white)
	}

	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-tvcast")
	resp := cookieGET(t, srv, personPhotoPath("tmdb:10", "profile"), cookie, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("headshot via cookie = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("headshot content-type = %q, want image/*", ct)
	}
	status, body := authBytes(t, srv, token, personPhotoPath("tmdb:10", "profile"))
	if status != http.StatusOK || string(body) != "PHOTO:https://img.example/white.jpg" {
		t.Errorf("headshot bytes = %q (status %d), want the fetched fake bytes", body, status)
	}

	// Edebiri has no headshot: present with name + character, photo route 404s.
	edebiri := findShowCredit(t, d, "Ayo Edebiri")
	if edebiri.PersonID != "tmdb:11" || edebiri.Character != "Sydney" {
		t.Errorf("Edebiri credit = %+v, want personId tmdb:11 + character (name/character kept)", edebiri)
	}
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:11", "profile")); st != http.StatusNotFound {
		t.Errorf("photoless person headshot = %d, want 404", st)
	}
}

// TestTVShowNoCastOmitsStrip: a Show whose faked details carry no cast returns an
// empty cast (the client omits the strip).
func TestTVShowNoCastOmitsStrip(t *testing.T) {
	srv, _, token, libID := tvCastSetup(t)
	enrichLib(t, srv, token, libID, "")

	showID := showIDByName(t, srv, token, libID, "The Daily")
	d := getShowCast(t, srv, token, showID)
	if len(d.Show.Cast) != 0 {
		t.Errorf("cast-less show returned %d cast members, want 0: %+v", len(d.Show.Cast), d.Show.Cast)
	}
}

// TestTVCastPhotosCrossKindDedupe: an actor in BOTH a movie (Dune) and a show (The
// Bear) resolves to a single download / one cached headshot referenced by both.
func TestTVCastPhotosCrossKindDedupe(t *testing.T) {
	requireFixtures(t)
	requireTVFixtures(t)
	prov := &fakeProvider{fn: tvCastMetaFor}
	fetch := &castFetcher{fail: map[string]error{
		"https://img.example/moss.jpg": enrich.ErrArtworkNotFound,
		"https://img.example/ford.jpg": enrich.ErrArtworkNotFound,
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	movieLib := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, movieLib, "")
	tvLib := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, tvLib, "")

	// Enrich the movie library first (Dune fetches Chalamet's headshot), then the TV
	// library (The Bear re-credits the SAME tmdb:1 → must NOT refetch).
	enrichLib(t, srv, token, movieLib, "")
	enrichLib(t, srv, token, tvLib, "")

	duneID := titleIDByName(t, srv, token, movieLib, "Dune")
	dune := getCastDetail(t, srv, token, duneID)
	showID := showIDByName(t, srv, token, tvLib, "The Bear")
	show := getShowCast(t, srv, token, showID)

	if findCredit(t, dune, "Timothée Chalamet").PersonID != "tmdb:1" ||
		findShowCredit(t, show, "Timothée Chalamet").PersonID != "tmdb:1" {
		t.Errorf("both the movie and the show should reference person ref tmdb:1")
	}
	// The shared headshot URL was fetched exactly once (cross-title, cross-kind dedupe).
	if n := fetch.count("https://img.example/chalamet.jpg"); n != 1 {
		t.Errorf("shared headshot fetched %d times, want 1 (cross-kind dedupe)", n)
	}
	// It still serves (one cached row referenced by both the movie and the show).
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:1", "profile")); st != http.StatusOK {
		t.Errorf("shared headshot serve = %d, want 200", st)
	}
}

// TestTVCastPhotosDownloadFailureNonFatal: a Show headshot whose download 404s
// leaves the Show enriched with that cast member's name + character intact and no
// photo; the pass does not fail.
func TestTVCastPhotosDownloadFailureNonFatal(t *testing.T) {
	srv, _, token, libID := tvCastSetup(t)
	res := enrichLib(t, srv, token, libID, "")
	if res.Failed != 0 {
		t.Fatalf("enrich result = %+v, want 0 failed (a headshot 404 must not fail the pass)", res)
	}

	showID := showIDByName(t, srv, token, libID, "The Bear")
	d := getShowCast(t, srv, token, showID)
	moss := findShowCredit(t, d, "Ebon Moss-Bachrach")
	if moss.Character != "Richie" || moss.PersonID != "tmdb:12" {
		t.Errorf("Moss-Bachrach credit = %+v, want name/character intact despite failed headshot", moss)
	}
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:12", "profile")); st != http.StatusNotFound {
		t.Errorf("failed-download headshot = %d, want 404 (no photo)", st)
	}
}
