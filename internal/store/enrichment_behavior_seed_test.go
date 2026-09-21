package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
)

// TestSeedIfEmptyRealFirstBootPersistsBehavior pins the production first-boot
// path — enrich.SeedIfEmpty against a REAL store.DB, not the fakeSeedStore
// internal/enrich/manager_test.go drives — writing the three behavior columns.
// It lives here rather than in internal/enrich because a real DB needs
// openTemp's migrated temp file; store_test (this file's package, an external
// test package) importing internal/enrich back is not a cycle, since the
// production internal/store package never imports internal/enrich.
func TestSeedIfEmptyRealFirstBootPersistsBehavior(t *testing.T) {
	db := openTemp(t)

	// All three values differ from the DDL defaults (true/0/0), so a bug that
	// left the defaults in place would not go unnoticed.
	seeded, err := enrich.SeedIfEmpty(db, enrich.SeedInput{
		MetadataLanguage:       "en-US",
		AutoEnrichAfterScan:    false,
		EnrichIntervalSeconds:  3600,
		MusicBrainzRateLimitMs: 1500,
	})
	if err != nil || !seeded {
		t.Fatalf("SeedIfEmpty = %v, %v; want true, nil", seeded, err)
	}

	var auto bool
	var interval, rate int
	if err := db.QueryRow(
		`SELECT auto_enrich_after_scan, enrich_interval_seconds, musicbrainz_rate_limit_ms
		   FROM metadata_settings WHERE id = 1`).Scan(&auto, &interval, &rate); err != nil {
		t.Fatalf("raw read of the seeded row: %v", err)
	}
	if auto || interval != 3600 || rate != 1500 {
		t.Errorf("raw columns = auto %v/interval %d/rate %d, want false/3600/1500", auto, interval, rate)
	}

	beh, err := db.EnrichmentBehavior()
	if err != nil {
		t.Fatalf("EnrichmentBehavior: %v", err)
	}
	if beh.AutoEnrichAfterScan || beh.EnrichIntervalSeconds != 3600 || beh.MusicBrainzRateLimitMs != 1500 {
		t.Errorf("EnrichmentBehavior = %+v, want false/3600/1500", beh)
	}
}
