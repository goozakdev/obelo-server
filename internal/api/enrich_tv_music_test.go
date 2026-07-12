package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// External-metadata-enrichment issue 03 black-box tests: scan a TV / Music
// library, drive POST /libraries/{id}/enrich with a FAKE per-kind provider +
// fetcher (zero network), and assert the decorated Show/Season/Episode and
// Artist/Album/Track through the HTTP API — including the date-based-episode
// identity-stability guarantee (display title changes, identity does not).

// tvMusicProvider returns canned metadata per kind, so one fake drives every
// entity the TV/Music passes look up.
func tvMusicProvider() *fakeProvider {
	return &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		switch ref.Kind {
		case "show":
			return enrich.TitleMetadata{
				Matched: true, Overview: "A restaurant drama.", ContentRating: "TV-MA",
				Studio: "FX", Genres: []string{"Comedy", "Drama"}, ExternalID: "1001", Source: "tmdb",
				Artwork: []enrich.ArtworkRef{
					{Role: "poster", URL: "https://img.example/show-poster.jpg"},
					{Role: "background", URL: "https://img.example/show-bg.jpg"},
				},
			}, nil
		case "season":
			return enrich.TitleMetadata{
				Matched: true, Source: "tmdb",
				Artwork: []enrich.ArtworkRef{{Role: "poster", URL: "https://img.example/season-poster.jpg"}},
			}, nil
		case "episode":
			return enrich.TitleMetadata{
				Matched: true, Name: "The Suitcase", Overview: "Carmy opens up.", Source: "tmdb",
				Artwork: []enrich.ArtworkRef{{Role: "poster", URL: "https://img.example/still.jpg"}},
			}, nil
		case "artist":
			return enrich.TitleMetadata{
				Matched: true, Overview: "English rock band from Oxford.",
				Genres: []string{"Alternative Rock"}, ExternalID: "mb-artist", Source: "musicbrainz",
				Artwork: []enrich.ArtworkRef{
					{Role: "poster", URL: "https://img.example/artist.jpg"},
					{Role: "background", URL: "https://img.example/artist-bg.jpg"},
					{Role: "logo", URL: "https://img.example/artist-logo.png"},
				},
			}, nil
		case "album":
			return enrich.TitleMetadata{
				Matched: true, ReleaseDate: "1997", Genres: []string{"Alternative Rock", "Art Rock"},
				ExternalID: "mb-rg", Source: "musicbrainz",
				Artwork: []enrich.ArtworkRef{{Role: "cover", URL: "https://img.example/cover.jpg"}},
			}, nil
		case "track":
			return enrich.TitleMetadata{
				Matched: true, Overview: "A standout track.", Name: "Canonical Title", Source: "musicbrainz",
			}, nil
		default:
			return enrich.TitleMetadata{}, enrich.ErrNoMatch
		}
	}}
}

// --- Test wire shapes (enriched parent/leaf fields issue 03 adds) -----------

