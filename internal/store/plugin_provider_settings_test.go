package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The OPERATOR's side of an uninstall (.scratch/plugin-system issue 17). Issue 16
// closed the guest's side — the key-value namespace goes with the plugins,
// plugin_settings and event_sinks rows — and left four tables holding rows an
// operator typed: an Installed Metadata provider's enable switch, key and base URL
// in metadata_providers, a Subtitle provider's in subtitle_providers, and the
// per-Library Enrichment policy's two shapes of slug (ADR-0027) in
// library_provider_override and library_enrichment_policy.authoritative_provider.
//
// The policy is UNINSTALL FORGETS, DISABLE REMEMBERS, and the whole of it is a
// WHERE clause per statement. That is exactly why it is tested here, slug by slug,
// as well as through the settings API: the way to break it is to widen one WHERE
// clause, and the eight Built-ins' rows live in the same tables under the same
// column.

// The two Built-in slugs whose rows an uninstall must never touch — one per
// provider table. They are spelled literally rather than imported from the enrich
// and subfetch registries because what is under test is a column value: a store
// that started deleting by something other than the slug would still compile
// against those constants.
const (
	builtinMetadataSlug = "tmdb"
	builtinSubtitleSlug = "opensubtitles"
)

// TestDeletePluginTakesTheOperatorsProviderRowsWithIt is the acceptance criteria
// at the layer the DELETE lives on: uninstalling `alpha` takes its provider
// settings and its Library overrides, and leaves `beta`'s and the Built-ins'
// exactly as they were.
func TestDeletePluginTakesTheOperatorsProviderRowsWithIt(t *testing.T) {
	db := openTemp(t)
	seedTwoPlugins(t, db)
	led := makeLibrary(t, db, "lib-led", "Anime", "movie")
	other := makeLibrary(t, db, "lib-other", "Documentaries", "movie")
	seedOperatorProviderRows(t, db, led, other)

	if err := db.DeletePlugin("alpha"); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}

	// metadata_providers: alpha's row is gone, and only alpha's.
	md := metadataProvidersBySlug(t, db)
	if row, ok := md["alpha"]; ok {
		t.Errorf("alpha's metadata_providers row survived its uninstall: %+v", row)
	}
	for _, slug := range []string{builtinMetadataSlug, "beta"} {
		row, ok := md[slug]
		if !ok {
			t.Errorf("%s's metadata_providers row was deleted with alpha's", slug)
			continue
		}
		if !row.Enabled || row.APIKey != slug+"-key" || row.BaseURL != baseURLFor(slug) {
			t.Errorf("%s's metadata_providers row changed when alpha was uninstalled: %+v", slug, row)
		}
	}

	// subtitle_providers: the same property in the other table.
	subs := subtitleProvidersBySlug(t, db)
	if row, ok := subs["alpha"]; ok {
		t.Errorf("alpha's subtitle_providers row survived its uninstall: %+v", row)
	}
	for _, slug := range []string{builtinSubtitleSlug, "beta"} {
		row, ok := subs[slug]
		if !ok {
			t.Errorf("%s's subtitle_providers row was deleted with alpha's", slug)
			continue
		}
		if !row.Enabled || row.APIKey != slug+"-key" || row.BaseURL != baseURLFor(slug) {
			t.Errorf("%s's subtitle_providers row changed when alpha was uninstalled: %+v", slug, row)
		}
	}

	// The Library alpha led falls back to the kind default (a NULL pointer), and
	// the Supplement override naming alpha is gone — but the OTHER keys on the same
	// policy row are untouched, which is why the pointer is cleared rather than the
	// row deleted.
	pol, err := db.LibraryEnrichmentPolicy(led)
	if err != nil {
		t.Fatalf("reading the led Library's policy: %v", err)
	}
	if pol.AuthoritativeProvider != nil {
		t.Errorf("the lead pointer still names %q after that Plugin was uninstalled, want NULL (inherit)",
			*pol.AuthoritativeProvider)
	}
	if pol.EnrichEnabled == nil || !*pol.EnrichEnabled {
		t.Errorf("EnrichEnabled = %v after an uninstall cleared the pointer beside it, want the stored true",
			pol.EnrichEnabled)
	}
	if _, ok := pol.SupplementOverrides["alpha"]; ok {
		t.Errorf("a Supplement override still names the uninstalled alpha: %+v", pol.SupplementOverrides)
	}
	for _, slug := range []string{builtinMetadataSlug, "beta"} {
		if forced, ok := pol.SupplementOverrides[slug]; !ok || forced {
			t.Errorf("%s's Supplement override = (%v, present=%v), want its stored force-off", slug, forced, ok)
		}
	}

	// A DIFFERENT Library led by beta keeps its pointer: the UPDATE is keyed to the
	// uninstalled slug, not to "any Library with a lead".
	otherPol, err := db.LibraryEnrichmentPolicy(other)
	if err != nil {
		t.Fatalf("reading the other Library's policy: %v", err)
	}
	if otherPol.AuthoritativeProvider == nil || *otherPol.AuthoritativeProvider != "beta" {
		t.Errorf("a Library led by beta lost its pointer when alpha was uninstalled: %v",
			otherPol.AuthoritativeProvider)
	}
	if _, ok := otherPol.SupplementOverrides["beta"]; !ok {
		t.Errorf("the other Library's beta override went with alpha: %+v", otherPol.SupplementOverrides)
	}
}

