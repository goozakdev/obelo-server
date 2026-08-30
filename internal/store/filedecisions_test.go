package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// File decision storage (ADR-0044): the file-anchored Match override the Scanner
// replays at resolve time. These tests pin the three cardinalities the matcher must
// express, the three states a File can be in (plus the fourth case of no row at
// all), the (library, path) anchoring, and the replace-a-whole-arrangement write
// path Apply commits through.

const (
	batmanS3 = "/media/TV/Batman (1992)/Season 3/Batman - S03E61 - Holiday Knights.mkv"
	batmanS4 = "/media/TV/Batman (1992)/Season 3/Batman - S03E62 - Sins of the Father.mkv"
	sampleMK = "/media/TV/Batman (1992)/Season 3/sample.mkv"
)

func newTVLibraries(t *testing.T) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv', 'TV', 'tv')`)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv2', 'TV 2', 'tv')`)
	return db
}

func placed(path string, group, slot int) store.FileDecision {
	return store.FileDecision{Path: path, State: store.DecisionPlaced, GroupNumber: group, SlotNumber: slot}
}

// TestFileDecisionRoundTrip: a Placement written by Apply comes back to the Scanner
// with every field intact, and is anchored on (library, path) — the same path in
// another Library is a different decision and must not leak across.
func TestFileDecisionRoundTrip(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{placed(batmanS3, 4, 1)},
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}
	// Same path, different Library: must be invisible to libtv's read.
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv2",
		Decisions: []store.FileDecision{placed(batmanS3, 9, 9)},
	}); err != nil {
		t.Fatalf("replace file decisions in second library: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got) != 1 || len(got[batmanS3]) != 1 {
		t.Fatalf("decisions = %#v, want exactly one row for %q", got, batmanS3)
	}
	d := got[batmanS3][0]
	if d.ID == "" {
		t.Errorf("id not assigned")
	}
	if d.LibraryID != "libtv" {
		t.Errorf("library id = %q, want libtv", d.LibraryID)
	}
	if d.State != store.DecisionPlaced || got[batmanS3].State() != store.DecisionPlaced {
		t.Errorf("state = %q, want placed", d.State)
	}
	if d.GroupNumber != 4 || d.SlotNumber != 1 {
		t.Errorf("slot = s%02de%02d, want s04e01", d.GroupNumber, d.SlotNumber)
	}
	// Ordinal is 1-based and defaults to the ordinary single-File case.
	if d.Ordinal != 1 {
		t.Errorf("ordinal = %d, want 1", d.Ordinal)
	}
	if d.Orphaned {
		t.Errorf("a freshly asserted Placement must not be orphaned")
	}
	if d.CreatedAt == "" {
		t.Errorf("created_at not set")
	}

	other, err := db.FileDecisionsByLibrary("libtv2")
	if err != nil {
		t.Fatalf("file decisions by library (second): %v", err)
	}
	if len(other[batmanS3]) != 1 || other[batmanS3][0].GroupNumber != 9 {
		t.Fatalf("second library's decision = %#v, want its own s09e09 row", other[batmanS3])
	}
}

// TestPlacementOneFileTwoSlots is the 1→2 case: a combined file the filenames
// cannot express (`S01E01-02` written as anything else). Two rows share a path with
// different slot_numbers and both survive, so resolve can build two co-File sibling
// Titles from one file.
func TestPlacementOneFileTwoSlots(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{placed(batmanS3, 1, 2), placed(batmanS3, 1, 1)},
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	rows := got[batmanS3].Placements()
	if len(rows) != 2 {
		t.Fatalf("rows for one file across two Slots = %d, want 2 (%#v)", len(rows), rows)
	}
	// Ordered by Slot, so the Scanner can walk a file's Slots in library order
	// regardless of the order Apply happened to write them.
	if rows[0].SlotNumber != 1 || rows[1].SlotNumber != 2 {
		t.Fatalf("slots = %d,%d, want 1,2 in Slot order", rows[0].SlotNumber, rows[1].SlotNumber)
	}
	if rows[0].ID == rows[1].ID {
		t.Errorf("the two Slots share a row id; they must be distinct rows")
	}
}

// TestPlacementTwoFilesOneSlot is the 2→1 case: a double-length episode the
// provider lists once. Two rows share a Slot with distinct ordinals, which is what
// makes them a multi-part Edition with a defined play order.
func TestPlacementTwoFilesOneSlot(t *testing.T) {
	db := newTVLibraries(t)

	first, second := placed(batmanS3, 3, 65), placed(batmanS4, 3, 65)
	first.Ordinal, second.Ordinal = 1, 2
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{second, first}, // written out of order on purpose
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("paths on the shared Slot = %d, want 2 (%#v)", len(got), got)
	}
	a, b := got[batmanS3], got[batmanS4]
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want one row per file, got %#v / %#v", a, b)
	}
	if a[0].SlotNumber != 65 || b[0].SlotNumber != 65 {
		t.Fatalf("slots = %d,%d, want both 65", a[0].SlotNumber, b[0].SlotNumber)
	}
	if a[0].Ordinal != 1 || b[0].Ordinal != 2 {
		t.Fatalf("ordinals = %d,%d, want 1,2 (part order within the Slot)", a[0].Ordinal, b[0].Ordinal)
	}
}

// TestUnassignedIsNotTheSameAsNoRow is the reason the table has three states.
// Sparse storage spends "no row" on "derive the Placement from the filename", so an
// explicit unassign MUST be a stored row — otherwise the next scan re-places the
// File from the very filename the Admin was overruling.
func TestUnassignedIsNotTheSameAsNoRow(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{{Path: batmanS3, State: store.DecisionUnassigned}},
	}); err != nil {
		t.Fatalf("unassign: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if state := got[batmanS3].State(); state != store.DecisionUnassigned {
		t.Fatalf("state of an explicitly unassigned file = %q, want unassigned", state)
	}
	if len(got[batmanS3].Placements()) != 0 {
		t.Errorf("an unassigned file reported Slots: %#v", got[batmanS3].Placements())
	}
	// batmanS4 was never touched: no row, and that is a DIFFERENT answer — follow
	// the parse — not merely the same absence of a Placement.
	if state := got[batmanS4].State(); state != "" {
		t.Fatalf("state of an untouched file = %q, want \"\" (follow the parse)", state)
	}
	if len(got[batmanS4]) != 0 {
		t.Fatalf("untouched file has rows: %#v", got[batmanS4])
	}

	// The distinction has to survive the round trip through SQL, not just the map:
	// the unassigned row carries no Slot at all (NULL group/slot), so nothing later
	// mistakes a zero for season 0.
	var groupNulls int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM file_decisions
		  WHERE library_id = 'libtv' AND state = 'unassigned'
		    AND group_number IS NULL AND slot_number IS NULL`).Scan(&groupNulls); err != nil {
		t.Fatalf("count settled rows: %v", err)
	}
	if groupNulls != 1 {
		t.Fatalf("unassigned rows with no Slot = %d, want 1", groupNulls)
	}
}

