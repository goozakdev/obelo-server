package store

import (
	"database/sql"
	"fmt"
)

// filedecisions.go stores what the Admin has SAID about a File inside an
// already-identified work: which Slot(s) it fills (its Placement), that they
// deliberately took it off its Slot, or that it should be ignored entirely.
//
// Every one of those is a correction the Scanner REPLAYS at resolve time
// (ADR-0044), not a decoration applied to live rows — a Placement decides how many
// Title rows exist, and only the resolve step creates, merges and splits Title
// rows, so anything written directly onto a Title would be undone by the next
// scheduled scan (writeTitleRow overwrites season_id, season_number and
// episode_number from the parse on every upsert).
//
// This file is the storage sibling of incremental.go's match_overrides: same
// family of thing (an Admin correction that outranks the parse and persists across
// rescans, surfaced rather than dropped when its anchor disappears), with a finer
// anchor — a file rather than a folder — because the work is already identified
// and it is the arrangement inside it that is wrong.
//
// Everything here is kind-neutral. A Slot is one numbered position within a
// browsable parent, so TV supplies season+episode and Music will supply disc+track
// through the same columns and the same reads; see 0048_file_decisions.sql.

// The three states of a File decision. A File with NO row is a fourth,
// deliberately distinct case: nothing was said, so the Placement is derived from
// the filename. That is why DecisionUnassigned has to exist at all — sparse storage
// spends "no row" on "follow the parse", so taking a File off its Slot cannot be
// recorded by deleting its row, or the next scan re-places it from the very
// filename the Admin was overruling.
const (
	// DecisionPlaced: the Admin said which Slot(s) this File fills. One row per
	// Slot, so a File can be placed on several and a Slot can hold several Files.
	DecisionPlaced = "placed"
	// DecisionUnassigned: taken off its Slot, UNDECIDED. Part of no Title and
	// invisible in browse, but still listed in the matcher, and it keeps its Show
	// in the Needs-Fixing queue until it is placed or ignored.
	DecisionUnassigned = "unassigned"
	// DecisionIgnored: not a Slot's File at all — a sample, a stray rip. SETTLED:
	// skipped by every future scan and absent from the queue. Never destructive;
	// the file stays exactly where it is on disk.
	DecisionIgnored = "ignored"
)

// FileDecision is one stored row: the Admin's decision about one File, and — when
// that decision is DecisionPlaced — one Slot it fills. A settled decision is
// exactly one row; a placed one is a row per Slot.
//
// Rows are SPARSE (ADR-0027's precedent): one exists only where the decision
// differs from what the parse would produce, so a filename later corrected on disk
// takes effect rather than being overruled by a stale record.
//
// The three cardinalities the matcher can express all fall out of the
// (path, group, slot) key:
//
//   - one File on one Slot — a single placed row;
//   - one File across several Slots — several placed rows sharing a path, which
//     resolve to co-File sibling Titles;
//   - several Files on one Slot — several placed rows sharing (group, slot) with
//     distinct Ordinals, which resolve to one Title with a multi-part Edition.
type FileDecision struct {
	ID        string
	LibraryID string
	// Path is the absolute on-disk file this decision anchors to.
	Path string
	// State is one of DecisionPlaced / DecisionUnassigned / DecisionIgnored.
	State string
	// GroupNumber is the assigned Slot's outer number: a season for TV, a disc
	// for Music. Always the LOCAL library's numbering, never a provider's. Zero
	// and meaningless unless State is DecisionPlaced — season 0 is a real value
	// (Specials), so read State, never GroupNumber, to know if there is a Slot.
	GroupNumber int
	// SlotNumber is the assigned Slot's inner number: an episode for TV, a track
	// for Music. Meaningless unless State is DecisionPlaced.
	SlotNumber int
	// Ordinal orders this File among the Files sharing one Slot, 1-based. It
	// decides Edition.Files order and therefore the joint playback timeline of a
	// multi-part Edition; it is 1 for the ordinary one-File-per-Slot case.
	Ordinal int
	// Orphaned is true once a scan finds no file at Path (the Admin renamed,
	// moved or deleted it). An orphaned Placement is promoted into the Needs
	// Fixing queue as a problem in its own right, never silently dropped. Only
	// placed rows are orphaned; see SetPlacementOrphaned.
	Orphaned  bool
	CreatedAt string
}

// FileDecisions is every stored row for ONE File — one row for a settled decision,
// one per Slot for a placed one, and empty for a File nobody has said anything
// about. It is a named slice so the Scanner's question ("what did the Admin say
// about this file?") is one map lookup and one method call rather than a lookup per
// state.
type FileDecisions []FileDecision

// State reports the File's decision, or "" for a File with no rows — which is NOT
// a fourth kind of nothing but the ordinary case: no one has said anything, so the
// Placement is derived from the filename (ADR-0002).
func (d FileDecisions) State() string {
	if len(d) == 0 {
		return ""
	}
	return d[0].State
}