type enrichedShowResp struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Overview         string   `json:"overview"`
	Genres           []string `json:"genres"`
	ContentRating    string   `json:"contentRating"`
	Network          string   `json:"network"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
	PosterURL        string   `json:"posterUrl"`
	BackgroundURL    string   `json:"backgroundUrl"`
}

type enrichedSeasonsResp struct {
	Show    enrichedShowResp `json:"show"`
	Seasons []struct {
		ID           string `json:"id"`
		SeasonNumber int    `json:"seasonNumber"`
		PosterURL    string `json:"posterUrl"`
	} `json:"seasons"`
}

type enrichedEpisodesResp struct {
	Episodes []struct {
		ID               string `json:"id"`
		Title            string `json:"title"`
		EpisodeLabel     string `json:"episodeLabel"`
		Overview         string `json:"overview"`
		EnrichmentStatus string `json:"enrichmentStatus"`
		StillURL         string `json:"stillUrl"`
	} `json:"episodes"`
}

type enrichedShowsListResp struct {
	Shows []enrichedShowResp `json:"shows"`
}

type enrichedEpisodeDetailResp struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	DisplayTitle     string `json:"displayTitle"`
	Overview         string `json:"overview"`
	EnrichmentStatus string `json:"enrichmentStatus"`
	Watched          bool   `json:"watched"`
	Episode          *struct {
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		EpisodeLabel  string `json:"episodeLabel"`
	} `json:"episode"`
}

type enrichedArtistResp struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Overview         string   `json:"overview"`
	Genres           []string `json:"genres"`
	EnrichmentStatus string   `json:"enrichmentStatus"`
	ArtworkURL       string   `json:"artworkUrl"`
	BackgroundURL    string   `json:"backgroundUrl"`
	LogoURL          string   `json:"logoUrl"`
}

type enrichedAlbumsResp struct {
	Artist enrichedArtistResp `json:"artist"`
	Albums []struct {
		ID         string   `json:"id"`
		Title      string   `json:"title"`
		HasArtwork bool     `json:"hasArtwork"`
		Genres     []string `json:"genres"`
	} `json:"albums"`
}

type enrichedArtistsListResp struct {
	Artists []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		ArtworkURL string `json:"artworkUrl"`
	} `json:"artists"`
}

type enrichedTracksResp struct {
	Tracks []struct {
		ID               string `json:"id"`
		Title            string `json:"title"`
		Overview         string `json:"overview"`
		EnrichmentStatus string `json:"enrichmentStatus"`
	} `json:"tracks"`
}

// --- TV ---------------------------------------------------------------------

func TestEnrichTVShowSeasonEpisode(t *testing.T) {
	requireTVFixtures(t)
	prov := tvMusicProvider()
	fetch := &fakeFetcher{data: []byte("FAKEART"), contentType: "image/jpeg"}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")

	res := enrichLib(t, srv, token, libID, "")
	if res.Matched == 0 {
		t.Fatalf("tv enrich matched 0 episodes; result=%+v", res)
	}

	// Find The Bear via the (enriched) Show grid.
	var grid enrichedShowsListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &grid)
	var showID string
	for _, s := range grid.Shows {
		if s.Title == "The Bear" {
			showID = s.ID
			if s.Overview == "" || len(s.Genres) == 0 || s.ContentRating != "TV-MA" || s.Network != "FX" {
				t.Errorf("show grid not enriched: %+v", s)
			}
			if s.PosterURL == "" {
				t.Errorf("show grid missing posterUrl: %+v", s)
			}
		}
	}
	if showID == "" {
		t.Fatalf("The Bear not found in grid: %+v", grid.Shows)
	}

	// Show detail + seasons.
	var seasons enrichedSeasonsResp
	srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &seasons)
	if seasons.Show.Overview == "" || seasons.Show.ContentRating != "TV-MA" || seasons.Show.PosterURL == "" {
		t.Errorf("show detail not enriched: %+v", seasons.Show)
	}
	var seasonWithPoster string
	var regularSeason string
	for _, s := range seasons.Seasons {
		if s.PosterURL == "" {
			t.Errorf("season %d missing posterUrl", s.SeasonNumber)
		} else {
			seasonWithPoster = s.PosterURL
		}
		if s.SeasonNumber == 1 {
			regularSeason = s.ID
		}
	}
	if regularSeason == "" {
		t.Fatalf("season 1 not found: %+v", seasons.Seasons)
	}

	// Episodes carry the canonical display title + overview + still.
	var eps enrichedEpisodesResp
	srv.AuthGET("/api/v1/seasons/"+regularSeason+"/episodes", token, &eps)
	if len(eps.Episodes) == 0 {
		t.Fatalf("no episodes in season 1")
	}
	for _, e := range eps.Episodes {
		if e.Title != "The Suitcase" || e.Overview == "" || e.EnrichmentStatus != "matched" || e.StillURL == "" {
			t.Errorf("episode not enriched: %+v", e)
		}
	}

	// Artwork bytes serve through the existing endpoints.
	if status, body := authBytes(t, srv, token, seasonWithPoster); status != http.StatusOK || string(body) != "FAKEART" {
		t.Errorf("season poster bytes = %d %q, want 200 FAKEART", status, body)
	}
	if status, body := authBytes(t, srv, token, seasons.Show.PosterURL); status != http.StatusOK || string(body) != "FAKEART" {
		t.Errorf("show poster bytes = %d %q, want 200 FAKEART", status, body)
	}
}

// TestEnrichTVDateBasedEpisodeIdentityStable: a date-based Episode (The Daily)
// gains a canonical DISPLAY title from Enrichment, but its identity_key,
// season/episode/label and watch state are byte-for-byte unchanged (ADR-0014).
func TestEnrichTVDateBasedEpisodeIdentityStable(t *testing.T) {
	requireTVFixtures(t)
	prov := tvMusicProvider()
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")

	// Locate The Daily's single date-based episode.
	var grid enrichedShowsListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &grid)
	var dailyID string
	for _, s := range grid.Shows {
		if s.Title == "The Daily" {
			dailyID = s.ID
		}
	}
	if dailyID == "" {
		t.Fatalf("The Daily not found")
	}
	var seasons enrichedSeasonsResp
	srv.AuthGET("/api/v1/shows/"+dailyID+"/seasons", token, &seasons)
	epID := firstEpisodeID(t, srv, token, seasons)

	var before enrichedEpisodeDetailResp
	srv.AuthGET("/api/v1/titles/"+epID, token, &before)

	// Mark watched BEFORE enrichment; a lost identity would drop the watch state.
	srv.JSON(http.MethodPut, "/api/v1/titles/"+epID+"/watchState", token, map[string]any{"watched": true}, nil)

	enrichLib(t, srv, token, libID, "")

	var after enrichedEpisodeDetailResp
	srv.AuthGET("/api/v1/titles/"+epID, token, &after)

	if after.DisplayTitle != "The Suitcase" {
		t.Errorf("display title not enriched: %q", after.DisplayTitle)
	}
	if after.Title != before.Title {
		t.Errorf("parsed identity title changed: %q -> %q", before.Title, after.Title)
	}
	if after.Episode == nil || before.Episode == nil ||
		after.Episode.SeasonNumber != before.Episode.SeasonNumber ||
		after.Episode.EpisodeNumber != before.Episode.EpisodeNumber ||
		after.Episode.EpisodeLabel != before.Episode.EpisodeLabel {
		t.Errorf("episode ordering changed: %+v -> %+v", before.Episode, after.Episode)
	}
	if !after.Watched {
		t.Errorf("watch state lost across enrichment (identity not preserved)")
	}
}

func firstEpisodeID(t *testing.T, srv *testharness.Server, token string, seasons enrichedSeasonsResp) string {
	t.Helper()
	for _, s := range seasons.Seasons {
		var eps enrichedEpisodesResp
		srv.AuthGET("/api/v1/seasons/"+s.ID+"/episodes", token, &eps)
		if len(eps.Episodes) > 0 {
			return eps.Episodes[0].ID
		}
	}
	t.Fatalf("no episodes found")
	return ""
}

// --- Music ------------------------------------------------------------------

func TestEnrichMusicArtistAlbumTrack(t *testing.T) {
	requireMusicFixtures(t)
	prov := tvMusicProvider()
	fetch := &fakeFetcher{data: []byte("COVERBYTES"), contentType: "image/jpeg"}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")

	enrichLib(t, srv, token, libID, "")

	// Find Radiohead via the artist list.
	var list enrichedArtistsListResp
	srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &list)
	var artistID, listArtworkURL string
	for _, a := range list.Artists {
		if a.Name == "Radiohead" {
			artistID = a.ID
			listArtworkURL = a.ArtworkURL
		}
	}
	if artistID == "" {
		t.Fatalf("Radiohead not found: %+v", list.Artists)
	}
	// The lean Artist list must advertise the fetched artist image, so the grid
	// shows it without clicking into the detail (regression: list previously
	// omitted artworkUrl and showed only initials placeholders).
	if listArtworkURL == "" {
		t.Errorf("artist list omitted artworkUrl for an enriched artist: %+v", list.Artists)
	}
	if status, body := authBytes(t, srv, token, listArtworkURL); status != http.StatusOK || string(body) != "COVERBYTES" {
		t.Errorf("artist image from list URL = %d %q, want 200 COVERBYTES", status, body)
	}

	var albums enrichedAlbumsResp
	srv.AuthGET("/api/v1/artists/"+artistID+"/albums", token, &albums)
	if albums.Artist.Overview == "" || len(albums.Artist.Genres) == 0 || albums.Artist.ArtworkURL == "" {
		t.Errorf("artist not enriched: %+v", albums.Artist)
	}
	// Artists carry a Background + ClearLOGO like a Show/Movie: the detail advertises
	// both role URLs and each serves the fetched bytes (the list, by contrast, stays
	// lean — only the artist photo above).
	if albums.Artist.BackgroundURL == "" || albums.Artist.LogoURL == "" {
		t.Errorf("artist detail missing background/logo URL: %+v", albums.Artist)
	}
	for _, u := range []string{albums.Artist.BackgroundURL, albums.Artist.LogoURL} {
		if status, body := authBytes(t, srv, token, u); status != http.StatusOK || string(body) != "COVERBYTES" {
			t.Errorf("artist artwork %q = %d %q, want 200 COVERBYTES", u, status, body)
		}
	}
	var albumID string
	for _, a := range albums.Albums {
		if a.Title == "OK Computer" {
			albumID = a.ID
			if !a.HasArtwork || len(a.Genres) == 0 {
				t.Errorf("album not enriched: %+v", a)
			}
		}
	}
	if albumID == "" {
		t.Fatalf("OK Computer not found: %+v", albums.Albums)
	}

	// Tracks gain an overview; the tag-derived title is NOT overwritten (tags win).
	var tracks enrichedTracksResp
	srv.AuthGET("/api/v1/albums/"+albumID+"/tracks", token, &tracks)
	if len(tracks.Tracks) == 0 {
		t.Fatalf("no tracks for OK Computer")
	}
	for _, tr := range tracks.Tracks {
		if tr.Overview == "" || tr.EnrichmentStatus != "matched" {
			t.Errorf("track not enriched: %+v", tr)
		}
		if tr.Title == "Canonical Title" {
			t.Errorf("track tag title was overwritten by enrichment (tags must win): %+v", tr)
		}
	}

	// The fetched album cover serves through the existing album artwork endpoint.
	if status, body := authBytes(t, srv, token, "/api/v1/albums/"+albumID+"/artwork"); status != http.StatusOK || string(body) != "COVERBYTES" {
		t.Errorf("album cover bytes = %d %q, want 200 COVERBYTES", status, body)
	}
	// The artist image serves too.
	if status, body := authBytes(t, srv, token, albums.Artist.ArtworkURL); status != http.StatusOK || string(body) != "COVERBYTES" {
		t.Errorf("artist image bytes = %d %q, want 200 COVERBYTES", status, body)
	}
}

// TestEnrichMusicWithoutTMDBKey: Music enrichment runs with only the MusicBrainz
// opt-in and NO TMDB key (MusicBrainz + Cover Art Archive need none), while a video
// library in the same config stays 'disabled' because TMDB (the video provider) has
// no key. This is the per-kind enablement gate (ADR-0001 offline-first preserved:
// Music is still off until explicitly opted in).
func TestEnrichMusicWithoutTMDBKey(t *testing.T) {
	requireMusicFixtures(t)
	requireTVFixtures(t)
	prov := tvMusicProvider()
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true), // note: NO WithEnrichmentKey
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("COVERBYTES"), contentType: "image/jpeg"}),
	)
	token := adminToken(t, srv)

	// Music library enriches with no TMDB key configured.
	musicLib := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, musicLib, "")
	res := enrichLib(t, srv, token, musicLib, "")
	if res.Matched == 0 {
		t.Fatalf("music enrich matched 0 tracks without a TMDB key; result=%+v", res)
	}

	var list enrichedArtistsListResp
	srv.AuthGET("/api/v1/libraries/"+musicLib+"/titles?limit=100", token, &list)
	var artistID string
	for _, a := range list.Artists {
		if a.Name == "Radiohead" {
			artistID = a.ID
		}
	}
	if artistID == "" {
		t.Fatalf("Radiohead not found: %+v", list.Artists)
	}
	var albums enrichedAlbumsResp
	srv.AuthGET("/api/v1/artists/"+artistID+"/albums", token, &albums)
	if albums.Artist.Overview == "" || len(albums.Artist.Genres) == 0 || albums.Artist.ArtworkURL == "" {
		t.Errorf("artist not enriched without a TMDB key: %+v", albums.Artist)
	}

	// A video (TV) library in the SAME server stays disabled — TMDB has no key.
	tvLib := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, tvLib, "")
	tvRes := enrichLib(t, srv, token, tvLib, "")
	if tvRes.Disabled == 0 || tvRes.Matched != 0 {
		t.Errorf("tv enrich without a key = %+v, want disabled>0 / matched 0", tvRes)
	}
}

// TestEnrichTVDisabledAndIdempotent: with no provider key the TV pass is a no-op
// (episodes 'disabled', no calls); a second full pass after enabling doesn't
// duplicate parent genres/artwork.
func TestEnrichTVDisabledAndIdempotent(t *testing.T) {
	requireTVFixtures(t)
	prov := tvMusicProvider()

	// Disabled: no key.
	off := testharness.New(t,
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, off)
	libID := createTVLibrary(t, off, token, tvRoot(t))
	scanLib(t, off, token, libID, "")
	res := enrichLib(t, off, token, libID, "")
	if res.Disabled == 0 || res.Matched != 0 {
		t.Fatalf("disabled tv result = %+v, want disabled>0 / matched 0", res)
	}
	if prov.calls() != 0 {
		t.Errorf("provider called %d times while disabled, want 0", prov.calls())
	}

	// Enabled + idempotent: two full passes, show genres stay deduped.
	prov2 := tvMusicProvider()
	on := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov2),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token2 := adminToken(t, on)
	lib2 := createTVLibrary(t, on, token2, tvRoot(t))
	scanLib(t, on, token2, lib2, "")
	enrichLib(t, on, token2, lib2, "full")
	enrichLib(t, on, token2, lib2, "full")

	var grid enrichedShowsListResp
	on.AuthGET("/api/v1/libraries/"+lib2+"/titles?limit=100", token2, &grid)
	for _, s := range grid.Shows {
		if s.Title == "The Bear" && len(s.Genres) != 2 {
			t.Errorf("show genres after re-enrich = %d, want 2 (no dup)", len(s.Genres))
		}
	}
}

// --- Cross-kind genre filter + Home/search surfacing ------------------------

type enrichedSearchResp struct {
	Shows    []enrichedShowResp `json:"shows"`
	Episodes []struct {
		Title            string   `json:"title"`
		Overview         string   `json:"overview"`
		Genres           []string `json:"genres"`
		EnrichmentStatus string   `json:"enrichmentStatus"`
	} `json:"episodes"`
}

type enrichedHomeResp struct {
	RecentlyAdded []struct {
		Kind         string `json:"kind"`
		Title        string `json:"title"`
		Overview     string `json:"overview"`
		DisplayTitle string `json:"displayTitle"`
	} `json:"recentlyAdded"`
}

// TestEnrichCrossKindGenreFilterAndSummaries closes the issue-03 acceptance gap:
// after enriching a TV and a Music library, filter[genre]= must work on the Show
// grid AND the Artist list, and the enriched descriptive fields must surface in
// the /search and /home summaries (not just the dedicated browse detail reads).
func TestEnrichCrossKindGenreFilterAndSummaries(t *testing.T) {
	requireTVFixtures(t)
	requireMusicFixtures(t)

	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(tvMusicProvider()),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("FAKEART"), contentType: "image/jpeg"}),
	)
	token := adminToken(t, srv)
	tvLib := createTVLibrary(t, srv, token, tvRoot(t))
	musicLib := createMusicLibrary(t, srv, token, musicRoot(t))
	for _, id := range []string{tvLib, musicLib} {
		scanLib(t, srv, token, id, "")
		enrichLib(t, srv, token, id, "")
	}

	// filter[genre] on the Show grid: "Comedy" matches The Bear (genres
	// Comedy+Drama from the fake), "Jazz" matches nothing.
	var shows enrichedShowsListResp
	srv.AuthGET("/api/v1/libraries/"+tvLib+"/titles?filter[genre]=Comedy&limit=100", token, &shows)
	if !hasShow(shows.Shows, "The Bear") {
		t.Errorf("filter[genre]=Comedy shows = %+v, want The Bear", shows.Shows)
	}
	shows = enrichedShowsListResp{}
	srv.AuthGET("/api/v1/libraries/"+tvLib+"/titles?filter[genre]=Jazz&limit=100", token, &shows)
	if hasShow(shows.Shows, "The Bear") {
		t.Errorf("filter[genre]=Jazz returned The Bear; want it excluded: %+v", shows.Shows)
	}

	// filter[genre] on the Artist list: "Alternative Rock" matches Radiohead.
	var artists enrichedArtistsListResp
	srv.AuthGET("/api/v1/libraries/"+musicLib+"/titles?filter[genre]=Alternative+Rock&limit=100", token, &artists)
	foundArtist := false
	for _, a := range artists.Artists {
		if a.Name == "Radiohead" {
			foundArtist = true
		}
	}
	if !foundArtist {
		t.Errorf("filter[genre]=Alternative Rock artists = %+v, want Radiohead", artists.Artists)
	}

	// /search summaries carry enrichment: the Show summary gains overview/genres/
	// status, and an Episode summary gains its enriched overview.
	var sr enrichedSearchResp
	srv.AuthGET("/api/v1/search?q=Bear", token, &sr)
	if !hasShow(sr.Shows, "The Bear") {
		t.Fatalf("search Bear shows = %+v, want The Bear", sr.Shows)
	}
	for _, s := range sr.Shows {
		if s.Title == "The Bear" && (s.Overview == "" || len(s.Genres) == 0 || s.EnrichmentStatus != "matched") {
			t.Errorf("search Show summary not enriched: %+v", s)
		}
	}
	sr = enrichedSearchResp{}
	srv.AuthGET("/api/v1/search?q=System", token, &sr) // The Bear S01E01 stored title
	if len(sr.Episodes) == 0 {
		t.Fatalf("search System returned no episodes")
	}
	if sr.Episodes[0].Overview == "" || sr.Episodes[0].EnrichmentStatus != "matched" {
		t.Errorf("search Episode summary not enriched: %+v", sr.Episodes[0])
	}

	// /home RecentlyAdded carries enrichment: a newly-added Episode shows its
	// enriched overview + canonical display title.
	var home enrichedHomeResp
	srv.AuthGET("/api/v1/home", token, &home)
	foundEnrichedEpisode := false
	for _, r := range home.RecentlyAdded {
		if r.Kind == "episode" && r.Overview != "" && r.DisplayTitle == "The Suitcase" {
			foundEnrichedEpisode = true
		}
	}
	if !foundEnrichedEpisode {
		t.Errorf("no enriched episode in /home recentlyAdded: %+v", home.RecentlyAdded)
	}
}

func hasShow(shows []enrichedShowResp, title string) bool {
	for _, s := range shows {
		if s.Title == title {
			return true
		}
	}
	return false
}
