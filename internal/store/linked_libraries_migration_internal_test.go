package store

import (
	"path/filepath"
	"testing"
)

// What migration 0063 does to a database that already has a catalog in it.
//
// 0063 rebuilds `files` to relax UNIQUE (edition_id, path) into a partial index
// that ignores the empty path a mirrored File carries. With foreign_keys ON — as
// every connection this server opens has it — a DROP TABLE performs an implicit
// DELETE FROM first, which fires `streams`' ON DELETE CASCADE. Migration 0008 did
// this same rebuild WITHOUT carrying the child rows out and back, and would have
// taken every Stream in the database with it.
//
// This test is the only thing standing between that and a silent, total loss of
// every audio and subtitle track on an upgrade, so it runs the REAL migration
// over REAL pre-0063 rows rather than asserting against a fresh schema.

// preLinkedLibrariesVersion is the last migration before the one under test.
const preLinkedLibrariesVersion = "0062_links"

func TestLinkedLibrariesMigrationKeepsEveryStream(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrateThrough(t, db, preLinkedLibrariesVersion)

	seed := []string{
		`INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Films', 'movie')`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		 VALUES ('t1', 'lib1', 'movie', 'Heat', 'heat|1995', 'heat')`,
		`INSERT INTO editions (id, title_id, name) VALUES ('e1', 't1', '1080p')`,
		`INSERT INTO files (id, edition_id, path, container, present, part_ordinal)
		 VALUES ('f1', 'e1', '/films/Heat (1995)/Heat.mkv', 'mkv', 1, 0)`,
		`INSERT INTO streams (id, file_id, stream_index, kind, codec, language, is_default, forced,
		   title, commentary, hearing_impaired)
		 VALUES ('s1', 'f1', 0, 'video', 'h264', '', 1, 0, '', 0, 0)`,
		`INSERT INTO streams (id, file_id, stream_index, kind, codec, language, is_default, forced,
		   title, commentary, hearing_impaired)
		 VALUES ('s2', 'f1', 1, 'audio', 'ac3', 'eng', 1, 0, 'Commentary', 1, 0)`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seeding (%s): %v", q, err)
		}
	}

	// Stamps written by 0061's triggers, read BEFORE the upgrade. The restore has
	// to preserve them: re-stamping every row would make every linked Server
	// re-pull its whole mirror for nothing.
	var fileStamp, streamStamp string
	if err := db.QueryRow(`SELECT updated_at FROM files WHERE id = 'f1'`).Scan(&fileStamp); err != nil {
		t.Fatalf("reading the file stamp: %v", err)
	}
	if err := db.QueryRow(`SELECT updated_at FROM streams WHERE id = 's2'`).Scan(&streamStamp); err != nil {
		t.Fatalf("reading the stream stamp: %v", err)
	}

	// The upgrade an existing install runs.
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM streams`).Scan(&n); err != nil {
		t.Fatalf("counting streams: %v", err)
	}
	if n != 2 {
		t.Fatalf("the rebuild left %d Streams, want 2 — the cascade ate them", n)
	}
	var kind, codec, lang, title string
	var commentary int
	if err := db.QueryRow(
		`SELECT kind, codec, language, title, commentary FROM streams WHERE id = 's2'`,
	).Scan(&kind, &codec, &lang, &title, &commentary); err != nil {
		t.Fatalf("reading the restored stream: %v", err)
	}
	if kind != "audio" || codec != "ac3" || lang != "eng" || title != "Commentary" || commentary != 1 {
		t.Errorf("the restored Stream lost fields: %s/%s/%s/%q commentary=%d",
			kind, codec, lang, title, commentary)
	}

	var gotFileStamp, gotStreamStamp string
	_ = db.QueryRow(`SELECT updated_at FROM files WHERE id = 'f1'`).Scan(&gotFileStamp)
	_ = db.QueryRow(`SELECT updated_at FROM streams WHERE id = 's2'`).Scan(&gotStreamStamp)
	if gotFileStamp != fileStamp {
		t.Errorf("the rebuild re-stamped the File (%s → %s); every mirror would re-pull",
			fileStamp, gotFileStamp)
	}
	if gotStreamStamp != streamStamp {
		t.Errorf("the restore re-stamped the Stream (%s → %s)", streamStamp, gotStreamStamp)
	}

	// The File itself survived with every column, and the FK still bites.
	var path, container string
	var part, present int
	if err := db.QueryRow(
		`SELECT path, container, part_ordinal, present FROM files WHERE id = 'f1'`,
	).Scan(&path, &container, &part, &present); err != nil {
		t.Fatalf("reading the rebuilt file: %v", err)
	}
	if path != "/films/Heat (1995)/Heat.mkv" || container != "mkv" || present != 1 {
		t.Errorf("the rebuilt File lost fields: %q/%q present=%d", path, container, present)
	}
	if _, err := db.Exec(
		`INSERT INTO streams (id, file_id, stream_index, kind) VALUES ('s9', 'nope', 0, 'audio')`,
	); err == nil {
		t.Error("a Stream under a non-existent File was accepted; the rebuilt FK is not enforced")
	}

	// And the triggers 0063 recreated still fire on the new table.
	if _, err := db.Exec(`UPDATE files SET container = 'mp4' WHERE id = 'f1'`); err != nil {
		t.Fatalf("touching the file: %v", err)
	}
	var afterTouch string
	_ = db.QueryRow(`SELECT updated_at FROM files WHERE id = 'f1'`).Scan(&afterTouch)
	if afterTouch == fileStamp {
		t.Error("files_touch_au did not survive the rebuild: an update left the stamp alone")
	}
}
