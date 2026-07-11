package enrich

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// TestPinnedProviderPrecedenceHelpers locks in the pure decisions behind per-item
// override precedence (issue 06): which provider a Title is pinned to, which
// provider leads its kind, and whether a provider is reachable in an effective
// config — the inputs processLeaf routes on (override-wins vs. orphaned→attention).
func TestPinnedProviderPrecedenceHelpers(t *testing.T) {
	// pinnedProviderFor: a video Title with a TMDB id pins TMDB; a Track with an MBID
	// pins MusicBrainz; a Title with no external id is unpinned.
	if slug, ok := pinnedProviderFor(store.Title{Kind: "movie", TMDBID: "555"}); !ok || slug != SlugTMDB {
		t.Errorf("movie w/ tmdb id: got %q/%v, want tmdb/true", slug, ok)
	}
	if slug, ok := pinnedProviderFor(store.Title{Kind: "track", MusicbrainzID: "mb"}); !ok || slug != SlugMusicBrainz {
		t.Errorf("track w/ mbid: got %q/%v, want musicbrainz/true", slug, ok)
	}
	if _, ok := pinnedProviderFor(store.Title{Kind: "movie"}); ok {
		t.Errorf("movie w/ no id: got pinned, want unpinned")
	}

	// authoritativeSlugFor: video reads the pointer (default TMDB, or a repoint);
	// music is always MusicBrainz.
	repointed := ProviderConfig{AuthoritativeVideo: SlugAniDB}
	if got := repointed.authoritativeSlugFor("show"); got != SlugAniDB {
		t.Errorf("video leader = %q, want anidb (repointed)", got)
	}
	if got := (ProviderConfig{}).authoritativeSlugFor("movie"); got != SlugTMDB {
		t.Errorf("default video leader = %q, want tmdb", got)
	}
	if got := repointed.authoritativeSlugFor("track"); got != SlugMusicBrainz {
		t.Errorf("music leader = %q, want musicbrainz", got)
	}

	// providerReachable: a keyed video provider is reachable; a keyless muted one is
	// not; MusicBrainz rides music-enablement.
	cfg := ProviderConfig{TMDBAPIKey: "tk", MusicBrainzEnabled: true}
	if !cfg.providerReachable(SlugTMDB) {
		t.Errorf("keyed TMDB reported unreachable")
	}
	if cfg.providerReachable(SlugOMDb) {
		t.Errorf("unkeyed OMDb reported reachable")
	}
	if !cfg.providerReachable(SlugMusicBrainz) {
		t.Errorf("MusicBrainz reported unreachable with music on")
	}
	if (ProviderConfig{}).providerReachable(SlugMusicBrainz) {
		t.Errorf("MusicBrainz reported reachable with music off")
	}
}

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
			global:   videoAndMusic,                   // en-US
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
			res := ResolveLibraryEnrichment(GlobalEnrichment{Config: tc.global}, tc.policy)
			if res.Config != tc.wantCfg {
				t.Errorf("cfg = %+v, want %+v", res.Config, tc.wantCfg)
			}
			if res.Enablement != tc.wantEnab {
				t.Errorf("enablement = %+v, want %+v", res.Enablement, tc.wantEnab)
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
	res := ResolveLibraryEnrichment(GlobalEnrichment{Config: global}, store.LibraryEnrichmentPolicy{})

	if res.Config != global {
		t.Errorf("effective cfg = %+v, want the global cfg unchanged", res.Config)
	}
	if want := DeriveEnablement(global); res.Enablement != want {
		t.Errorf("effective enablement = %+v, want the global derivation %+v", res.Enablement, want)
	}
	if res.AuthoritativeFallback != "" {
		t.Errorf("empty policy fallback = %q, want none", res.AuthoritativeFallback)
	}
}

