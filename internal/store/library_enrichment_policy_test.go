package store_test

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// makeLibrary creates a Library (no roots) and returns its id, for the policy
// tests that need a real Library to attach a policy to.
func makeLibrary(t *testing.T, db *store.DB, id, name, kind string) string {
	t.Helper()
	if _, err := db.CreateLibrary(id, name, kind, nil); err != nil {
		t.Fatalf("create library %q: %v", id, err)
	}
	return id
}

func boolPtr(b bool) *bool { return &b }

// TestLibraryEnrichmentPolicyRoundTrip proves the sparse-storage contract: an
// absent row reads as the empty policy (inherit), a set value round-trips, and
// clearing back to inherit is distinguishable from a stored value (NULL, not a
// sentinel) — the Model A invariant (ADR-0027).
func TestLibraryEnrichmentPolicyRoundTrip(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-1", "Movies", "movie")

	// No row yet: empty policy, every key inherits.
	pol, err := db.LibraryEnrichmentPolicy(lib)
	if err != nil {
		t.Fatalf("read empty policy: %v", err)
	}
	if pol.EnrichEnabled != nil {
		t.Errorf("empty policy EnrichEnabled = %v, want nil (inherit)", *pol.EnrichEnabled)
	}

	// Set enrich_enabled = false (a deliberate override).
	if err := db.SetLibraryEnrichEnabled(lib, boolPtr(false)); err != nil {
		t.Fatalf("set enrich_enabled=false: %v", err)
	}
	pol, err = db.LibraryEnrichmentPolicy(lib)
	if err != nil {
		t.Fatalf("read after set false: %v", err)
	}
	if pol.EnrichEnabled == nil || *pol.EnrichEnabled != false {
		t.Errorf("EnrichEnabled = %v, want a stored false", pol.EnrichEnabled)
	}

	// Flip to true (partial re-set of the same key).
	if err := db.SetLibraryEnrichEnabled(lib, boolPtr(true)); err != nil {
		t.Fatalf("set enrich_enabled=true: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.EnrichEnabled == nil || *pol.EnrichEnabled != true {
		t.Errorf("EnrichEnabled = %v, want a stored true", pol.EnrichEnabled)
	}

	// Clear back to inherit: NULL, distinguishable from "set to false".
	if err := db.SetLibraryEnrichEnabled(lib, nil); err != nil {
		t.Fatalf("clear enrich_enabled: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.EnrichEnabled != nil {
		t.Errorf("after clear EnrichEnabled = %v, want nil (inherit, not a stored value)", *pol.EnrichEnabled)
	}
}

func strPtr(s string) *string { return &s }

// TestLibraryEnrichmentPolicyLanguageRoundTrip proves the metadata-language key
// (issue 02) round-trips sparsely and coexists with enrich_enabled on the same row:
// setting one leaves the other intact, and clearing a key back to inherit is NULL
// (not a sentinel), so inherit stays distinguishable from a deliberate value.
func TestLibraryEnrichmentPolicyLanguageRoundTrip(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-lang", "Foreign Film", "movie")

	// Empty policy: language inherits (nil).
	pol, err := db.LibraryEnrichmentPolicy(lib)
	if err != nil {
		t.Fatalf("read empty policy: %v", err)
	}
	if pol.MetadataLanguage != nil {
		t.Errorf("empty policy MetadataLanguage = %q, want nil (inherit)", *pol.MetadataLanguage)
	}

	// Set a deliberate language override.
	if err := db.SetLibraryMetadataLanguage(lib, strPtr("ja-JP")); err != nil {
		t.Fatalf("set metadata_language: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.MetadataLanguage == nil || *pol.MetadataLanguage != "ja-JP" {
		t.Errorf("MetadataLanguage = %v, want a stored ja-JP", pol.MetadataLanguage)
	}

	// Setting enrich_enabled on the same Library leaves the language override intact
	// (the two keys share one row but are written column-independently).
	if err := db.SetLibraryEnrichEnabled(lib, boolPtr(false)); err != nil {
		t.Fatalf("set enrich_enabled: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.MetadataLanguage == nil || *pol.MetadataLanguage != "ja-JP" {
		t.Errorf("language clobbered by an enrich_enabled write: %v", pol.MetadataLanguage)
	}
	if pol.EnrichEnabled == nil || *pol.EnrichEnabled != false {
		t.Errorf("EnrichEnabled = %v, want stored false alongside the language", pol.EnrichEnabled)
	}

	// Clear the language back to inherit: NULL, and enrich_enabled is untouched.
	if err := db.SetLibraryMetadataLanguage(lib, nil); err != nil {
		t.Fatalf("clear metadata_language: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.MetadataLanguage != nil {
		t.Errorf("after clear MetadataLanguage = %v, want nil (inherit)", *pol.MetadataLanguage)
	}
	if pol.EnrichEnabled == nil || *pol.EnrichEnabled != false {
		t.Errorf("clearing language disturbed enrich_enabled: %v", pol.EnrichEnabled)
	}
}

// TestLibraryEnrichmentPolicyAuthoritativeRoundTrip proves the authoritative-
// provider pointer (issue 03) round-trips sparsely and coexists with the other keys
// on the same row: NULL = inherit the kind default, a slug is a deliberate override,
// and clearing is distinguishable from a set value.
func TestLibraryEnrichmentPolicyAuthoritativeRoundTrip(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-auth", "Anime", "tv")

	// Empty: authoritative inherits (nil).
	pol, err := db.LibraryEnrichmentPolicy(lib)
	if err != nil {
		t.Fatalf("read empty policy: %v", err)
	}
	if pol.AuthoritativeProvider != nil {
		t.Errorf("empty AuthoritativeProvider = %q, want nil (inherit)", *pol.AuthoritativeProvider)
	}

	// Point it at a slug.
	if err := db.SetLibraryAuthoritativeProvider(lib, strPtr("anidb")); err != nil {
		t.Fatalf("set authoritative: %v", err)
	}
	// Set the other keys too — they must coexist on the one row.
	if err := db.SetLibraryEnrichEnabled(lib, boolPtr(true)); err != nil {
		t.Fatalf("set enrich_enabled: %v", err)
	}
	if err := db.SetLibraryMetadataLanguage(lib, strPtr("ja-JP")); err != nil {
		t.Fatalf("set language: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.AuthoritativeProvider == nil || *pol.AuthoritativeProvider != "anidb" {
		t.Errorf("AuthoritativeProvider = %v, want anidb", pol.AuthoritativeProvider)
	}
	if pol.EnrichEnabled == nil || !*pol.EnrichEnabled || pol.MetadataLanguage == nil || *pol.MetadataLanguage != "ja-JP" {
		t.Errorf("coexisting keys clobbered: enrich=%v lang=%v", pol.EnrichEnabled, pol.MetadataLanguage)
	}

	// Clear back to inherit: NULL, other keys untouched.
	if err := db.SetLibraryAuthoritativeProvider(lib, nil); err != nil {
		t.Fatalf("clear authoritative: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if pol.AuthoritativeProvider != nil {
		t.Errorf("after clear AuthoritativeProvider = %v, want nil (inherit)", pol.AuthoritativeProvider)
	}
	if pol.MetadataLanguage == nil || *pol.MetadataLanguage != "ja-JP" {
		t.Errorf("clearing authoritative disturbed language: %v", pol.MetadataLanguage)
	}
}

// TestLibraryProviderOverrideRoundTrip proves the per-provider Supplement tri-state
// (issue 05) round-trips by ROW PRESENCE: a set row is a forced on/off, clearing
// deletes the row (inherit), and the overrides surface on the policy read as a map
// where an absent slug means inherit.
func TestLibraryProviderOverrideRoundTrip(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-sup", "Docs", "movie")

	// Empty: no overrides on the policy (nil map = inherit every provider).
	pol, err := db.LibraryEnrichmentPolicy(lib)
	if err != nil {
		t.Fatalf("read empty policy: %v", err)
	}
	if len(pol.SupplementOverrides) != 0 {
		t.Errorf("empty SupplementOverrides = %v, want none", pol.SupplementOverrides)
	}

	// Force OMDb on and TheTVDB off.
	if err := db.SetLibraryProviderOverride(lib, "omdb", boolPtr(true)); err != nil {
		t.Fatalf("force omdb on: %v", err)
	}
	if err := db.SetLibraryProviderOverride(lib, "thetvdb", boolPtr(false)); err != nil {
		t.Fatalf("force thetvdb off: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if v, ok := pol.SupplementOverrides["omdb"]; !ok || !v {
		t.Errorf("omdb override = %v/%v, want forced on", v, ok)
	}
	if v, ok := pol.SupplementOverrides["thetvdb"]; !ok || v {
		t.Errorf("thetvdb override = %v/%v, want forced off", v, ok)
	}
	// A provider with no row inherits (absent from the map).
	if _, ok := pol.SupplementOverrides["fanarttv"]; ok {
		t.Errorf("fanarttv should be absent (inherit), got a stored override")
	}

	// Clear OMDb back to inherit: the row is deleted (absent = inherit), leaving
	// TheTVDB's override intact.
	if err := db.SetLibraryProviderOverride(lib, "omdb", nil); err != nil {
		t.Fatalf("clear omdb: %v", err)
	}
	pol, _ = db.LibraryEnrichmentPolicy(lib)
	if _, ok := pol.SupplementOverrides["omdb"]; ok {
		t.Errorf("omdb still present after clear, want inherit (row deleted)")
	}
	if v, ok := pol.SupplementOverrides["thetvdb"]; !ok || v {
		t.Errorf("thetvdb override disturbed by clearing omdb: %v/%v", v, ok)
	}
}

// TestLibraryProviderOverrideCascade proves deleting a Library drops its provider
// override rows (ON DELETE CASCADE).
func TestLibraryProviderOverrideCascade(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-sup-c", "Docs", "movie")
	if err := db.SetLibraryProviderOverride(lib, "omdb", boolPtr(false)); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if err := db.DeleteLibrary(lib); err != nil {
		t.Fatalf("delete library: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM library_provider_override WHERE library_id = ?`, lib).Scan(&n); err != nil {
		t.Fatalf("count override rows: %v", err)
	}
	if n != 0 {
		t.Errorf("override rows after library delete = %d, want 0 (cascade)", n)
	}
}

// TestLibraryEnrichmentPolicyIsolation proves a policy on one Library never leaks
// into another (other Libraries stay on inherit).
func TestLibraryEnrichmentPolicyIsolation(t *testing.T) {
	db := openTemp(t)
	a := makeLibrary(t, db, "lib-a", "Home Videos", "movie")
	b := makeLibrary(t, db, "lib-b", "Films", "movie")

	if err := db.SetLibraryEnrichEnabled(a, boolPtr(false)); err != nil {
		t.Fatalf("set policy on a: %v", err)
	}

	polB, err := db.LibraryEnrichmentPolicy(b)
	if err != nil {
		t.Fatalf("read b: %v", err)
	}
	if polB.EnrichEnabled != nil {
		t.Errorf("library b picked up library a's override: %v", *polB.EnrichEnabled)
	}
}

// TestLibraryEnrichmentPolicyCascade proves deleting a Library drops its policy
// row (ON DELETE CASCADE; foreign_keys is ON), leaving no orphan.
func TestLibraryEnrichmentPolicyCascade(t *testing.T) {
	db := openTemp(t)
	lib := makeLibrary(t, db, "lib-c", "Anime", "tv")
	if err := db.SetLibraryEnrichEnabled(lib, boolPtr(false)); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	if err := db.DeleteLibrary(lib); err != nil {
		t.Fatalf("delete library: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM library_enrichment_policy WHERE library_id = ?`, lib).Scan(&n); err != nil {
		t.Fatalf("count policy rows: %v", err)
	}
	if n != 0 {
		t.Errorf("policy rows after library delete = %d, want 0 (cascade)", n)
	}
}
