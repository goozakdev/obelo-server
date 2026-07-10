package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real providers' ArtworkCandidates HTTP/parse layer (the Edit-item image
// picker, item-editing/03), exercised against httptest serving canned JSON — the
// secondary, lower seam. The project's black-box tests use a fake provider's
// ArtworkCandidates instead; no live network is ever touched here.

const tmdbMovieImagesJSON = `{
  "posters": [
    {"file_path": "/poster-a.jpg", "width": 2000, "height": 3000},
    {"file_path": "/poster-b.jpg", "width": 1000, "height": 1500}
  ],
  "backdrops": [
    {"file_path": "/back-a.jpg", "width": 3840, "height": 2160}
  ],
  "logos": [
    {"file_path": "/logo-svg.svg", "width": 1600, "height": 620},
    {"file_path": "/logo-a.png", "width": 800, "height": 310}
  ]
}`

// TestTMDBArtworkCandidatesMoviePosters: a movie poster query hits
// /movie/{id}/images and maps the posters[] into candidates carrying the full
// image URL + dimensions; the requested role selects poster vs backdrop.
func TestTMDBArtworkCandidatesMoviePosters(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/movie/438631/images" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tmdbMovieImagesJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/movie/438631/images" {
		t.Fatalf("expected one /movie/438631/images call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("poster candidates = %d, want 2", len(cands))
	}
	if cands[0].URL != "https://img//poster-a.jpg" || cands[0].Width != 2000 || cands[0].Height != 3000 {
		t.Errorf("candidate[0] = %+v", cands[0])
	}
	if cands[0].Source != "tmdb" {
		t.Errorf("source = %q, want tmdb", cands[0].Source)
	}

	// The background role selects backdrops[].
	bg, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}, "background")
	if err != nil {
		t.Fatalf("ArtworkCandidates(background): %v", err)
	}
	if len(bg) != 1 || bg[0].URL != "https://img//back-a.jpg" {
		t.Errorf("background candidates = %+v", bg)
	}

	// The logo role selects logos[] (the Edit-item Logo tab). The SVG rendition is
	// never offered — the artwork pipeline is raster-only, and a picked SVG would
	// be cached under a raster extension and render nowhere.
	logos, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}, "logo")
	if err != nil {
		t.Fatalf("ArtworkCandidates(logo): %v", err)
	}
	if len(logos) != 1 || logos[0].URL != "https://img//logo-a.png" || logos[0].Width != 800 {
		t.Errorf("logo candidates = %+v", logos)
	}
}

// TestTMDBArtworkCandidatesEpisodeStills: an Episode's poster role lists the
// still[] under /tv/{id}/season/{s}/episode/{e}/images.
func TestTMDBArtworkCandidatesEpisodeStills(t *testing.T) {
	const stillsJSON = `{"stills": [{"file_path": "/still.jpg", "width": 1920, "height": 1080}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tv/1399/season/2/episode/5/images" {
			_, _ = w.Write([]byte(stillsJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "episode", TMDBID: "1399", SeasonNumber: 2, EpisodeNumber: 5}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].URL != "https://img//still.jpg" {
		t.Errorf("episode still candidates = %+v", cands)
	}
}

// TestTMDBArtworkCandidatesNoIDNoCall: without a resolved TMDB id there is no
// record to list, so no HTTP call is made and no candidates come back.
func TestTMDBArtworkCandidatesNoIDNoCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if called {
		t.Errorf("made an HTTP call with no TMDB id")
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want 0", len(cands))
	}
}

const caaReleaseGroupJSON = `{
  "images": [
    {"image": "https://caa/full-1.jpg", "front": true, "thumbnails": {"250": "https://caa/250-1.jpg", "500": "https://caa/500-1.jpg"}},
    {"image": "https://caa/full-2.jpg", "front": false, "thumbnails": {"500": "https://caa/500-2.jpg"}}
  ]
}`

// TestMusicBrainzArtworkCandidatesAlbumCovers: an album image query hits the Cover
// Art Archive release-group endpoint and maps the images into candidates,
// preferring the 500px derivative.
func TestMusicBrainzArtworkCandidatesAlbumCovers(t *testing.T) {
	var seen []string
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/release-group/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(caaReleaseGroupJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-123"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/release-group/rg-123" {
		t.Fatalf("expected one /release-group/rg-123 call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("cover candidates = %d, want 2", len(cands))
	}
	if cands[0].URL != "https://caa/500-1.jpg" || cands[0].Source != "coverartarchive" {
		t.Errorf("candidate[0] = %+v (want the 500px derivative)", cands[0])
	}
}

// TestMusicBrainzArtworkCandidatesArtistNone: an Artist has no listable image set
// (CAA is release-group keyed), so it yields no candidates and makes no call.
func TestMusicBrainzArtworkCandidatesArtistNone(t *testing.T) {
	called := false
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "art-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if called || len(cands) != 0 {
		t.Errorf("artist yielded candidates/made a call: called=%v cands=%d", called, len(cands))
	}
}

// TestMusicBrainzArtworkCandidatesNoArt404: a 404 from the Cover Art Archive is the
// normal "no cover art" outcome — no candidates, no error.
func TestMusicBrainzArtworkCandidatesNoArt404(t *testing.T) {
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-none"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want 0 on a 404", len(cands))
	}
}
