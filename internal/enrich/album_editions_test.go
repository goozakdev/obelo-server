package enrich

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0052's last decision, at the seam that implements it: an edition can be
// chosen WITHOUT LEAVING OBELO.
//
// The operator found the right edition of Andrea Bocelli's *Viaggio Italiano* on
// musicbrainz.org and pasted its URL back into the app, because the app could not
// show them the choice: *"It shows the best guess, but I cant choose a specific
// edition out of that release-group."* Everything here is about that list existing,
// being right, being cheap, and saying which one is in use.

// The real ids from the motivating library, plus its editions. The local album has
// SIXTEEN tracks, which is the number that picks a pressing out of this list.
const (
	viaggioCD    = "b1f0e1b3-1111-4111-8111-111111111111"
	viaggioIT    = "b1f0e1b3-2222-4222-8222-222222222222"
	viaggioDelux = "b1f0e1b3-3333-4333-8333-333333333333"
)

// viaggioEditions is the release-group as MusicBrainz holds it: an Italian original,
// an international CD of the same 16 tracks, and a 2-disc deluxe that does not fit.
func viaggioEditions() []stubRelease {
	return []stubRelease{
		{ID: viaggioIT, Date: "1995-11-06", RGID: viaggioGroup, Country: "IT", Format: "CD",
			Discs: [][]stubTrack{disc("it", 16)}},
		{ID: viaggioCD, Date: "1996-02-27", RGID: viaggioGroup, Country: "XE", Format: "CD",
			Disamb: "international edition", Discs: [][]stubTrack{disc("xe", 16)}},
		{ID: viaggioDelux, Date: "2000-01-01", RGID: viaggioGroup, Country: "US", Format: "CD",
			Disamb: "deluxe edition", Discs: [][]stubTrack{disc("dlxA", 16), disc("dlxB", 4)}},
	}
}

// viaggioFixture is the motivating album over a real store: sixteen local tracks,
// matched to the Viaggio Italiano release-group, served by a real MusicBrainzProvider
// pointed at a canned MusicBrainz.
func viaggioFixture(t *testing.T, al seedAlbum) (*Service, *store.DB, *tracklistStub) {
	t.Helper()
	prov, stub := newTracklistStub(t, viaggioEditions()...)
	if al.entityRecord == "" && al.rgTag == "" {
		al.entityRecord = viaggioGroup
	}
	if len(al.tracks) == 0 {
		for i := 1; i <= 16; i++ {
			al.tracks = append(al.tracks, seedTrack{
				id:    "t" + string(rune('a'+i-1)),
				title: "Track " + string(rune('a'+i-1)),
				num:   i,
			})
		}
	}
	svc, db := newAlbumFixture(t, prov, al)
	return svc, db, stub
}

func editionIDs(eds []ReleaseEdition) []string {
	out := make([]string, 0, len(eds))
	for _, e := range eds {
		out = append(out, e.ReleaseID)
	}
	return out
}

// --- 1. the headline ----------------------------------------------------------

