package catalog_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/scanner"
	"github.com/goozakdev/obelo-server/internal/store"
)

// AN EPISODE PIN SURVIVES THE SCANNER (enrichment-override-durability/01).
//
// placement_rescan_test.go pins apply-then-rescan as a whole, and its snapshot now
// covers the pin — but that catches this only as a side effect of a much larger
// invariant, and only for the arrangements that file happens to enumerate. The
// durability of an Enrichment override across a scan is a promise in its own right
// (CONTEXT.md "Enrichment override"; ADR-0019), so it is asserted as one here,
// end-to-end: real files on disk, the real Scanner walking them, the real Apply
// that writes the pin.
//
// Both scans are exercised deliberately. The TARGETED scan (ADR-0030) is the one
// that runs seconds after the Admin closes the matcher, so a bug there is visible
// immediately; the FULL scan is the 4am one, which is where a pin actually went to
// die. They reach the store through different loaders and must agree.
//
// What makes this worth its own file rather than one more line in a snapshot: a
// pin is a PAIR (which series, and which episode within it), and the scan stripped
// exactly one half. The surviving half still resolves — against the Show's own
// series at the borrowed numbering — so five correct episodes were replaced by
// five wrong ones with no error anywhere (ADR-0044).

// borrowedSeries is the re-numbered continuation series the records come from —
// The New Batman Adventures' role in the motivating case. It is not the Show's own
// series and is not derivable from any filename, which is the whole point.
const borrowedSeries = "77777"

// applyWithPins is apply() plus the record half of the Slot: which provider
// episode decorates it. Position is always the LOCAL Slot; Record is where that
// Slot's record lives in the borrowed series.
func (f *rescanFixture) applyWithPins(pins []catalog.SlotPin, decisions ...store.FileDecision) {
	f.t.Helper()
	if _, err := f.cat.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.showID, Decisions: decisions, Pins: pins,
	}); err != nil {
		f.t.Fatalf("apply: %v", err)
	}
}

