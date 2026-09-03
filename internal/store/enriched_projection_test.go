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

// TestEnrichedReadCarriesTheRecordingTagID is the same regression, one media kind
// later, and it was found by the work that made it cost something (issue 17).
//
// enrichedTitleColumns did not select musicbrainz_recording_id, so
// TitleForEnrichmentByID — again, the read behind EVERY single-Title re-enrich —
// returned an empty tag id for a Track whose FILE names its recording exactly.
// TracksForAlbum, the read a library pass collects its leaves through, has always
// carried it. So ADR-0049's second tier existed on one path and not the other: a
// Picard-tagged Track re-enriched on its own resolved as though untagged, spending
// the search cluster on an id sitting in the file.
func TestEnrichedReadCarriesTheRecordingTagID(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libm', 'Music', 'music')`)
	mustExec(t, db, `INSERT INTO artists (id, library_id, name, identity_key, sort_name)
	                 VALUES ('ar1', 'libm', 'Harry Connick Jr.', 'artist:hcj', 'harry connick jr')`)
	mustExec(t, db, `INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
	                 VALUES ('al1', 'ar1', 'She', 'artist:hcj|album:she', 'she')`)
	mustExec(t, db, `INSERT INTO titles
	                   (id, library_id, kind, title, identity_key, sort_title,
	                    album_id, disc_number, track_number, musicbrainz_recording_id)
	                 VALUES ('t1', 'libm', 'track', 'Whisper Your Name', 'artist:hcj|album:she|d01t01',
	                         'whisper your name', 'al1', 1, 1, 'rec-tag')`)

	got, err := db.TitleForEnrichmentByID("t1")
	if err != nil {
		t.Fatalf("TitleForEnrichmentByID: %v", err)
	}
	if got.MusicbrainzRecordingID != "rec-tag" {
		t.Fatalf("read back %q, want rec-tag — the enrichment read is dropping the exact "+
			"recording id the file asserts, so a single-Track re-enrich falls past ADR-0049's "+
			"tag tier and onto the search cluster the ADR took it off",
			got.MusicbrainzRecordingID)
	}
	// The enrichment RECORD is a different column with a different owner (ADR-0045 /
	// ADR-0049), and reading the tag must not have filled it in.
	if got.MusicbrainzID != "" {
		t.Errorf("musicbrainz_id = %q on a Track with only a tag id — the scanner's column "+
			"and the enrichment record must not be conflated", got.MusicbrainzID)
	}
}