// TestAFailedUninstallLeavesTheOperatorsProviderRows extends issue 16's rollback
// property over the four statements issue 17 added: they are in the SAME
// transaction, so a Plugin is uninstalled or it is not. The failure is staged the
// only way a store test honestly can — by taking away the table the last statement
// needs.
func TestAFailedUninstallLeavesTheOperatorsProviderRows(t *testing.T) {
	db := openTemp(t)
	seedTwoPlugins(t, db)
	led := makeLibrary(t, db, "lib-led", "Anime", "movie")
	seedOperatorProviderRows(t, db, led)
	mustExec(t, db, `DROP TABLE plugins`)

	if err := db.DeletePlugin("alpha"); err == nil {
		t.Fatal("DeletePlugin reported success with its last statement unable to run")
	}

	if row, ok := metadataProvidersBySlug(t, db)["alpha"]; !ok || row.APIKey != "alpha-key" {
		t.Errorf("alpha's metadata_providers row = (%+v, present=%v) after a failed uninstall, want it untouched", row, ok)
	}
	if row, ok := subtitleProvidersBySlug(t, db)["alpha"]; !ok || row.APIKey != "alpha-key" {
		t.Errorf("alpha's subtitle_providers row = (%+v, present=%v) after a failed uninstall, want it untouched", row, ok)
	}
	pol, err := db.LibraryEnrichmentPolicy(led)
	if err != nil {
		t.Fatalf("reading the policy after a failed uninstall: %v", err)
	}
	if pol.AuthoritativeProvider == nil || *pol.AuthoritativeProvider != "alpha" {
		t.Errorf("the lead pointer = %v after a failed uninstall, want it still naming alpha", pol.AuthoritativeProvider)
	}
	if _, ok := pol.SupplementOverrides["alpha"]; !ok {
		t.Errorf("alpha's Supplement override was rolled forward rather than back: %+v", pol.SupplementOverrides)
	}
}

// seedOperatorProviderRows gives every slug a row in both provider tables and
// every named Library a policy that names one — a Built-in, the Plugin about to be
// uninstalled, and a second Installed Plugin, so every assertion can be made slug
// by slug. The first Library is the one `alpha` leads; any others lead `beta`.
func seedOperatorProviderRows(t *testing.T, db *store.DB, libraryIDs ...string) {
	t.Helper()
	for _, slug := range []string{builtinMetadataSlug, "alpha", "beta"} {
		if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: slug, Enabled: true, APIKey: slug + "-key", BaseURL: baseURLFor(slug),
		}); err != nil {
			t.Fatalf("seeding the metadata_providers row for %s: %v", slug, err)
		}
	}
	for _, slug := range []string{builtinSubtitleSlug, "alpha", "beta"} {
		if err := db.UpsertSubtitleProvider(store.SubtitleProviderUpsert{
			Slug: slug, Enabled: true, APIKey: slug + "-key", BaseURL: baseURLFor(slug),
		}); err != nil {
			t.Fatalf("seeding the subtitle_providers row for %s: %v", slug, err)
		}
	}
	for i, id := range libraryIDs {
		lead := "beta"
		if i == 0 {
			lead = "alpha"
		}
		// A co-resident key on the same policy row: clearing the pointer must not take
		// it with it.
		if err := db.SetLibraryEnrichEnabled(id, boolPtr(true)); err != nil {
			t.Fatalf("seeding enrich_enabled for %s: %v", id, err)
		}
		if err := db.SetLibraryAuthoritativeProvider(id, strPtr(lead)); err != nil {
			t.Fatalf("seeding the lead pointer for %s: %v", id, err)
		}
		for _, slug := range []string{builtinMetadataSlug, "alpha", "beta"} {
			if err := db.SetLibraryProviderOverride(id, slug, boolPtr(false)); err != nil {
				t.Fatalf("seeding %s's override of %s: %v", id, slug, err)
			}
		}
	}
}

func baseURLFor(slug string) string { return "https://" + slug + ".example.test/v1" }

func metadataProvidersBySlug(t *testing.T, db *store.DB) map[string]store.MetadataProviderRow {
	t.Helper()
	rows, err := db.MetadataProviders()
	if err != nil {
		t.Fatalf("listing metadata providers: %v", err)
	}
	out := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out
}

func subtitleProvidersBySlug(t *testing.T, db *store.DB) map[string]store.SubtitleProviderRow {
	t.Helper()
	rows, err := db.SubtitleProviders()
	if err != nil {
		t.Fatalf("listing subtitle providers: %v", err)
	}
	out := make(map[string]store.SubtitleProviderRow, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out
}
