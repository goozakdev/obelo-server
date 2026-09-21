package store_test

import (
	"strings"
	"testing"
)

// These four columns carry no default a writer could lean on:
// entity_artwork.added_at stamps datetime('now') like artwork.added_at, while
// unmatched_files.kind, artwork.source and plugins.origin require a value,
// because every writer names the column. Each test pins the declaration directly
// against the schema, since no Go writer omits these columns.

// TestEntityArtworkAddedAtDefaultsToNow: an INSERT that omits added_at still
// gets a real, non-empty timestamp (matching artwork.added_at's declaration).
func TestEntityArtworkAddedAtDefaultsToNow(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db,
		`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source)
		 VALUES ('ea1', 'show', 's1', 'poster', '/p.jpg', 'fetched')`)

	var addedAt string
	if err := db.QueryRow(`SELECT added_at FROM entity_artwork WHERE id = 'ea1'`).Scan(&addedAt); err != nil {
		t.Fatalf("select added_at: %v", err)
	}
	if addedAt == "" {
		t.Error("added_at defaulted to '', want a real datetime('now') stamp")
	}
}

// TestUnmatchedFilesKindRequiresAValue: the only writer (catalog.go) always
// names kind, so the column carries no default; an INSERT that omits it fails.
func TestUnmatchedFilesKindRequiresAValue(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)

	_, err := db.Exec(
		`INSERT INTO unmatched_files (id, library_id, path, reason) VALUES ('u1', 'lib', '/x.mkv', '')`)
	if err == nil {
		t.Fatal("insert omitting kind succeeded, want a NOT NULL failure")
	}
	if !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("error = %v, want a NOT NULL constraint failure", err)
	}
}

// TestArtworkSourceRequiresAValue: every writer (catalog.go, enrich.go) names
// source, so the column carries no default; an INSERT that omits it fails.
func TestArtworkSourceRequiresAValue(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	mustExec(t, db,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
		 VALUES ('t1', 'lib', 'movie', 't1', 't1', 't1')`)

	_, err := db.Exec(
		`INSERT INTO artwork (id, title_id, role, path) VALUES ('a1', 't1', 'poster', '/p.jpg')`)
	if err == nil {
		t.Fatal("insert omitting source succeeded, want a NOT NULL failure")
	}
	if !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("error = %v, want a NOT NULL constraint failure", err)
	}
}

// TestPluginsOriginRequiresAValue: the column carries no default, so an INSERT
// that omits origin fails.
func TestPluginsOriginRequiresAValue(t *testing.T) {
	db := openTemp(t)

	_, err := db.Exec(
		`INSERT INTO plugins (id, source, installed_at) VALUES ('p1', 'upload', datetime('now'))`)
	if err == nil {
		t.Fatal("insert omitting origin succeeded, want a NOT NULL failure")
	}
	if !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("error = %v, want a NOT NULL constraint failure", err)
	}
}

// TestPluginsOriginCheckRejectsUnknownValue: origin is CHECKed to 'admin' or
// 'bundled' — nothing else a Plugin can be attributed to.
func TestPluginsOriginCheckRejectsUnknownValue(t *testing.T) {
	db := openTemp(t)

	_, err := db.Exec(
		`INSERT INTO plugins (id, source, installed_at, origin) VALUES ('p1', 'upload', datetime('now'), 'other')`)
	if err == nil {
		t.Fatal("insert with origin = 'other' succeeded, want a CHECK failure")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("error = %v, want a CHECK constraint failure", err)
	}
}

// TestEntityEnrichmentIDNamespaceCheck: entity_enrichment is CHECKed so a
// non-empty external_id always carries a non-empty external_id_namespace —
// the shape entityNamespace (internal/store/record_ids.go) already leaves on
// every row either writer (WriteEntityEnrichment, SetEntityExternalMatch)
// produces. The reverse — a namespace beside a blank id — is not CHECKed:
// entityNamespace returns "" whenever externalID == "", so neither writer
// ever produces that shape either, and the schema has no need to reject it.
func TestEntityEnrichmentIDNamespaceCheck(t *testing.T) {
	db := openTemp(t)

	_, err := db.Exec(
		`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, external_id_namespace)
		 VALUES ('show', 's1', 'tt1', '')`)
	if err == nil {
		t.Fatal("insert with an id and a blank namespace succeeded, want a CHECK failure")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("error = %v, want a CHECK constraint failure", err)
	}

	mustExec(t, db,
		`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, external_id_namespace)
		 VALUES ('show', 's2', '', 'tmdb')`)
}
