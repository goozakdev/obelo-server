package catalog_test

import (
	"context"
	"testing"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Which series a Show's Slots are listed from (file-matcher/10).
//
// A Show has TWO possible sources for that id and they live in different tables.
// `shows.tmdb_id` is written by exactly two narrow paths — an embedded
// {tmdb-12345} in the folder name, and a Wrong-item re-key — while the ordinary
// way a Show gets matched, a provider title search, writes
// `entity_enrichment.external_id` and leaves `shows.tmdb_id` empty. Reading only
// the column therefore reports "this Show never matched a series" for the
// OVERWHELMING majority of real Shows, which are matched, enriched and showing
// correct posters everywhere else in the app.
//
// These tests seed the Show the way real enrichment does — an entity_enrichment
// row, an empty column — which is exactly what every other matcher fixture does
// NOT do (they hand-write the column, and so arrange the one state enrichment
// never produces).

// embeddedSeries is a folder-name {tmdb-…} id, deliberately different from the
// series Enrichment settled on, so a test can tell which source was read.
const embeddedSeries = "99999"

// enrichedShowFixture is the ordinary Show: five episodes, a provider title-search
// match recorded where Enrichment actually records it, and NOTHING in
// shows.tmdb_id.
func enrichedShowFixture(t *testing.T) (*fixture, *fakeSlotLister) {
	t.Helper()
	var eps []store.EpisodeTree
	for i := 1; i <= 5; i++ {
		eps = append(eps, seedEpisode(1, i, "Own S01E0"+itoa(i)+".mkv", 1000))
	}
	f := newFixture(t, eps...)
	lister := &fakeSlotLister{}
	f.svc.SetSlotLister(lister)
	return f, lister
}

// matchShow records a provider match exactly as an enrich pass does: through
// WriteEntityEnrichment, which is the only writer of a searched-and-matched
// parent's external id. It never touches shows.tmdb_id, and neither does the
// real pass.
func matchShow(t *testing.T, f *fixture, series string) {
	t.Helper()
	if err := f.db.WriteEntityEnrichment(store.EntityShow, f.show, store.EntityEnrichmentWrite{
		Overview:   "A Show that matched the ordinary way.",
		Source:     "tmdb",
		ExternalID: series,
	}, nil); err != nil {
		t.Fatalf("seeding the Show's enrichment match: %v", err)
	}
}

// showSeriesColumn reads shows.tmdb_id, so a test can assert the column really is
// empty and is therefore not the thing under test.
func showSeriesColumn(t *testing.T, f *fixture) string {
	t.Helper()
	var got string
	if err := f.db.QueryRow(`SELECT tmdb_id FROM shows WHERE id = ?`, f.show).Scan(&got); err != nil {
		t.Fatalf("reading shows.tmdb_id: %v", err)
	}
	return got
}

// TestAShowMatchedTheOrdinaryWayListsItsSlots is the defect itself: the Show is
// matched, enriched and decorated everywhere else, and the matcher must not tell
// the Admin it never matched.
func TestAShowMatchedTheOrdinaryWayListsItsSlots(t *testing.T) {
	f, lister := enrichedShowFixture(t)
	matchShow(t, f, batmanTAS)
	if col := showSeriesColumn(t, f); col != "" {
		t.Fatalf("shows.tmdb_id = %q; this fixture must reproduce the real shape, an EMPTY column", col)
	}

	one := 1
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &one)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if m.Unavailable != "" {
		t.Fatalf("slots unavailable = %q, want them listed: the Show IS matched, "+
			"its series is just in entity_enrichment rather than shows.tmdb_id", m.Unavailable)
	}
	if m.SeriesExternalID != batmanTAS {
		t.Errorf("series = %q, want the Enrichment match %q", m.SeriesExternalID, batmanTAS)
	}
	g, ok := groupOf(m, 1)
	if !ok {
		t.Fatalf("season 1 is missing: %+v", m.Groups)
	}
	if g.Source != catalog.SlotSourceProvider || g.SlotCount != 5 {
		t.Errorf("season 1 = {source %q, count %d}, want the provider's own 5", g.Source, g.SlotCount)
	}
	for _, sl := range g.Slots {
		if sl.Name == "" {
			t.Errorf("S01E%02d is bare; the provider was never asked", sl.Slot)
		}
	}
	if len(lister.calls) == 0 {
		t.Errorf("the provider was never asked for any records")
	}
}