// TestOneSettledDecisionPerPath: the partial unique index bites. SQLite treats
// NULLs as distinct in a UNIQUE constraint, so without it two 'unassigned' rows for
// one File would both be accepted and the File would have two contradictory
// decisions.
func TestOneSettledDecisionPerPath(t *testing.T) {
	db := newTVLibraries(t)

	err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			{Path: batmanS3, State: store.DecisionUnassigned},
			{Path: batmanS3, State: store.DecisionUnassigned},
		},
	})
	if err == nil {
		t.Fatalf("two unassigned rows for one File accepted, want a uniqueness error")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("error = %v, want the settled-decision uniqueness violation", err)
	}

	// An unassigned row and an ignored row are also two decisions about one File.
	err = db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			{Path: batmanS3, State: store.DecisionUnassigned},
			{Path: batmanS3, State: store.DecisionIgnored},
		},
	})
	if err == nil {
		t.Fatalf("unassigned + ignored for one File accepted, want a uniqueness error")
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a rejected set left %d paths behind, want none: %#v", len(got), got)
	}
}

// TestPlacedAndSettledCannotCoexist: a File placed on a Slot AND ignored is a
// contradiction with no defensible reading — resolve would have to decide whether
// to build a Title from a File it was also told to skip. The schema refuses it in
// both directions and in both orders.
func TestPlacedAndSettledCannotCoexist(t *testing.T) {
	db := newTVLibraries(t)

	cases := []struct {
		name      string
		decisions []store.FileDecision
	}{
		{"placed then ignored", []store.FileDecision{
			placed(batmanS3, 4, 1),
			{Path: batmanS3, State: store.DecisionIgnored},
		}},
		{"ignored then placed", []store.FileDecision{
			{Path: batmanS3, State: store.DecisionIgnored},
			placed(batmanS3, 4, 1),
		}},
		{"unassigned then placed", []store.FileDecision{
			{Path: batmanS3, State: store.DecisionUnassigned},
			placed(batmanS3, 4, 1),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := db.ReplaceFileDecisions(store.FileDecisionSet{
				LibraryID: "libtv", Decisions: tc.decisions,
			})
			if err == nil {
				t.Fatalf("a File both placed and settled was accepted")
			}
			if !strings.Contains(err.Error(), "either placed or settled") {
				t.Fatalf("error = %v, want the placed/settled exclusion", err)
			}
			got, err := db.FileDecisionsByLibrary("libtv")
			if err != nil {
				t.Fatalf("file decisions by library: %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("a rejected set left rows behind: %#v", got)
			}
		})
	}
}

// TestSettledDecisionCannotBeMovedOntoAPlacedPath guards the same invariant against
// an UPDATE rather than an INSERT — re-pointing a settled row at a path that is
// already placed would smuggle the contradiction in the back door.
func TestSettledDecisionCannotBeMovedOntoAPlacedPath(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			placed(batmanS3, 4, 1),
			{Path: sampleMK, State: store.DecisionIgnored},
		},
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}

	_, err := db.Exec(
		`UPDATE file_decisions SET path = ? WHERE library_id = 'libtv' AND state = 'ignored'`,
		batmanS3)
	if err == nil {
		t.Fatalf("moved an ignored row onto a placed path, want the placed/settled exclusion")
	}
	if !strings.Contains(err.Error(), "either placed or settled") {
		t.Fatalf("error = %v, want the placed/settled exclusion", err)
	}
}