// record reads a Slot's record as the enrichment pass would: the series it
// resolves against and the position within it.
//
// The series is the RECORD id — the Admin's Enrichment override when there is one,
// else whatever local naming asserted (ADR-0045, store.recordExternalIDs). Reading
// the raw column instead would assert where the value is kept rather than what the
// lookup resolves to, which is not the promise this file exists to hold down.
func (f *rescanFixture) record(season, episode int) (string, int, int) {
	f.t.Helper()
	var series string
	var pinSeason, pinEpisode int
	if err := f.db.QueryRow(
		`SELECT COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id),
		        COALESCE(enrichment_season, -1), COALESCE(enrichment_episode, -1)
		   FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		rescanEpisodeKey(season, episode),
	).Scan(&series, &pinSeason, &pinEpisode); err != nil {
		f.t.Fatalf("reading the record of S%02dE%02d: %v", season, episode, err)
	}
	return series, pinSeason, pinEpisode
}

// assertRecord insists on the pin as a pair, and names the half-pin failure for
// what it is. "Lost the series, kept the numbering" is not a smaller bug than
// "lost the pin": it is the only one of the two that silently returns a real
// record of the wrong show.
func (f *rescanFixture) assertRecord(season, episode int, wantSeries string, wantSeason, wantEpisode int) {
	f.t.Helper()
	series, pinSeason, pinEpisode := f.record(season, episode)
	if series == wantSeries && pinSeason == wantSeason && pinEpisode == wantEpisode {
		return
	}
	if series != wantSeries && pinSeason == wantSeason && pinEpisode == wantEpisode {
		f.t.Fatalf("S%02dE%02d kept its pinned numbering S%02dE%02d but its series became %q, want %q: "+
			"the lookup now means the Show's OWN series at the borrowed numbering — a real record, and the wrong one",
			season, episode, pinSeason, pinEpisode, series, wantSeries)
	}
	f.t.Fatalf("S%02dE%02d record = %s S%02dE%02d, want %s S%02dE%02d",
		season, episode, series, pinSeason, pinEpisode, wantSeries, wantSeason, wantEpisode)
}

// TestAnEpisodePinSurvivesAScan is the issue's first Done-when, both scans.
//
// The arrangement is the motivating one in miniature: two files the Admin moved
// off the end of season 1 into a season 2 the Show's own series does not have, and
// whose records live in another series' season 1 — numbering that would collide
// head-on with the Show's real season 1 if the pin moved anything but the lookup.
func TestAnEpisodePinSurvivesAScan(t *testing.T) {
	for _, mode := range []struct {
		name   string
		rescan func(*rescanFixture)
	}{
		{"full", func(f *rescanFixture) { f.fullScan() }},
		{"targeted", func(f *rescanFixture) { f.targetedScan() }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			f := newRescanFixture(t, oneSeason...)
			f.applyWithPins(
				[]catalog.SlotPin{
					{
						Position: catalog.SlotPosition{Group: 2, Slot: 1},
						Series:   borrowedSeries,
						Record:   catalog.SlotPosition{Group: 1, Slot: 1},
					},
					{
						Position: catalog.SlotPosition{Group: 2, Slot: 2},
						Series:   borrowedSeries,
						Record:   catalog.SlotPosition{Group: 1, Slot: 2},
					},
				},
				f.place(relE03, 2, 1, 1),
				f.place(relE04, 2, 2, 1),
			)
			f.assertRecord(2, 1, borrowedSeries, 1, 1)
			f.assertRecord(2, 2, borrowedSeries, 1, 2)

			// The scan that runs unattended.
			mode.rescan(f)
			f.assertRecord(2, 1, borrowedSeries, 1, 1)
			f.assertRecord(2, 2, borrowedSeries, 1, 2)

			// And the one after that: a rule that only survives one pass is not one.
			mode.rescan(f)
			f.assertRecord(2, 1, borrowedSeries, 1, 1)
			f.assertRecord(2, 2, borrowedSeries, 1, 2)

			// The Slots nobody repointed are still decorated from the Show's own
			// series at their own numbers — the pin must not spread.
			if series, s, e := f.record(1, 1); series != "" || s != -1 || e != -1 {
				t.Errorf("S01E01 was never repointed but carries record %q S%02dE%02d", series, s, e)
			}
		})
	}
}

// TestAPinnedShowSurvivesTheRescanInvariantWhole runs the file-matcher's own
// apply-then-rescan invariant over an arrangement that CARRIES a pin, so the
// snapshot's tmdb_id / enrichment_season / enrichment_episode columns are
// exercised with real values rather than with the empty strings every other case
// in that file leaves them at. Without this, the column could be in the snapshot
// and still prove nothing.
func TestAPinnedShowSurvivesTheRescanInvariantWhole(t *testing.T) {
	for _, mode := range []struct {
		name   string
		rescan func(*rescanFixture)
	}{
		{"full", func(f *rescanFixture) { f.fullScan() }},
		{"targeted", func(f *rescanFixture) { f.targetedScan() }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			f := newRescanFixture(t, oneSeason...)
			f.applyWithPins(
				[]catalog.SlotPin{{
					Position: catalog.SlotPosition{Group: 1, Slot: 2},
					Series:   borrowedSeries,
					Record:   catalog.SlotPosition{Group: 3, Slot: 4},
				}},
				f.place(relE01, 1, 1, 1),
				f.place(relE02, 1, 2, 1),
				f.place(relE03, 1, 3, 1),
				f.place(relE04, 1, 4, 1),
			)
			f.assertRecord(1, 2, borrowedSeries, 3, 4)
			f.assertRescanIsNoop(func() { mode.rescan(f) })
		})
	}
}

// A MOVIE'S FIX INFO SURVIVES A SCAN OF A FOLDER THAT NAMES ITS OWN RECORD
// (enrichment-override-durability/02, ADR-0045).
//
// This is the narrow half issue 01's fix could not reach. Its guard let a scan
// FILL an external id but not blank one — and a folder like
// `Dune (2021) {tmdb-438631}` gives the scanner a real id to fill, so every scan
// restated the folder's id over the Admin's Fix info. Silently, overnight, having
// already told the Admin the correction applied.
//
// End-to-end on purpose: the real Scanner walking a real folder is what made the
// two claims collide, and the store-level twin of this test cannot see the folder
// name at all.
func TestAMoviesFixInfoSurvivesAScanOfAnEmbeddedIDFolder(t *testing.T) {
	const (
		folderRecord    = "438631" // what `{tmdb-438631}` says
		correctedRecord = "693134" // what the Admin says, and means
	)

	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Dune (2021) {tmdb-"+folderRecord+"}", "Dune (2021).mkv"))

	db := openTemp(t)
	if _, err := db.CreateLibrary("libmov", "Movies", "movie",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}
	sc := scanner.NewService(db, rescanProber{})
	scan := func(label string) {
		t.Helper()
		if _, err := sc.Scan(context.Background(), "libmov"); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	scan("seed scan")

	var titleID, identityKey string
	if err := db.QueryRow(
		`SELECT id, identity_key FROM titles WHERE library_id = 'libmov'`,
	).Scan(&titleID, &identityKey); err != nil {
		t.Fatalf("seed scan produced no Title: %v", err)
	}
	// The embedded id IS the identity (ADR-0002) — that part never changes.
	if identityKey != "tmdb:"+folderRecord {
		t.Fatalf("identity_key = %q, want %q; this fixture must reproduce the real shape, "+
			"a folder whose id keys the row", identityKey, "tmdb:"+folderRecord)
	}

	// Fix info: filed right, matched wrong. It touches no identity (ADR-0019).
	if err := db.SetTitleExternalMatch(titleID, store.ExternalMatch{TMDBID: correctedRecord}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}

	// The 4am scan, twice — a rule that survives one pass is not one.
	scan("rescan")
	scan("second rescan")

	var record, identityID, keyAfter string
	if err := db.QueryRow(
		`SELECT COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id), tmdb_id, identity_key
		   FROM titles WHERE id = ?`, titleID,
	).Scan(&record, &identityID, &keyAfter); err != nil {
		t.Fatalf("reading the Movie after the scan: %v", err)
	}
	if record != correctedRecord {
		t.Errorf("the Movie resolves against record %q, want the Admin's %q: the scan restated the "+
			"folder's id over a correction it had already accepted", record, correctedRecord)
	}
	// And the folder keeps every bit of authority it ever had over identity.
	if identityID != folderRecord || keyAfter != "tmdb:"+folderRecord {
		t.Errorf("identity became (tmdb_id %q, key %q), want (%q, %q): an Enrichment override "+
			"must not move identity or the watch state keyed to it (ADR-0014)",
			identityID, keyAfter, folderRecord, "tmdb:"+folderRecord)
	}
}
