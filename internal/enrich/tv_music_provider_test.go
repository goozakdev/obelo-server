package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The real TMDB (TV) + MusicBrainz provider HTTP/parse layers, exercised against
// httptest servers serving canned JSON — the secondary, lower seam (the project's
// black-box tests use a fake provider). No live network is ever touched.

// --- TMDB TV ----------------------------------------------------------------

func tmdbTVStub(t *testing.T) *TMDBProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search/tv":
			_, _ = w.Write([]byte(`{"results":[{"id":1399}]}`))
		case strings.Contains(r.URL.Path, "/season/") && strings.Contains(r.URL.Path, "/episode/"):
			_, _ = w.Write([]byte(`{"name":"Winter Is Coming","overview":"Ned heads south.","still_path":"/still.jpg"}`))
		case strings.Contains(r.URL.Path, "/season/"):
			_, _ = w.Write([]byte(`{"poster_path":"/season1.jpg","overview":"Season one."}`))
		case strings.HasPrefix(r.URL.Path, "/tv/"):
			_, _ = w.Write([]byte(`{
			  "id": 1399, "overview": "Noble families vie for the throne.",
			  "genres": [{"name":"Drama"},{"name":"Fantasy"}],
			  "networks": [{"name":"HBO"}],
			  "poster_path": "/poster.jpg", "backdrop_path": "/bg.jpg",
			  "content_ratings": {"results": [
			     {"iso_3166_1":"GB","rating":"15"},
			     {"iso_3166_1":"US","rating":"TV-MA"}
			  ]}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewTMDBProvider("k", "en-US", srv.URL, "https://img/")
}

func TestTMDBShowSeasonEpisode(t *testing.T) {
	p := tmdbTVStub(t)

	show, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "Game of Thrones", Year: 2011})
	if err != nil {
		t.Fatalf("show lookup: %v", err)
	}
	if !show.Matched || show.Overview == "" || show.ContentRating != "TV-MA" || show.Studio != "HBO" {
		t.Errorf("show metadata wrong: %+v", show)
	}
	if len(show.Genres) != 2 || show.ExternalID != "1399" {
		t.Errorf("show genres/id wrong: %+v", show)
	}
	roles := map[string]bool{}
	for _, a := range show.Artwork {
		roles[a.Role] = true
	}
	if !roles["poster"] || !roles["background"] {
		t.Errorf("show artwork roles = %+v", show.Artwork)
	}

	season, err := p.Lookup(context.Background(), TitleRef{Kind: "season", TMDBID: "1399", SeasonNumber: 1})
	if err != nil || !season.Matched || len(season.Artwork) != 1 || season.Artwork[0].Role != "poster" {
		t.Errorf("season lookup wrong: %+v err=%v", season, err)
	}

	ep, err := p.Lookup(context.Background(), TitleRef{Kind: "episode", TMDBID: "1399", SeasonNumber: 1, EpisodeNumber: 1})
	if err != nil {
		t.Fatalf("episode lookup: %v", err)
	}
	if ep.Name != "Winter Is Coming" || ep.Overview == "" {
		t.Errorf("episode metadata wrong: %+v", ep)
	}
	if len(ep.Artwork) != 1 || ep.Artwork[0].Role != "poster" {
		t.Errorf("episode still wrong: %+v", ep.Artwork)
	}
}

func TestTMDBSeasonEpisodeNeedShowID(t *testing.T) {
	p := tmdbTVStub(t)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "season", SeasonNumber: 1}); err != ErrNoMatch {
		t.Errorf("season without show id err = %v, want ErrNoMatch", err)
	}
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "episode", SeasonNumber: 1, EpisodeNumber: 1}); err != ErrNoMatch {
		t.Errorf("episode without show id err = %v, want ErrNoMatch", err)
	}
}

// --- MusicBrainz ------------------------------------------------------------

func mbStub(t *testing.T) (*MusicBrainzProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artist":
			_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","disambiguation":"English rock band","area":{"name":"Oxford"},"tags":[{"name":"alternative rock"},{"name":"art rock"}]}]}`))
		case "/release-group":
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg-1","first-release-date":"1997-05-21","tags":[{"name":"alternative rock"}]}]}`))
		case "/recording":
			_, _ = w.Write([]byte(`{"recordings":[{"id":"rec-1","title":"Paranoid Android"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	p.MinInterval = 0 // don't throttle the test
	return p, &seen
}

func TestMusicBrainzArtistAlbumTrack(t *testing.T) {
	p, _ := mbStub(t)

	artist, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("artist lookup: %v", err)
	}
	if !artist.Matched || artist.Overview == "" || len(artist.Genres) == 0 || artist.ExternalID != "mb-1" {
		t.Errorf("artist metadata wrong: %+v", artist)
	}

	album, err := p.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("album lookup: %v", err)
	}
	if !album.Matched || album.ReleaseDate != "1997-05-21" || len(album.Genres) == 0 {
		t.Errorf("album metadata wrong: %+v", album)
	}
	if len(album.Artwork) != 1 || album.Artwork[0].Role != "cover" ||
		album.Artwork[0].URL != "https://coverart/release-group/rg-1/front-500" {
		t.Errorf("album cover wrong: %+v", album.Artwork)
	}

	track, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Paranoid Android", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("track lookup: %v", err)
	}
	if !track.Matched || track.Name != "Paranoid Android" {
		t.Errorf("track metadata wrong: %+v", track)
	}
}

func TestMusicBrainzNoResultsIsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[],"release-groups":[],"recordings":[]}`))
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	p.MinInterval = 0
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Nobody"}); err != ErrNoMatch {
		t.Errorf("artist err = %v, want ErrNoMatch", err)
	}
}

// A 503 (MusicBrainz rate-limit/temporary-unavailable) is retried with back-off
// rather than dropped: the second attempt succeeds.
func TestMusicBrainzRetriesOn503(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","tags":[{"name":"rock"}]}]}`))
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, "https://coverart", "en-US")
	p.MinInterval = time.Millisecond // tiny throttle + back-off so the test stays fast

	got, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup after 503 retry: %v", err)
	}
	if !got.Matched || got.ExternalID != "mb-1" {
		t.Errorf("metadata wrong after retry: %+v", got)
	}
	if calls != 2 {
		t.Errorf("want 2 attempts (503 then 200), got %d", calls)
	}
}

// CompositeProvider routes by kind: video → TMDB, music → MusicBrainz.
func TestCompositeRoutesByKind(t *testing.T) {
	tmdb := tmdbTVStub(t)
	mb, _ := mbStub(t)
	c := CompositeProvider{Video: tmdb, Music: mb}

	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "show", Title: "GoT"}); err != nil || m.Source != "tmdb" {
		t.Errorf("show routed wrong: %+v err=%v", m, err)
	}
	if m, err := c.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"}); err != nil || m.Source != "musicbrainz" {
		t.Errorf("artist routed wrong: %+v err=%v", m, err)
	}
	// A nil sub-provider degrades to ErrNoMatch for its kinds.
	videoOnly := CompositeProvider{Video: tmdb}
	if _, err := videoOnly.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "x"}); err != ErrNoMatch {
		t.Errorf("nil music provider err = %v, want ErrNoMatch", err)
	}
}
