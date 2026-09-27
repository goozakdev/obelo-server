package store_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The two fixtures in testdata pin both ends of an upgrade. 0001_init_frozen.sql
// is 0001_init.sql as databases in the field were built from it; the embedded
// 0001 must never drift from it, since an existing database never runs it again.
// schema_after_0002.sql is the whole schema written as one file: a database
// that runs every migration, fresh or upgraded, must end with exactly it.
const (
	frozenInitFixture   = "testdata/0001_init_frozen.sql"
	wantSchemaFixture   = "testdata/schema_after_0002.sql"
	schemaMigrationsDDL = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`
)

// TestMigrationInitIsFrozen: 0001_init.sql is byte-for-byte what existing
// databases were built from. A schema change goes in a new numbered file.
func TestMigrationInitIsFrozen(t *testing.T) {
	got, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("read 0001: %v", err)
	}
	want, err := os.ReadFile(frozenInitFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("migrations/0001_init.sql differs from %s: 0001 is frozen; put the change in a new numbered migration", frozenInitFixture)
	}
}

// TestMigrateFreshMatchesSchema: a fresh database migrated end to end has the
// same tables, columns, indexes, triggers, CHECKs and foreign keys as one built
// from the whole schema in a single file.
func TestMigrateFreshMatchesSchema(t *testing.T) {
	db := openTemp(t)
	assertSchemaEqual(t, migrationSchema(t, db), migrationSchema(t, wantSchemaDB(t)))
}

// TestMigrateUpgradesExistingDatabase: a database built from the frozen 0001,
// holding rows in users and every table that hangs off a User, upgrades in
// place — every row kept, foreign keys clean, and the schema a fresh database
// gets.
func TestMigrateUpgradesExistingDatabase(t *testing.T) {
	db := existingDB(t)
	before := migrationRows(t, db, nil)

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate existing: %v", err)
	}

	after := migrationRows(t, db, before)
	for table, rows := range before {
		if strings.Join(after[table], "\n") != strings.Join(rows, "\n") {
			t.Errorf("table %s rows changed across the upgrade:\nbefore %q\nafter  %q", table, rows, after[table])
		}
	}
	assertForeignKeysClean(t, db)
	var ok string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check = %q, %v", ok, err)
	}
	var origin, mapped int
	if err := db.QueryRow("SELECT external_origin, role_mapped FROM users WHERE id = 'u-admin'").Scan(&origin, &mapped); err != nil {
		t.Fatalf("read new user columns: %v", err)
	}
	if origin != 0 || mapped != 0 {
		t.Errorf("existing user external_origin, role_mapped = %d, %d; want 0, 0", origin, mapped)
	}
	assertSchemaEqual(t, migrationSchema(t, db), migrationSchema(t, wantSchemaDB(t)))
}

// TestMigrateTwiceIsNoOp: running Migrate again on an upgraded database changes
// neither its schema, its rows nor its record of applied migrations.
func TestMigrateTwiceIsNoOp(t *testing.T) {
	db := existingDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	schema := migrationSchema(t, db)
	rows := migrationRows(t, db, nil)
	applied := appliedVersions(t, db)

	if err := db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	assertSchemaEqual(t, migrationSchema(t, db), schema)
	if got := migrationRows(t, db, nil); fmt.Sprint(got) != fmt.Sprint(rows) {
		t.Errorf("rows changed on the second run")
	}
	if got := appliedVersions(t, db); got != applied {
		t.Errorf("applied migrations %q -> %q on the second run", applied, got)
	}
}

// TestMigrateAdmitsMappedAdminWithoutPassword: after an upgrade the users CHECK
// is the new one, on the same connection that ran it — a password-less Admin
// minted from an External identity and made Admin by a Group mapping inserts,
// while a password-less Admin set by hand is still refused.
func TestMigrateAdmitsMappedAdminWithoutPassword(t *testing.T) {
	db := existingDB(t)
	const mapped = `INSERT INTO users (id, username, role, password_hash, external_origin, role_mapped)
		VALUES ('u-mapped', 'mapped', 'admin', NULL, 1, 1)`
	if _, err := db.Exec(`INSERT INTO users (id, username, role, password_hash)
		VALUES ('u-pre', 'pre', 'admin', NULL)`); err == nil {
		t.Fatalf("old CHECK admitted a password-less admin before the upgrade")
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate existing: %v", err)
	}
	if _, err := db.Exec(mapped); err != nil {
		t.Fatalf("insert password-less mapped admin after upgrade: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, role, password_hash, external_origin, role_mapped)
		VALUES ('u-hand', 'hand', 'admin', NULL, 1, 0)`); err == nil {
		t.Fatalf("password-less admin not set by a Group mapping was admitted")
	}
}

