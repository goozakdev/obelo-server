package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Repointing a Slot's RECORD from the matcher (file-matcher/09, ADR-0044): the
// HTTP half.
//
// The contract question these settle is WHEN a pin is written. It rides in the
// Apply payload rather than in a call of its own, and that is forced rather than
// chosen: the pin is stored on a Title, and the Slots this feature exists to
// repoint are ones the Admin has just placed files onto, whose Titles do not exist
// until Apply commits. There is nothing to pin at the moment of the gesture — and
// a pin that applied immediately would be the one change Revert could not undo.

const foreignSeries = "77777"

// fakeSeriesProvider lists episodes PER SERIES, which the shared
// fakeEpisodeProvider deliberately does not: the whole point here is borrowing a
// record from a series that is not the Show's, so the two must be told apart.
type fakeSeriesProvider struct {
	fakeProvider
}

func (f *fakeSeriesProvider) SeriesSeasons(_ context.Context, seriesID string) ([]enrich.SeasonSummary, error) {
	if seriesID == foreignSeries {
		return []enrich.SeasonSummary{{Season: 1, EpisodeCount: 3}}, nil
	}
	return []enrich.SeasonSummary{{Season: 1, EpisodeCount: 3}}, nil
}

func (f *fakeSeriesProvider) SeasonEpisodes(_ context.Context, seriesID string, season int) ([]enrich.EpisodeCandidate, error) {
	// The Show's own series has no season 4 — the state the pin exists to fix.
	if seriesID != foreignSeries && season != 1 {
		return nil, nil
	}
	name := "Own"
	if seriesID == foreignSeries {
		name = "Borrowed"
	}
	var out []enrich.EpisodeCandidate
	for i := 1; i <= 3; i++ {
		out = append(out, enrich.EpisodeCandidate{
			Season: season, Episode: i,
			Name:     name + " episode " + string(rune('0'+i)),
			Overview: "an overview",
			StillURL: "https://img.example/still.jpg",
		})
	}
	return out, nil
}

// repointFixture scans the matcher tree with a per-series listing provider and
// returns the Sorted Show, plus the paths of its two numbered files.
func repointFixture(t *testing.T) (*testharness.Server, string, string, matcherResp) {
	t.Helper()
	requireMatcherFixtures(t)
	prov := &fakeSeriesProvider{}
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return enrich.TitleMetadata{}, enrich.ErrNoMatch }
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")
	return srv, token, showID, getMatcher(t, srv, token, showID, "")
}

func slotNumbered(g matcherGroupResp, n int) (matcherSlotResp, bool) {
	for _, s := range g.Slots {
		if s.Slot == n {
			return s, true
		}
	}
	return matcherSlotResp{}, false
}

// TestApplyMatcherPinsTheRecordItPlacedTheFileOn is the whole timing argument in
// one call: the same request that CREATES the Title at S04E01 also repoints its
// record. Split into two calls there would be nothing to pin.
func TestApplyMatcherPinsTheRecordItPlacedTheFileOn(t *testing.T) {
	srv, token, showID, before := repointFixture(t)
	two, ok := fileNamed(before, "Sorted Show (2018) - S01E02 - Two.mkv")
	if !ok {
		t.Fatalf("S01E02 missing: %+v", before.Files)
	}

	var after matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": two.Path, "state": "placed", "placements": []map[string]any{{"group": 4, "slot": 1}}},
		},
		"slots": []map[string]any{
			{"group": 4, "slot": 1, "record": map[string]any{
				"externalId": foreignSeries, "group": 1, "slot": 2,
			}},
		},
	}, &after)
	if status != http.StatusOK {
		t.Fatalf("apply = %d, want 200; body: %s", status, body)
	}

	g, ok := groupNumbered(after, 4)
	if !ok {
		t.Fatalf("season 4 missing from the re-read: %+v", after.Groups)
	}
	sl, ok := slotNumbered(g, 1)
	if !ok {
		t.Fatalf("S04E01 missing: %+v", g.Slots)
	}
	if sl.Record == nil {
		t.Fatalf("S04E01 reports no record; the pin did not ride the apply: %+v", sl)
	}
	if sl.Record.ExternalID != foreignSeries || sl.Record.Group != 1 || sl.Record.Slot != 2 {
		t.Errorf("record = %+v, want the foreign series at group 1 slot 2", sl.Record)
	}
	// The Slot keeps its OWN position. A borrowed record numbered 1/2 landing on
	// the Slot's code is the collision ADR-0044 exists to prevent.
	if sl.Group != 4 || sl.Slot != 1 {
		t.Errorf("the pin moved the Slot to %d/%d", sl.Group, sl.Slot)
	}

	// Expanding the group fetches the borrowed record's own words, so the screen
	// can show the title it just borrowed rather than a bare reference.
	expanded := getMatcher(t, srv, token, showID, "?group=4")
	g, _ = groupNumbered(expanded, 4)
	sl, _ = slotNumbered(g, 1)
	if sl.Record == nil || sl.Record.Name != "Borrowed episode 2" {
		t.Errorf("expanded record = %+v, want the foreign series' own name", sl.Record)
	}
	if sl.Record.StillURL == "" || sl.Record.StillURL[0] != '/' {
		t.Errorf("record stillUrl = %q, want a same-origin proxy reference", sl.Record.StillURL)
	}
	// The Show's own series has no season 4, so the Slot itself is still bare —
	// which is what clearing the pin has to fall back to.
	if sl.Name != "" {
		t.Errorf("slot name = %q; the Show's own series has no season 4", sl.Name)
	}
}