// TestTheEnrichmentMatchWinsOverAnEmbeddedID: the two sources can only disagree
// when an Admin used Fix info to point the Show at a different record. That is a
// later, explicit statement about which record decorates this Show — which is
// precisely what a Slot's titles are — so it wins.
func TestTheEnrichmentMatchWinsOverAnEmbeddedID(t *testing.T) {
	f, lister := enrichedShowFixture(t)
	mustExec(t, f.db, `UPDATE shows SET tmdb_id = ? WHERE id = ?`, embeddedSeries, f.show)
	matchShow(t, f, batmanTAS)

	one := 1
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &one)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if m.SeriesExternalID != batmanTAS {
		t.Errorf("series = %q, want the Enrichment match %q rather than the embedded id",
			m.SeriesExternalID, batmanTAS)
	}
	for _, c := range lister.calls {
		if c == embeddedSeries+"/1" {
			t.Errorf("the records were fetched from the embedded id %q: %v", embeddedSeries, lister.calls)
		}
	}
}

// TestAnEmbeddedIDStillListsSlotsWithoutEnrichment: the fallback is for the
// never-enriched and enrichment-disabled cases, not to overrule a correction.
func TestAnEmbeddedIDStillListsSlotsWithoutEnrichment(t *testing.T) {
	f, _ := enrichedShowFixture(t)
	mustExec(t, f.db, `UPDATE shows SET tmdb_id = ? WHERE id = ?`, batmanTAS, f.show)

	m, err := f.svc.ShowMatcher(context.Background(), f.show, nil)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if m.Unavailable != "" {
		t.Fatalf("slots unavailable = %q, want them listed from the embedded id", m.Unavailable)
	}
	if m.SeriesExternalID != batmanTAS {
		t.Errorf("series = %q, want the embedded id %q", m.SeriesExternalID, batmanTAS)
	}
}

// TestNoSeriesMeansNeitherSource: SlotsNoSeries is rendered to the Admin as "this
// Show never matched a series", so that sentence has to be true when it appears.
// An enrichment row that matched NOTHING (status unmatched, no external id) is
// still no series, and so is an empty column.
func TestNoSeriesMeansNeitherSource(t *testing.T) {
	f, lister := enrichedShowFixture(t)
	if err := f.db.SetEntityEnrichmentStatus(store.EntityShow, f.show, "unmatched"); err != nil {
		t.Fatalf("seeding an unmatched Show: %v", err)
	}

	m, err := f.svc.ShowMatcher(context.Background(), f.show, nil)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	if m.Unavailable != catalog.SlotsNoSeries {
		t.Errorf("slots unavailable = %q, want %q", m.Unavailable, catalog.SlotsNoSeries)
	}
	if m.SeriesExternalID != "" {
		t.Errorf("series = %q, want none", m.SeriesExternalID)
	}
	if len(lister.calls) != 0 {
		t.Errorf("the provider was asked about a Show with no series: %v", lister.calls)
	}
	// The local half survives: bare numbered Slots are what pure renumbering needs.
	if g, ok := groupOf(m, 1); !ok || len(g.Slots) != 5 {
		t.Errorf("the local slots were lost with the series: %+v", m.Groups)
	}
}

// --- Clearing a pin hands the Slot back the Show's RESOLVED series -----------