// TestResolveAuthoritativePointer table-drives the Authoritative-provider pointer
// rules (ADR-0027, issue 03): kind-constrained, always-active-if-keyed (leads even
// when globally disabled), and unreachable-after-selection → fallback to the kind
// default + attention flag. Asserted on the resolved effective config + fallback.
func TestResolveAuthoritativePointer(t *testing.T) {
	// A global where TMDB is enabled+keyed and OMDb is present but GLOBALLY DISABLED
	// yet KEYED (so it is selectable as an always-active authoritative).
	global := GlobalEnrichment{
		Config: ProviderConfig{TMDBAPIKey: "tk", MetadataLanguage: "en-US"},
		Providers: map[string]ProviderState{
			SlugTMDB:    {Enabled: true, Keyed: true, APIKey: "tk"},
			SlugOMDb:    {Enabled: false, Keyed: true, APIKey: "ok"}, // disabled but keyed
			SlugTheTVDB: {Enabled: false, Keyed: false},              // disabled + unkeyed
		},
	}

	t.Run("keyed Full provider leads even when globally disabled", func(t *testing.T) {
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
			AuthoritativeProvider: strPtr(SlugOMDb),
		})
		if res.Config.videoAuthoritativeSlug() != SlugOMDb {
			t.Errorf("authoritative = %q, want omdb (always-active-if-keyed)", res.Config.videoAuthoritativeSlug())
		}
		if res.Config.OMDbAPIKey != "ok" {
			t.Errorf("OMDb key = %q, want it injected so BuildProvider composes the lead", res.Config.OMDbAPIKey)
		}
		if !res.Enablement.Video {
			t.Errorf("video = %+v, want on (the authoritative is keyed)", res.Enablement)
		}
		if res.AuthoritativeFallback != "" {
			t.Errorf("fallback = %q, want none (OMDb reachable)", res.AuthoritativeFallback)
		}
	})

	t.Run("unkeyed authoritative falls back to the kind default + flags attention", func(t *testing.T) {
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
			AuthoritativeProvider: strPtr(SlugTheTVDB), // disabled + unkeyed
		})
		// Fell back to the video default (TMDB), which is keyed → video still on.
		if res.Config.videoAuthoritativeSlug() != SlugTMDB {
			t.Errorf("authoritative = %q, want the default tmdb fallback", res.Config.videoAuthoritativeSlug())
		}
		if res.AuthoritativeFallback != SlugTheTVDB {
			t.Errorf("fallback = %q, want thetvdb flagged for attention", res.AuthoritativeFallback)
		}
		if !res.Enablement.Video {
			t.Errorf("video = %+v, want on (fell back to keyed TMDB, never stalls)", res.Enablement)
		}
	})

	t.Run("a non-Full / wrong-kind slug is ignored (inherit the default)", func(t *testing.T) {
		// fanart.tv is Artwork-only → can never lead; the pointer is ignored.
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
			AuthoritativeProvider: strPtr(SlugFanartTV),
		})
		if res.Config.videoAuthoritativeSlug() != SlugTMDB {
			t.Errorf("authoritative = %q, want tmdb (artwork-only pointer ignored)", res.Config.videoAuthoritativeSlug())
		}
		if res.AuthoritativeFallback != "" {
			t.Errorf("fallback = %q, want none (ignored, not a fallback)", res.AuthoritativeFallback)
		}
	})
}

