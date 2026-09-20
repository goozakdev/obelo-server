package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

// What migration 0072 does to a library that already exists (ADR-0060 decisions
// 2–4, .scratch/bundled-plugins issue 14).
//
// A Title's record lived in three columns named after sources and a parent's in one
// untagged external_id. The migration moves each id into a row keyed by the
// namespace it provably belongs to — the matched source that wrote it, where that is
// an Authoritative namespace, else the namespace the column was always read as — and
// drops the columns. The case it exists for is the AniDB aid that an AniDB-led
// Library wrote into enrichment_tmdb_id: it must come out as an `anidb` id, not be
// made a TMDB id for good. This file runs the REAL statements over REAL pre-0072 rows.

// preNamespacedIDsVersion is the last migration before the one under test.
const preNamespacedIDsVersion = "0071_coverart_becomes_musicbrainz_url2"

func namespacedIDsFixture(t *testing.T, seed func(exec func(string, ...any))) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrateThrough(t, db, preNamespacedIDsVersion)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	seed(exec)

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// titleRecord reads a migrated Title's record rows and record namespace.
func titleRecord(t *testing.T, db *DB, titleID string) (map[string]string, string) {
	t.Helper()
	rows, err := db.Query(
		`SELECT namespace, external_id FROM title_external_ids WHERE title_id = ?`, titleID)
	if err != nil {
		t.Fatalf("reading record rows of %s: %v", titleID, err)
	}
	defer rows.Close()
	ids := map[string]string{}
	for rows.Next() {
		var ns, id string
		if err := rows.Scan(&ns, &id); err != nil {
			t.Fatalf("scanning record row: %v", err)
		}
		ids[ns] = id
	}
	var ns string
	if err := db.QueryRow(`SELECT enrichment_id_namespace FROM titles WHERE id = ?`, titleID).Scan(&ns); err != nil {
		t.Fatalf("reading record namespace of %s: %v", titleID, err)
	}
	return ids, ns
}

