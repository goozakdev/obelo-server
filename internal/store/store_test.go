package store_test

import (
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// openTemp opens a fresh DB in a temp dir, migrated, with cleanup registered.
func openTemp(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestWALAndFTS5 confirms the driver build gives us WAL mode and FTS5, both
// required by ADR-0007. Open() itself verifies these, so a successful open is
// the assertion; we also exercise an FTS5 table end-to-end.
func TestWALAndFTS5(t *testing.T) {
	db := openTemp(t)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	if _, err := db.Exec("CREATE VIRTUAL TABLE docs USING fts5(body)"); err != nil {
		t.Fatalf("create fts5 table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO docs(body) VALUES ('the quick brown fox')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM docs WHERE docs MATCH 'brown'").Scan(&n); err != nil {
		t.Fatalf("fts5 match: %v", err)
	}
	if n != 1 {
		t.Fatalf("fts5 match count = %d, want 1", n)
	}
}

// TestMigrateIdempotent confirms migrations run once and re-running Migrate is
// a no-op (the startup contract).
func TestMigrateIdempotent(t *testing.T) {
	db := openTemp(t)

	// Already migrated by openTemp; run again and confirm no error / no dupes.
	if err := db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var applied int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied < 1 {
		t.Fatalf("applied migrations = %d, want >= 1", applied)
	}

	// Run a third time for good measure; count must not grow.
	if err := db.Migrate(); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	var applied2 int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&applied2); err != nil {
		t.Fatalf("count migrations again: %v", err)
	}
	if applied2 != applied {
		t.Fatalf("migration count changed on re-run: %d -> %d", applied, applied2)
	}
}

// TestOverrideAnchor: a fix-match anchor is the FILE for a bare file dropped at a
// Library root (so a loose yearless movie is individually fixable) and the FOLDER
// for a foldered movie — matching how the scanner keys overrides.
func TestOverrideAnchor(t *testing.T) {
	roots := []string{"/media/movies", "/mnt/more"}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"bare file at a root → the file itself", "/media/movies/Yearless.mkv", "/media/movies/Yearless.mkv"},
		{"bare file at the other root", "/mnt/more/Loose Movie.mkv", "/mnt/more/Loose Movie.mkv"},
		{"foldered movie → its folder", "/media/movies/Dune (2021)/dune.mkv", "/media/movies/Dune (2021)"},
		{"root with a trailing slash still matches", "/media/movies/Bare.mkv", "/media/movies/Bare.mkv"},
		{"empty path → empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := store.OverrideAnchor(c.path, roots); got != c.want {
				t.Errorf("OverrideAnchor(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
	// A trailing slash on a configured root is normalized before comparing.
	if got := store.OverrideAnchor("/media/movies/Bare.mkv", []string{"/media/movies/"}); got != "/media/movies/Bare.mkv" {
		t.Errorf("trailing-slash root: got %q, want the file path", got)
	}
}

// TestNeedsReviewAnchor: each kind's fix-match anchor matches how the scanner keys
// its override — a Movie's folder (or the loose file), a Show's TOP-LEVEL folder
// (the Episode file is nested in a Season subfolder), a Track's album folder; an
// Episode (a numbering problem) has none.
func TestNeedsReviewAnchor(t *testing.T) {
	roots := []string{"/media/tv", "/media/movies", "/media/music"}
	cases := []struct {
		name, kind, path, want string
	}{
		{"movie foldered → its folder", "movie", "/media/movies/Dune (2021)/dune.mkv", "/media/movies/Dune (2021)"},
		{"movie loose at root → the file", "movie", "/media/movies/Loose.mkv", "/media/movies/Loose.mkv"},
		{"show episode nested → the show folder", "show", "/media/tv/Anime Show/Season 01/ep.mkv", "/media/tv/Anime Show"},
		{"show episode loose in show folder → show folder", "show", "/media/tv/Loose Show/ep.mkv", "/media/tv/Loose Show"},
		{"track → its album folder", "track", "/media/music/Artist/Album/01.mp3", "/media/music/Artist/Album"},
		{"episode has no folder fix", "episode", "/media/tv/Show/Season 01/ep.mkv", ""},
		{"empty path → empty", "movie", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := store.NeedsReviewAnchor(c.kind, c.path, roots); got != c.want {
				t.Errorf("NeedsReviewAnchor(%q, %q) = %q, want %q", c.kind, c.path, got, c.want)
			}
		})
	}
}

// TestCountUsersFreshDB confirms a fresh DB has zero Users (drives setupRequired).
func TestCountUsersFreshDB(t *testing.T) {
	db := openTemp(t)
	n, err := db.CountUsers()
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountUsers on fresh DB = %d, want 0", n)
	}
}

// TestArtworkVersionsForTitles: the per-Title poster cache-bust token is the
// newest artwork added_at, keyed by Title id and absent when a Title has no
// artwork. Because a re-enrich replaces a role's row (so its added_at advances),
// MAX(added_at) is what changes exactly when the served bytes could have — which
// is why a browse grid can bust only the posters that actually changed.
func TestArtworkVersionsForTitles(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Movies', 'movie')`)
	for _, id := range []string{"t1", "t2", "t3"} {
		mustExec(t, db,
			`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title)
			 VALUES (?, 'lib1', 'movie', ?, ?, ?)`, id, id, id, id)
	}
	// t1: a local poster, then a (later) fetched poster — the fetched added_at is
	// newer, so it must win MAX (mirrors a re-enrich landing fresh artwork).
	mustExec(t, db,
		`INSERT INTO artwork (id, title_id, role, path, source, added_at)
		 VALUES ('a1', 't1', 'poster', '/p1.jpg', 'local', '2025-01-01 00:00:00')`)
	mustExec(t, db,
		`INSERT INTO artwork (id, title_id, role, path, source, added_at)
		 VALUES ('a2', 't1', 'poster', '/p1-fetched.jpg', 'fetched', '2025-06-01 00:00:00')`)
	// t2: a single background — its own added_at is the version. t3: no artwork.
	mustExec(t, db,
		`INSERT INTO artwork (id, title_id, role, path, source, added_at)
		 VALUES ('a3', 't2', 'background', '/b2.jpg', 'fetched', '2025-03-03 00:00:00')`)

	got, err := db.ArtworkVersionsForTitles([]string{"t1", "t2", "t3"})
	if err != nil {
		t.Fatalf("ArtworkVersionsForTitles: %v", err)
	}
	if got["t1"] != "2025-06-01 00:00:00" {
		t.Errorf("t1 version = %q, want the newest (fetched) added_at", got["t1"])
	}
	if got["t2"] != "2025-03-03 00:00:00" {
		t.Errorf("t2 version = %q, want its sole added_at", got["t2"])
	}
	if _, ok := got["t3"]; ok {
		t.Errorf("t3 has no artwork; want it absent from the map, got %q", got["t3"])
	}

	// Empty input is a no-op (no query), matching the other bulk readers.
	empty, err := db.ArtworkVersionsForTitles(nil)
	if err != nil {
		t.Fatalf("ArtworkVersionsForTitles(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty input → %d entries, want 0", len(empty))
	}
}

// TestEntityArtworkVersionsForMany: the parent-entity (Show/Season/Artist/Album)
// poster cache-bust token is the newest entity_artwork added_at, keyed by entity
// id, scoped to the entity_type, and absent when the entity has no fetched
// artwork. Mirrors the title-keyed TestArtworkVersionsForTitles — the parent
// table gained added_at in migration 0013 so a re-enrich (which replaces the row)
// advances the version.
func TestEntityArtworkVersionsForMany(t *testing.T) {
	db := openTemp(t)
	ins := func(entityType, entityID, role, source, addedAt string) {
		mustExec(t, db,
			`INSERT INTO entity_artwork (id, entity_type, entity_id, role, path, source, added_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			entityType+entityID+role+source, entityType, entityID, role, "/"+entityID+".jpg", source, addedAt)
	}
	// s1: poster + background — the newer background added_at wins MAX.
	ins("show", "s1", "poster", "fetched", "2025-01-01 00:00:00")
	ins("show", "s1", "background", "fetched", "2025-06-01 00:00:00")
	// s2: a single poster. s3: nothing.
	ins("show", "s2", "poster", "fetched", "2025-03-03 00:00:00")
	// A same-id Artist with a later stamp must NOT leak into the "show" query.
	ins("artist", "s1", "poster", "fetched", "2099-01-01 00:00:00")

	got, err := db.EntityArtworkVersionsForMany("show", []string{"s1", "s2", "s3"})
	if err != nil {
		t.Fatalf("EntityArtworkVersionsForMany: %v", err)
	}
	if got["s1"] != "2025-06-01 00:00:00" {
		t.Errorf("s1 version = %q, want the newest (background) added_at", got["s1"])
	}
	if got["s2"] != "2025-03-03 00:00:00" {
		t.Errorf("s2 version = %q, want its sole added_at", got["s2"])
	}
	if _, ok := got["s3"]; ok {
		t.Errorf("s3 has no artwork; want it absent, got %q", got["s3"])
	}

	// The same query scoped to "artist" sees only the artist row (entity_type gate).
	artists, err := db.EntityArtworkVersionsForMany("artist", []string{"s1"})
	if err != nil {
		t.Fatalf("EntityArtworkVersionsForMany(artist): %v", err)
	}
	if artists["s1"] != "2099-01-01 00:00:00" {
		t.Errorf("artist s1 version = %q, want the artist row's added_at", artists["s1"])
	}

	empty, err := db.EntityArtworkVersionsForMany("show", nil)
	if err != nil {
		t.Fatalf("EntityArtworkVersionsForMany(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty input → %d entries, want 0", len(empty))
	}
}

// mustExec runs a statement and fails the test on error.
func mustExec(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