// clearedSlotRecord reads what a Slot's Title is left holding after a Clear: the
// enrichment record, and whether it is still marked as anybody's choice.
func clearedSlotRecord(t *testing.T, f *fixture, season, episode int) (series string, locked bool) {
	t.Helper()
	var origin string
	if err := f.db.QueryRow(
		`SELECT enrichment_tmdb_id, enrichment_id_origin
		   FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		episodeKey(season, episode)).Scan(&series, &origin); err != nil {
		t.Fatalf("reading the cleared Slot's record: %v", err)
	}
	return series, store.RecordOrigin(origin).Locked()
}

// pinThenClear pins S01E03 onto a foreign series and then clears it, which is the
// only sequence that exercises the Clear branch's write.
func pinThenClear(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Pins: []catalog.SlotPin{{
			Position: catalog.SlotPosition{Group: 1, Slot: 3},
			Series:   theNewBatmanAdventures,
			Record:   catalog.SlotPosition{Group: 1, Slot: 4},
		}},
	}); err != nil {
		t.Fatalf("apply pin: %v", err)
	}
	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Pins:   []catalog.SlotPin{{Position: catalog.SlotPosition{Group: 1, Slot: 3}, Clear: true}},
	}); err != nil {
		t.Fatalf("apply clear: %v", err)
	}
}

// TestAClearedSlotGetsTheShowsResolvedSeries: "back to the Show's own series" has
// to mean the series that actually decorates the Show, and for the ordinary
// searched-and-matched Show that id is not in shows.tmdb_id at all — it is in
// entity_enrichment, exactly as showSeries resolves it (file-matcher/10, ADR-0045).
// Writing the column instead left the cleared Slot with no anchor of its own, so
// nothing that looks a leaf up by its own record — a single-Title re-enrich, the
// Edit-item image tab — could resolve it.
//
// This is only safe to write because a Clear RELEASES enrichment_id_origin: the
// cascade's skip rule reads that value rather than "the id is non-empty"
// (.scratch/enrichment-override-durability/issues/03), so a cleared Slot keeps an
// anchor without becoming permanently immune to its Show's next Cascade. Both
// halves are asserted here; the second is the one that used to make this change
// impossible.
func TestAClearedSlotGetsTheShowsResolvedSeries(t *testing.T) {
	f, _ := enrichedShowFixture(t)
	matchShow(t, f, batmanTAS)
	if col := showSeriesColumn(t, f); col != "" {
		t.Fatalf("shows.tmdb_id = %q; this fixture must reproduce the real shape, an EMPTY column", col)
	}

	pinThenClear(t, f)

	series, locked := clearedSlotRecord(t, f, 1, 3)
	if series != batmanTAS {
		t.Errorf("cleared Slot's record = %q, want the Show's resolved series %q", series, batmanTAS)
	}
	if locked {
		t.Errorf("cleared Slot's record is marked as the Admin's choice; a Clear is the "+
			"Admin taking a choice BACK, and a locked record would exclude the Slot "+
			"from every later Cascade (identity_key %s)", episodeKey(1, 3))
	}
}

// TestAClearedSlotDoesNotGoBackToACorrectedAwayFromID is the live edge issue 03
// named: a Show whose folder embeds {tmdb-N} AND whose record an Admin corrected
// with Fix info. Handing the Slot the embedded id anchors it to the very series
// the Admin corrected away from — a real record, confidently wrong.
func TestAClearedSlotDoesNotGoBackToACorrectedAwayFromID(t *testing.T) {
	f, _ := enrichedShowFixture(t)
	mustExec(t, f.db, `UPDATE shows SET tmdb_id = ? WHERE id = ?`, embeddedSeries, f.show)
	matchShow(t, f, batmanTAS)

	pinThenClear(t, f)

	series, _ := clearedSlotRecord(t, f, 1, 3)
	if series == embeddedSeries {
		t.Errorf("cleared Slot went back to the folder's embedded id %q, which the Admin "+
			"corrected away from", embeddedSeries)
	}
	if series != batmanTAS {
		t.Errorf("cleared Slot's record = %q, want the Admin's corrected series %q", series, batmanTAS)
	}
}
