package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestMetadataProvidersRoundTrip covers the DB-backed provider settings
// (metadata-providers 02): a fresh DB reports empty, an upsert inserts and then
// replaces a row (idempotently), an empty api_key/base_url reads back as "", and
// the singleton language round-trips.
func TestMetadataProvidersRoundTrip(t *testing.T) {
	db := openTemp(t)

	// A migrated-but-unwritten DB has no settings — the first-boot seed signal.
	empty, err := db.MetadataSettingsEmpty()
	if err != nil {
		t.Fatalf("MetadataSettingsEmpty: %v", err)
	}
	if !empty {
		t.Fatalf("fresh DB: MetadataSettingsEmpty = false, want true")
	}
	if rows, err := db.MetadataProviders(); err != nil || len(rows) != 0 {
		t.Fatalf("fresh DB providers = %v (err %v), want none", rows, err)
	}
	if lang, err := db.MetadataLanguage(); err != nil || lang != "" {
		t.Fatalf("fresh DB language = %q (err %v), want \"\"", lang, err)
	}

	// Insert a keyed provider with both a base-URL and an image-host override.
	if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
		Slug: "tmdb", Enabled: true, APIKey: "k1",
		BaseURL: "http://stub.local", ImageBaseURL: "http://img.stub.local",
	}); err != nil {
		t.Fatalf("upsert tmdb: %v", err)
	}
	// Insert a keyless provider with no override — both nullable columns empty.
	if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
		Slug: "musicbrainz", Enabled: true,
	}); err != nil {
		t.Fatalf("upsert musicbrainz: %v", err)
	}

	if seeded, err := db.MetadataSettingsEmpty(); err != nil || seeded {
		t.Fatalf("after upsert: MetadataSettingsEmpty = %v (err %v), want false", seeded, err)
	}

	rows, err := db.MetadataProviders()
	if err != nil {
		t.Fatalf("MetadataProviders: %v", err)
	}
	got := map[string]store.MetadataProviderRow{}
	for _, r := range rows {
		got[r.Slug] = r
	}
	if r := got["tmdb"]; !r.Enabled || r.APIKey != "k1" || r.BaseURL != "http://stub.local" || r.ImageBaseURL != "http://img.stub.local" {
		t.Errorf("tmdb row = %+v, want enabled/k1/stub/img", r)
	}
	if r := got["musicbrainz"]; !r.Enabled || r.APIKey != "" || r.BaseURL != "" || r.ImageBaseURL != "" {
		t.Errorf("musicbrainz row = %+v, want enabled with empty key/baseURL/imageBaseURL", r)
	}
	// Rows are slug-ordered.
	if rows[0].Slug != "musicbrainz" || rows[1].Slug != "tmdb" {
		t.Errorf("rows not slug-ordered: %q, %q", rows[0].Slug, rows[1].Slug)
	}

	// Replace tmdb: disable it and clear the key (idempotent upsert path).
	if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
		Slug: "tmdb", Enabled: false, APIKey: "", BaseURL: "",
	}); err != nil {
		t.Fatalf("re-upsert tmdb: %v", err)
	}
	rows, _ = db.MetadataProviders()
	if len(rows) != 2 {
		t.Fatalf("after replace: %d rows, want 2 (upsert, not insert)", len(rows))
	}
	for _, r := range rows {
		if r.Slug == "tmdb" && (r.Enabled || r.APIKey != "" || r.BaseURL != "" || r.ImageBaseURL != "") {
			t.Errorf("tmdb after clear = %+v, want disabled with empty key/baseURL/imageBaseURL", r)
		}
	}

	// Singleton language round-trips and overwrites.
	if err := db.SetMetadataLanguage("fr-FR"); err != nil {
		t.Fatalf("SetMetadataLanguage: %v", err)
	}
	if lang, _ := db.MetadataLanguage(); lang != "fr-FR" {
		t.Errorf("language = %q, want fr-FR", lang)
	}
	if err := db.SetMetadataLanguage("de-DE"); err != nil {
		t.Fatalf("SetMetadataLanguage 2: %v", err)
	}
	if lang, _ := db.MetadataLanguage(); lang != "de-DE" {
		t.Errorf("language after overwrite = %q, want de-DE", lang)
	}
}

