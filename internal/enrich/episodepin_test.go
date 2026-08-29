package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The episode pin: WHICH provider episode decorates a file, chosen by the Admin
// instead of derived from its filename.
//
// It exists for a shape of library that was previously unfixable. Enrichment looks
// an Episode up as /tv/{show}/season/{S}/episode/{E}, with S and E parsed from the
// filename (ADR-0002 — local naming is the identity authority). When a provider
// numbers a series differently from the files on disk — the standard case being a
// run of episodes at the end of a season that the provider counts as the start of
// the next one — that lookup asks for the wrong episode forever. Re-picking the
// series changed nothing, because the series was never the part that was wrong.
//
// These pin down the two halves: the pin redirects the LOOKUP, and it redirects
// ONLY the lookup.

// TestEpisodePinRedirectsTheLookup is the whole feature in one assertion: a Title
// filed as S03E11 whose pin says S04E01 must be looked up as S04E01.
func TestEpisodePinRedirectsTheLookup(t *testing.T) {
	title := store.Title{
		Kind: "episode", TMDBID: "1438",
		SeasonNumber: 3, EpisodeNumber: 11,
		EnrichmentSeason: 4, EnrichmentEpisode: 1,
	}
	ref := refFor(title)
	if ref.SeasonNumber != 4 || ref.EpisodeNumber != 1 {
		t.Errorf("lookup ref = S%02dE%02d, want the PINNED S04E01",
			ref.SeasonNumber, ref.EpisodeNumber)
	}
}

// TestEpisodePinLeavesTheTitleAlone: the pin is an ENRICHMENT override, so the
// Title's own numbers — which decide where it sits in the library, its
// identity_key, and the watch state keyed to it (ADR-0014) — must not move.
func TestEpisodePinLeavesTheTitleAlone(t *testing.T) {
	title := store.Title{
		Kind: "episode", TMDBID: "1438",
		SeasonNumber: 3, EpisodeNumber: 11,
		EnrichmentSeason: 4, EnrichmentEpisode: 1,
	}
	_ = refFor(title)
	if title.SeasonNumber != 3 || title.EpisodeNumber != 11 {
		t.Errorf("Title moved to S%02dE%02d; the pin must not touch it",
			title.SeasonNumber, title.EpisodeNumber)
	}
}

// TestNoPinUsesTheParsedNumbers: the default path is unchanged — an unpinned
// Episode is still looked up by what its filename said.
func TestNoPinUsesTheParsedNumbers(t *testing.T) {
	ref := refFor(store.Title{
		Kind: "episode", TMDBID: "1438", SeasonNumber: 2, EpisodeNumber: 7,
		EnrichmentSeason: store.NoEpisodePin, EnrichmentEpisode: store.NoEpisodePin,
	})
	if ref.SeasonNumber != 2 || ref.EpisodeNumber != 7 {
		t.Errorf("lookup ref = S%02dE%02d, want the parsed S02E07",
			ref.SeasonNumber, ref.EpisodeNumber)
	}
}

// TestZeroValuedTitleIsNotTreatedAsPinned guards the trap in this design: only the
// enriched projection reads the pin columns, so every Title built by a leaner read
// (or a test literal) carries a zero value. If "pinned" were tested against a
// sentinel, all of those would look like "pinned to season 0, episode 0" and have
// their lookups silently redirected to a nonexistent episode.
func TestZeroValuedTitleIsNotTreatedAsPinned(t *testing.T) {
	ref := refFor(store.Title{Kind: "episode", TMDBID: "1438", SeasonNumber: 1, EpisodeNumber: 5})
	if ref.SeasonNumber != 1 || ref.EpisodeNumber != 5 {
		t.Errorf("a zero-valued Title was treated as pinned: got S%02dE%02d, want S01E05",
			ref.SeasonNumber, ref.EpisodeNumber)
	}
}

// TestEpisodePinAllowsSpecials: season 0 is the real Specials season, so a pin into
// it must be honoured — which is why the "is pinned" test is on the EPISODE number
// (always >= 1), never on the season.
func TestEpisodePinAllowsSpecials(t *testing.T) {
	ref := refFor(store.Title{
		Kind: "episode", TMDBID: "1438", SeasonNumber: 3, EpisodeNumber: 11,
		EnrichmentSeason: 0, EnrichmentEpisode: 2,
	})
	if ref.SeasonNumber != 0 || ref.EpisodeNumber != 2 {
		t.Errorf("lookup ref = S%02dE%02d, want the pinned Specials S00E02",
			ref.SeasonNumber, ref.EpisodeNumber)
	}
}

// TestSeasonEpisodesListsAPickableSeason: the data behind the chooser — episode
// numbers, names and stills for one season of a series.
func TestSeasonEpisodesListsAPickableSeason(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"episodes":[
			{"episode_number":1,"season_number":4,"name":"Holiday Knights","air_date":"1997-09-13","overview":"Two tales.","still_path":"/hk.jpg"},
			{"episode_number":2,"season_number":4,"name":"Sins of the Father","air_date":"1997-09-20"}
		]}`))
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	eps, err := p.SeasonEpisodes(context.Background(), "1438", 4)
	if err != nil {
		t.Fatalf("SeasonEpisodes: %v", err)
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/tv/1438/season/4") {
		t.Fatalf("expected one /tv/1438/season/4 call, saw %v", seen)
	}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"seasons":[
			{"season_number":0,"episode_count":3},
			{"season_number":3,"episode_count":20},
			{"season_number":4,"episode_count":10}
		]}`))
	}))
	defer srv.Close()
	p := NewTMDBProvider("k", "en-US", srv.URL, "https://img/")

	seasons, err := p.SeriesSeasons(context.Background(), "1438")
	if err != nil {
		t.Fatalf("SeriesSeasons: %v", err)
	}
	if len(seasons) != 3 || seasons[0].Season != 0 || seasons[2].Season != 4 {
		t.Fatalf("seasons = %+v", seasons)
	}
	if seasons[2].EpisodeCount != 10 {
		t.Errorf("season 4 episode count = %d, want 10", seasons[2].EpisodeCount)
	}
}

// TestDefaultSeasonFallsBackWhenTheSeriesLacksIt: the chooser opens on the file's
// own season when that season exists, and otherwise on the first real one. The
// fallback matters because the Admin is here precisely BECAUSE the disk and the
// provider disagree — the record they want may live in a re-numbered continuation
// with no such season, and opening on an empty list reads as "no episodes here".
func TestDefaultSeasonFallsBackWhenTheSeriesLacksIt(t *testing.T) {
	batman := []SeasonSummary{{Season: 0}, {Season: 1}, {Season: 2}, {Season: 3}, {Season: 4}}
	tnba := []SeasonSummary{{Season: 1}, {Season: 2}}

	if got := defaultSeason(batman, 3); got != 3 {
		t.Errorf("season present: got %d, want the file's own 3", got)
	}
	// A file filed as S03 pointed at a series that only has seasons 1-2.
	if got := defaultSeason(tnba, 3); got != 1 {
		t.Errorf("season absent: got %d, want the first real season 1", got)
	}
	// Specials is a poor landing place when a numbered season exists.
	if got := defaultSeason([]SeasonSummary{{Season: 0}, {Season: 2}}, 9); got != 2 {
		t.Errorf("got %d, want 2 (a numbered season beats Specials)", got)
	}
	// A series listing only Specials still opens on something real.
	if got := defaultSeason([]SeasonSummary{{Season: 0}}, 9); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
	// No seasons at all: ask for what we wanted and let the provider answer.
	if got := defaultSeason(nil, 5); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}
