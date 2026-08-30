package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Repointing a Slot's RECORD (file-matcher/09, ADR-0044) — the second half of a
// Slot, and the last mile of the case the whole matcher exists for.
//
// A Slot has two independent halves. Its POSITION is always the local library's
// own numbering; its RECORD is what decorates it, and that is repointable. Issue
// 06 shipped the first half: the five files at the end of Batman: The Animated
// Series' season 3 can be put in Season 4, where they belong. But TMDB's Batman
// has no season 4, so those Slots sit bare — the titles live in a re-numbered
// continuation series, The New Batman Adventures, whose own season 1 they are.
//
// What these tests hold down is the line between the two halves. Borrowing the
// records must move NOTHING: not the Slot's position, not its identity_key, not
// any User's watch state — and above all not the numbering, because the borrowed
// series counts from 1 and the Show has a real Season 1 of its own. That
// collision is the reason ADR-0044 kept the record a separate decision, and it is
// what the first test asserts directly.

// theNewBatmanAdventures is the foreign series the records are borrowed from.
const theNewBatmanAdventures = "77777"

// batmanTAS is the Show's own series — the one that has no season 4.
const batmanTAS = "1438"

// fakeSlotLister serves two series' records: the Show's own (which knows nothing
// past season 3) and a foreign one whose season 1 holds the five records the
// Admin is after.
type fakeSlotLister struct {
	unavailable string
	// calls records every (series, group) asked for, because "one call for a whole
	// borrowed run" is a claim only a counter can check.
	calls []string
}

// errNoSuchGroup is what a provider says when a series has no such season. It is
// not a configuration problem, and it must not stop the BORROWED records — which
// live in another series entirely — from being fetched.
var errNoSuchGroup = errors.New("no such group in this series")

func (f *fakeSlotLister) Unavailable() string { return f.unavailable }

func (f *fakeSlotLister) ListGroups(_ context.Context, seriesID string) ([]catalog.SlotGroupSummary, error) {
	if seriesID == theNewBatmanAdventures {
		return []catalog.SlotGroupSummary{{Number: 1, SlotCount: 5}}, nil
	}
	return []catalog.SlotGroupSummary{
		{Number: 1, SlotCount: 5},
		{Number: 3, SlotCount: 5},
	}, nil
}

func (f *fakeSlotLister) ListSlots(_ context.Context, seriesID string, group int) ([]catalog.SlotRecord, error) {
	f.calls = append(f.calls, seriesID+"/"+itoa(group))
	name := "Batman TAS"
	if seriesID == theNewBatmanAdventures {
		name = "New Batman"
	}
	// The Show's own series simply has no season 4 — which is the whole problem,
	// and a real provider answers "no such season" with a 404, not an empty list.
	if seriesID != theNewBatmanAdventures && group == 4 {
		return nil, errNoSuchGroup
	}
	var out []catalog.SlotRecord
	for i := 1; i <= 5; i++ {
		out = append(out, catalog.SlotRecord{
			Group: group, Slot: i,
			Name:     name + " S" + itoa(group) + "E" + itoa(i),
			Overview: "borrowed overview " + itoa(i),
			StillURL: "https://img.example/" + seriesID + "-" + itoa(group) + "-" + itoa(i) + ".jpg",
		})
	}
	return out, nil
}

// batmanFixture seeds the Show as the Scanner left it: a real Season 1 of five
// episodes, and five more files at the end of Season 3 that the provider counts
// somewhere else entirely.
func batmanFixture(t *testing.T) (*fixture, *fakeSlotLister) {
	t.Helper()
	var eps []store.EpisodeTree
	for i := 1; i <= 5; i++ {
		eps = append(eps, seedEpisode(1, i, "Own S01E0"+itoa(i)+".mkv", 1000))
	}
	for i := 61; i <= 65; i++ {
		eps = append(eps, seedEpisode(3, i, "Tail S03E"+itoa(i)+".mkv", 1000))
	}
	f := newFixture(t, eps...)
	mustExec(t, f.db, `UPDATE shows SET tmdb_id = ? WHERE id = ?`, batmanTAS, f.show)
	lister := &fakeSlotLister{}
	f.svc.SetSlotLister(lister)
	return f, lister
}