func TestExistingRecordIdsMoveIntoNamespacedRows(t *testing.T) {
	db := namespacedIDsFixture(t, func(exec func(string, ...any)) {
		exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Anime', 'tv')`)
		exec(`INSERT INTO libraries (id, name, kind) VALUES ('mus', 'Music', 'music')`)
		title := func(id, kind, lib, tmdbRecord, imdbRecord, mbid, status, source string) {
			exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
			                          enrichment_tmdb_id, enrichment_imdb_id, musicbrainz_id,
			                          enrichment_status, enrichment_source)
			      VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, lib, kind, id, kind+":"+id, id, tmdbRecord, imdbRecord, mbid, status, source)
		}
		// The bug being rescued: an AniDB lead's aid in the TMDB column, written in
		// the same statement as a 'matched' status naming anidb.
		title("anidb-leak", "episode", "lib", "4563", "", "", "matched", "anidb")
		// A stale source: an Admin's override reset the row to 'pending' and left the
		// old source standing. The id is the Admin's, read as TMDB, as it always was.
		title("stale-source", "movie", "lib", "550", "", "", "pending", "anidb")
		// A matched TMDB movie that also carries an IMDb cross-reference.
		title("both", "movie", "lib", "603", "tt0133093", "", "matched", "tmdb")
		// An IMDb-only record: the IMDb row is the record.
		title("imdb-only", "movie", "lib", "", "tt0111161", "", "matched", "omdb")
		// A Track: musicbrainz_id was always the music record.
		title("track", "track", "mus", "", "", "rec-1", "matched", "musicbrainz")
		// A matched row whose source is no Authoritative namespace falls back too.
		title("supplement", "movie", "lib", "680", "", "", "matched", "omdb")
		// Nothing recorded at all.
		title("none", "movie", "lib", "", "", "", "pending", "")
	})

	for _, c := range []struct {
		id     string
		wantID map[string]string
		wantNS string
	}{
		{"anidb-leak", map[string]string{"anidb": "4563"}, "anidb"},
		{"stale-source", map[string]string{"tmdb": "550"}, "tmdb"},
		{"both", map[string]string{"tmdb": "603", "imdb": "tt0133093"}, "tmdb"},
		{"imdb-only", map[string]string{"imdb": "tt0111161"}, "imdb"},
		{"track", map[string]string{"musicbrainz": "rec-1"}, "musicbrainz"},
		{"supplement", map[string]string{"tmdb": "680"}, "tmdb"},
		{"none", map[string]string{}, ""},
	} {
		ids, ns := titleRecord(t, db, c.id)
		if !reflect.DeepEqual(ids, c.wantID) {
			t.Errorf("%s: record rows = %v, want %v", c.id, ids, c.wantID)
		}
		if ns != c.wantNS {
			t.Errorf("%s: record namespace = %q, want %q", c.id, ns, c.wantNS)
		}
	}

	// The store's reads see the same thing: the derived TMDBID is the tmdb
	// record-or-identity id, so the rescued aid is NOT a TMDB id any more.
	leak, err := db.TitleForEnrichmentByID("anidb-leak")
	if err != nil {
		t.Fatalf("reading the rescued Title: %v", err)
	}
	if leak.TMDBID != "" || leak.RecordNamespace != "anidb" || leak.RecordID("anidb") != "4563" {
		t.Errorf("rescued Title reads TMDBID %q, namespace %q, anidb %q; want \"\", anidb, 4563",
			leak.TMDBID, leak.RecordNamespace, leak.RecordID("anidb"))
	}
	track, err := db.TitleForEnrichmentByID("track")
	if err != nil {
		t.Fatalf("reading the Track: %v", err)
	}
	if track.MusicbrainzID != "rec-1" {
		t.Errorf("Track MusicbrainzID = %q, want rec-1", track.MusicbrainzID)
	}

	// The three record columns are gone; the tag-identity MBIDs one level up stay.
	for _, c := range []struct {
		table, column string
		want          int
	}{
		{"titles", "enrichment_tmdb_id", 0},
		{"titles", "enrichment_imdb_id", 0},
		{"titles", "musicbrainz_id", 0},
		{"titles", "tmdb_id", 1},
		{"titles", "imdb_id", 1},
		{"artists", "musicbrainz_id", 1},
		{"albums", "musicbrainz_id", 1},
	} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.column,
		).Scan(&n); err != nil {
			t.Fatalf("reading %s schema: %v", c.table, err)
		}
		if n != c.want {
			t.Errorf("%s.%s present = %d, want %d", c.table, c.column, n, c.want)
		}
	}
}

func TestExistingParentRecordsGainTheirNamespace(t *testing.T) {
	db := namespacedIDsFixture(t, func(exec func(string, ...any)) {
		parent := func(kind, id, externalID, status, source string) {
			exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status, enrichment_source)
			      VALUES (?, ?, ?, ?, ?)`, kind, id, externalID, status, source)
		}
		parent("show", "show-anidb", "4563", "matched", "anidb")
		parent("show", "show-stale", "1399", "pending", "anidb")
		parent("season", "season-tvdb", "121361", "matched", "thetvdb")
		parent("season", "season-default", "3624", "failed", "")
		parent("artist", "artist-mb", "art-1", "matched", "musicbrainz")
		parent("album", "album-default", "rg-1", "pending", "musicbrainz")
		parent("show", "show-empty", "", "unmatched", "tmdb")
	})

	for _, c := range []struct{ kind, id, want string }{
		{"show", "show-anidb", "anidb"},
		{"show", "show-stale", "tmdb"},
		{"season", "season-tvdb", "thetvdb"},
		{"season", "season-default", "tmdb"},
		{"artist", "artist-mb", "musicbrainz"},
		{"album", "album-default", "musicbrainz"},
		{"show", "show-empty", ""},
	} {
		e, err := db.EntityEnrichmentByID(c.kind, c.id)
		if err != nil {
			t.Fatalf("reading %s %s: %v", c.kind, c.id, err)
		}
		if e.Namespace != c.want {
			t.Errorf("%s %s: namespace = %q, want %q", c.kind, c.id, e.Namespace, c.want)
		}
	}
}
