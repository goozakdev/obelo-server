package store_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A Show or Movie whose folder name carries no year takes the year from its
// matched metadata, clears needs_review, and keeps both across rescans. A year
// parsed from the folder always wins, and so does an Admin's identity fix.

func yearRow(t *testing.T, db *store.DB, table, id string) (year sql.NullInt64, review, reviewed int) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT year, needs_review, reviewed FROM `+table+` WHERE id = ?`, id,
	).Scan(&year, &review, &reviewed); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	return year, review, reviewed
}

func yearlessShowTree(year int) store.ShowTree {
	key := "andor"
	if year != 0 {
		key = "andor|2022"
	}
	return store.ShowTree{
		Show: store.Show{
			ID: "show1", LibraryID: "libtv", Title: "Andor", Year: year,
			IdentityKey: key, SortTitle: "andor", NeedsReview: year == 0,
		},
		Seasons: []store.SeasonTree{{
			SeasonNumber: 1, IdentityKey: key + "|s01",
			Episodes: []store.EpisodeTree{
				episodeTree("t1", key+"|s01e01", 1, 1, "f1", "/media/TV/Andor/Season 01/Andor - S01E01.mkv"),
			},
		}},
	}
}

func seedYearlessShow(t *testing.T) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)
	if err := db.UpsertShowTree(yearlessShowTree(0)); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	return db
}

func enrichShowYear(t *testing.T, db *store.DB, year int) {
	t.Helper()
	if err := db.WriteEntityEnrichment(store.EntityShow, "show1", store.EntityEnrichmentWrite{
		Overview: "x", Source: "tmdb", ExternalID: "83867", Year: year,
	}, nil); err != nil {
		t.Fatalf("enrich show: %v", err)
	}
}

func TestShowEnrichmentFillsYearAndClearsNeedsReview(t *testing.T) {
	db := seedYearlessShow(t)
	if _, review, _ := yearRow(t, db, "shows", "show1"); review != 1 {
		t.Fatalf("seed needs_review = %d, want 1", review)
	}
	enrichShowYear(t, db, 2022)
	year, review, _ := yearRow(t, db, "shows", "show1")
	if year.Int64 != 2022 || review != 0 {
		t.Fatalf("after enrich year=%v needs_review=%d, want 2022 / 0", year, review)
	}
}

func TestMovieEnrichmentFillsYearAndClearsNeedsReview(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)
	tree := movieTree("m1", "dune", "Dune", 0, "/media/Movies/Dune/Dune.mkv")
	tree.NeedsReview = true
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Source: "tmdb", ReleaseDate: "2021-10-22", Year: 2021,
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	year, review, _ := yearRow(t, db, "titles", "m1")
	if year.Int64 != 2021 || review != 0 {
		t.Fatalf("after enrich year=%v needs_review=%d, want 2021 / 0", year, review)
	}

	// The scanner re-resolves the same yearless folder.
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	year, review, _ = yearRow(t, db, "titles", "m1")
	if year.Int64 != 2021 || review != 0 {
		t.Fatalf("after rescan year=%v needs_review=%d, want 2021 / 0", year, review)
	}
}

func TestRescanOfYearlessFolderKeepsMetadataYear(t *testing.T) {
	db := seedYearlessShow(t)
	enrichShowYear(t, db, 2022)
	for i := 0; i < 2; i++ {
		if err := db.UpsertShowTree(yearlessShowTree(0)); err != nil {
			t.Fatalf("rescan: %v", err)
		}
		year, review, _ := yearRow(t, db, "shows", "show1")
		if year.Int64 != 2022 || review != 0 {
			t.Fatalf("rescan %d: year=%v needs_review=%d, want 2022 / 0", i, year, review)
		}
	}
}

func TestFolderYearBeatsMetadataYear(t *testing.T) {
	db := seedYearlessShow(t)
	enrichShowYear(t, db, 2021)
	// The folder gains a year (the same row: identity_key is rewritten by the
	// caller only on a Match override, so key the tree to the existing row).
	tree := yearlessShowTree(2022)
	tree.Show.IdentityKey = "andor"
	tree.Seasons[0].IdentityKey = "andor|s01"
	tree.Seasons[0].Episodes[0].IdentityKey = "andor|s01e01"
	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	year, review, _ := yearRow(t, db, "shows", "show1")
	if year.Int64 != 2022 || review != 0 {
		t.Fatalf("folder year lost: year=%v needs_review=%d, want 2022 / 0", year, review)
	}
	// A later enrichment must not replace the folder's year.
	enrichShowYear(t, db, 2019)
	if year, _, _ = yearRow(t, db, "shows", "show1"); year.Int64 != 2022 {
		t.Fatalf("metadata overrode folder year: %v", year)
	}
	// ...and a folder that loses its year again falls back to unknown, not to 2019.
	if err := db.UpsertShowTree(yearlessShowTree(0)); err != nil {
		t.Fatalf("yearless rescan: %v", err)
	}
	year, review, _ = yearRow(t, db, "shows", "show1")
	if year.Valid || review != 1 {
		t.Fatalf("year=%v needs_review=%d, want NULL / 1", year, review)
	}
}

func TestAdminIdentityFixBeatsMetadataYear(t *testing.T) {
	db := seedYearlessShow(t)
	enrichShowYear(t, db, 2019)
	if err := db.RekeyShowIdentity("show1", "Andor", 2022, "83867", "andor|2022"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	enrichShowYear(t, db, 2019)
	if year, _, _ := yearRow(t, db, "shows", "show1"); year.Int64 != 2022 {
		t.Fatalf("metadata overrode the Admin's year: %v", year)
	}
}

func TestReviewedRowsKeepTheirReviewState(t *testing.T) {
	db := seedYearlessShow(t)
	mustExec(t, db, `UPDATE shows SET reviewed = 1, needs_review = 0 WHERE id = 'show1'`)
	enrichShowYear(t, db, 2022)
	if err := db.UpsertShowTree(yearlessShowTree(0)); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	_, review, reviewed := yearRow(t, db, "shows", "show1")
	if review != 0 || reviewed != 1 {
		t.Fatalf("needs_review=%d reviewed=%d, want 0 / 1", review, reviewed)
	}
}

// The upgrade back-fills Movies from the release date they already store and
// queues every yearless matched Show for one re-enrichment (which fills the year
// from the provider on the next enrichment pass).
func TestMigrationBackfillsYearlessMatchedRows(t *testing.T) {
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
		`INSERT INTO libraries (id, name, kind) VALUES ('lm','M','movie'), ('lt','T','tv')`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, needs_review, release_date, enrichment_status)
		   VALUES ('m-date','lm','movie','A','a','a',1,'2021-10-22','matched'),
		          ('m-none','lm','movie','B','b','b',1,'','matched'),
		          ('m-year','lm','movie','C','c|1999','c',0,'2005-01-01','matched')`,
		`UPDATE titles SET year = 1999 WHERE id = 'm-year'`,
		`INSERT INTO shows (id, library_id, title, identity_key, sort_title, needs_review) VALUES
		   ('s-yl','lt','Y','y','y',1), ('s-yr','lt','Z','z|2000','z',0)`,
		`UPDATE shows SET year = 2000 WHERE id = 's-yr'`,
		`INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status) VALUES
		   ('show','s-yl','matched'), ('show','s-yr','matched')`,
	} {
		mustExec(t, db, q)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if y, r, _ := yearRow(t, db, "titles", "m-date"); y.Int64 != 2021 || r != 0 {
		t.Errorf("m-date year=%v needs_review=%d, want 2021 / 0", y, r)
	}
	if y, r, _ := yearRow(t, db, "titles", "m-none"); y.Valid || r != 1 {
		t.Errorf("m-none year=%v needs_review=%d, want NULL / 1", y, r)
	}
	if y, _, _ := yearRow(t, db, "titles", "m-year"); y.Int64 != 1999 {
		t.Errorf("m-year year=%v, want 1999 (folder year kept)", y)
	}
	status := func(id string) (s string) {
		if err := db.QueryRow(`SELECT enrichment_status FROM entity_enrichment WHERE entity_type='show' AND entity_id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("status %s: %v", id, err)
		}
		return s
	}
	if s := status("s-yl"); s != "pending" {
		t.Errorf("yearless Show status = %q, want pending (queued for re-enrichment)", s)
	}
	if s := status("s-yr"); s != "matched" {
		t.Errorf("Show with a year status = %q, want matched", s)
	}
}
