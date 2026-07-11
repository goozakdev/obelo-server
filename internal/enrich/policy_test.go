package enrich

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// withLanguage returns a copy of cfg with its MetadataLanguage replaced — the
// expected effective config when a Library overrides just its language.
func withLanguage(cfg ProviderConfig, lang string) ProviderConfig {
	cfg.MetadataLanguage = lang
	return cfg
}

// TestResolveLibraryEnrichment table-drives the resolution derivation the way
// TestSettingsToProviderConfig / TestBuildProviderComposition are driven: given a
// global config and a Library's policy, assert the effective config + enablement.
// This slice covers the two spine rules (empty ⇒ global; disabled ⇒ off) plus the
// enrich_enabled=true "not an off-switch" case.
func TestResolveLibraryEnrichment(t *testing.T) {
	// A representative "fully configured" global: video (TMDB key) + music on.
	videoAndMusic := ProviderConfig{
		TMDBAPIKey:         "tk",
		TMDBBaseURL:        "http://tmdb.stub",
		MetadataLanguage:   "en-US",
		MusicBrainzEnabled: true,
	}
	// A global with nothing configured — every kind off.
	unconfigured := ProviderConfig{MetadataLanguage: "en-US"}

	tests := []struct {
		name     string
		global   ProviderConfig
		policy   store.LibraryEnrichmentPolicy
		wantCfg  ProviderConfig
		wantEnab Enablement
	}{
		{
			name:     "empty policy inherits the global config byte-for-byte",
			global:   videoAndMusic,
			policy:   store.LibraryEnrichmentPolicy{}, // no overrides
			wantCfg:  videoAndMusic,
			wantEnab: Enablement{Video: true, Music: true},
		},
		{
			name:     "empty policy over an unconfigured global stays fully off",
			global:   unconfigured,
			policy:   store.LibraryEnrichmentPolicy{},
			wantCfg:  unconfigured,
			wantEnab: Enablement{}, // both off — nothing configured to inherit
		},
		{
			name:     "enrich_enabled=false forces the whole Library off",
			global:   videoAndMusic,
			policy:   store.LibraryEnrichmentPolicy{EnrichEnabled: boolPtr(false)},
			wantCfg:  videoAndMusic, // config carried through; enablement is the gate
			wantEnab: Enablement{},  // no chain, zero calls
		},
		{
			name:     "enrich_enabled=true is not an off-switch — still inherits global enablement",
			global:   videoAndMusic,
			policy:   store.LibraryEnrichmentPolicy{EnrichEnabled: boolPtr(true)},
			wantCfg:  videoAndMusic,
			wantEnab: Enablement{Video: true, Music: true},
		},
		{
			name:     "enrich_enabled=true cannot conjure a kind the global config leaves off",
			global:   unconfigured,
			policy:   store.LibraryEnrichmentPolicy{EnrichEnabled: boolPtr(true)},
			wantCfg:  unconfigured,
			wantEnab: Enablement{}, // true ≠ "turn on"; global has no keys
		},
		{
			name:   "metadata_language override localizes just this Library",
			global: videoAndMusic,
			policy: store.LibraryEnrichmentPolicy{MetadataLanguage: strPtr("ja-JP")},
			// Only the language changes; every other field inherits the global config.
			wantCfg:  withLanguage(videoAndMusic, "ja-JP"),
			wantEnab: Enablement{Video: true, Music: true},
		},
		{
			name:     "metadata_language unset inherits the global language live",
			global:   videoAndMusic, // en-US
			policy:   store.LibraryEnrichmentPolicy{}, // no language override
			wantCfg:  videoAndMusic,
			wantEnab: Enablement{Video: true, Music: true},
		},
		{
			name:   "metadata_language override does not enable a kind the global leaves off",
			global: unconfigured,
			policy: store.LibraryEnrichmentPolicy{MetadataLanguage: strPtr("fr-FR")},
			// Language localizes the (still-off) chain; it never turns a kind on.
			wantCfg:  withLanguage(unconfigured, "fr-FR"),
			wantEnab: Enablement{},
		},
		{
			name:   "metadata_language override survives an enrich_enabled=false gate",
			global: videoAndMusic,
			policy: store.LibraryEnrichmentPolicy{
				EnrichEnabled:    boolPtr(false),
				MetadataLanguage: strPtr("de-DE"),
			},
			// The language delta is still applied to the carried-through cfg, but the
			// hard off-switch means no kind enriches (zero calls regardless).
			wantCfg:  withLanguage(videoAndMusic, "de-DE"),
			wantEnab: Enablement{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCfg, gotEnab := ResolveLibraryEnrichment(tc.global, tc.policy)
			if gotCfg != tc.wantCfg {
				t.Errorf("cfg = %+v, want %+v", gotCfg, tc.wantCfg)
			}
			if gotEnab != tc.wantEnab {
				t.Errorf("enablement = %+v, want %+v", gotEnab, tc.wantEnab)
			}
		})
	}
}

// TestResolveLibraryEnrichmentMatchesGlobalForEmptyPolicy locks in the acceptance
// criterion that an empty policy is indistinguishable from today's global path:
// the resolved (cfg, enablement) equals what the global path (BuildProvider's own
// derivation) produces, so an untouched Library enriches exactly as before.
func TestResolveLibraryEnrichmentMatchesGlobalForEmptyPolicy(t *testing.T) {
	global := ProviderConfig{TMDBAPIKey: "tk", MusicBrainzEnabled: true, MetadataLanguage: "en-GB"}
	cfg, enab := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{})

	if cfg != global {
		t.Errorf("effective cfg = %+v, want the global cfg unchanged", cfg)
	}
	if want := DeriveEnablement(global); enab != want {
		t.Errorf("effective enablement = %+v, want the global derivation %+v", enab, want)
	}
}
