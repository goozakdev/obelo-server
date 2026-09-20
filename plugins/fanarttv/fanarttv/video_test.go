package fanarttv

import (
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The video half of the same provider — the port of the movie/show tests from
// internal/enrich/fanarttv_test.go, against the same canned bodies.

const fanartMovieJSON = `{
  "name": "Dune",
  "tmdb_id": "438631",
  "movieposter": [
     {"id": "1", "url": "https://assets.fanart.tv/movie-poster-low.jpg", "likes": "4"},
     {"id": "2", "url": "https://assets.fanart.tv/movie-poster-best.jpg", "likes": "31"}
  ],
  "moviebackground": [
     {"id": "3", "url": "https://assets.fanart.tv/movie-bg-best.jpg", "likes": "18"},
     {"id": "4", "url": "https://assets.fanart.tv/movie-bg-low.jpg", "likes": "2"}
  ]
}`

const fanartTVShowJSON = `{
  "name": "Game of Thrones",
  "thetvdb_id": "121361",
  "tvposter": [
     {"id": "1", "url": "https://assets.fanart.tv/tv-poster-low.jpg", "likes": "7"},
     {"id": "2", "url": "https://assets.fanart.tv/tv-poster-best.jpg", "likes": "40"}
  ],
  "showbackground": [
     {"id": "3", "url": "https://assets.fanart.tv/tv-bg-best.jpg", "likes": "22"}
  ]
}`

func TestFanartTVMovieArtworkByTMDBID(t *testing.T) {
	p, host := fanartStub(t, fanartMovieJSON, 0)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "438631"})
	meta := resp.Record
	if resp.Outcome != pluginapi.OutcomeMatched || !meta.Matched || meta.Source != "fanart.tv" {
		t.Errorf("meta = %+v (outcome %q), want matched fanart.tv result", meta, resp.Outcome)
	}
	// Artwork-only: no text fields leak through.
	if meta.Name != "" || meta.Overview != "" || len(meta.Genres) != 0 {
		t.Errorf("video lookup contributed text fields: %+v", meta)
	}
	// The highest-likes poster + background are parsed into their roles.
	if len(meta.Artwork) != 2 {
		t.Fatalf("artwork = %+v, want poster + background", meta.Artwork)
	}
	if meta.Artwork[0].Role != "poster" || meta.Artwork[0].URL != "https://assets.fanart.tv/movie-poster-best.jpg" {
		t.Errorf("poster = %+v, want the highest-likes movieposter", meta.Artwork[0])
	}
	if meta.Artwork[1].Role != "background" || meta.Artwork[1].URL != "https://assets.fanart.tv/movie-bg-best.jpg" {
		t.Errorf("background = %+v, want the highest-likes moviebackground", meta.Artwork[1])
	}
	// The movie endpoint is id-keyed.
	seen := host.Paths()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/movies/438631") {
		t.Errorf("request paths = %v, want a single /movies/438631", seen)
	}
}

func TestFanartTVMovieArtworkByIMDBID(t *testing.T) {
	// No TMDB id on the ref: the movie endpoint falls back to the IMDb id.
	p, host := fanartStub(t, fanartMovieJSON, 0)
	meta := lookup(t, p, pluginapi.MediaRef{Kind: "movie", IMDBID: "tt1160419"}).Record
	if len(meta.Artwork) != 2 || meta.Artwork[0].Role != "poster" {
		t.Errorf("artwork = %+v, want poster + background", meta.Artwork)
	}
	seen := host.Paths()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/movies/tt1160419") {
		t.Errorf("request paths = %v, want a single /movies/tt1160419", seen)
	}
}

func TestFanartTVShowArtworkByTheTVDBID(t *testing.T) {
	p, host := fanartStub(t, fanartTVShowJSON, 0)
	meta := lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "121361"}).Record
	if len(meta.Artwork) != 2 {
		t.Fatalf("artwork = %+v, want poster + background", meta.Artwork)
	}
	if meta.Artwork[0].Role != "poster" || meta.Artwork[0].URL != "https://assets.fanart.tv/tv-poster-best.jpg" {
		t.Errorf("poster = %+v, want the highest-likes tvposter", meta.Artwork[0])
	}
	if meta.Artwork[1].Role != "background" || meta.Artwork[1].URL != "https://assets.fanart.tv/tv-bg-best.jpg" {
		t.Errorf("background = %+v, want the highest-likes showbackground", meta.Artwork[1])
	}
	// The tv endpoint is TheTVDB-id keyed.
	seen := host.Paths()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/tv/121361") {
		t.Errorf("request paths = %v, want a single /tv/121361", seen)
	}
}

