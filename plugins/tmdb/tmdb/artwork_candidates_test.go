package tmdb

import (
	"context"
	"net/http"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// ArtworkCandidates — the Edit-item image picker (item-editing/03). Ported from
// internal/enrich/artwork_candidates_test.go's TMDB half, assertions intact.

const movieImagesJSON = `{
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

func artworkCandidates(t *testing.T, p *Provider, ref pluginapi.MediaRef, role string) []pluginapi.ArtworkCandidate {
	t.Helper()
	resp, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{Ref: ref, Role: role})
	if err != nil {
		t.Fatalf("ArtworkCandidates(%s): %v", role, err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("ArtworkCandidates(%s) outcome = %q, want matched", role, resp.Outcome)
	}
	return resp.Candidates
}

// TestTMDBArtworkCandidatesMoviePosters: a movie poster query hits
// /movie/{id}/images and maps the posters[] into candidates carrying the full
// image URL + dimensions; the requested role selects poster vs backdrop.
func TestTMDBArtworkCandidatesMoviePosters(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/movie/438631/images" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(movieImagesJSON))
				return
			}
			http.NotFound(w, r)
		}),
	)
	p := New(host)
	ref := pluginapi.MediaRef{Kind: "movie", TMDBID: "438631"}

	cands := artworkCandidates(t, p, ref, "poster")
	if seen := host.Paths(); len(seen) != 1 || seen[0] != "/movie/438631/images" {
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
	bg := artworkCandidates(t, p, ref, "background")
	if len(bg) != 1 || bg[0].URL != "https://img//back-a.jpg" {
		t.Errorf("background candidates = %+v", bg)
	}

	// The logo role selects logos[] (the Edit-item Logo tab). The SVG rendition is
	// never offered — the artwork pipeline is raster-only, and a picked SVG would
	// be cached under a raster extension and render nowhere.
	logos := artworkCandidates(t, p, ref, "logo")
	if len(logos) != 1 || logos[0].URL != "https://img//logo-a.png" || logos[0].Width != 800 {
		t.Errorf("logo candidates = %+v", logos)
	}
}

// TestTMDBArtworkCandidatesEpisodeStills: an Episode's poster role lists the
// still[] under /tv/{id}/season/{s}/episode/{e}/images.
func TestTMDBArtworkCandidatesEpisodeStills(t *testing.T) {
	const stillsJSON = `{"stills": [{"file_path": "/still.jpg", "width": 1920, "height": 1080}]}`
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/tv/1399/season/2/episode/5/images" {
				_, _ = w.Write([]byte(stillsJSON))
				return
			}
			http.NotFound(w, r)
		}),
	)
	p := New(host)

	cands := artworkCandidates(t, p,
		pluginapi.MediaRef{Kind: "episode", TMDBID: "1399", SeasonNumber: 2, EpisodeNumber: 5}, "poster")
	if len(cands) != 1 || cands[0].URL != "https://img//still.jpg" {
		t.Errorf("episode still candidates = %+v", cands)
	}
}

// TestTMDBArtworkCandidatesNoIDNoCall: without a resolved TMDB id there is no
// record to list, so no fetch is made and no candidates come back.
func TestTMDBArtworkCandidatesNoIDNoCall(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }),
	)
	p := New(host)

	cands := artworkCandidates(t, p, pluginapi.MediaRef{Kind: "movie"}, "poster")
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("made a fetch with no TMDB id: %v", seen)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want 0", len(cands))
	}
}