// TestSettledDecisionCarriesNoSlot: the CHECK constraint. A settled state that
// carried half a Placement would be read as real by later code, so the schema
// refuses it — and the store never sends one, dropping whatever the caller left in
// the struct.
func TestSettledDecisionCarriesNoSlot(t *testing.T) {
	db := newTVLibraries(t)

	// The store nulls the Slot out rather than storing a stray group/slot.
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			{Path: batmanS3, State: store.DecisionIgnored, GroupNumber: 3, SlotNumber: 61},
		},
	}); err != nil {
		t.Fatalf("ignore with leftover Slot fields: %v", err)
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM file_decisions
		  WHERE state = 'ignored' AND group_number IS NULL AND slot_number IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("ignored rows with no Slot = %d, want 1", n)
	}

	// And the schema itself refuses a settled row with a Slot, and a placed row
	// without one, however it is written.
	if _, err := db.Exec(
		`INSERT INTO file_decisions (id, library_id, path, state, group_number, slot_number)
		 VALUES ('x1', 'libtv', ?, 'ignored', 3, 61)`, sampleMK); err == nil {
		t.Errorf("a settled row with a Slot was accepted")
	}
	if _, err := db.Exec(
		`INSERT INTO file_decisions (id, library_id, path, state)
		 VALUES ('x2', 'libtv', ?, 'placed')`, sampleMK); err == nil {
		t.Errorf("a placed row without a Slot was accepted")
	}
	if _, err := db.Exec(
		`INSERT INTO file_decisions (id, library_id, path, state)
		 VALUES ('x3', 'libtv', ?, 'maybe')`, sampleMK); err == nil {
		t.Errorf("an unknown state was accepted")
	}
}

