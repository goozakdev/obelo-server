package enrich

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The Cascade's skip rule: WHICH children a parent's "apply to children" must not
// touch (CONTEXT.md **Cascade** — "a child's own Enrichment override or Locked
// field wins").
//
// The rule turns on one question — did the ADMIN choose this child's record, ON
// THIS CHILD? — and it took two goes to record the answer.
//
// Until ADR-0045 nothing recorded it at all, so the code guessed from "the child's
// record id is non-empty". The guess is wrong in one direction and only that one:
// a child acquires a record id nobody chose routinely (an enrichment pass persists
// the record it resolved so the artwork picker has an anchor; a split's co-File
// sibling inherits the survivor's series; clearing an Episode pin writes the
// Show's own series back), and every one of those made the child permanently
// immune to a correction the Admin asked for, silently
// (.scratch/enrichment-override-durability/issues/03).
//
// The flag that replaced the guess recorded THAT the record was chosen, not BY
// WHOM — and a Cascade writes its children through the same Admin path, so the
// children of a first Cascade read as having chosen for themselves and a second
// Cascade from the same parent skipped every one of them (issues/04). ADR-0046
// makes the recorded value three-valued; this rule asks OwnChoice().
//
// These pin the rule to that value.

// lockedFieldsStore answers the ONE store call the skip rule makes. Embedding the
// interface keeps the fake honest: any other call the rule grew would panic here
// rather than pass quietly against a stub.
type lockedFieldsStore struct {
	Store
	locks map[string]map[string]bool
}

func (s lockedFieldsStore) LockedFields(titleID string) (map[string]bool, error) {
	return s.locks[titleID], nil
}

func skipService(locks map[string]map[string]bool) *Service {
	return &Service{store: lockedFieldsStore{locks: locks}}
}

func mustSkip(t *testing.T, svc *Service, child store.Title, want bool, why string) {
	t.Helper()
	got, err := svc.childHasOwnOverride(child)
	if err != nil {
		t.Fatalf("childHasOwnOverride(%s): %v", child.ID, err)
	}
	if got != want {
		t.Errorf("childHasOwnOverride(%s) = %v, want %v — %s", child.ID, got, want, why)
	}
}

// TestAnInheritedRecordIsNotAChoice is the defect itself: an Episode carrying a
// record id it never chose — the id an enrichment pass resolved, the Show's own
// series written back when its pin was cleared, the series a split's sibling
// inherited — takes its parent's Cascade like any other child.
func TestAnInheritedRecordIsNotAChoice(t *testing.T) {
	svc := skipService(nil)

	mustSkip(t, svc, store.Title{ID: "ep-auto", Kind: "episode", RecordIDs: map[string]string{"tmdb": "1438"}, RecordNamespace: "tmdb"}, false,
		"a record id with no lock is what a pass resolved or a Clear wrote back, not an Admin's pick")
	mustSkip(t, svc, store.Title{ID: "tr-auto", Kind: "track", RecordIDs: map[string]string{"musicbrainz": "rec-auto"}, RecordNamespace: "musicbrainz"}, false,
		"a Track's recording id is filled by every ordinary MusicBrainz lookup")
}

// TestAChosenRecordStillWins: the promise the skip rule exists for. A child the
// Admin repointed by hand outranks the parent's correction, exactly as before.
func TestAChosenRecordStillWins(t *testing.T) {
	svc := skipService(nil)

	mustSkip(t, svc, store.Title{
		ID: "ep-fixed", Kind: "episode", TMDBID: "60625", EnrichmentIDOrigin: store.OriginChosen,
	}, true, "an Episode whose record the Admin chose must survive a parent Cascade")
	mustSkip(t, svc, store.Title{
		ID: "tr-fixed", Kind: "track", MusicbrainzID: "rec-manual", EnrichmentIDOrigin: store.OriginChosen,
	}, true, "a Track's own Fix info must survive its Album's Cascade")
}

// TestACascadedRecordIsNotTheChildsOwn is issue 04: the record a parent's PREVIOUS
// Cascade wrote is the parent's choice held by the child, so the parent's next
// Cascade re-applies over it (ADR-0046). Without this a second "apply to children"
// from the same parent reports Updated: 0 on exactly the children the first one
// fixed — no error, no count, nothing surfaced.
//
// The distinction is invisible to Locked(): both rows below are durable overrides
// that no enrichment pass will re-match. Only OwnChoice() separates them, which is
// why the stored value had to stop being a boolean.
func TestACascadedRecordIsNotTheChildsOwn(t *testing.T) {
	svc := skipService(nil)

	mustSkip(t, svc, store.Title{
		ID: "ep-cascaded", Kind: "episode", TMDBID: "555", EnrichmentIDOrigin: store.OriginCascaded,
	}, false, "an Episode its Show's last Cascade wrote must take that Show's next one")
	mustSkip(t, svc, store.Title{
		ID: "tr-cascaded", Kind: "track", MusicbrainzID: "rec-cascaded", EnrichmentIDOrigin: store.OriginCascaded,
	}, false, "a Track its Album's last Cascade wrote must take that Album's next one")

	// Both provenances are still durable — the property ADR-0019 wanted from the
	// flag — and the rule is the only place they differ.
	if !store.OriginCascaded.Locked() {
		t.Errorf("a cascaded record must stay durable against an enrichment pass")
	}
}

// TestALockedFieldStillSkips: the rule's second half is untouched — a hand-edited
// field protects its child whether or not any record was ever chosen.
func TestALockedFieldStillSkips(t *testing.T) {
	svc := skipService(map[string]map[string]bool{
		"tr-edited": {"overview": true},
	})

	mustSkip(t, svc, store.Title{ID: "tr-edited", Kind: "track"}, true,
		"a Locked field wins over a Cascade on its own")
	mustSkip(t, svc, store.Title{ID: "tr-plain", Kind: "track"}, false,
		"a child with neither a chosen record nor a locked field takes the Cascade")
}

// TestOriginChosenIsSkippedUntilTheOriginIsCleared: the rule reads OriginChosen at
// face value regardless of how the row got there — it does not (and cannot)
// distinguish an Admin's pick from any other route to the same value — and only a
// Fix info, a Wrong item, or a cleared pin (which resets the origin to
// OriginDerived) rejoins the row to its parent's Cascade.
func TestOriginChosenIsSkippedUntilTheOriginIsCleared(t *testing.T) {
	svc := skipService(nil)

	row := store.Title{
		ID: "ep-chosen", Kind: "episode", TMDBID: "1438", EnrichmentIDOrigin: store.OriginChosen,
	}
	mustSkip(t, svc, row, true, "OriginChosen always wins, whatever set it")

	// Clearing the origin is honest again and rejoins the Cascade.
	row.EnrichmentIDOrigin = store.OriginDerived
	mustSkip(t, svc, row, false,
		"once the origin is settled honestly the row takes its parent's Cascade")
}
