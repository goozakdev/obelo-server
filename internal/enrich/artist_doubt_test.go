package enrich

import (
	"context"
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0053's amendment, "corroboration also works as doubt", driven through real
// passes over a real store.
//
// ADR-0053 stopped NEW wrong Artist matches and said, in its own Consequences,
// that the ones already in the library "do not repair themselves": they read
// `matched`, and a recheck (ADR-0051) re-asks only settled NON-answers. Sixteen
// artists in the motivating library carry the signature this file is about —
// matched, with not one matched Album beneath them — including "The Eagles",
// pointing at a4852e21-… , a 1960s UK instrumental group, while every album under
// it failed to match.
//
// So the fixtures reuse the two-Eagles stub from artist_corroboration_test.go on
// purpose: the DECOY is the assertion. An artist literally named "The Eagles" is
// sitting in the stub, spelled exactly as the local row spells it, so a re-ask
// that resolves by NAME lands right back on a4852e21-… and fails here. Only
// corroboration passes.
//
// Everything is asserted on the CALL LOG as well as on the row, because "the
// recheck doubted this Artist" and "the recheck skipped it" leave the same
// database behind whenever the answer does not change — and the whole cost of the
// feature is in what the provider was asked.

// eaglesDoubtFixture seeds the motivating shape onto newEaglesLibrary: the Artist
// is `matched` to the WRONG band, and its one Album is a settled non-answer, so
// nothing beneath the Artist corroborates it. The Track is `matched` so that the
// only calls a pass can make are parent calls.
func eaglesDoubtFixture(albumStatus string) func(exec func(string, ...any)) {
	return func(exec func(string, ...any)) {
		exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
		      VALUES ('artist', 'ar1', ?, 'matched')`, britishEaglesMBID)
		exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
		      VALUES ('album', 'al1', '', ?)`, albumStatus)
		exec(`UPDATE titles SET enrichment_status = 'matched', musicbrainz_id = 'rec-1' WHERE id = 't1'`)
	}
}

func artistRow(t *testing.T, db *store.DB) store.EntityEnrichment {
	t.Helper()
	got, err := db.EntityEnrichmentByID(store.EntityArtist, "ar1")
	if err != nil {
		t.Fatalf("read artist: %v", err)
	}
	return got
}

// --- the headline -------------------------------------------------------------

// "A `matched` Artist with no matched Album is re-asked in ModeRecheck, and —
// through corroboration — lands on the album-corroborated id rather than its old
// one."
//
// This is the only mechanism in the system that reaches those sixteen rows short
// of a full pass over 10,550 tracks or a human noticing that an artist page shows
// another band's photo.
func TestARecheckReAsksAMatchedArtistThatNoAlbumCorroborates(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, eaglesDoubtFixture("unmatched"))

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}

	got := artistRow(t, db)
	if got.ExternalID == britishEaglesMBID {
		t.Fatalf("the Artist still holds %s — the 1960s UK instrumental group. Either the "+
			"doubt never fired, or the re-ask resolved by NAME and found the decoy again "+
			"(calls: %v)", got.ExternalID, stub.calls())
	}
	if got.ExternalID != americanEaglesMBID {
		t.Fatalf("artist external_id = %q, want %q (calls: %v)",
			got.ExternalID, americanEaglesMBID, stub.calls())
	}
	if got.Status != "matched" {
		t.Errorf("artist status = %q, want matched", got.Status)
	}
	// Corroboration, not the name search: the doubt exists because the name is
	// exactly what cannot tell these two bands apart.
	if n := stub.nameSearches(); n != 0 {
		t.Errorf("%d artist name searches (calls: %v) — a re-ask by name is the same wrong "+
			"answer a second time", n, stub.calls())
	}
	// The evidence was the album the library has held all along: one lookup of its
	// tagged release-group, and no search anywhere.
	if n := stub.searches(); n != 0 {
		t.Errorf("%d searches (calls: %v) — the corroborating Album carries a tag "+
			"release-group id, so this costs one lookup", n, stub.calls())
	}
}

// --- the cost floor, which issue 07 established and this must not break -------