// THE acceptance criterion, at the service seam: the album the operator could not
// fix inside Obelo now lists every edition of its release-group, with the four facts
// that tell them apart and the local track count beside them.
func TestViaggioItalianoListsItsEditionsWithoutLeavingObelo(t *testing.T) {
	svc, _, stub := viaggioFixture(t, seedAlbum{})

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if got.ReleaseGroupID != viaggioGroup {
		t.Errorf("releaseGroupId = %q, want %q", got.ReleaseGroupID, viaggioGroup)
	}
	if len(got.Editions) != 3 {
		t.Fatalf("listed %d editions, want 3: %v", len(got.Editions), editionIDs(got.Editions))
	}
	// The LOCAL count, stated rather than left to arithmetic: it is the number that
	// makes "20 tracks" recognizable as the wrong pressing at a glance.
	if got.LocalTrackCount != 16 {
		t.Errorf("localTrackCount = %d, want 16 — the number the whole choice turns on", got.LocalTrackCount)
	}

	// Every field the picker's row prints: date · country · format · N tracks, plus
	// the source's own disambiguation.
	it := got.Editions[0]
	if it.ReleaseID != viaggioIT || it.Date != "1995-11-06" || it.Country != "IT" ||
		it.Format != "CD" || it.TrackCount != 16 {
		t.Errorf("first edition = %+v, want the 1995 Italian 16-track CD", it)
	}
	if got.Editions[1].Disambiguation != "international edition" {
		t.Errorf("disambiguation = %q, want %q", got.Editions[1].Disambiguation, "international edition")
	}
	// A two-disc edition counts BOTH discs and names the medium once: 16+4 tracks on
	// 2×CD is exactly the pressing an operator must be able to rule out.
	dlx := got.Editions[2]
	if dlx.TrackCount != 20 || dlx.Format != "2×CD" {
		t.Errorf("deluxe = %d tracks on %q, want 20 on 2×CD", dlx.TrackCount, dlx.Format)
	}

	// One browse, and the same one fit-selection pays for.
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1: %v", len(reqs), reqs)
	}
	for _, want := range []string{"release-group=" + viaggioGroup, "inc=recordings", "limit=100"} {
		if !strings.Contains(reqs[0], want) {
			t.Errorf("browse %q is missing %q", reqs[0], want)
		}
	}
}

// --- 2. which one is in use ---------------------------------------------------

// With nobody having chosen and nothing in the tags, the edition in use is the
// system's own guess — and the picker names it as one. "It shows the best guess" was
// true; what was missing is the list beside it and the word "guess" on it.
func TestTheBestGuessIsMarkedInUseAndNamedAsAGuess(t *testing.T) {
	svc, _, _ := viaggioFixture(t, seedAlbum{})

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	// Two editions fit 16; the earlier one wins, which is fit-selection's own rule.
	if got.InUseReleaseID != viaggioIT || got.InUseSource != EditionSourceFit {
		t.Errorf("in use = %q (%s), want the earliest 16-track edition %q by fit",
			got.InUseReleaseID, got.InUseSource, viaggioIT)
	}
	if got.ChosenReleaseID != "" {
		t.Errorf("chosenReleaseId = %q, want empty — nobody chose one", got.ChosenReleaseID)
	}
}

// The edition in use is the edition the tracklist actually comes back from. The two
// are computed in different places over different shapes, and a picker that marks a
// row the album is not decorated from is worse than no marker at all.
func TestTheMarkedEditionIsTheOneTheTracklistComesFrom(t *testing.T) {
	svc, _, _ := viaggioFixture(t, seedAlbum{})
	prov, _ := newTracklistStub(t, viaggioEditions()...)

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	tl, err := prov.AlbumTracklist(context.Background(),
		TracklistRequest{ReleaseGroupID: viaggioGroup, LocalTrackCount: 16})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	// The Italian edition's titles are "it 1".."it 16"; the international's are "xe".
	if !strings.HasPrefix(tl[0].Title, "it ") {
		t.Fatalf("tracklist came from %q, expected the Italian edition", tl[0].Title)
	}
	if got.InUseReleaseID != viaggioIT {
		t.Errorf("picker marks %q in use while the tracklist comes from %q — one rule, two answers",
			got.InUseReleaseID, viaggioIT)
	}
}

// A human's choice outranks the guess and is labelled as theirs, which is the whole
// licence ADR-0052 grants (issue 11 spends it).
func TestAChosenEditionIsTheOneMarkedInUse(t *testing.T) {
	svc, db, _ := viaggioFixture(t, seedAlbum{})
	pinEdition(t, db, viaggioCD)

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if got.ChosenReleaseID != viaggioCD {
		t.Errorf("chosenReleaseId = %q, want %q", got.ChosenReleaseID, viaggioCD)
	}
	if got.InUseReleaseID != viaggioCD || got.InUseSource != EditionSourceChosen {
		t.Errorf("in use = %q (%s), want the CHOSEN %q", got.InUseReleaseID, got.InUseSource, viaggioCD)
	}
}