// TestPlacementSameSlotSamePathRejected: (file, Slot) is the unit of decision, so
// asserting the same file on the same Slot twice is a caller bug, and the failed
// set writes NOTHING (Apply is atomic).
func TestPlacementSameSlotSamePathRejected(t *testing.T) {
	db := newTVLibraries(t)

	dup := placed(batmanS3, 3, 61)
	dup.Ordinal = 2
	err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{placed(batmanS3, 3, 61), dup},
	})
	if err == nil {
		t.Fatalf("duplicate (file, Slot) accepted, want a uniqueness error")
	}
	// Specifically the table's uniqueness, not some unrelated failure.
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: file_decisions.library_id") {
		t.Fatalf("error = %v, want the (library, path, group, slot) uniqueness violation", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a rejected set left %d paths behind, want none: %#v", len(got), got)
	}
}

// TestFileDecisionsAreReplacedInScope: Apply commits a whole arrangement, so a path
// in scope that keeps no row is returned to the parse (the only way to say "I take
// it back"), while a path outside the scope — another Show the Admin was not
// looking at — is untouched.
func TestFileDecisionsAreReplacedInScope(t *testing.T) {
	db := newTVLibraries(t)
	const otherShow = "/media/TV/Frasier (1993)/Season 1/Frasier - S01E01.mkv"

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			placed(batmanS3, 4, 1),
			placed(batmanS4, 4, 2),
			placed(otherShow, 1, 3),
		},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Second Apply of the Batman matcher only: batmanS3 moves, batmanS4 is returned
	// to the parse (in scope, no row).
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Paths:     []string{batmanS3, batmanS4},
		Decisions: []store.FileDecision{placed(batmanS3, 4, 5)},
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got[batmanS3]) != 1 || got[batmanS3][0].SlotNumber != 5 {
		t.Fatalf("re-placed file = %#v, want a single s04e05 row", got[batmanS3])
	}
	if len(got[batmanS4]) != 0 {
		t.Fatalf("file returned to the parse kept %d rows, want 0: %#v", len(got[batmanS4]), got[batmanS4])
	}
	if len(got[otherShow]) != 1 {
		t.Fatalf("a path outside the replace scope was disturbed: %#v", got[otherShow])
	}
}

// TestFileDecisionOutsideScopeRejected: a row written outside its own replace scope
// could never be cleared by a later Apply of the same screen, so it is refused
// rather than left behind as an unreachable correction.
func TestFileDecisionOutsideScopeRejected(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Paths:     []string{batmanS3},
		Decisions: []store.FileDecision{placed(batmanS4, 4, 2)},
	}); err == nil {
		t.Fatalf("placement outside the replace scope accepted, want an error")
	}
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Paths:     []string{batmanS3},
		Decisions: []store.FileDecision{{Path: batmanS4, State: store.DecisionIgnored}},
	}); err == nil {
		t.Fatalf("ignore outside the replace scope accepted, want an error")
	}
}

// TestIgnoredRoundTrip: Ignoring is a stored decision — NOT titles.hidden, which is
// the derived all-Files-Missing cache reset on every upsert. It is scoped to its
// Library and is cleared by a later Apply that no longer ignores the file.
func TestIgnoredRoundTrip(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{{Path: sampleMK, State: store.DecisionIgnored}},
	}); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv2",
		Decisions: []store.FileDecision{{Path: batmanS3, State: store.DecisionIgnored}},
	}); err != nil {
		t.Fatalf("ignore in second library: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if got[sampleMK].State() != store.DecisionIgnored {
		t.Fatalf("state = %q, want ignored", got[sampleMK].State())
	}
	if got[batmanS3].State() != "" {
		t.Errorf("another Library's ignore leaked into this one")
	}

	// Un-ignoring: the same scope applied with no decision clears it, and the File
	// goes back to following its filename.
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv", Paths: []string{sampleMK},
	}); err != nil {
		t.Fatalf("un-ignore: %v", err)
	}
	got, err = db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions after un-ignore: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("decisions after un-ignore = %#v, want empty", got)
	}
}

