package app

import (
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/config"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/store"
)

// TestNewKeyRotatorGating covers the "when is the rotation channel OFF" matrix
// (ADR-0032): a build-from-source binary (no enc key) never runs it, the disable
// flag turns it off, a full-BYOK operator bypasses it, and a partial-BYOK operator
// still runs it (for the un-BYOK'd provider). The url/enc-key overrides model the
// black-box test seam (and force the channel on).
func TestNewKeyRotatorGating(t *testing.T) {
	base := config.Defaults()
	base.KeyRotationURL = "https://rotate.example/v1/keys"

	cases := []struct {
		name        string
		mutate      func(c *config.Config)
		encOverride string
		urlOverride string
		wantNil     bool
	}{
		{
			name:        "enabled with enc key runs",
			encOverride: "enc",
			wantNil:     false,
		},
		{
			name:    "no enc key (build-from-source) is off",
			wantNil: true, // config.AppEncKey() is empty in tests and no override given
		},
		{
			name:        "disable flag is off",
			mutate:      func(c *config.Config) { c.KeyRotationEnabled = false },
			encOverride: "enc",
			wantNil:     true,
		},
		{
			name:        "no URL is off",
			mutate:      func(c *config.Config) { c.KeyRotationURL = "" },
			encOverride: "enc",
			wantNil:     true,
		},
		{
			name:        "full BYOK bypasses",
			mutate:      func(c *config.Config) { c.TMDBAPIKey, c.FanartTVAPIKey = "op-t", "op-f" },
			encOverride: "enc",
			wantNil:     true,
		},
		{
			name:        "partial BYOK still runs",
			mutate:      func(c *config.Config) { c.TMDBAPIKey = "op-t" },
			encOverride: "enc",
			wantNil:     false,
		},
		{
			name:        "url override forces on despite disable flag",
			mutate:      func(c *config.Config) { c.KeyRotationEnabled = false },
			encOverride: "enc",
			urlOverride: "https://stub.example/keys",
			wantNil:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			kr := newKeyRotator(cfg, nil, nil, tc.urlOverride, tc.encOverride)
			if (kr == nil) != tc.wantNil {
				t.Fatalf("newKeyRotator nil=%v, want nil=%v", kr == nil, tc.wantNil)
			}
		})
	}
}

// TestApplyDefaultProvenanceGuard is the correctness core of the propagation: the
// rotator overwrites a default it planted (bootstrap → rotation), but NEVER an
// operator's own key entered through the admin UI — a value it did not plant — so
// BYOK-via-UI wins exactly like BYOK-via-env (ADR-0032).
func TestApplyDefaultProvenanceGuard(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Simulate a fresh official install that seeded the bootstrap key into the row.
	if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
		Slug: enrich.SlugTMDB, Enabled: true, APIKey: "boot-key", BaseURL: "https://tmdb.example",
	}); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	kr := &keyRotator{db: db, plantedTMDB: "boot-key"}

	// A rotation key supersedes the planted bootstrap key: it writes and reports a
	// change, preserving the enabled flag + base URL.
	changed := kr.mustApply(t, enrich.SlugTMDB, "rot-A", config.CredentialRotation, &kr.plantedTMDB)
	if !changed {
		t.Fatal("first rotation should have changed the stored key")
	}
	if got := providerKey(t, db, enrich.SlugTMDB); got != "rot-A" {
		t.Fatalf("stored key = %q, want rot-A", got)
	}

	// Re-applying the same key is a no-op (no churn / no needless Reload).
	if kr.mustApply(t, enrich.SlugTMDB, "rot-A", config.CredentialRotation, &kr.plantedTMDB) {
		t.Fatal("re-applying the same key should report no change")
	}

	// The operator now sets THEIR OWN key through the admin UI (a value the rotator
	// never planted).
	if err := db.UpsertMetadataProvider(store.MetadataProviderUpsert{
		Slug: enrich.SlugTMDB, Enabled: true, APIKey: "ui-key", BaseURL: "https://tmdb.example",
	}); err != nil {
		t.Fatalf("operator UI override: %v", err)
	}
	// A further rotation must NOT clobber the operator's key.
	if kr.mustApply(t, enrich.SlugTMDB, "rot-B", config.CredentialRotation, &kr.plantedTMDB) {
		t.Fatal("rotation clobbered the operator's admin-UI key (BYOK must win)")
	}
	if got := providerKey(t, db, enrich.SlugTMDB); got != "ui-key" {
		t.Fatalf("stored key after operator override = %q, want ui-key preserved", got)
	}

	// An operator-source resolution is skipped outright (env BYOK), regardless of the
	// stored value.
	if kr.mustApply(t, enrich.SlugTMDB, "env-key", config.CredentialOperator, &kr.plantedTMDB) {
		t.Fatal("operator-source key should never be written by the rotator")
	}
}

// mustApply is a tiny wrapper that fails the test on an applyDefault error and
// returns whether it changed the row.
func (kr *keyRotator) mustApply(t *testing.T, slug, key string, src config.CredentialSource, planted *string) bool {
	t.Helper()
	rows, err := kr.db.MetadataProviders()
	if err != nil {
		t.Fatalf("reading rows: %v", err)
	}
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}
	changed, err := kr.applyDefault(slug, byslug, key, src, planted)
	if err != nil {
		t.Fatalf("applyDefault: %v", err)
	}
	return changed
}

// providerKey reads a provider row's stored API key.
func providerKey(t *testing.T, db *store.DB, slug string) string {
	t.Helper()
	rows, err := db.MetadataProviders()
	if err != nil {
		t.Fatalf("reading rows: %v", err)
	}
	for _, r := range rows {
		if r.Slug == slug {
			return r.APIKey
		}
	}
	return ""
}