// The FILES' edition is in use when nobody chose one — the middle tier of ADR-0052's
// precedence, and what a Picard-tagged library gets for free.
func TestTheEditionTheFilesNameIsMarkedInUse(t *testing.T) {
	svc, _, _ := viaggioFixture(t, seedAlbum{entityRecord: viaggioGroup, releaseTag: viaggioDelux})

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if got.InUseReleaseID != viaggioDelux || got.InUseSource != EditionSourceTagged {
		t.Errorf("in use = %q (%s), want the TAGGED %q — even though it does not fit",
			got.InUseReleaseID, got.InUseSource, viaggioDelux)
	}
}

// A pin MusicBrainz has since moved out of this release-group is stored and NOT in
// use. Reporting it as in use would tell the operator their correction had taken
// effect when the tracklist read had already fallen back past it.
func TestAStaleChosenEditionIsReportedButNotMarkedInUse(t *testing.T) {
	svc, db, _ := viaggioFixture(t, seedAlbum{})
	pinEdition(t, db, strangerRelease)

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if got.ChosenReleaseID != strangerRelease {
		t.Errorf("chosenReleaseId = %q, want the stored pin %q reported", got.ChosenReleaseID, strangerRelease)
	}
	if got.InUseReleaseID != viaggioIT || got.InUseSource != EditionSourceFit {
		t.Errorf("in use = %q (%s), want the fit %q — a pin outside this release-group does not apply",
			got.InUseReleaseID, got.InUseSource, viaggioIT)
	}
}

// --- 3. the shapes that must not become errors --------------------------------

// A release-group with exactly ONE release still renders. It is not a degenerate
// case to skip: it is the confirmation that there was no choice to make, and an
// operator who sees nothing cannot tell it from a list that failed to load.
func TestAReleaseGroupWithOneReleaseStillLists(t *testing.T) {
	prov, _ := newTracklistStub(t, stubRelease{
		ID: viaggioIT, Date: "1995-11-06", RGID: viaggioGroup, Country: "IT", Format: "CD",
		Discs: [][]stubTrack{disc("it", 16)},
	})
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: viaggioGroup,
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if len(got.Editions) != 1 || got.Editions[0].ReleaseID != viaggioIT {
		t.Fatalf("listed %v, want the single edition %q", editionIDs(got.Editions), viaggioIT)
	}
	if got.InUseReleaseID != viaggioIT {
		t.Errorf("in use = %q, want the only edition there is", got.InUseReleaseID)
	}
}

// An album nobody has matched has no release-group, so there is nothing to list —
// and no provider call to spend finding that out. The picker above the section is
// where that album is fixed.
func TestAnUnmatchedAlbumListsNoEditionsAndSpendsNoRequest(t *testing.T) {
	prov, stub := newTracklistStub(t, viaggioEditions()...)
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})

	got, err := svc.AlbumEditions(context.Background(), "al1")
	if err != nil {
		t.Fatalf("AlbumEditions: %v", err)
	}
	if got.ReleaseGroupID != "" || len(got.Editions) != 0 {
		t.Errorf("got %+v, want an empty listing for an unmatched album", got)
	}
	if reqs := stub.requests(); len(reqs) != 0 {
		t.Errorf("made %d requests for an album with no release-group: %v", len(reqs), reqs)
	}
}