// existingDB builds a database the way one in the field looks before this
// upgrade: the frozen 0001 applied and recorded, holding a row in every table
// that references users and in the tables those rows need.
func existingDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "existing.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	body, err := os.ReadFile(frozenInitFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	mustExec(t, db, schemaMigrationsDDL)
	mustExec(t, db, string(body))
	mustExec(t, db, "INSERT INTO schema_migrations (version) VALUES ('0001_init')")

	for _, q := range []string{
		`INSERT INTO libraries (id, name, kind) VALUES ('lib-m', 'Movies', 'movie'), ('lib-t', 'TV', 'tv')`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title) VALUES ('t-1', 'lib-m', 'movie', 'Film', 'film', 'film')`,
		`INSERT INTO shows (id, library_id, title, identity_key, sort_title) VALUES ('s-1', 'lib-t', 'Show', 'show', 'show')`,
		`INSERT INTO users (id, username, role, password_hash, max_streams) VALUES
			('u-admin', 'admin', 'admin', 'hash-a', NULL),
			('u-member', 'member', 'member', 'hash-m', 2),
			('u-remote', 'peer', 'remote', NULL, NULL)`,
		`INSERT INTO devices (id, user_id, client_id, name, platform) VALUES
			('d-1', 'u-admin', 'c-1', 'Laptop', 'web'), ('d-2', 'u-member', 'c-2', 'iPad', 'ios')`,
		`INSERT INTO auth_tokens (token_hash, device_id, user_id) VALUES ('tok-1', 'd-1', 'u-admin'), ('tok-2', 'd-2', 'u-member')`,
		`INSERT INTO device_auth_requests (device_code_hash, user_code, client_id, device_name, device_platform, state, approved_user_id, created_at, expires_at)
			VALUES ('dc-1', 'ABCD', 'c-3', 'TV', 'tvos', 'approved', 'u-member', '2026-01-01', '2026-01-02')`,
		`INSERT INTO link_invites (code_hash, user_id, created_at, expires_at) VALUES ('li-1', 'u-remote', '2026-01-01', '2026-01-02')`,
		`INSERT INTO playlists (id, owner_user_id, name) VALUES ('p-1', 'u-member', 'Mine')`,
		`INSERT INTO playlist_items (id, playlist_id, title_id, position) VALUES ('pi-1', 'p-1', 't-1', 0)`,
		`INSERT INTO show_audio_memory (id, user_id, show_id, language) VALUES ('sam-1', 'u-member', 's-1', 'en')`,
		`INSERT INTO show_video_memory (id, user_id, show_id, codec) VALUES ('svm-1', 'u-member', 's-1', 'h264')`,
		`INSERT INTO stream_tokens (token_hash, session_id, user_id, created_at, expires_at) VALUES ('st-1', 'sess-1', 'u-admin', '2026-01-01', '2026-01-02')`,
		`INSERT INTO title_audio_memory (id, user_id, title_id, language) VALUES ('tam-1', 'u-admin', 't-1', 'fr')`,
		`INSERT INTO title_video_memory (id, user_id, title_id, codec) VALUES ('tvm-1', 'u-admin', 't-1', 'hevc')`,
		`INSERT INTO user_library_access (user_id, library_id) VALUES ('u-member', 'lib-m'), ('u-remote', 'lib-t')`,
		`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, watched) VALUES ('ws-1', 'u-admin', 't-1', 1234, 0), ('ws-2', 'u-member', 't-1', 0, 1)`,
	} {
		mustExec(t, db, q)
	}

	// Every table that references users must hold a row, so an upgrade that
	// rebuilt users with foreign keys on would show up as lost rows.
	refs, err := db.Query(`SELECT DISTINCT m.name FROM sqlite_master m, pragma_foreign_key_list(m.name) f
		WHERE m.type = 'table' AND f."table" = 'users'`)
	if err != nil {
		t.Fatalf("list tables referencing users: %v", err)
	}
	var children []string
	for refs.Next() {
		var name string
		if err := refs.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		children = append(children, name)
	}
	refs.Close()
	for _, name := range children {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + name + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if n == 0 {
			t.Fatalf("existing database has no row in %s, which references users", name)
		}
	}
	return db
}

// wantSchemaDB is a database built from the whole schema in one file, without
// Migrate.
func wantSchemaDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "want.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	body, err := os.ReadFile(wantSchemaFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	mustExec(t, db, string(body))
	return db
}

// migrationSchema is a database's schema as sorted lines: every sqlite_master
// entry with its exact SQL (so CHECKs, defaults and triggers count), then each
// table's columns, foreign keys and indexes as SQLite reports them. The
// migration bookkeeping table is left out: only Migrate creates it.
func migrationSchema(t *testing.T, db *store.DB) []string {
	t.Helper()
	var lines []string
	add := func(prefix, query string, args ...any) {
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s: %v", query, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%s=%q/%v", cols[i], v.String, v.Valid)
			}
			lines = append(lines, prefix+" "+strings.Join(parts, " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows %s: %v", query, err)
		}
	}

	add("master", `SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE tbl_name <> 'schema_migrations'`)
	for _, table := range migrationTables(t, db) {
		add("column "+table, `SELECT * FROM pragma_table_xinfo(?)`, table)
		add("fk "+table, `SELECT * FROM pragma_foreign_key_list(?)`, table)
		add("index "+table, `SELECT * FROM pragma_index_list(?)`, table)
		idx, err := db.Query(`SELECT name FROM pragma_index_list(?)`, table)
		if err != nil {
			t.Fatalf("index_list %s: %v", table, err)
		}
		var names []string
		for idx.Next() {
			var n string
			if err := idx.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			names = append(names, n)
		}
		idx.Close()
		for _, n := range names {
			add("indexcol "+n, `SELECT * FROM pragma_index_xinfo(?)`, n)
		}
	}
	sort.Strings(lines)
	return lines
}

// migrationTables lists the ordinary tables, bookkeeping excluded.
func migrationTables(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
		AND name <> 'schema_migrations' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

// migrationRows is every row of every table, as sorted strings per table. With
// a reference snapshot, only its tables and columns are read, so rows compare
// across an upgrade that adds columns and tables.
func migrationRows(t *testing.T, db *store.DB, reference map[string][]string) map[string][]string {
	t.Helper()
	out := make(map[string][]string)
	for _, table := range migrationTables(t, db) {
		if reference != nil {
			if _, ok := reference[table]; !ok {
				continue
			}
		}
		cols := referenceColumns(t, db, table, reference)
		rows, err := db.Query(`SELECT ` + strings.Join(cols, ", ") + ` FROM "` + table + `"`)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		lines := []string{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			lines = append(lines, strings.Join(cols, ",")+"="+fmt.Sprintf("%q", fmt.Sprint(vals...)))
		}
		rows.Close()
		sort.Strings(lines)
		out[table] = lines
	}
	return out
}

// referenceColumns is the table's column list as quoted identifiers: the
// columns the reference snapshot read, or all of them without one.
func referenceColumns(t *testing.T, db *store.DB, table string, reference map[string][]string) []string {
	t.Helper()
	if reference != nil {
		if rows := reference[table]; len(rows) > 0 {
			return strings.Split(rows[0][:strings.Index(rows[0], "=")], ",")
		}
	}
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols = append(cols, `"`+n+`"`)
	}
	return cols
}

func assertForeignKeysClean(t *testing.T, db *store.DB) {
	t.Helper()
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Errorf("foreign_key_check: %s row %v references a missing %s", table, rowid, parent)
	}
}

func appliedVersions(t *testing.T, db *store.DB) string {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT group_concat(version || '@' || applied_at, ',') FROM (SELECT * FROM schema_migrations ORDER BY version)").Scan(&s); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return s
}

// assertSchemaEqual reports the lines each schema has that the other lacks.
func assertSchemaEqual(t *testing.T, got, want []string) {
	t.Helper()
	inGot := make(map[string]int)
	for _, l := range got {
		inGot[l]++
	}
	for _, l := range want {
		inGot[l]--
	}
	var extra, missing []string
	for l, n := range inGot {
		for ; n > 0; n-- {
			extra = append(extra, l)
		}
		for ; n < 0; n++ {
			missing = append(missing, l)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	for _, l := range missing {
		t.Errorf("schema lacks: %s", l)
	}
	for _, l := range extra {
		t.Errorf("schema has extra: %s", l)
	}
}
