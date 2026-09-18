package enrich

import (
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

// TestALibraryPassHonorsTheEpisodePin closes the gap that made the matcher's
// repoint useless in practice.
//
// There are two ways a leaf reaches a lookup: refFor, for a single-Title
// re-enrich, and collectTVLeaves, which a LIBRARY pass uses to build its own refs.
// A pass is exactly what runs after a matcher Apply (ADR-0044's post-commit
// re-enrich), so a pass that ignored the pin would look every freshly repointed
// Slot up by the numbers it had just been pinned AWAY from — and the Batman files
// would stay bare however carefully the Admin borrowed their records.
func TestALibraryPassHonorsTheEpisodePin(t *testing.T) {
	pinned := store.Title{
		Kind: "episode", TMDBID: "1438",
		SeasonNumber: 3, EpisodeNumber: 61,
		EnrichmentSeason: 1, EnrichmentEpisode: 1,
	}
	// The ref a pass builds before the pin is applied: the Title's own numbers.
	ref := TitleRef{
		Kind: "episode", Title: pinned.Title, TMDBID: pinned.TMDBID,
		SeasonNumber: pinned.SeasonNumber, EpisodeNumber: pinned.EpisodeNumber,
	}
	got := withEpisodePin(ref, pinned)
	if got.SeasonNumber != 1 || got.EpisodeNumber != 1 {
		t.Errorf("pass lookup = S%02dE%02d, want the PINNED S01E01",
			got.SeasonNumber, got.EpisodeNumber)
	}
	// The two paths must agree, or a Title's record would depend on which one
	// happened to touch it.
	if single := refFor(pinned); single.SeasonNumber != got.SeasonNumber ||
		single.EpisodeNumber != got.EpisodeNumber {
		t.Errorf("refFor = S%02dE%02d but a pass = S%02dE%02d; the two must not disagree",
			single.SeasonNumber, single.EpisodeNumber, got.SeasonNumber, got.EpisodeNumber)
	}
	// An unpinned Title is untouched by the same call.
	plain := store.Title{Kind: "episode", TMDBID: "1438", SeasonNumber: 3, EpisodeNumber: 61}
	if out := withEpisodePin(ref, plain); out.SeasonNumber != 3 || out.EpisodeNumber != 61 {
		t.Errorf("unpinned lookup = S%02dE%02d, want the parsed S03E61",
			out.SeasonNumber, out.EpisodeNumber)
	}
}