// A provider that cannot list editions — and music enrichment switched off — are the
// same "not now" every other provider-backed list gives. The API layer turns it into
// 503 SEARCH_UNAVAILABLE and the picker degrades to the paste box; an error value of
// any other shape would degrade to an error page instead.
func TestAProviderThatCannotListEditionsIsUnavailable(t *testing.T) {
	svc, _ := newAlbumFixture(t, fakeProvider{}, seedAlbum{
		entityRecord: viaggioGroup,
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	if _, err := svc.AlbumEditions(context.Background(), "al1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("provider without the capability: %v, want ErrSearchUnavailable", err)
	}

	prov, _ := newTracklistStub(t, viaggioEditions()...)
	svc.SetProvider(prov, Enablement{Video: true}) // music off
	if _, err := svc.AlbumEditions(context.Background(), "al1"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("music enrichment off: %v, want ErrSearchUnavailable", err)
	}
}

// --- 4. the cache -------------------------------------------------------------

// Opening the edition section twice costs ONE request — the same property issue 02
// asserts for the tracklist, and it matters more here: this is the list an Admin
// toggles open and shut while they decide, at the host ADR-0049 watched shed load.
func TestOpeningTheEditionListTwiceCostsOneRequest(t *testing.T) {
	svc, _, stub := viaggioFixture(t, seedAlbum{})

	for i := 0; i < 2; i++ {
		got, err := svc.AlbumEditions(context.Background(), "al1")
		if err != nil || len(got.Editions) != 3 {
			t.Fatalf("read %d: (%d editions, %v)", i, len(got.Editions), err)
		}
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — the second open must be served from the cache: %v",
			len(reqs), reqs)
	}
}

// A provider swap empties the cache: the listed release ids are the OLD provider's,
// and offering an Admin an edition the new one never listed is a pin that resolves
// to nothing.
func TestAProviderSwapDropsTheCachedEditions(t *testing.T) {
	svc, _, stub := viaggioFixture(t, seedAlbum{})

	if _, err := svc.AlbumEditions(context.Background(), "al1"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	prov2, stub2 := newTracklistStub(t, viaggioEditions()...)
	svc.SetProvider(prov2, Enablement{Video: true, Music: true})
	if _, err := svc.AlbumEditions(context.Background(), "al1"); err != nil {
		t.Fatalf("read after swap: %v", err)
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("old provider took %d requests, want 1", len(reqs))
	}
	if reqs := stub2.requests(); len(reqs) != 1 {
		t.Errorf("new provider took %d requests, want 1 — the swap must not be answered from "+
			"the old provider's list", len(reqs))
	}
}

// --- 5. the provider seam -----------------------------------------------------

// The listing is EXTRACTED from the browse fit-selection already pays for, not added
// beside it: one endpoint, one inc, one limit. A second browse would have doubled
// what an album costs at a rate-limited host for information already in hand.
func TestReleaseGroupEditionsReadsTheBrowseFitSelectionAlreadyPaysFor(t *testing.T) {
	prov, stub := newTracklistStub(t, viaggioEditions()...)

	eds, err := prov.ReleaseGroupEditions(context.Background(), viaggioGroup)
	if err != nil {
		t.Fatalf("ReleaseGroupEditions: %v", err)
	}
	if len(eds) != 3 {
		t.Fatalf("listed %d editions, want 3", len(eds))
	}
	if _, err := prov.AlbumTracklist(context.Background(),
		TracklistRequest{ReleaseGroupID: viaggioGroup, LocalTrackCount: 16}); err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (one each): %v", len(reqs), reqs)
	}
	if reqs[0] != reqs[1] {
		t.Errorf("the two halves of one question asked two different things:\n  %s\n  %s", reqs[0], reqs[1])
	}
}

// An unknown release-group is an empty list, not an error: the picker renders
// "nothing to choose from", and the album's own match is what the Admin fixes.
func TestAnUnknownReleaseGroupListsNoEditions(t *testing.T) {
	prov, _ := newTracklistStub(t, viaggioEditions()...)

	eds, err := prov.ReleaseGroupEditions(context.Background(), "rg-nobody-has")
	if err != nil {
		t.Fatalf("ReleaseGroupEditions: %v", err)
	}
	if len(eds) != 0 {
		t.Errorf("listed %d editions for an unknown release-group, want 0", len(eds))
	}
}

// A refusal is returned as itself (ADR-0049): the picker must be able to say "the
// source is busy" rather than "this album has no editions", which is a fact it never
// established.
func TestARefusedEditionBrowseIsReportedAsAFailure(t *testing.T) {
	prov, stub := newTracklistStub(t, viaggioEditions()...)
	stub.fail("/release", 503)

	_, err := prov.ReleaseGroupEditions(context.Background(), viaggioGroup)
	if err == nil {
		t.Fatal("ReleaseGroupEditions: nil error on a 503 — a busy host is not an empty catalogue")
	}
	if errors.Is(err, ErrNoMatch) {
		t.Errorf("a 503 read as ErrNoMatch (%v) — the two must stay distinguishable", err)
	}
}
