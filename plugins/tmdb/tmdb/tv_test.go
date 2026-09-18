package tmdb

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The TV half: the show/season/episode lookups, and the EpisodeLister capability
// behind the episode chooser. Ported from internal/enrich/tv_music_provider_test.go
// and internal/enrich/episodepin_test.go, assertions intact.

func tvStub(t *testing.T) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		}),
	)
	return New(host), host
}

func TestTMDBShowSeasonEpisode(t *testing.T) {
	p, _ := tvStub(t)

	show := lookup(t, p, pluginapi.MediaRef{Kind: "show", Title: "Game of Thrones", Year: 2011}).Record
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

	season := lookup(t, p, pluginapi.MediaRef{Kind: "season", TMDBID: "1399", SeasonNumber: 1}).Record
	if !season.Matched || len(season.Artwork) != 1 || season.Artwork[0].Role != "poster" {
		t.Errorf("season lookup wrong: %+v", season)
	}

	ep := lookup(t, p, pluginapi.MediaRef{Kind: "episode", TMDBID: "1399", SeasonNumber: 1, EpisodeNumber: 1}).Record
	if ep.Name != "Winter Is Coming" || ep.Overview == "" {
		t.Errorf("episode metadata wrong: %+v", ep)
	}
	if len(ep.Artwork) != 1 || ep.Artwork[0].Role != "poster" {
		t.Errorf("episode still wrong: %+v", ep.Artwork)
	}
}

func TestTMDBSeasonEpisodeNeedShowID(t *testing.T) {
	p, _ := tvStub(t)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "season", SeasonNumber: 1}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("season without show id outcome = %q, want no-match", got)
	}
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "episode", SeasonNumber: 1, EpisodeNumber: 1}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("episode without show id outcome = %q, want no-match", got)
	}
}

// TestSeasonEpisodesListsAPickableSeason: the data behind the chooser — episode
// numbers, names and stills for one season of a series.
func TestSeasonEpisodesListsAPickableSeason(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":1,"season_number":4,"name":"Holiday Knights","air_date":"1997-09-13","overview":"Two tales.","still_path":"/hk.jpg"},
				{"episode_number":2,"season_number":4,"name":"Sins of the Father","air_date":"1997-09-20"}
			]}`))
		}),
	)
	p := New(host)

	resp, err := p.SeasonEpisodes(context.Background(), pluginapi.SeasonEpisodesRequest{SeriesID: "1438", Season: 4})
	if err != nil {
		t.Fatalf("SeasonEpisodes: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if seen := host.Paths(); len(seen) != 1 || !strings.HasSuffix(seen[0], "/tv/1438/season/4") {
		t.Fatalf("expected one /tv/1438/season/4 call, saw %v", seen)
	}
	eps := resp.Episodes
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	if eps[0].Season != 4 || eps[0].Episode != 1 || eps[0].Name != "Holiday Knights" {
		t.Errorf("episode[0] = %+v", eps[0])
	}
	if eps[0].StillURL != "https://img//hk.jpg" {
		t.Errorf("still = %q", eps[0].StillURL)
	}
	// An episode with no still is still pickable — the name and number identify it.
	if eps[1].StillURL != "" {
		t.Errorf("episode[1] still = %q, want empty", eps[1].StillURL)
	}
}

// TestSeriesSeasonsListsSeasons: the season chooser's options.
func TestSeriesSeasonsListsSeasons(t *testing.T) {
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"seasons":[
				{"season_number":0,"episode_count":3},
				{"season_number":3,"episode_count":20},
				{"season_number":4,"episode_count":10}
			]}`))
		}),
	)
	p := New(host)

	resp, err := p.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "1438"})
	if err != nil {
		t.Fatalf("SeriesSeasons: %v", err)
	}
	seasons := resp.Seasons
	if len(seasons) != 3 || seasons[0].Season != 0 || seasons[2].Season != 4 {
		t.Fatalf("seasons = %+v", seasons)
	}
	if seasons[2].EpisodeCount != 10 {
		t.Errorf("season 4 episode count = %d, want 10", seasons[2].EpisodeCount)
	}
}