// tailPlacements puts the five season-3 stragglers on S04E01..E05, in order.
func tailPlacements() []store.FileDecision {
	var out []store.FileDecision
	for i := 1; i <= 5; i++ {
		out = append(out, store.FileDecision{
			Path:        episodePath(3, "Tail S03E"+itoa(60+i)+".mkv"),
			State:       store.DecisionPlaced,
			GroupNumber: 4, SlotNumber: i, Ordinal: 1,
		})
	}
	return out
}

// foreignFill is the bulk gesture's payload: the group's five Slots take the
// foreign series' season-1 records 1..5, in order.
func foreignFill() []catalog.SlotPin {
	var out []catalog.SlotPin
	for i := 1; i <= 5; i++ {
		out = append(out, catalog.SlotPin{
			Position: catalog.SlotPosition{Group: 4, Slot: i},
			Series:   theNewBatmanAdventures,
			Record:   catalog.SlotPosition{Group: 1, Slot: i},
		})
	}
	return out
}

func groupOf(m catalog.Matcher, n int) (catalog.MatcherGroup, bool) {
	for _, g := range m.Groups {
		if g.Number == n {
			return g, true
		}
	}
	return catalog.MatcherGroup{}, false
}

// TestFillingAGroupsRecordsFromAForeignSeries is the PRD's opening example, end
// to end: five files placed into a Season 4 that exists on no disk and in no
// provider, then decorated from another series' season 1.
//
// The assertion that matters most is the negative one. The borrowed records are
// numbered 1..5 and the Show has a real Season 1 of its own, so if a Slot ever
// inherited its record's numbering those five files would land on top of it. Both
// halves are checked: the Slots read S04E01..E05, and Season 1 still holds its own
// five episodes with their own records.
func TestFillingAGroupsRecordsFromAForeignSeries(t *testing.T) {
	f, lister := batmanFixture(t)

	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show, Decisions: tailPlacements(), Pins: foreignFill(),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	four := 4
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &four)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	g, ok := groupOf(m, 4)
	if !ok {
		t.Fatalf("season 4 is not in the matcher: %+v", m.Groups)
	}
	if len(g.Slots) != 5 {
		t.Fatalf("season 4 has %d slots, want the five files placed on it: %+v", len(g.Slots), g.Slots)
	}
	for i, sl := range g.Slots {
		want := i + 1
		// The POSITION is local, and stays local. A borrowed record's numbering
		// reaching this line is the collision ADR-0044 exists to prevent.
		if sl.Group != 4 || sl.Slot != want {
			t.Errorf("slot %d reads S%02dE%02d, want S04E%02d — the foreign numbering leaked",
				i, sl.Group, sl.Slot, want)
		}
		if sl.TitleID == "" {
			t.Errorf("S04E%02d has no Title; the file did not land", want)
		}
		// The Show's OWN series has no season 4, so the Slot itself is bare — which
		// is exactly the state the pin exists to fix, and exactly what must be left
		// behind if the pin is cleared.
		if sl.Name != "" {
			t.Errorf("S04E%02d has its own record %q; the fixture's series has no season 4", want, sl.Name)
		}
		if sl.Record == nil {
			t.Fatalf("S04E%02d reports no borrowed record: %+v", want, sl)
		}
		if sl.Record.Series != theNewBatmanAdventures ||
			sl.Record.Position.Group != 1 || sl.Record.Position.Slot != want {
			t.Errorf("S04E%02d borrowed %+v, want %s S01E%02d",
				want, sl.Record, theNewBatmanAdventures, want)
		}
		// The whole point: the titles, overviews and stills are now there.
		if sl.Record.Name != "New Batman S1E"+itoa(want) {
			t.Errorf("S04E%02d record name = %q, want the foreign series' own", want, sl.Record.Name)
		}
		if sl.Record.Overview == "" || sl.Record.StillURL == "" {
			t.Errorf("S04E%02d borrowed a name but no overview/still: %+v", want, sl.Record)
		}
	}

	// The Show's own season 4 could not be listed at all — there is no such season
	// — and the borrowed records arrived anyway. That is the whole case: the group
	// whose own list fails is exactly the group that needs to borrow.
	if g.Unavailable == "" {
		t.Errorf("season 4 reports no problem listing its own records; the fixture has none")
	}

	// A whole borrowed run costs ONE extra provider call, because a run borrows
	// from one group. (The other call is season 4's own — empty — record list.)
	var borrowed int
	for _, c := range lister.calls {
		if c == theNewBatmanAdventures+"/1" {
			borrowed++
		}
	}
	if borrowed != 1 {
		t.Errorf("filling five slots cost %d calls to the foreign group, want 1: %v", borrowed, lister.calls)
	}

	// And Season 1 — the season the borrowed numbering would have collided with —
	// is exactly as it was.
	one := 1
	m, err = f.svc.ShowMatcher(context.Background(), f.show, &one)
	if err != nil {
		t.Fatalf("matcher season 1: %v", err)
	}
	g, _ = groupOf(m, 1)
	if len(g.Slots) != 5 {
		t.Fatalf("season 1 has %d slots, want its own five: %+v", len(g.Slots), g.Slots)
	}
	for _, sl := range g.Slots {
		if sl.Record != nil {
			t.Errorf("S01E%02d was repointed; nothing asked for that: %+v", sl.Slot, sl.Record)
		}
		if sl.Name == "" {
			t.Errorf("S01E%02d lost its own record", sl.Slot)
		}
	}
}

