package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real FanartTVProvider's HTTP/parse layer, exercised against an httptest
// server serving canned fanart.tv JSON — the secondary, lower seam (the project's
// black-box tests use a fake provider). No live network is ever touched.

const fanartArtistJSON = `{
  "name": "Radiohead",
  "mbid_id": "a74b1b7f-71a5-4011-9441-d0b5e4122711",
  "artistthumb": [
     {"id": "1", "url": "https://assets.fanart.tv/thumb-low.jpg", "likes": "3"},
     {"id": "2", "url": "https://assets.fanart.tv/thumb-best.jpg", "likes": "27"},
     {"id": "3", "url": "https://assets.fanart.tv/thumb-mid.jpg", "likes": "9"}
  ],
  "artistbackground": [
     {"id": "4", "url": "https://assets.fanart.tv/bg.jpg", "likes": "5"}
  ]
}`

const mbid = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

// fanartStub serves the artist endpoint and records the request paths so a test
// can assert the lookup is MBID-keyed.
func fanartStub(t *testing.T, body string, status int) (*FanartTVProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewFanartTVProvider("k", srv.URL), &seen
}

func TestFanartTVBestArtistThumb(t *testing.T) {
	p, seen := fanartStub(t, fanartArtistJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched || meta.Source != "fanart.tv" {
		t.Errorf("meta = %+v, want matched fanart.tv result", meta)
	}
	// The highest-likes artistthumb is parsed into the one poster-role ref.
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "poster" ||
		meta.Artwork[0].URL != "https://assets.fanart.tv/thumb-best.jpg" {
		t.Errorf("artwork = %+v, want single poster = thumb-best", meta.Artwork)
	}
	// The request is MBID-keyed.
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/music/"+mbid) {
		t.Errorf("request paths = %v, want a single /music/%s", *seen, mbid)
	}
}

func TestFanartTVNoImageIsNoMatch(t *testing.T) {
	// A 200 with no artistthumb (only a background) is a no-match for the poster.
	p, _ := fanartStub(t, `{"name":"X","artistbackground":[{"url":"https://x/bg.jpg","likes":"1"}]}`, 0)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestFanartTVNotFoundIsNoMatch(t *testing.T) {
	// fanart.tv answers an unknown MBID with 404 — the normal "no record" outcome.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestFanartTVNonArtistOrNoMBIDSkips(t *testing.T) {
	p, seen := fanartStub(t, fanartArtistJSON, 0)
	// A non-artist kind is not fanart.tv's.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: mbid}); err != ErrNoMatch {
		t.Errorf("album err = %v, want ErrNoMatch", err)
	}
	// An artist with no MBID is skipped entirely — fanart.tv is strictly MBID-keyed.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist"}); err != ErrNoMatch {
		t.Errorf("no-mbid err = %v, want ErrNoMatch", err)
	}
	if len(*seen) != 0 {
		t.Errorf("expected zero requests for non-artist / no-mbid lookups; saw %v", *seen)
	}
}

func TestFanartTVCachesByMBID(t *testing.T) {
	p, seen := fanartStub(t, fanartArtistJSON, 0)
	for i := 0; i < 3; i++ {
		if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	// The response cache means a re-enrich of the same artist re-hits fanart.tv once.
	if len(*seen) != 1 {
		t.Errorf("request count = %d, want 1 (cached); paths=%v", len(*seen), *seen)
	}
}

// TestFanartTVArtistCandidates: the Artist Photo picker (artwork-management/02)
// surfaces the FULL artistthumb[] as candidates — not just the one "best" the
// single-image Lookup collapses to — highest-"likes" first, tagged fanart.tv, and
// MBID-keyed.
func TestFanartTVArtistCandidates(t *testing.T) {
	p, seen := fanartStub(t, fanartArtistJSON, 0)
	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	// All three thumbs are surfaced (the list is no longer discarded).
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3 (the full artistthumb[])", len(cands))
	}
	// Ordered by likes descending: best(27) → mid(9) → low(3).
	want := []string{
		"https://assets.fanart.tv/thumb-best.jpg",
		"https://assets.fanart.tv/thumb-mid.jpg",
		"https://assets.fanart.tv/thumb-low.jpg",
	}
	for i, w := range want {
		if cands[i].URL != w {
			t.Errorf("candidate[%d].URL = %q, want %q", i, cands[i].URL, w)
		}
		if cands[i].Source != "fanart.tv" {
			t.Errorf("candidate[%d].Source = %q, want fanart.tv", i, cands[i].Source)
		}
	}
	// The request is MBID-keyed, exactly like the single-image Lookup.
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/music/"+mbid) {
		t.Errorf("request paths = %v, want a single /music/%s", *seen, mbid)
	}
}

func TestFanartTVArtistCandidatesNonArtistOrNoMBID(t *testing.T) {
	p, seen := fanartStub(t, fanartArtistJSON, 0)
	// A non-artist kind: fanart.tv owns no listable set there (video lists via TMDB).
	if _, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "album", MusicbrainzID: mbid}, "cover"); err != ErrSearchUnavailable {
		t.Errorf("album err = %v, want ErrSearchUnavailable", err)
	}
	// An artist with no MBID is skipped entirely — strictly MBID-keyed — no call, no
	// candidates, no error (the picker degrades gracefully).
	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist"}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("no-mbid = (%+v, %v), want (nil, nil)", cands, err)
	}
	if len(*seen) != 0 {
		t.Errorf("expected zero requests for non-artist / no-mbid candidates; saw %v", *seen)
	}
}