// TestEnrichmentBehaviorRoundTrip covers the three behavior knobs
// (enrichment-runtime-settings, NOT NULL columns): a missing row reads back the
// same defaults the DDL gives an inserted row (auto true, interval/rate 0);
// SetEnrichmentBehavior round-trips concrete values, including 0, and leaves
// metadata_language intact.
func TestEnrichmentBehaviorRoundTrip(t *testing.T) {
	db := openTemp(t)

	// A fresh (migrated) DB has no settings row → resolves to the DDL defaults.
	beh, err := db.EnrichmentBehavior()
	if err != nil {
		t.Fatalf("EnrichmentBehavior on fresh DB: %v", err)
	}
	if !beh.AutoEnrichAfterScan || beh.EnrichIntervalSeconds != 0 || beh.MusicBrainzRateLimitMs != 0 {
		t.Errorf("fresh behavior = %+v, want true/0/0", beh)
	}

	// Write concrete values; they round-trip.
	if err := db.SetEnrichmentBehavior(false, 3600, 250); err != nil {
		t.Fatalf("SetEnrichmentBehavior: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if beh.AutoEnrichAfterScan || beh.EnrichIntervalSeconds != 3600 || beh.MusicBrainzRateLimitMs != 250 {
		t.Errorf("behavior after set = %+v, want false/3600/250", beh)
	}
	// A real 0 round-trips like any other value.
	if err := db.SetEnrichmentBehavior(true, 0, 0); err != nil {
		t.Fatalf("SetEnrichmentBehavior zeros: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if beh.EnrichIntervalSeconds != 0 || beh.MusicBrainzRateLimitMs != 0 {
		t.Errorf("behavior after zero-set = %+v, want 0/0", beh)
	}

	// SetEnrichmentBehavior touches only the three columns, leaving language intact.
	if err := db.SetMetadataLanguage("es-ES"); err != nil {
		t.Fatalf("SetMetadataLanguage: %v", err)
	}
	if err := db.SetEnrichmentBehavior(true, 60, 500); err != nil {
		t.Fatalf("SetEnrichmentBehavior after language: %v", err)
	}
	if lang, _ := db.MetadataLanguage(); lang != "es-ES" {
		t.Errorf("language after SetEnrichmentBehavior = %q, want es-ES (untouched)", lang)
	}
}

// TestEnrichmentBehaviorColumnsAreNotNull pins the schema (0001_init.sql): each
// of the three behavior columns rejects an explicit SQL NULL. Setting
// auto_enrich_after_scan's declaration back to bare INTEGER (no NOT NULL) makes
// this fail on that column alone, confirming the assertion is live.
func TestEnrichmentBehaviorColumnsAreNotNull(t *testing.T) {
	db := openTemp(t)
	if err := db.SetEnrichmentBehavior(true, 60, 500); err != nil {
		t.Fatalf("SetEnrichmentBehavior: %v", err)
	}
	for _, col := range []string{"auto_enrich_after_scan", "enrich_interval_seconds", "musicbrainz_rate_limit_ms"} {
		if _, err := db.Exec("UPDATE metadata_settings SET " + col + " = NULL WHERE id = 1"); err == nil {
			t.Errorf("UPDATE %s = NULL succeeded, want a NOT NULL constraint failure", col)
		}
	}
}

// TestEnrichmentBehaviorNoRowMatchesConsentOnlyRow pins the no-row defaults
// EnrichmentBehavior() returns in Go against the DDL defaults themselves,
// rather than restating the numbers a second time: a fresh DB with no
// metadata_settings row at all must read back identically to a DB where
// SetEnrichmentConsent alone (never SetEnrichmentBehavior) created row 1, since
// that INSERT never names the three columns and takes the DDL defaults. If a
// DDL default drifted from the Go no-row fallback, only the first DB would show
// it.
func TestEnrichmentBehaviorNoRowMatchesConsentOnlyRow(t *testing.T) {
	noRow := openTemp(t)
	want, err := noRow.EnrichmentBehavior()
	if err != nil {
		t.Fatalf("EnrichmentBehavior (no row): %v", err)
	}

	consentOnly := openTemp(t)
	if err := consentOnly.SetEnrichmentConsent(true); err != nil {
		t.Fatalf("SetEnrichmentConsent: %v", err)
	}
	got, err := consentOnly.EnrichmentBehavior()
	if err != nil {
		t.Fatalf("EnrichmentBehavior (consent-only row): %v", err)
	}
	if got != want {
		t.Errorf("consent-only row behavior = %+v, want %+v (the no-row default)", got, want)
	}
}
