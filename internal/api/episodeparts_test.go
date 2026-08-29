package api

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Part labelling: telling apart the FILES that share one provider episode.
//
// A double-length episode the provider lists once may ship as two files. Parks and
// Recreation's season 6 finale is a single 44-minute record on TMDB and two files on
// disk, so both files are pointed at that one record — correctly, via the episode
// pin. Without labelling they then render as two rows carrying the same title, the
// same synopsis and the same still, and nothing tells the viewer which half is which.
//
// The labelling is DISPLAY ONLY: it never touches identity_key, episode_number,
// season_id or watch state (ADR-0002/0014). Ordering is untouched too — part 1 is
// simply whichever file the existing episode_number order already puts first.

func ep(id string, season, episode int, tmdb string) store.Title {
	return store.Title{
		ID: id, Kind: "episode", TMDBID: tmdb,
		SeasonNumber: season, EpisodeNumber: episode,
		EnrichmentSeason: store.NoEpisodePin, EnrichmentEpisode: store.NoEpisodePin,
	}
}

func pinned(t store.Title, season, episode int) store.Title {
	t.EnrichmentSeason, t.EnrichmentEpisode = season, episode
	return t
}

// TestPartLabelsTheParksAndRecCase is the case the feature exists for: two files,
// one provider record, currently indistinguishable in the episode list.
func TestPartLabelsTheParksAndRecCase(t *testing.T) {
	// S06E21 is the provider's 44-minute finale; the second file is filed as E22 and
	// pinned back to that same record.
	first := ep("a", 6, 21, "8592")
	second := pinned(ep("b", 6, 22, "8592"), 6, 21)

	parts := episodePartLabels([]store.Title{first, second})
	if got := parts["a"]; got.Number != 1 || got.Count != 2 {
		t.Errorf("first file = %+v, want part 1 of 2", got)
	}
	if got := parts["b"]; got.Number != 2 || got.Count != 2 {
		t.Errorf("second file = %+v, want part 2 of 2", got)
	}
	// The label the viewer actually reads.
	if s := partSuffix(parts["a"]); s != " (1 of 2)" {
		t.Errorf("suffix = %q, want %q", s, " (1 of 2)")
	}
}

// TestPartLabelsFollowTheListOrder: part 1 must be the file that plays first, which
// is the store's episode_number order — the caller's order, not a re-sort.
func TestPartLabelsFollowTheListOrder(t *testing.T) {
	// Three files sharing one record, handed over in play order.
	titles := []store.Title{
		pinned(ep("x", 1, 1, "s"), 1, 1),
		pinned(ep("y", 1, 2, "s"), 1, 1),
		pinned(ep("z", 1, 3, "s"), 1, 1),
	}
	parts := episodePartLabels(titles)
	for i, id := range []string{"x", "y", "z"} {
		if got := parts[id]; got.Number != i+1 || got.Count != 3 {
			t.Errorf("%s = %+v, want part %d of 3", id, got, i+1)
		}
	}
}

// TestOrdinaryEpisodesAreNeverLabelled: the overwhelmingly normal case must be
// completely untouched — no "(1 of 1)", no fields set.
func TestOrdinaryEpisodesAreNeverLabelled(t *testing.T) {
	titles := []store.Title{
		ep("a", 1, 1, "s"),
		ep("b", 1, 2, "s"),
		ep("c", 1, 3, "s"),
	}
	parts := episodePartLabels(titles)
	if len(parts) != 0 {
		t.Fatalf("labelled %d ordinary episodes, want none: %+v", len(parts), parts)
	}
	if s := partSuffix(parts["a"]); s != "" {
		t.Errorf("suffix on an ordinary episode = %q, want empty", s)
	}
}

// TestMultiEpisodeFileIsNotLabelled guards the MIRROR case explicitly. A file named
// S01E05-E06 maps ONE file to TWO Episodes (docs/naming-convention.md), which is the
// opposite arrangement and already handled by the scanner. Those Episodes carry
// DIFFERENT numbers, so they must land in different groups and stay unlabelled —
// calling them "part 1 of 2" of each other would be plainly wrong.
func TestMultiEpisodeFileIsNotLabelled(t *testing.T) {
	titles := []store.Title{ep("e5", 1, 5, "s"), ep("e6", 1, 6, "s")}
	if parts := episodePartLabels(titles); len(parts) != 0 {
		t.Errorf("a multi-episode file was labelled: %+v", parts)
	}
}