func TestFanartTVArtistCandidatesNotFoundIsEmpty(t *testing.T) {
	// A 404 (unknown MBID) is the normal "no images" outcome — empty, not an error.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("404 = (%+v, %v), want (nil, nil)", cands, err)
	}
}

func TestFanartTVArtistCandidatesNon2xxIsError(t *testing.T) {
	// A non-404 non-2xx (e.g. 500) is a real error the chain logs and swallows.
	p, _ := fanartStub(t, `oops`, http.StatusInternalServerError)
	if _, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}, "poster"); err == nil {
		t.Errorf("err = nil, want a real error on a 500")
	}
}

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
	p, seen := fanartStub(t, fanartMovieJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched || meta.Source != "fanart.tv" {
		t.Errorf("meta = %+v, want matched fanart.tv result", meta)
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
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/movies/438631") {
		t.Errorf("request paths = %v, want a single /movies/438631", *seen)
	}
}

func TestFanartTVMovieArtworkByIMDBID(t *testing.T) {
	// No TMDB id on the ref: the movie endpoint falls back to the IMDb id.
	p, seen := fanartStub(t, fanartMovieJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", IMDBID: "tt1160419"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(meta.Artwork) != 2 || meta.Artwork[0].Role != "poster" {
		t.Errorf("artwork = %+v, want poster + background", meta.Artwork)
	}
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/movies/tt1160419") {
		t.Errorf("request paths = %v, want a single /movies/tt1160419", *seen)
	}
}

func TestFanartTVShowArtworkByTheTVDBID(t *testing.T) {
	p, seen := fanartStub(t, fanartTVShowJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "show", TheTVDBID: "121361"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
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
	if len(*seen) != 1 || !strings.HasSuffix((*seen)[0], "/tv/121361") {
		t.Errorf("request paths = %v, want a single /tv/121361", *seen)
	}
}

func TestFanartTVVideoNoIDIsNoMatch(t *testing.T) {
	p, seen := fanartStub(t, fanartMovieJSON, 0)
	// A movie with no TMDB/IMDb id, and a show with no TheTVDB id, are strictly
	// id-keyed no-matches — no outbound call.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"}); err != ErrNoMatch {
		t.Errorf("no-id movie err = %v, want ErrNoMatch", err)
	}
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "show", Title: "Game of Thrones"}); err != ErrNoMatch {
		t.Errorf("no-id show err = %v, want ErrNoMatch", err)
	}
	// Season/episode are not served by the video path (fanart.tv keys by series id only).
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "episode", TheTVDBID: "121361", SeasonNumber: 1, EpisodeNumber: 5}); err != ErrNoMatch {
		t.Errorf("episode err = %v, want ErrNoMatch", err)
	}
	if len(*seen) != 0 {
		t.Errorf("expected zero requests for id-less / unsupported video lookups; saw %v", *seen)
	}
}

func TestFanartTVVideoNotFoundIsNoMatch(t *testing.T) {
	// fanart.tv answers an unknown id with 404 — the normal "no record" outcome.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "0"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestFanartTVVideoEmptyImagesIsNoMatch(t *testing.T) {
	// A 200 with no poster/background lists has nothing to contribute.
	p, _ := fanartStub(t, `{"name":"Dune","tmdb_id":"438631"}`, 0)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestFanartTVVideoNon2xxIsError(t *testing.T) {
	// A non-404 non-2xx (e.g. 500) is a real error the chain logs and swallows.
	p, _ := fanartStub(t, `oops`, http.StatusInternalServerError)
	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"})
	if err == nil || err == ErrNoMatch {
		t.Errorf("err = %v, want a real (non-ErrNoMatch) error", err)
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
	p, seen := fanartStub(t, both, 0)
	// Same raw id "123" used as an MBID and as a TMDB id — distinct cache namespaces.
	artist, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: "123"})
	if err != nil {
		t.Fatalf("artist Lookup: %v", err)
	}
	if len(artist.Artwork) != 1 || artist.Artwork[0].URL != "https://x/artist.jpg" {
		t.Errorf("artist artwork = %+v, want the artistthumb", artist.Artwork)
	}
	movie, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "123"})
	if err != nil {
		t.Fatalf("movie Lookup: %v", err)
	}
	if len(movie.Artwork) != 2 || movie.Artwork[0].URL != "https://x/movie-poster.jpg" {
		t.Errorf("movie artwork = %+v, want the movieposter+moviebackground", movie.Artwork)
	}
	// Both paths hit the host (different endpoints/namespaces), proving no collision.
	if len(*seen) != 2 {
		t.Errorf("request count = %d, want 2 (artist + movie, no cache collision); paths=%v", len(*seen), *seen)
	}
	if !strings.HasSuffix((*seen)[0], "/music/123") || !strings.HasSuffix((*seen)[1], "/movies/123") {
		t.Errorf("paths = %v, want /music/123 then /movies/123", *seen)
	}
}

func TestFanartTVVideoCachesByID(t *testing.T) {
	p, seen := fanartStub(t, fanartMovieJSON, 0)
	for i := 0; i < 3; i++ {
		if _, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	if len(*seen) != 1 {
		t.Errorf("request count = %d, want 1 (video response cached); paths=%v", len(*seen), *seen)
	}
}
