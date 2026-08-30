package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The storage half of Placement Apply (file-matcher/03, ADR-0044). The catalog
// tests exercise the whole operation; these pin the two properties that only the
// transaction itself can guarantee.

// TestApplyShowArrangementIsAtomic: a failure part-way leaves the Show exactly as
// it was. The decisions are written FIRST (so the record the Scanner replays and
// the rows Apply writes can never disagree), which is precisely why a later
// failure has to take them back down with it.
func TestApplyShowArrangementIsAtomic(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	                 VALUES ('sh1','libtv','The Bear','the bear|2022','bear')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key)
	                 VALUES ('se1','sh1',1,'the bear|2022|s01')`)
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                     season_id, season_number, episode_number)
	                 VALUES ('t1','libtv','episode','System','the bear|2022|s01e01','system','se1',1,1)`)

	const path = "/media/TV/The Bear (2022)/Season 01/The Bear (2022) - S01E01 - System.mkv"
	err := db.ApplyShowArrangement(store.ShowArrangement{
		ShowID:    "sh1",
		LibraryID: "libtv",
		Decisions: store.FileDecisionSet{
			LibraryID: "libtv",
			Paths:     []string{path},
			Decisions: []store.FileDecision{{
				Path: path, State: store.DecisionPlaced, GroupNumber: 4, SlotNumber: 1, Ordinal: 1,
			}},
		},
		// A re-key of a Title that is not there: a plausible mid-flight failure
		// (the row went away between the plan and the commit), raised AFTER the
		// decisions above have already been inserted.
		Rekeys: []store.TitleRekey{{TitleID: "gone", IdentityKey: "the bear|2022|s04e01"}},
	})
	if err == nil {
		t.Fatalf("apply with a missing Title succeeded; want a failure")
	}

	decisions, derr := db.FileDecisionsByLibrary("libtv")
	if derr != nil {
		t.Fatalf("read decisions: %v", derr)
	}
	if len(decisions) != 0 {
		t.Fatalf("a failed Apply left decisions behind: %#v", decisions)
	}
	var key string
	if err := db.QueryRow(`SELECT identity_key FROM titles WHERE id = 't1'`).Scan(&key); err != nil {
		t.Fatalf("read title: %v", err)
	}
	if key != "the bear|2022|s01e01" {
		t.Fatalf("identity_key = %q, want the Show untouched", key)
	}
}

// TestApplyShowArrangementSwapsThroughATemporaryKey: two Titles trading Slots
// would trip UNIQUE (library_id, identity_key) whichever one moved first, so the
// movers are parked on a temporary key for the length of the transaction — and
// none of those parking keys may survive it.
func TestApplyShowArrangementSwapsThroughATemporaryKey(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	                 VALUES ('sh1','libtv','The Bear','the bear|2022','bear')`)
	mustExec(t, db, `INSERT INTO seasons (id, show_id, season_number, identity_key)
	                 VALUES ('se1','sh1',1,'the bear|2022|s01')`)
	for _, row := range []struct{ id, key, title string }{
		{"t1", "the bear|2022|s01e01", "System"},
		{"t2", "the bear|2022|s01e02", "Hands"},
	} {
		mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
		                     season_id, season_number, episode_number)
		                 VALUES (?, 'libtv','episode', ?, ?, ?, 'se1', 1, 1)`,
			row.id, row.title, row.key, row.title)
	}

	if err := db.ApplyShowArrangement(store.ShowArrangement{
		ShowID:    "sh1",
		LibraryID: "libtv",
		Decisions: store.FileDecisionSet{LibraryID: "libtv"},
		Rekeys: []store.TitleRekey{
			{TitleID: "t1", IdentityKey: "the bear|2022|s01e02"},
			{TitleID: "t2", IdentityKey: "the bear|2022|s01e01"},
		},
	}); err != nil {
		t.Fatalf("swap: %v", err)
	}

	keys := map[string]string{}
	rows, err := db.Query(`SELECT id, identity_key FROM titles WHERE library_id = 'libtv'`)
	if err != nil {
		t.Fatalf("read keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if store.IsTempIdentityKey(key) {
			t.Fatalf("a parking key survived the commit: %q", key)
		}
		keys[id] = key
	}
	if keys["t1"] != "the bear|2022|s01e02" || keys["t2"] != "the bear|2022|s01e01" {
		t.Fatalf("keys = %#v, want them swapped", keys)
	}
}
