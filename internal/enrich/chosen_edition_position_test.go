package enrich

import (
	"context"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0052, the MATCHING half (issue 11): a chosen edition licenses position-alone
// mapping, and nothing else does.
//
// Every test here comes in a PAIR, and the pairs are in one file on purpose. The
// licensed half is what the operator gets after naming their edition; the
// unlicensed half is ADR-0050's guarantee — the identical album, the identical
// tracklist, and it still declines — which is the half a later refactor is likely
// to erode without noticing, because nothing about it looks like a feature.
//
// The fixture is real. Andrea Bocelli's *Viaggio Italiano* out of the operator's
// library, tags verbatim: composer prefixes the release does not carry, and
// BACKTICKS where apostrophes belong. Its numbering is 1..16, sequential and
// complete — as it was on all 54 albums measured in that state. The positions were
// right every time and the title comparison vetoed them.

// viaggioLocal is the album as it sits on disk. Positions 1 and 9 are the two that
// matched before ADR-0052 (their titles happen to agree once folded); 2, 4 and 11
// are three of the ten that read `not-in-tracklist`.
func viaggioLocal() []store.Title {
	return []store.Title{
		lt("t1", 1, 1, "Puccini: Turandot - Nessun Dorma"),
		lt("t2", 1, 2, "Cilea: L'Arlesiana - Lamento Di Federico"),
		lt("t4", 1, 4, "Verdi: Rigoletto - La Donna E Mobile"),
		lt("t9", 1, 9, "Core N`Grato"),
		lt("t11", 1, 11, "It`E Vurria Vasa"),
	}
}

// viaggioTracklist is the release the operator pinned, spelled the way MusicBrainz
// spells it. No amount of normalization brings a composer prefix or "I' te vurria
// vasà" to meet "It`E Vurria Vasa" — that is the point of the case. Only the
// POSITIONS agree, and only a human's assertion makes them evidence.
func viaggioTracklist() []TrackCandidate {
	return []TrackCandidate{
		tc(1, 1, "Puccini: Turandot - Nessun dorma", "rec-1"),
		tc(1, 2, "L'arlesiana: È la solita storia del pastore (Lamento di Federico)", "rec-2"),
		tc(1, 4, "Rigoletto: La donna è mobile", "rec-4"),
		tc(1, 9, "Core 'ngrato", "rec-9"),
		tc(1, 11, "I' te vurria vasà", "rec-11"),
	}
}

// viaggioRecordings is what a /recording/<mbid> lookup answers for each entry, so a
// real pass can turn a mapped id into a record.
func viaggioRecordings() map[string]string {
	recs := map[string]string{}
	for _, e := range viaggioTracklist() {
		recs[e.ExternalID] = e.Title
	}
	return recs
}

// --- the pair, at the rule ----------------------------------------------------

// THE HEADLINE. With the edition chosen, the three tracks whose titles disagree are
// pinned to the entries at their own numbers. Rule 4, and nothing else, does this.
func TestAChosenEditionPinsViaggioItalianoByPosition(t *testing.T) {
	wantPairs(t, mapTracks(viaggioLocal(), viaggioTracklist(), licensed), map[string]string{
		// Rules 1: these two agreed on title as well and matched before ADR-0052.
		"t1": "rec-1",
		"t9": "rec-9",
		// Rule 4: the licence. A disagreeing title stops being a veto and becomes
		// information.
		"t2":  "rec-2",
		"t4":  "rec-4",
		"t11": "rec-11",
	})
}

// THE GUARANTEE, and it is ADR-0050's. The same album, the same tracklist, nobody's
// choice: the three still decline and fall through to search. If this test ever
// starts passing with rule 4's answers, position-alone mapping has leaked out of
// the licence and every hand-numbered rip in every library is being decorated from
// a stranger's numbering again.
func TestWithoutAChosenEditionViaggioItalianoStillDeclines(t *testing.T) {
	wantPairs(t, mapTracks(viaggioLocal(), viaggioTracklist(), unlicensed), map[string]string{
		"t1": "rec-1",
		"t9": "rec-9",
	})
}

// --- what the licence does NOT change -----------------------------------------

// Rules 1–3 run first and stay first: a track whose title uniquely names a
// DIFFERENT position goes to that position, not to its own number. The licence can
// only ever add matches to what the title rules already decided, so both answers
// here are identical — which is what "a chosen edition never makes matching worse"
// means mechanically.
func TestTheTitleRulesOutrankPositionUnderTheLicence(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Alpha"),
		lt("t2", 1, 2, "Beta"),
		lt("t3", 1, 3, "Gamma"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Beta", "rec-beta"),
		tc(1, 2, "Delta", "rec-delta"),
		tc(1, 3, "Gamma", "rec-gamma"),
		// A fourth position with no local track, so the leftover pair (rule 3) cannot
		// fire and mask what rule 4 would otherwise be doing.
		tc(1, 4, "Epsilon", "rec-epsilon"),
	}
	// t2 is the assertion: its title names position 1 and its number says 2. Rule 2
	// wins, under the licence exactly as without it. t1 declines both ways — the
	// entry at its own position was already claimed by a better rule.
	want := map[string]string{"t3": "rec-gamma", "t2": "rec-beta"}
	wantPairs(t, mapTracks(local, list, licensed), want)
	wantPairs(t, mapTracks(local, list, unlicensed), want)
}

// A position the pinned release simply has no entry for still declines under the
// licence. That is what keeps `not-in-tracklist` meaningful once an edition is
// chosen: it narrows from "we could not spell your title" to "the release you named
// genuinely has no track there", which is a real disagreement about the album.
func TestAPositionWithNoEntryDeclinesUnderTheLicence(t *testing.T) {
	local := []store.Title{
		lt("t1", 1, 1, "Alpha"),
		lt("t2", 1, 2, "Beta"),
		lt("t9", 1, 9, "Hidden Track"),
	}
	list := []TrackCandidate{
		tc(1, 1, "Un altro nome", "rec-1"),
		tc(1, 2, "Qualcos'altro", "rec-2"),
	}
	wantPairs(t, mapTracks(local, list, licensed), map[string]string{"t1": "rec-1", "t2": "rec-2"})
}

// Two local Tracks claiming one position decline TOGETHER, the same ambiguity guard
// rules 1 and 2 apply. This is not a confidence test — it is what keeps mapTracks
// order-independent (and therefore safe to memoize per album): resolving it by
// whichever row the database returned first would make the answer depend on an
// ORDER BY.
func TestTwoLocalTracksAtOnePositionDeclineUnderTheLicence(t *testing.T) {
	local := []store.Title{
		lt("t5a", 1, 5, "Alpha"),
		lt("t5b", 1, 5, "Beta"),
		lt("t6", 1, 6, "Gamma"),
	}
	list := []TrackCandidate{
		tc(1, 5, "Uno sconosciuto", "rec-5"),
		tc(1, 6, "Un altro", "rec-6"),
	}
	wantPairs(t, mapTracks(local, list, licensed), map[string]string{"t6": "rec-6"})
}

// --- the pair, through a real pass --------------------------------------------

// viaggioProvider is the editionProvider serving the Viaggio fixture: the same
// tracklist under the same release id, so the ONLY difference between the two pass
// tests below is whether a human pinned it.
func viaggioProvider() *editionProvider {
	return &editionProvider{
		albumTierProvider: &albumTierProvider{recordings: viaggioRecordings()},
		parentOf:          map[string]string{viaggioRelease: viaggioGroup},
		byRelease:         map[string][]TrackCandidate{viaggioRelease: viaggioTracklist()},
	}
}

// viaggioSeed is the album in the store. Its ROW is the shared fixture's ("She", by
// Harry Connick Jr.) because newAlbumFixture owns those columns; everything this
// test is about lives on the tracks. The release the FILES name is the same release
// the operator chose, which is what makes the pair below differ in exactly one
// fact: who asserted it.
func viaggioSeed() seedAlbum {
	return seedAlbum{
		entityRecord: viaggioGroup,
		releaseTag:   viaggioRelease,
		tracks: []seedTrack{
			{id: "t1", title: "Puccini: Turandot - Nessun Dorma", num: 1},
			{id: "t2", title: "Cilea: L'Arlesiana - Lamento Di Federico", num: 2},
			{id: "t4", title: "Verdi: Rigoletto - La Donna E Mobile", num: 4},
			{id: "t9", title: "Core N`Grato", num: 9},
			{id: "t11", title: "It`E Vurria Vasa", num: 11},
		},
	}
}

// The operator's album, through the pass, with their edition pinned: ten minutes on
// musicbrainz.org turns into every track resolved.
func TestThePassPinsViaggioItalianoFromTheChosenEdition(t *testing.T) {
	prov := viaggioProvider()
	svc, db := newAlbumFixture(t, prov, viaggioSeed())
	pinEdition(t, db, viaggioRelease)

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}

	// The licence travelled with the request: chosen=true is the whole difference
	// between this test and the next one.
	if n := prov.count("tracklist:" + viaggioGroup + "|" + viaggioRelease + "|chosen=true"); n != 1 {
		t.Fatalf("made %d chosen-edition tracklist reads, want 1 (calls: %v)", n, prov.history())
	}
	for id, rec := range map[string]string{
		"t1": "rec-1", "t2": "rec-2", "t4": "rec-4", "t9": "rec-9", "t11": "rec-11",
	} {
		if got := trackRow(t, db, id).MusicbrainzID; got != rec {
			t.Errorf("track %s pinned to %q, want %q — the operator named this edition and its "+
				"numbering is their assertion, not our assumption (calls: %v)",
				id, got, rec, prov.history())
		}
	}

	// A rule-4 track is DERIVED, not chosen. It was derived from the ALBUM's record;
	// nobody made a decision about this track, a later pass may revise it, and an
	// Admin's own pin on it still wins (ADR-0052, issue 04's rule unchanged).
	got := trackRow(t, db, "t2")
	if got.EnrichmentIDOrigin != store.OriginDerived {
		t.Errorf("origin = %q, want OriginDerived — a chosen EDITION is not a choice about "+
			"this track", got.EnrichmentIDOrigin)
	}
	if got.EnrichmentStatus != "matched" {
		t.Errorf("status = %q, want matched", got.EnrichmentStatus)
	}
	if got.EnrichmentReason != store.EnrichmentReasonNone {
		t.Errorf("reason = %q, want it cleared — a reason that outlives the failure it "+
			"described is worse than none (ADR-0050)", got.EnrichmentReason)
	}
}