// TestUnnumberedEpisodesDoNotGroup: a Season can hold several episodes the scanner
// could not number (date-named files all landing on episode 0). They are not parts
// of one another — they are simply unnumbered — so an episode number of 0 must never
// form a group. Without this guard every such Season would sprout bogus part labels.
func TestUnnumberedEpisodesDoNotGroup(t *testing.T) {
	titles := []store.Title{
		{ID: "a", Kind: "episode", TMDBID: "s", SeasonNumber: 1, EpisodeLabel: "2002-06-02",
			EnrichmentSeason: store.NoEpisodePin, EnrichmentEpisode: store.NoEpisodePin},
		{ID: "b", Kind: "episode", TMDBID: "s", SeasonNumber: 1, EpisodeLabel: "2002-06-09",
			EnrichmentSeason: store.NoEpisodePin, EnrichmentEpisode: store.NoEpisodePin},
	}
	if parts := episodePartLabels(titles); len(parts) != 0 {
		t.Errorf("unnumbered episodes were grouped: %+v", parts)
	}
}

// TestDifferentSeriesNeverGroup: the series id is part of the key, so two shows that
// happen to share a season/episode number can never be called parts of one another.
func TestDifferentSeriesNeverGroup(t *testing.T) {
	titles := []store.Title{ep("a", 1, 1, "show-one"), ep("b", 1, 1, "show-two")}
	if parts := episodePartLabels(titles); len(parts) != 0 {
		t.Errorf("episodes of different series were grouped: %+v", parts)
	}
}

// TestPinBeatsTheParsedNumberForGrouping: grouping must use the EFFECTIVE provider
// episode. Two files whose parsed numbers differ but whose pins agree are one
// episode; two files whose parsed numbers agree but whose pins differ are not.
func TestPinBeatsTheParsedNumberForGrouping(t *testing.T) {
	// Parsed numbers differ, pins agree → one group.
	same := episodePartLabels([]store.Title{
		pinned(ep("a", 6, 21, "s"), 6, 21),
		pinned(ep("b", 6, 22, "s"), 6, 21),
	})
	if len(same) != 2 {
		t.Errorf("pins agree but were not grouped: %+v", same)
	}
	// Pins differ → two groups, so neither is a part.
	apart := episodePartLabels([]store.Title{
		pinned(ep("a", 6, 21, "s"), 6, 21),
		pinned(ep("b", 6, 21, "s"), 6, 22),
	})
	if len(apart) != 0 {
		t.Errorf("differing pins were grouped: %+v", apart)
	}
}

// TestPartLabellingTouchesNothingElse: the label is display-only, so the Titles
// handed in must come back unmodified.
func TestPartLabellingTouchesNothingElse(t *testing.T) {
	first := ep("a", 6, 21, "s")
	second := pinned(ep("b", 6, 22, "s"), 6, 21)
	before := []store.Title{first, second}
	_ = episodePartLabels(before)
	if before[0].EpisodeNumber != 21 || before[1].EpisodeNumber != 22 {
		t.Errorf("labelling moved the episode numbers: %d, %d",
			before[0].EpisodeNumber, before[1].EpisodeNumber)
	}
	if before[1].IdentityKey != second.IdentityKey || before[0].SeasonNumber != 6 {
		t.Error("labelling mutated identity or season")
	}
}

// TestPartSuffixIsAppendedToTheDisplayTitle: the suffix rides on the DISPLAY title
// so every client benefits without a change, while the structured fields let a
// client that would rather draw a badge do so.
func TestPartSuffixIsAppendedToTheDisplayTitle(t *testing.T) {
	title := pinned(ep("b", 6, 22, "s"), 6, 21)
	title.Title = "Moving Up"
	js := toEpisodeSummary(title, store.WatchState{}, "", episodePart{Number: 2, Count: 2})
	if js.Title != "Moving Up (2 of 2)" {
		t.Errorf("title = %q, want %q", js.Title, "Moving Up (2 of 2)")
	}
	if js.PartNumber != 2 || js.PartCount != 2 {
		t.Errorf("structured part = %d of %d, want 2 of 2", js.PartNumber, js.PartCount)
	}

	// An ordinary episode keeps its title exactly, and sends no part fields.
	plain := ep("a", 1, 1, "s")
	plain.Title = "Pilot"
	pjs := toEpisodeSummary(plain, store.WatchState{}, "", episodePart{})
	if pjs.Title != "Pilot" || pjs.PartNumber != 0 || pjs.PartCount != 0 {
		t.Errorf("ordinary episode = %q part %d/%d, want untouched",
			pjs.Title, pjs.PartNumber, pjs.PartCount)
	}
}
