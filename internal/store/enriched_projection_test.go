package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestEnrichedReadCarriesEpisodeNumbers is a regression guard for a bug that made
// hand-correcting a TV episode silently impossible.
//
// enrichedTitleColumns did not select season_number / episode_number, so
// TitleForEnrichmentByID — the read behind EVERY single-Title re-enrich
// (enrichmentMatch, enrichmentOverride, the episode pin) — returned zeros. The
// provider lookup is keyed on exactly those numbers, so a hand-corrected Episode
// was fetched as /tv/{show}/season/0/episode/0 and 404'd every time, while a
// full-library pass resolved the same Episode fine because it collects its leaves
// with the numbers attached. The symptom read as "this episode cannot be matched";
// the cause was a projection that dropped the fields the lookup is keyed on.
func TestEnrichedReadCarriesEpisodeNumbers(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	                 VALUES ('sh1', 'libtv', 'Batman', 'batman', 'batman')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key)
	                 VALUES ('se3', 'sh1', 3, 'batman|s03')`)
	mustExec(t, db, `INSERT INTO titles
	                   (id, library_id, kind, title, identity_key, sort_title,
	                    season_id, season_number, episode_number, episode_label)
	                 VALUES ('ep1', 'libtv', 'episode', 'Holiday Knights', 'batman|s03e11',
	                         'holiday knights', 'se3', 3, 11, '')`)

	got, err := db.TitleForEnrichmentByID("ep1")
	if err != nil {
		t.Fatalf("TitleForEnrichmentByID: %v", err)
	}
	if got.SeasonNumber != 3 || got.EpisodeNumber != 11 {
		t.Fatalf("read back S%02dE%02d, want S03E11 — the enrichment read is dropping the "+
			"very numbers the provider lookup is keyed on, so every hand-correction of an "+
			"Episode resolves to season 0", got.SeasonNumber, got.EpisodeNumber)
	}
}

// TestEnrichedReadCarriesTheEpisodePin: the Admin's pinned provider episode must
// survive the same read, or applying it would have no effect on the next lookup.
func TestEnrichedReadCarriesTheEpisodePin(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO titles
	                   (id, library_id, kind, title, identity_key, sort_title,
	                    season_number, episode_number)
	                 VALUES ('ep1', 'libtv', 'episode', 'Holiday Knights', 'k', 'hk', 3, 11)`)

	// Unpinned by default: the parsed numbers stand.
	got, err := db.TitleForEnrichmentByID("ep1")
	if err != nil {
		t.Fatalf("TitleForEnrichmentByID: %v", err)
	}
	if _, _, pinned := got.EpisodePin(); pinned {
		t.Fatalf("a fresh Episode reports a pin: %+v", got)
	}

	// Pin it to the provider's numbering, then read it back.
	if err := db.SetTitleExternalMatch("ep1", store.ExternalMatch{
		TMDBID: "2098", EpisodeSeason: 4, EpisodeNumber: 1,
	}, store.OriginChosen); err != nil {
		t.Fatalf("SetTitleExternalMatch: %v", err)
	}
	got, err = db.TitleForEnrichmentByID("ep1")
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	season, episode, pinned := got.EpisodePin()
	if !pinned || season != 4 || episode != 1 {
		t.Errorf("pin = S%02dE%02d (pinned %v), want the pinned S04E01", season, episode, pinned)
	}
	// The Title's OWN numbers are untouched — the pin redirects the lookup, not the
	// file's place in the library or the watch state keyed to it (ADR-0014).
	if got.SeasonNumber != 3 || got.EpisodeNumber != 11 {
		t.Errorf("the pin moved the Title to S%02dE%02d; it must stay S03E11",
			got.SeasonNumber, got.EpisodeNumber)
	}
}