// "A recheck over a healthy library still makes ZERO provider calls."
//
// This is the property that lets an operator press the button without thinking
// about it, and the doubt is the first thing ever added to ModeRecheck that could
// have cost something on a library where nothing is wrong. One matched Album is
// all the corroboration a matched Artist needs.
func TestARecheckOverACorroboratedMusicLibraryStillMakesNoProviderCalls(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, eaglesDoubtFixture("matched"))

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("a recheck over a fully matched Music library made %d calls: %v — the doubt "+
			"must fire on the shape that means 'this cannot be right' and on nothing else",
			len(stub.calls()), stub.calls())
	}
	if res.Total != 0 {
		t.Errorf("the pass visited %d leaves, want 0", res.Total)
	}
	if got := artistRow(t, db); got.ExternalID != britishEaglesMBID {
		t.Errorf("artist external_id = %q, want it untouched at %q — a corroborated Artist "+
			"is not re-asked, however wrong it may privately be",
			got.ExternalID, britishEaglesMBID)
	}
}

// "An Artist with no Albums at all is not re-asked." There is nothing to
// corroborate WITH, so the signature never forms: the re-ask would spend a request
// to be told the same thing by the same name search.
func TestARecheckDoesNotDoubtAnArtistThatHasNoAlbums(t *testing.T) {
	svc, _, stub := newEaglesLibrary(t, hellFreezesOverRGID, func(exec func(string, ...any)) {
		exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
		      VALUES ('artist', 'ar1', ?, 'matched')`, britishEaglesMBID)
		exec(`DELETE FROM titles WHERE album_id = 'al1'`)
		exec(`DELETE FROM albums WHERE id = 'al1'`)
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("an Artist with an empty discography was asked about again (calls: %v) — "+
			"'no Album matched' and 'no Album exists' are different facts, and only the "+
			"first one is evidence", stub.calls())
	}
}

// --- the mode boundary --------------------------------------------------------

// "ModeNew and ModeFull are unchanged: this fires only in ModeRecheck."
//
// ModeNew is what every scan runs. If the doubt reached it, each scheduled scan
// would re-ask every uncorroborated Artist forever — the standing unbounded cost
// ADR-0051 rejected when it refused to widen ModeNew.
func TestTheDoubtFiresOnlyInModeRecheck(t *testing.T) {
	// The doubted shape exactly as the recheck test builds it: matched Artist,
	// uncorroborated by its one settled Album.
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, eaglesDoubtFixture("unmatched"))

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("only-new pass: %v", err)
	}
	if len(stub.calls()) != 0 {
		t.Fatalf("an only-new pass made %d calls: %v — a scan is not a code change, and the "+
			"doubt is a recheck's business", len(stub.calls()), stub.calls())
	}
	if got := artistRow(t, db); got.ExternalID != britishEaglesMBID {
		t.Errorf("artist external_id = %q, want %q untouched by ModeNew",
			got.ExternalID, britishEaglesMBID)
	}

	// ModeFull is the control at the other end: it re-asks every parent regardless,
	// and always did, so the doubt neither adds to it nor is needed by it.
	svcFull, dbFull, stubFull := newEaglesLibrary(t, hellFreezesOverRGID, eaglesDoubtFixture("unmatched"))
	if _, err := svcFull.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("full pass: %v", err)
	}
	if got := artistRow(t, dbFull); got.ExternalID != americanEaglesMBID {
		t.Errorf("a full pass left the Artist at %q, want %q — ModeFull re-resolves every "+
			"parent and is unchanged by this issue (calls: %v)",
			got.ExternalID, americanEaglesMBID, stubFull.calls())
	}
}

// --- doubt is not a diagnosis -------------------------------------------------

// "The doubt never writes a status by itself — an Artist that re-fails records
// exactly what the failure recorded before."
//
// A 503 on the corroborating lookup is a transient failure, so the Artist is
// parked with a scheduled retry exactly as any other failing parent is (ADR-0048)
// — and, critically, its stored id is NOT cleared. Nothing is unmatched or blanked
// on the strength of a doubt; only the re-ask's own answer is written.
func TestADoubtedArtistThatFailsRecordsOnlyWhatTheFailureRecords(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, eaglesDoubtFixture("unmatched"))
	stub.status = map[string]int{"/release-group/" + hellFreezesOverRGID: http.StatusServiceUnavailable}

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	got := artistRow(t, db)
	if got.Status != "failed" {
		t.Fatalf("artist status = %q, want failed (calls: %v)", got.Status, stub.calls())
	}
	if got.RetryAt == "" {
		t.Errorf("the failed Artist was parked with no scheduled retry — a shed 503 is " +
			"in-flight work the server owns (ADR-0048), and the doubt changes nothing " +
			"about how a failure is recorded")
	}
	if got.ExternalID != britishEaglesMBID {
		t.Errorf("artist external_id = %q, want the old id %q still there — doubt is not a "+
			"diagnosis: nothing is cleared in advance of an answer",
			got.ExternalID, britishEaglesMBID)
	}
	// And it did not quietly resolve by name while MusicBrainz was shedding load,
	// which would write the wrong band back as `matched` (issue 13's judgement call).
	if n := stub.nameSearches(); n != 0 {
		t.Errorf("%d name searches after a transient corroboration failure (calls: %v)",
			n, stub.calls())
	}
}

// The other half: a doubted Artist whose re-ask genuinely finds nothing settles as
// 'unmatched', which is what any unmatched parent records. The library here is the
// "no identifiable albums" case ADR-0053 names — a mistyped tag and an album title
// MusicBrainz has never heard of — and it is re-asked once per recheck and re-fails,
// which the ADR accepts explicitly.
func TestADoubtedArtistThatFindsNothingSettlesAsUnmatched(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, "", func(exec func(string, ...any)) {
		eaglesDoubtFixture("unmatched")(exec)
		exec(`UPDATE artists SET name = 'The Eagels' WHERE id = 'ar1'`)
		exec(`UPDATE albums SET title = 'Untitled Rehearsal Bootleg' WHERE id = 'al1'`)
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	got := artistRow(t, db)
	if got.Status != "unmatched" {
		t.Fatalf("artist status = %q, want unmatched (calls: %v)", got.Status, stub.calls())
	}
	// The last resort still ran: corroboration had nothing to offer, so the Artist
	// asked exactly what it always asked before it gave up.
	if n := stub.nameSearches(); n != 1 {
		t.Errorf("%d name searches, want 1 (calls: %v) — an Artist whose albums are "+
			"unidentifiable falls through to the name search, unchanged", n, stub.calls())
	}
	if got.ExternalID != britishEaglesMBID {
		t.Errorf("artist external_id = %q — a non-answer does not blank the stored id, and "+
			"the doubt never wrote anything of its own", got.ExternalID)
	}
}

// --- the doubt is the Artist's alone ------------------------------------------

// Only an Artist is ever doubted. An Album's children are Tracks, and a matched
// Album whose Tracks are all unmatched is the ordinary state ADR-0050's tracklist
// tier exists to fix — re-asking the Album there would undo the very
// short-circuit that hands the tier its anchor (issue 07).
func TestAMatchedAlbumWithNoMatchedTrackIsNotDoubted(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1, status: "unmatched"}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if n := prov.count("rg"); n != 0 {
		t.Fatalf("the matched Album was looked up %d times (calls: %v) — doubt reads an "+
			"ARTIST's albums, and a Track's match state is not evidence about its Album",
			n, prov.history())
	}
	// And the Artist above it is corroborated by that matched Album, so it is silent
	// too — the same one fact, read the other way.
	if n := prov.count("artist"); n != 0 {
		t.Fatalf("the Artist was re-asked %d times (calls: %v) — one matched Album is all "+
			"the corroboration a matched Artist needs", n, prov.history())
	}
	if got, want := prov.history()[0], "tracklist:rg-she||1"; got != want {
		t.Fatalf("first call %q, want %q — the Album still short-circuits to its stored id",
			got, want)
	}
}

// --- the predicate ------------------------------------------------------------

// uncorroboratedMatch is one sentence and it is worth pinning directly: `matched`
// AND doubted, and no other combination. In particular a doubted parent that is
// PENDING or DISABLED is not selected by this rule (pending is admitted by every
// mode already, and disabled is not an answer), and an undoubted parent is never
// selected at all.
func TestUncorroboratedMatchIsMatchedAndDoubtedAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		status  string
		doubted bool
		want    bool
	}{
		{"matched", true, true},
		{"matched", false, false},
		{"pending", true, false},
		{"unmatched", true, false},
		{"failed", true, false},
		{"disabled", true, false},
	} {
		if got := uncorroboratedMatch(tc.status, tc.doubted); got != tc.want {
			t.Errorf("uncorroboratedMatch(%q, %v) = %v, want %v", tc.status, tc.doubted, got, tc.want)
		}
	}
}