// TestRepointingLeavesTheSlotWhereItIs: the pin is an ENRICHMENT override, so
// identity_key, the Title's own numbers and every User's watch state must come
// through untouched (ADR-0014). A record decorates a Slot; it never moves one.
func TestRepointingLeavesTheSlotWhereItIs(t *testing.T) {
	f, _ := batmanFixture(t)

	titleID := f.titleIDForPath(episodePath(1, "Own S01E01.mkv"))
	f.setWatch(titleID, 4321, true)

	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Pins: []catalog.SlotPin{{
			Position: catalog.SlotPosition{Group: 1, Slot: 1},
			Series:   theNewBatmanAdventures,
			Record:   catalog.SlotPosition{Group: 1, Slot: 4},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	id, season, episode, hidden, ok := f.titleAt(1, 1)
	if !ok {
		t.Fatalf("S01E01 lost its identity key after being repointed")
	}
	if id != titleID {
		t.Errorf("S01E01 is now title %s, was %s — the row moved and took the watch state with it", id, titleID)
	}
	if season != 1 || episode != 1 || hidden {
		t.Errorf("S01E01 moved to S%02dE%02d (hidden=%v)", season, episode, hidden)
	}
	if ws := f.watch(titleID); ws.ResumePositionMs != 4321 || !ws.Watched {
		t.Errorf("watch state = %+v, want the resume and watched flag it had", ws)
	}
}

// TestClearingARecordReturnsTheSlotToItsDefault: clearing is not "forget the
// numbers" — it is "go back to this series at this position". Leaving the borrowed
// SERIES behind with the numbers cleared would look the Show's own position up in
// somebody else's series, which is a wrong record dressed as a fixed one.
func TestClearingARecordReturnsTheSlotToItsDefault(t *testing.T) {
	f, _ := batmanFixture(t)

	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show, Decisions: tailPlacements(), Pins: foreignFill(),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show, Decisions: tailPlacements(),
		Pins: []catalog.SlotPin{{Position: catalog.SlotPosition{Group: 4, Slot: 2}, Clear: true}},
	}); err != nil {
		t.Fatalf("apply clear: %v", err)
	}

	four := 4
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &four)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	g, _ := groupOf(m, 4)
	for _, sl := range g.Slots {
		switch sl.Slot {
		case 2:
			if sl.Record != nil {
				t.Errorf("S04E02 still reports a borrowed record after being cleared: %+v", sl.Record)
			}
			// The Show's own series has no season 4, so the default record is nothing
			// at all — a cleared Slot goes bare again rather than borrowing quietly.
			if sl.Name != "" {
				t.Errorf("S04E02 name = %q, want bare", sl.Name)
			}
		default:
			if sl.Record == nil {
				t.Errorf("S04E%02d lost its record; only E02 was cleared", sl.Slot)
			}
		}
	}

	var series string
	if err := f.db.QueryRow(
		`SELECT COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id)
		   FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		episodeKey(4, 2)).Scan(&series); err != nil {
		t.Fatalf("reading the cleared Slot's series: %v", err)
	}
	if series != batmanTAS {
		t.Errorf("cleared Slot resolves against series %q, want the Show's own %q", series, batmanTAS)
	}
}

// TestASameSeriesPinIsOfferedTheSameWay: the common case is not the exotic one —
// the provider counting a run in the NEXT season of the same series is what an
// Admin hits most. It stores and reports identically.
func TestASameSeriesPinIsOfferedTheSameWay(t *testing.T) {
	f, _ := batmanFixture(t)

	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Pins: []catalog.SlotPin{{
			Position: catalog.SlotPosition{Group: 1, Slot: 2},
			Series:   batmanTAS,
			Record:   catalog.SlotPosition{Group: 3, Slot: 4},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	one := 1
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &one)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	g, _ := groupOf(m, 1)
	for _, sl := range g.Slots {
		if sl.Slot != 2 {
			continue
		}
		if sl.Record == nil {
			t.Fatalf("a same-series pin is not reported: %+v", sl)
		}
		if sl.Record.Series != batmanTAS || sl.Record.Position.Group != 3 || sl.Record.Position.Slot != 4 {
			t.Errorf("record = %+v, want the Show's own series at S03E04", sl.Record)
		}
		if sl.Record.Name != "Batman TAS S3E4" {
			t.Errorf("record name = %q, want the borrowed position's own", sl.Record.Name)
		}
		// Still S01E02 in the library, still where the file sits.
		if sl.Group != 1 || sl.Slot != 2 {
			t.Errorf("the pin moved the Slot to S%02dE%02d", sl.Group, sl.Slot)
		}
		return
	}
	t.Fatalf("S01E02 vanished: %+v", g.Slots)
}

// TestRepointingAnEmptySlotIsRefused: a record decorates something. An empty Slot
// has nothing to decorate and no Title to carry the pin, so accepting it would
// mean reporting success and storing nothing.
func TestRepointingAnEmptySlotIsRefused(t *testing.T) {
	f, _ := batmanFixture(t)

	_, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Pins: []catalog.SlotPin{{
			Position: catalog.SlotPosition{Group: 9, Slot: 9},
			Series:   theNewBatmanAdventures,
			Record:   catalog.SlotPosition{Group: 1, Slot: 1},
		}},
	})
	if !errors.Is(err, catalog.ErrEmptySlot) {
		t.Fatalf("pinning an empty slot = %v, want ErrEmptySlot", err)
	}
	// Refused whole: the arrangement it rode with must not have been written either.
	if _, _, _, _, ok := f.titleAt(9, 9); ok {
		t.Errorf("a Title appeared at S09E09")
	}
}

// TestASecondApplyKeepsAnExistingRecord: Apply rewrites every Episode row of the
// Show from the plan, so a Slot's borrowed SERIES has to be carried through it. If
// it were not, the next unrelated Apply would strip the series and leave the
// borrowed season/episode pointing into the Show's OWN series — the wrong record,
// silently, and exactly the collision the pin exists to avoid.
func TestASecondApplyKeepsAnExistingRecord(t *testing.T) {
	f, _ := batmanFixture(t)

	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show, Decisions: tailPlacements(), Pins: foreignFill(),
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// A second, unrelated Apply: ignore one of Season 1's files. It says nothing
	// about season 4, but it rebuilds every Episode row of the Show.
	if _, err := f.svc.ApplyPlacement(catalog.PlacementInput{
		ShowID: f.show,
		Decisions: append(tailPlacements(), store.FileDecision{
			Path:  episodePath(1, "Own S01E05.mkv"),
			State: store.DecisionIgnored,
		}),
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	four := 4
	m, err := f.svc.ShowMatcher(context.Background(), f.show, &four)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	g, _ := groupOf(m, 4)
	if len(g.Slots) != 5 {
		t.Fatalf("season 4 has %d slots after the second apply: %+v", len(g.Slots), g.Slots)
	}
	for _, sl := range g.Slots {
		if sl.Record == nil {
			t.Fatalf("S04E%02d lost its record to an unrelated apply", sl.Slot)
		}
		if sl.Record.Series != theNewBatmanAdventures {
			t.Errorf("S04E%02d resolves against %q, want the borrowed series %q",
				sl.Slot, sl.Record.Series, theNewBatmanAdventures)
		}
	}
}