func TestFanartTVVideoNoIDIsNoMatch(t *testing.T) {
	p, host := fanartStub(t, fanartMovieJSON, 0)
	// A movie with no TMDB/IMDb id, and a show with no TheTVDB id, are strictly
	// id-keyed no-matches — no outbound call.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "movie", Title: "Dune"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("no-id movie outcome = %q, want no-match", got)
	}
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "show", Title: "Game of Thrones"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("no-id show outcome = %q, want no-match", got)
	}
	// Season/episode are not served by the video path (fanart.tv keys by series id only).
	ep := pluginapi.MediaRef{Kind: "episode", TheTVDBID: "121361", SeasonNumber: 1, EpisodeNumber: 5}
	if got := lookup(t, p, ep).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("episode outcome = %q, want no-match", got)
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("expected zero requests for id-less / unsupported video lookups; saw %v", seen)
	}
}

func TestFanartTVVideoNotFoundIsNoMatch(t *testing.T) {
	// fanart.tv answers an unknown id with 404 — the normal "no record" outcome.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "0"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestFanartTVVideoEmptyImagesIsNoMatch(t *testing.T) {
	// A 200 with no poster/background lists has nothing to contribute.
	p, _ := fanartStub(t, `{"name":"Dune","tmdb_id":"438631"}`, 0)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "438631"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestFanartTVVideoAndArtistCachesDoNotCollide(t *testing.T) {
	// The video cache is namespaced from the artist cache: an artist lookup and a
	// movie lookup that happen to share an id string never serve each other's result.
	// Serve one JSON that carries BOTH artist and movie image lists so each path picks
	// its own fields.
	both := `{
      "artistthumb": [{"url":"https://x/artist.jpg","likes":"5"}],
      "movieposter": [{"url":"https://x/movie-poster.jpg","likes":"5"}],
      "moviebackground": [{"url":"https://x/movie-bg.jpg","likes":"5"}]
    }`
	p, host := fanartStub(t, both, 0)
	// Same raw id "123" used as an MBID and as a TMDB id — distinct cache namespaces.
	artist := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: "123"}).Record
	if len(artist.Artwork) != 1 || artist.Artwork[0].URL != "https://x/artist.jpg" {
		t.Errorf("artist artwork = %+v, want the artistthumb", artist.Artwork)
	}
	movie := lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "123"}).Record
	if len(movie.Artwork) != 2 || movie.Artwork[0].URL != "https://x/movie-poster.jpg" {
		t.Errorf("movie artwork = %+v, want the movieposter+moviebackground", movie.Artwork)
	}
	// Both paths hit the host (different endpoints/namespaces), proving no collision.
	seen := host.Paths()
	if len(seen) != 2 {
		t.Errorf("request count = %d, want 2 (artist + movie, no cache collision); paths=%v", len(seen), seen)
	}
	if !strings.HasSuffix(seen[0], "/music/123") || !strings.HasSuffix(seen[1], "/movies/123") {
		t.Errorf("paths = %v, want /music/123 then /movies/123", seen)
	}
}

func TestFanartTVVideoCachesByID(t *testing.T) {
	p, host := fanartStub(t, fanartMovieJSON, 0)
	for i := 0; i < 3; i++ {
		if got := lookup(t, p, pluginapi.MediaRef{Kind: "movie", TMDBID: "438631"}).Outcome; got != pluginapi.OutcomeMatched {
			t.Fatalf("Lookup %d outcome = %q, want matched", i, got)
		}
	}
	if seen := host.Paths(); len(seen) != 1 {
		t.Errorf("request count = %d, want 1 (video response cached); paths=%v", len(seen), seen)
	}
}
