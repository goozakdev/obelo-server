package store

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// What migration 0051 does to a library that already exists (ADR-0046).
//
// 0051 replaces the two "the Admin chose this record" booleans with a three-valued
// origin — '' (nobody chose it), 'chosen' (chosen on this item), 'cascaded'
// (applied by a parent's Cascade). The distinction it introduces is not in the
// database it inherits: until this migration a record a Cascade wrote and one an
// Admin picked by hand were the same bit and are byte-identical in every other
// column, so no backfill can tell them apart. This test pins which way the
// migration resolves that, because "we could not know" is not the same as "it does
// not matter" — the choice decides whether an existing install's corrections can be
// silently overwritten by the next Cascade.
//
// It runs the REAL migration over a REAL pre-0051 row rather than asserting against
// a hand-set column, which is the only way the backfill statements themselves are
// covered.

// legacyVersion is the last migration before the one under test: the schema this
// test builds, seeds, and then upgrades.
const legacyVersion = "0050_title_enrichment_record"

// migrateThrough applies the embedded migrations in the order Migrate does, up to
// and including `last`, leaving the DB at that older schema. Migrate() then applies
// exactly the remainder, which is what an upgrading install runs.
func migrateThrough(t *testing.T, db *DB, last string) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		t.Fatalf("creating schema_migrations: %v", err)
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("reading embedded migrations: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("reading migration %q: %v", name, err)
		}
		if err := db.applyMigration(version, string(body)); err != nil {
			t.Fatalf("applying migration %q: %v", version, err)
		}
		if version == last {
			return
		}
	}
	t.Fatalf("migration %q not found; the constant needs updating", last)
}

// TestRecordOriginBackfillReadsEveryLockAsChosen: every pre-0051 lock becomes
// 'chosen', at both grains, and a record nobody locked stays unchosen.
//
// 'chosen' is the deliberate direction, and the two wrong answers are not
// symmetric. Reading an old lock as 'cascaded' would let the next Cascade silently
// overwrite a Fix-info correction an Admin really made — destructive, invisible,
// unrecoverable. Reading it as 'chosen' reproduces exactly what that install did
// yesterday: the row keeps being skipped by its parent's Cascade. That is the bug
// issue 04 describes, preserved for old rows — but it is visible (the Cascade
// summary counts the skip) and repairable (a Fix info, a Wrong item or a cleared
// pin on the child re-states the record and settles its origin honestly), which is
// the same call migration 0050 made, for the same reason.
//
// So an existing library gets the fix on records written AFTER upgrading; children
// a Cascade wrote before it go on being skipped. enrich.TestABackfilledRowKeepsItsOldReading
// pins what the skip rule then does with such a row.
func TestRecordOriginBackfillReadsEveryLockAsChosen(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrateThrough(t, db, legacyVersion)

	if _, err := db.Exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Films', 'movie')`); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	// Two leaves at the old schema. The locked one is deliberately ambiguous: at
	// this schema a record an Admin picked on the Title and one its Show's Cascade
	// pinned are the same row, which is precisely why the backfill has to pick.
	for _, r := range []struct {
		id, key, record string
		locked          int
	}{
		{"t-locked", "movie:corrected", "550", 1},
		{"t-auto", "movie:auto", "551", 0},
	} {
		if _, err := db.Exec(
			`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
			                     enrichment_tmdb_id, enrichment_id_locked)
			 VALUES (?, 'lib1', 'movie', 'A Film', ?, 'a film', ?, ?)`,
			r.id, r.key, r.record, r.locked,
		); err != nil {
			t.Fatalf("seed title %s: %v", r.id, err)
		}
	}
	// The same ambiguity one grain up: an Album an Artist's Cascade pinned is
	// indistinguishable from one the Admin fixed by hand.
	for _, r := range []struct {
		id     string
		locked int
	}{{"alb-locked", 1}, {"alb-auto", 0}} {
		if _, err := db.Exec(
			`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, external_id_locked, enrichment_status)
			 VALUES ('album', ?, 'rg-1', ?, 'matched')`, r.id, r.locked,
		); err != nil {
			t.Fatalf("seed entity %s: %v", r.id, err)
		}
	}

	// The upgrade an existing install runs.
	if err := db.Migrate(); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	originOf := func(query string, args ...any) RecordOrigin {
		t.Helper()
		var o string
		if err := db.QueryRow(query, args...).Scan(&o); err != nil {
			t.Fatalf("reading origin: %v", err)
		}
		return RecordOrigin(o)
	}
	titleOrigin := func(id string) RecordOrigin {
		return originOf(`SELECT enrichment_id_origin FROM titles WHERE id = ?`, id)
	}
	albumOrigin := func(id string) RecordOrigin {
		return originOf(`SELECT external_id_origin FROM entity_enrichment WHERE entity_type = 'album' AND entity_id = ?`, id)
	}

	if got := titleOrigin("t-locked"); got != OriginChosen {
		t.Errorf("a pre-0051 locked Title reads %q, want %q — an existing correction must "+
			"not be demoted into something the next Cascade may overwrite", got, OriginChosen)
	}
	if got := titleOrigin("t-auto"); got != OriginDerived {
		t.Errorf("a pre-0051 UNLOCKED Title reads %q, want %q — an id a pass resolved was "+
			"nobody's choice before the upgrade and is nobody's after it", got, OriginDerived)
	}
	if got := albumOrigin("alb-locked"); got != OriginChosen {
		t.Errorf("a pre-0051 locked Album reads %q, want %q", got, OriginChosen)
	}
	if got := albumOrigin("alb-auto"); got != OriginDerived {
		t.Errorf("a pre-0051 UNLOCKED Album reads %q, want %q", got, OriginDerived)
	}

	// The old booleans are gone rather than kept in step: a bit that is a strict
	// function of the origin is a method (RecordOrigin.Locked()), and leaving the
	// column behind would let a future reader consult half the truth — which is how
	// this bug got in.
	for _, c := range []struct{ table, column string }{
		{"titles", "enrichment_id_locked"},
		{"entity_enrichment", "external_id_locked"},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column,
		).Scan(&n); err != nil {
			t.Fatalf("reading %s schema: %v", c.table, err)
		}
		if n != 0 {
			t.Errorf("%s.%s still exists; the origin column replaces it", c.table, c.column)
		}
	}
}
