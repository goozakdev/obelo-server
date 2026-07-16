package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Local TV artwork, end to end (naming-convention.md "Local artwork"). Movies and
// Music have honored on-disk artwork since the beginning; TV did not, so a TV
// Library on a server with NO metadata provider had no artwork at all and no
// local escape hatch. These tests run against exactly that server — testharness.New
// with no enrichment key — so anything they see came off the disk.
//
// Double Show is the fixture's only Show with local artwork; the other three carry
// none and are the absent case, asserted here rather than assumed.

// findShowSummary returns the listed Show with the given title.
func findShowSummary(t *testing.T, shows showsListResp, title string) showSummaryResp {
	t.Helper()
	for _, s := range shows.Shows {
		if s.Title == title {
			return s
		}
	}
	t.Fatalf("%q not in the shows list: %+v", title, shows.Shows)
	return showSummaryResp{}
}

// TestTVLocalShowArtworkServed: `poster.jpg`/`fanart.jpg` in a Show folder reach
// the Shows list as posterUrl/backgroundUrl and serve real bytes — with no
// provider, no key, and no network.
func TestTVLocalShowArtworkServed(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	shows := listShows(t, srv, token, libID)
	dbl := findShowSummary(t, shows, "Double Show")
	if dbl.PosterURL == "" {
		t.Errorf("show posterUrl empty — the Show folder's poster.jpg was not discovered")
	}
	if dbl.BackgroundURL == "" {
		t.Errorf("show backgroundUrl empty — the Show folder's fanart.jpg was not discovered")
	}

	// The URL must actually serve the local file, not 404 with a populated field.
	status, body := authBytes(t, srv, token, "/api/v1/shows/"+dbl.ID+"/artwork/poster")
	if status != http.StatusOK {
		t.Fatalf("show artwork GET = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("show artwork served 0 bytes")
	}

	// A Show with no local images stays absent — the field is evidence, not
	// decoration, and this server has no Enrichment to fill it.
	for _, s := range shows.Shows {
		if s.Title != "Double Show" && s.PosterURL != "" {
			t.Errorf("%q has posterUrl %q but ships no artwork; something is matching too broadly",
				s.Title, s.PosterURL)
		}
	}
}

// TestTVLocalSeasonPosterServed: `Season 01.jpg` in the SHOW folder becomes that
// Season's posterUrl and serves its bytes. The Bear's seasons ship no image and
// must stay empty on this provider-less server — the pair is what shows the field
// tracks a real file. (That a poster attaches to the season it NAMES, rather than
// to every season, is pinned precisely by the resolver's TestSeasonPosterDiscovered,
// which needs two seasons on one Show.)
func TestTVLocalSeasonPosterServed(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	shows := listShows(t, srv, token, libID)

	seasons := showSeasons(t, srv, token, findShow(t, shows, "Double Show"))
	if len(seasons.Seasons) != 1 || seasons.Seasons[0].SeasonNumber != 1 {
		t.Fatalf("want Double Show's single Season 01; got %+v", seasons.Seasons)
	}
	s1 := seasons.Seasons[0]
	if s1.PosterURL == "" {
		t.Fatalf("Season 01 posterUrl empty — `Season 01.jpg` was not discovered")
	}
	status, body := authBytes(t, srv, token, "/api/v1/seasons/"+s1.ID+"/artwork/poster")
	if status != http.StatusOK {
		t.Fatalf("season artwork GET = %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("season artwork served 0 bytes")
	}

	// A Show with no `Season NN.jpg` gets no season posters at all here.
	bear := showSeasons(t, srv, token, findShow(t, shows, "The Bear"))
	for _, s := range bear.Seasons {
		if s.PosterURL != "" {
			t.Errorf("The Bear season %d has posterUrl %q, but ships no local image and this "+
				"server has no Enrichment", s.SeasonNumber, s.PosterURL)
		}
	}
}

// TestTVLocalArtworkSurvivesRescan: a rescan must not drop local TV artwork. The
// writer clears source='local' and rewrites it, so a second scan is the case that
// would expose a clear-without-rewrite or a duplicate-row violation of
// UNIQUE(entity_type, entity_id, role, source).
func TestTVLocalArtworkSurvivesRescan(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	scanLib(t, srv, token, libID, "") // the rescan
	shows := listShows(t, srv, token, libID)
	dbl := findShowSummary(t, shows, "Double Show")
	if dbl.PosterURL == "" {
		t.Errorf("show posterUrl empty after a rescan — local artwork was cleared and not rewritten")
	}
	if status, _ := authBytes(t, srv, token, "/api/v1/shows/"+dbl.ID+"/artwork/poster"); status != http.StatusOK {
		t.Errorf("show artwork GET after rescan = %d, want 200", status)
	}
	seasons := showSeasons(t, srv, token, dbl.ID)
	for _, s := range seasons.Seasons {
		if s.SeasonNumber == 1 && s.PosterURL == "" {
			t.Errorf("Season 01 posterUrl empty after a rescan")
		}
	}
}

// TestTVLocalArtworkBeatsEnrichment: the whole point of source='local'. A server
// WITH a provider fetches Show/Season artwork, but the on-disk image still wins —
// "Local always wins; Enrichment only fills what is absent" (naming-convention.md).
// Double Show ships local art and must keep serving it; The Bear ships none and
// takes the fetched bytes, which is what proves the enrichment actually ran (and
// is asserted in full by TestEnrichTVShowSeasonEpisode).
func TestTVLocalArtworkBeatsEnrichment(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	fetch := &fakeFetcher{data: []byte("REMOTEPOSTER"), contentType: "image/jpeg"}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	showID := findShow(t, shows, "Double Show")

	status, body := authBytes(t, srv, token, "/api/v1/shows/"+showID+"/artwork/poster")
	if status != http.StatusOK {
		t.Fatalf("show artwork GET = %d, want 200", status)
	}
	if string(body) == "REMOTEPOSTER" {
		t.Errorf("served the FETCHED show poster; the local poster.jpg must win")
	}

	// The Bear has no local art, so it must take the fetched bytes — without this the
	// test above could pass on a server where enrichment silently did nothing.
	bearID := findShow(t, shows, "The Bear")
	if st, b := authBytes(t, srv, token, "/api/v1/shows/"+bearID+"/artwork/poster"); st != http.StatusOK || string(b) != "REMOTEPOSTER" {
		t.Errorf("The Bear show poster = %d %q, want the fetched REMOTEPOSTER — enrichment did not run", st, b)
	}

	seasons := showSeasons(t, srv, token, showID)
	for _, s := range seasons.Seasons {
		if s.SeasonNumber != 1 {
			continue
		}
		st, b := authBytes(t, srv, token, "/api/v1/seasons/"+s.ID+"/artwork/poster")
		if st != http.StatusOK {
			t.Fatalf("season artwork GET = %d, want 200", st)
		}
		if string(b) == "REMOTEPOSTER" {
			t.Errorf("served the FETCHED season poster; the local `Season 01.jpg` must win")
		}
	}
}