// TestApplyMatcherClearsARecord: a null record is not a missing field — it is the
// Admin saying "back to this series, this position", which for a Slot the Show's
// series does not list means bare again.
func TestApplyMatcherClearsARecord(t *testing.T) {
	srv, token, showID, before := repointFixture(t)
	one, ok := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("S01E01 missing: %+v", before.Files)
	}

	pin := map[string]any{
		"files": []map[string]any{
			{"path": one.Path, "state": "placed", "placements": []map[string]any{{"group": 1, "slot": 1}}},
		},
		"slots": []map[string]any{
			{"group": 1, "slot": 1, "record": map[string]any{
				"externalId": foreignSeries, "group": 1, "slot": 3,
			}},
		},
	}
	if status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, pin, nil); status != http.StatusOK {
		t.Fatalf("pin = %d, want 200; body: %s", status, body)
	}

	var after matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": pin["files"],
		"slots": []map[string]any{{"group": 1, "slot": 1, "record": nil}},
	}, &after)
	if status != http.StatusOK {
		t.Fatalf("clear = %d, want 200; body: %s", status, body)
	}
	g, _ := groupNumbered(after, 1)
	sl, ok := slotNumbered(g, 1)
	if !ok {
		t.Fatalf("S01E01 vanished: %+v", g.Slots)
	}
	if sl.Record != nil {
		t.Errorf("S01E01 still reports a record after being cleared: %+v", sl.Record)
	}
}

// TestApplyMatcherRefusesARecordOnAnEmptySlot: a record decorates something. An
// empty Slot has no Title to carry the pin, so accepting one would report success
// and store nothing.
func TestApplyMatcherRefusesARecordOnAnEmptySlot(t *testing.T) {
	srv, token, showID, _ := repointFixture(t)

	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{},
		"slots": []map[string]any{
			{"group": 9, "slot": 9, "record": map[string]any{"externalId": foreignSeries, "group": 1, "slot": 1}},
		},
	}, &errBody)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("pinning an empty slot = %d, want 422; body: %s", status, body)
	}
	if errBody.Error.Code != "EMPTY_SLOT" {
		t.Errorf("error code = %q, want EMPTY_SLOT; body: %s", errBody.Error.Code, body)
	}
}

// TestApplyMatcherRejectsARecordThatNamesNoSlot: "clear it" and "pin it to
// nothing in particular" must not be the same request.
func TestApplyMatcherRejectsARecordThatNamesNoSlot(t *testing.T) {
	srv, token, showID, _ := repointFixture(t)

	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{},
		"slots": []map[string]any{
			{"group": 1, "slot": 1, "record": map[string]any{"externalId": foreignSeries, "group": 1}},
		},
	}, nil)
	if status != http.StatusBadRequest {
		t.Errorf("a record with no slot = %d, want 400; body: %s", status, body)
	}
}

// TestApplyMatcherWithoutSlotsLeavesRecordsAlone: the `slots` list is sparse, not
// the whole set. An Apply that says nothing about records must not clear them —
// otherwise every ordinary rearrangement would quietly undo the Admin's pins.
func TestApplyMatcherWithoutSlotsLeavesRecordsAlone(t *testing.T) {
	srv, token, showID, before := repointFixture(t)
	one, ok := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("S01E01 missing: %+v", before.Files)
	}
	files := []map[string]any{
		{"path": one.Path, "state": "placed", "placements": []map[string]any{{"group": 1, "slot": 1}}},
	}

	if status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": files,
		"slots": []map[string]any{
			{"group": 1, "slot": 1, "record": map[string]any{"externalId": foreignSeries, "group": 1, "slot": 3}},
		},
	}, nil); status != http.StatusOK {
		t.Fatalf("pin = %d, want 200; body: %s", status, body)
	}

	var after matcherResp
	if status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token,
		map[string]any{"files": files}, &after); status != http.StatusOK {
		t.Fatalf("second apply = %d, want 200; body: %s", status, body)
	}
	g, _ := groupNumbered(after, 1)
	sl, _ := slotNumbered(g, 1)
	if sl.Record == nil || sl.Record.ExternalID != foreignSeries || sl.Record.Slot != 3 {
		t.Errorf("S01E01 record = %+v after an apply that said nothing about records; want it kept", sl.Record)
	}
}
