package store_test

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
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
// (enrichment-runtime-settings): an unset column reads back as a nil (unset) field
// distinct from a real 0; SetEnrichmentBehavior round-trips concrete values and
// leaves metadata_language intact; and the upgrade-backfill fills ONLY the NULL
// columns (preserving an existing value).
func TestEnrichmentBehaviorRoundTrip(t *testing.T) {
	db := openTemp(t)

	// A fresh (migrated) DB has no settings row → every field reads unset (nil), and
	// the resolver accessors default sensibly (auto true, interval/rate 0).
	beh, err := db.EnrichmentBehavior()
	if err != nil {
		t.Fatalf("EnrichmentBehavior on fresh DB: %v", err)
	}
	if beh.AutoEnrichAfterScan != nil || beh.EnrichIntervalSeconds != nil || beh.MusicBrainzRateLimitMs != nil {
		t.Errorf("fresh behavior = %+v, want all-nil (unset)", beh)
	}
	if !beh.Auto() || beh.IntervalSeconds() != 0 || beh.RateLimitMs() != 0 {
		t.Errorf("fresh resolved = auto %v/interval %d/rate %d, want true/0/0", beh.Auto(), beh.IntervalSeconds(), beh.RateLimitMs())
	}

	// Write concrete values; they round-trip as non-nil fields.
	if err := db.SetEnrichmentBehavior(false, 3600, 250); err != nil {
		t.Fatalf("SetEnrichmentBehavior: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if beh.AutoEnrichAfterScan == nil || *beh.AutoEnrichAfterScan != false ||
		beh.EnrichIntervalSeconds == nil || *beh.EnrichIntervalSeconds != 3600 ||
		beh.MusicBrainzRateLimitMs == nil || *beh.MusicBrainzRateLimitMs != 250 {
		t.Errorf("behavior after set = %+v, want false/3600/250 (all set)", beh)
	}
	// A real 0 is distinguishable from unset — it reads back as a set field.
	if err := db.SetEnrichmentBehavior(true, 0, 0); err != nil {
		t.Fatalf("SetEnrichmentBehavior zeros: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if beh.EnrichIntervalSeconds == nil || *beh.EnrichIntervalSeconds != 0 ||
		beh.MusicBrainzRateLimitMs == nil || *beh.MusicBrainzRateLimitMs != 0 {
		t.Errorf("behavior after zero-set = %+v, want 0/0 as SET (not nil)", beh)
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

// TestBackfillEnrichmentBehaviorIfUnset proves the upgrade backfill fills ONLY the
// NULL columns from the config-derived seed, preserving an operator's already-set
// value (so a restart never reverts a UI change).
func TestBackfillEnrichmentBehaviorIfUnset(t *testing.T) {
	db := openTemp(t)

	// Simulate the upgrade state: a 0018 row exists (language set) but the 0019
	// behavior columns are NULL (never written).
	if err := db.SetMetadataLanguage("en-US"); err != nil {
		t.Fatalf("SetMetadataLanguage: %v", err)
	}
	beh, _ := db.EnrichmentBehavior()
	if beh.AutoEnrichAfterScan != nil || beh.EnrichIntervalSeconds != nil || beh.MusicBrainzRateLimitMs != nil {
		t.Fatalf("precondition: behavior columns = %+v, want all-nil (upgrade state)", beh)
	}

	// Backfill fills every NULL column from config-derived seed values.
	if err := db.BackfillEnrichmentBehaviorIfUnset(true, 21600, 1000); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if !beh.Auto() || beh.IntervalSeconds() != 21600 || beh.RateLimitMs() != 1000 {
		t.Errorf("after backfill = auto %v/%d/%d, want true/21600/1000", beh.Auto(), beh.IntervalSeconds(), beh.RateLimitMs())
	}

	// An operator then changes the interval (a UI save). A LATER boot's backfill (a
	// different config seed) must NOT revert it — COALESCE keeps the set value.
	if err := db.SetEnrichmentBehavior(false, 120, 2000); err != nil {
		t.Fatalf("SetEnrichmentBehavior (UI save): %v", err)
	}
	if err := db.BackfillEnrichmentBehaviorIfUnset(true, 21600, 1000); err != nil {
		t.Fatalf("Backfill again: %v", err)
	}
	beh, _ = db.EnrichmentBehavior()
	if beh.Auto() || beh.IntervalSeconds() != 120 || beh.RateLimitMs() != 2000 {
		t.Errorf("after second backfill = auto %v/%d/%d, want false/120/2000 (preserved, not reverted)", beh.Auto(), beh.IntervalSeconds(), beh.RateLimitMs())
	}
}