// THE SAME ALBUM AND THE SAME TRACKLIST, with nobody's choice behind it: three
// tracks still decline, and still say `not-in-tracklist`. The pass reads the
// release the FILES name here, which is the very release the other test pins — so
// the entries are byte-identical and the only thing that changed is the assertion
// behind them. That is ADR-0050's guarantee, and it is the half of this feature
// most likely to be refactored away by accident.
func TestThePassStillDeclinesViaggioItalianoWithNoChosenEdition(t *testing.T) {
	prov := viaggioProvider()
	svc, db := newAlbumFixture(t, prov, viaggioSeed())

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}

	for _, c := range prov.history() {
		if strings.Contains(c, "chosen=true") {
			t.Fatalf("asked for an edition nobody chose (%q) (calls: %v)", c, prov.history())
		}
	}
	// The two the title comparison could still carry.
	for id, rec := range map[string]string{"t1": "rec-1", "t9": "rec-9"} {
		if got := trackRow(t, db, id).MusicbrainzID; got != rec {
			t.Errorf("track %s pinned to %q, want %q (calls: %v)", id, got, rec, prov.history())
		}
	}
	// The three the title comparison vetoed. Unlicensed, a bare position is our
	// assumption about a stranger's numbering, and on a hand-numbered rip acting on
	// it is a confident wrong answer (ADR-0049/0050).
	for _, id := range []string{"t2", "t4", "t11"} {
		row := trackRow(t, db, id)
		if row.MusicbrainzID != "" {
			t.Errorf("track %s pinned to %q with no chosen edition — position alone is not "+
				"evidence until a human asserts the edition (calls: %v)",
				id, row.MusicbrainzID, prov.history())
		}
		if row.EnrichmentStatus != "unmatched" {
			t.Errorf("track %s status = %q, want unmatched", id, row.EnrichmentStatus)
		}
		if row.EnrichmentReason != store.EnrichmentReasonNotInTracklist {
			t.Errorf("track %s reason = %q, want %q — the Album read a tracklist and this "+
				"track was declined by the match rule",
				id, row.EnrichmentReason, store.EnrichmentReasonNotInTracklist)
		}
	}
}