// TestStateChangesInOneCommit: a File can move between states in a single Apply —
// ignored to placed and back — without ever holding two contradictory decisions.
func TestStateChangesInOneCommit(t *testing.T) {
	db := newTVLibraries(t)

	steps := []struct {
		name string
		d    store.FileDecision
	}{
		{"ignored", store.FileDecision{Path: batmanS3, State: store.DecisionIgnored}},
		{"placed", placed(batmanS3, 4, 1)},
		{"unassigned", store.FileDecision{Path: batmanS3, State: store.DecisionUnassigned}},
		{"ignored again", store.FileDecision{Path: batmanS3, State: store.DecisionIgnored}},
	}
	for _, s := range steps {
		if err := db.ReplaceFileDecisions(store.FileDecisionSet{
			LibraryID: "libtv",
			Paths:     []string{batmanS3},
			Decisions: []store.FileDecision{s.d},
		}); err != nil {
			t.Fatalf("apply %s: %v", s.name, err)
		}
		got, err := db.FileDecisionsByLibrary("libtv")
		if err != nil {
			t.Fatalf("file decisions by library: %v", err)
		}
		if len(got[batmanS3]) != 1 {
			t.Fatalf("after %s the File has %d rows, want exactly 1: %#v",
				s.name, len(got[batmanS3]), got[batmanS3])
		}
		if got[batmanS3].State() != s.d.State {
			t.Fatalf("after %s the state is %q, want %q", s.name, got[batmanS3].State(), s.d.State)
		}
	}
}

// TestSetPlacementOrphaned: an anchor that vanishes from disk must be SURFACED, not
// dropped — a correction pointing at nothing is broken rather than done. The flag
// is reversible, because the file can come back (an unmounted drive, a rename
// undone). Settled decisions are deliberately never orphaned: there is nothing to
// fix, and re-surfacing one would un-settle an ignore that correctly re-applies if
// the File returns.
func TestSetPlacementOrphaned(t *testing.T) {
	db := newTVLibraries(t)

	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			placed(batmanS3, 4, 1),
			{Path: sampleMK, State: store.DecisionIgnored},
		},
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}
	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	id, ignoredID := got[batmanS3][0].ID, got[sampleMK][0].ID

	if err := db.SetPlacementOrphaned(id, true); err != nil {
		t.Fatalf("set orphaned: %v", err)
	}
	if err := db.SetPlacementOrphaned(ignoredID, true); err != nil {
		t.Fatalf("set orphaned on a settled row: %v", err)
	}
	got, err = db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions after orphaning: %v", err)
	}
	if len(got[batmanS3]) != 1 {
		t.Fatalf("an orphaned Placement was dropped from the read; it must be surfaced")
	}
	if !got[batmanS3][0].Orphaned {
		t.Errorf("orphaned = false, want true")
	}
	if got[sampleMK][0].Orphaned {
		t.Errorf("a settled decision was orphaned; only Placements orphan")
	}

	if err := db.SetPlacementOrphaned(id, false); err != nil {
		t.Fatalf("clear orphaned: %v", err)
	}
	got, _ = db.FileDecisionsByLibrary("libtv")
	if got[batmanS3][0].Orphaned {
		t.Errorf("orphaned still true after the file came back")
	}
}