// Placements returns the Slots this File was placed on, in (group, slot, ordinal)
// order — empty for every other state, so a caller can range over it without
// checking State first.
func (d FileDecisions) Placements() []FileDecision {
	if d.State() != DecisionPlaced {
		return nil
	}
	return d
}

// FileDecisionSet is one Apply's worth of file-anchored decisions: the rows to
// keep, plus the scope they replace. It is a set rather than a row-at-a-time API
// because Apply is a whole-arrangement commit — the Admin sorts a Show and presses
// Apply once — and because reverting a File to "follow the parse" can only be
// expressed by its rows' ABSENCE from the new set, which needs the old set deleted
// in the same transaction that writes the new one.
//
// Paths is the replace scope: every File the Admin was looking at, whether or not
// it ended up with a row. Rows outside it are left alone, so sorting one Show never
// disturbs another's corrections. It defaults to the paths mentioned by Decisions,
// which is what a caller correcting a handful of known files wants; pass the full
// list explicitly to CLEAR a decision, since a path that keeps no row must still be
// in scope for its old rows to be deleted.
type FileDecisionSet struct {
	LibraryID string
	Paths     []string
	Decisions []FileDecision
}

// ReplaceFileDecisions commits one FileDecisionSet: within a single transaction it
// drops every decision in scope and writes the new sparse set, so a failure
// part-way leaves the previous arrangement exactly as it was.
//
// Deleting the scope first (rather than upserting row by row) is what makes the set
// authoritative: a File returned to the parse has no row, and only a delete can say
// that. It is also what lets a File change state — placed to ignored and back —
// inside one Apply without ever holding two contradictory decisions at once.
func (db *DB) ReplaceFileDecisions(set FileDecisionSet) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin replace file decisions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The body lives in placement.go, because Apply must commit the decisions and
	// the Title rows they imply in ONE transaction: decisions without rows is a
	// Show that only rearranges on the next scan, and rows without decisions is a
	// rearrangement the next scan undoes.
	if err := replaceFileDecisionsTx(tx, set); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit replace file decisions: %w", err)
	}
	return nil
}

// FileDecisionsByLibrary returns every stored decision in a Library keyed by file
// path — everything the Scanner needs in ONE read, because resolve consults it per
// walked file and a per-file query would be a query per file in the library.
//
// One read covers all three states deliberately: the Scanner asks "what did the
// Admin say about this file?", and answering that with a lookup per state invites
// the three to be consulted in different places and drift out of agreement. A path
// absent from the map is the common case — nothing was said, follow the parse.
//
// Each slice is ordered by (group, slot, ordinal) so a File spanning several Slots
// comes back in Slot order and the Files sharing one Slot come back in part order.
//
// Orphaned rows are included: their path is by definition absent from disk so
// resolve will never look them up, and the Needs Fixing queue reads the same map to
// list them.
func (db *DB) FileDecisionsByLibrary(libraryID string) (map[string]FileDecisions, error) {
	rows, err := db.Query(
		`SELECT id, library_id, path, state, group_number, slot_number, ordinal, orphaned, created_at
		   FROM file_decisions WHERE library_id = ?
		  ORDER BY path, group_number, slot_number, ordinal`, libraryID)
	if err != nil {
		return nil, fmt.Errorf("store: listing file decisions: %w", err)
	}
	defer rows.Close()

	out := map[string]FileDecisions{}
	for rows.Next() {
		var d FileDecision
		var group, slot sql.NullInt64
		var orphaned int
		if err := rows.Scan(&d.ID, &d.LibraryID, &d.Path, &d.State, &group, &slot,
			&d.Ordinal, &orphaned, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning file decision: %w", err)
		}
		// NULL group/slot is the settled states' normal shape; the zero values it
		// leaves behind are never read, because State says there is no Slot.
		d.GroupNumber, d.SlotNumber = int(group.Int64), int(slot.Int64)
		d.Orphaned = orphaned == 1
		out[d.Path] = append(out[d.Path], d)
	}
	return out, rows.Err()
}

// SetPlacementOrphaned flags/unflags a Placement whose anchor file is (or is again)
// on disk. The Scanner calls it after the walk, exactly as it already calls
// SetMatchOverrideOrphaned for folder anchors: a correction pointing at nothing is
// broken rather than done, and is surfaced instead of being dropped.
//
// Only placed rows are orphaned. A settled decision about a File that has gone is
// not a broken correction — there is nothing to fix and nothing to show — and
// re-surfacing it would both add noise and un-settle an ignore that will correctly
// re-apply if the File ever comes back.
func (db *DB) SetPlacementOrphaned(id string, orphaned bool) error {
	_, err := db.Exec(
		`UPDATE file_decisions SET orphaned = ? WHERE id = ? AND state = 'placed'`,
		boolToInt(orphaned), id)
	if err != nil {
		return fmt.Errorf("store: setting placement orphaned: %w", err)
	}
	return nil
}