// TestResolveSupplementTriState table-drives the per-provider Supplement tri-state
// (ADR-0027, issue 05): force-on activates a globally-disabled-but-keyed source,
// force-off mutes a globally-enabled one, and inherit (slug absent) tracks the
// global config. Asserted on the effective config's per-provider keys.
func TestResolveSupplementTriState(t *testing.T) {
	// Global: TMDB authoritative (keyed+enabled); OMDb enabled+keyed (a live
	// supplement); TheTVDB DISABLED but keyed (available to force on).
	global := GlobalEnrichment{
		Config: ProviderConfig{TMDBAPIKey: "tk", OMDbAPIKey: "ok", MetadataLanguage: "en-US"},
		Providers: map[string]ProviderState{
			SlugTMDB:    {Enabled: true, Keyed: true, APIKey: "tk"},
			SlugOMDb:    {Enabled: true, Keyed: true, APIKey: "ok"},
			SlugTheTVDB: {Enabled: false, Keyed: true, APIKey: "vk"}, // disabled but keyed
		},
	}

	t.Run("force-on a globally-disabled-but-keyed supplement runs it", func(t *testing.T) {
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
			SupplementOverrides: map[string]bool{SlugTheTVDB: true},
		})
		if res.Config.TheTVDBAPIKey != "vk" {
			t.Errorf("TheTVDB key = %q, want injected (force-on activates the keyed source)", res.Config.TheTVDBAPIKey)
		}
	})

	t.Run("force-off a globally-enabled supplement mutes it", func(t *testing.T) {
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
			SupplementOverrides: map[string]bool{SlugOMDb: false},
		})
		if res.Config.OMDbAPIKey != "" {
			t.Errorf("OMDb key = %q, want cleared (force-off mutes it, zero calls)", res.Config.OMDbAPIKey)
		}
		// The authoritative and the rest are untouched.
		if res.Config.TMDBAPIKey != "tk" {
			t.Errorf("TMDB key disturbed by an OMDb force-off: %q", res.Config.TMDBAPIKey)
		}
	})

	t.Run("inherit (no override) tracks the global config", func(t *testing.T) {
		res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{})
		if res.Config.OMDbAPIKey != "ok" || res.Config.TheTVDBAPIKey != "" {
			t.Errorf("effective keys = omdb:%q tvdb:%q, want the global config (omdb on, tvdb off)", res.Config.OMDbAPIKey, res.Config.TheTVDBAPIKey)
		}
	})

	t.Run("force-on an UNKEYED supplement stays off (offline-first)", func(t *testing.T) {
		g := global
		g.Providers = map[string]ProviderState{
			SlugTMDB:    {Enabled: true, Keyed: true, APIKey: "tk"},
			SlugTheTVDB: {Enabled: false, Keyed: false}, // no key on file
		}
		res := ResolveLibraryEnrichment(g, store.LibraryEnrichmentPolicy{
			SupplementOverrides: map[string]bool{SlugTheTVDB: true},
		})
		if res.Config.TheTVDBAPIKey != "" {
			t.Errorf("TheTVDB key = %q, want empty (force-on never conjures a key)", res.Config.TheTVDBAPIKey)
		}
	})
}

// TestResolveSupplementOverrideAuthoritativeNoOp locks in the ADR-0027 invariant
// that force-OFF of the CURRENT Authoritative provider is a no-op — its off-switch
// is enrich_enabled=false, not a per-provider toggle. (Issue 05 owns the tri-state
// surface; the invariant is asserted here.)
func TestResolveSupplementOverrideAuthoritativeNoOp(t *testing.T) {
	global := GlobalEnrichment{
		Config: ProviderConfig{TMDBAPIKey: "tk", OMDbAPIKey: "ok", MetadataLanguage: "en-US"},
		Providers: map[string]ProviderState{
			SlugTMDB: {Enabled: true, Keyed: true, APIKey: "tk"},
			SlugOMDb: {Enabled: true, Keyed: true, APIKey: "ok"},
		},
	}

	// TMDB is the (default) authoritative; forcing it OFF must not disable it.
	res := ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
		SupplementOverrides: map[string]bool{SlugTMDB: false},
	})
	if res.Config.TMDBAPIKey != "tk" {
		t.Errorf("TMDB key = %q, want kept (force-off of the leader is a no-op)", res.Config.TMDBAPIKey)
	}
	if !res.Enablement.Video {
		t.Errorf("video = %+v, want still on (leader can't be force-off'd)", res.Enablement)
	}

	// A repointed authoritative (OMDb) is likewise protected from its own force-off,
	// while a genuine supplement (TMDB, now demoted) can still be muted.
	res = ResolveLibraryEnrichment(global, store.LibraryEnrichmentPolicy{
		AuthoritativeProvider: strPtr(SlugOMDb),
		SupplementOverrides:   map[string]bool{SlugOMDb: false, SlugTMDB: false},
	})
	if res.Config.OMDbAPIKey != "ok" {
		t.Errorf("OMDb key = %q, want kept (force-off of the current leader is a no-op)", res.Config.OMDbAPIKey)
	}
	if res.Config.TMDBAPIKey != "" {
		t.Errorf("TMDB key = %q, want cleared (a genuine supplement CAN be muted)", res.Config.TMDBAPIKey)
	}
}