// TestFileDecisionsByLibraryIsOneRead: the Scanner consults decisions per walked
// file and must learn any file's state — all three, plus "nothing was said" — from
// a single read, rather than one query per state (which invites the three to be
// consulted in different places and drift).
func TestFileDecisionsByLibraryIsOneRead(t *testing.T) {
	db := newTVLibraries(t)
	const third = "/media/TV/Batman (1992)/Season 3/Batman - S03E63 - Never Fear.mkv"
	const untouched = "/media/TV/Batman (1992)/Season 3/Batman - S03E64 - Joker's Millions.mkv"

	partA, partB := placed(batmanS4, 4, 2), placed(third, 4, 2)
	partA.Ordinal, partB.Ordinal = 1, 2
	if err := db.ReplaceFileDecisions(store.FileDecisionSet{
		LibraryID: "libtv",
		Decisions: []store.FileDecision{
			placed(batmanS3, 4, 1),
			partA, partB,
			placed(third, 4, 3),
			{Path: sampleMK, State: store.DecisionIgnored},
			{Path: "/media/TV/Batman (1992)/extra.mkv", State: store.DecisionUnassigned},
		},
	}); err != nil {
		t.Fatalf("replace file decisions: %v", err)
	}

	got, err := db.FileDecisionsByLibrary("libtv")
	if err != nil {
		t.Fatalf("file decisions by library: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("paths = %d, want 5 (%#v)", len(got), got)
	}
	total := 0
	for _, rows := range got {
		total += len(rows)
	}
	if total != 6 {
		t.Fatalf("rows = %d, want 6", total)
	}
	// Every state answered by the one map, including the file nobody touched.
	for path, want := range map[string]string{
		batmanS3:                            store.DecisionPlaced,
		third:                               store.DecisionPlaced,
		sampleMK:                            store.DecisionIgnored,
		"/media/TV/Batman (1992)/extra.mkv": store.DecisionUnassigned,
		untouched:                           "",
	} {
		if state := got[path].State(); state != want {
			t.Errorf("state of %q = %q, want %q", path, state, want)
		}
	}
	if len(got[third]) != 2 || got[third][0].SlotNumber != 2 || got[third][1].SlotNumber != 3 {
		t.Fatalf("the multi-Slot file came back as %#v, want its Slots 2 then 3", got[third])
	}

	// Empty library: an empty map, not an error and not nil-dereference bait.
	empty, err := db.FileDecisionsByLibrary("libtv2")
	if err != nil {
		t.Fatalf("decisions for an untouched library: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("untouched library = %#v, want empty", empty)
	}
}

// TestEpisodePinCommentDoesNotContradictContext guards the 0047 comment against
// the claim ADR-0044 reverses. The pin used to be described as keeping a file's
// "place in the library" while ALSO being the fix for a misnumbered season — which
// cannot both be true now that Placement moves a file and the pin only repoints
// what decorates the Slot it sits on. The comment is load-bearing documentation
// for a schema that cannot be re-migrated, so it is worth a test.
func TestEpisodePinCommentDoesNotContradictContext(t *testing.T) {
	body, err := os.ReadFile("migrations/0047_episode_enrichment_pin.sql")
	if err != nil {
		t.Fatalf("read 0047: %v", err)
	}
	text := string(body)

	for _, banned := range []string{
		"keeps its parsed numbers",
		"the file keeps its\n-- place in the library",
	} {
		if strings.Contains(text, banned) {
			t.Errorf("0047's comment still claims %q, which ADR-0044 reverses", banned)
		}
	}
	// It must instead point at the decision that DOES move a file.
	if !strings.Contains(text, "ADR-0044") || !strings.Contains(text, "Placement") {
		t.Errorf("0047's comment does not say that Placement, not the pin, moves a file")
	}
	// And the SQL itself must be untouched: 0047 may already have run on a
	// developer's database, so only the comment is safe to edit.
	if !strings.Contains(text, "ALTER TABLE titles ADD COLUMN enrichment_season INTEGER;") ||
		!strings.Contains(text, "ALTER TABLE titles ADD COLUMN enrichment_episode INTEGER;") {
		t.Errorf("0047's SQL changed; an already-applied migration's statements must not be edited")
	}
}