// --- the pair, through the cascade --------------------------------------------

// The cascade path must reach the same answer as the pass from the same fact: one
// rule, both callers, licence included (ADR-0050's "one rule, both callers",
// extended by ADR-0052 to the licence itself).
func TestTheCascadePinsViaggioItalianoFromTheChosenEdition(t *testing.T) {
	prov := viaggioProvider()
	prov.searchAlbums = []Candidate{{
		ExternalID: viaggioGroup, Title: "She", Kind: "album",
		Tracklist: viaggioTracklist(),
	}}
	svc, db := newAlbumFixture(t, prov, viaggioSeed())
	pinEdition(t, db, viaggioRelease)

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", viaggioGroup)
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 5 || sum.Attention != 0 {
		t.Fatalf("cascade summary %+v, want 5 updated / 0 attention (calls: %v)", sum, prov.history())
	}
	for id, rec := range map[string]string{
		"t1": "rec-1", "t2": "rec-2", "t4": "rec-4", "t9": "rec-9", "t11": "rec-11",
	} {
		if got := trackRow(t, db, id).MusicbrainzID; got != rec {
			t.Errorf("cascade pinned track %s to %q, want %q — the cascade and the pass must "+
				"not disagree about which tracks a chosen edition resolves (calls: %v)",
				id, got, rec, prov.history())
		}
	}
}

// The cascade's half of ADR-0050's guarantee: the same album, the same entries
// (here the candidate's own preview tracklist), no chosen edition — three tracks go
// to the attention list rather than taking a recording nobody verified.
func TestTheCascadeStillDeclinesViaggioItalianoWithNoChosenEdition(t *testing.T) {
	prov := viaggioProvider()
	prov.searchAlbums = []Candidate{{
		ExternalID: viaggioGroup, Title: "She", Kind: "album",
		Tracklist: viaggioTracklist(),
	}}
	svc, db := newAlbumFixture(t, prov, viaggioSeed())

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", viaggioGroup)
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 2 || sum.Attention != 3 {
		t.Fatalf("cascade summary %+v, want 2 updated / 3 attention (calls: %v)", sum, prov.history())
	}
	for _, id := range []string{"t2", "t4", "t11"} {
		row := trackRow(t, db, id)
		if row.MusicbrainzID != "" {
			t.Errorf("cascade pinned track %s to %q with no chosen edition (calls: %v)",
				id, row.MusicbrainzID, prov.history())
		}
		if row.EnrichmentReason != store.EnrichmentReasonNotInTracklist {
			t.Errorf("track %s reason = %q, want %q", id, row.EnrichmentReason,
				store.EnrichmentReasonNotInTracklist)
		}
	}
}
