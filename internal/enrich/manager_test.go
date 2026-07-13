package enrich

import (
	"context"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/store"
)

// fakeManagerStore is an in-memory ManagerStore for the Manager tests (no DB).
type fakeManagerStore struct {
	rows        []store.MetadataProviderRow
	lang        string
	rateLimitMs int
	// policies maps libraryID → its sparse Enrichment policy (absent = empty).
	policies map[string]store.LibraryEnrichmentPolicy
	// consentDeclined withholds first-run consent (ADR-0032) when true; the zero
	// value grants it, so existing Manager tests see enrichment enabled as before.
	consentDeclined bool
}

func (f *fakeManagerStore) MetadataProviders() ([]store.MetadataProviderRow, error) {
	return f.rows, nil
}
func (f *fakeManagerStore) MetadataLanguage() (string, error) { return f.lang, nil }
func (f *fakeManagerStore) EnrichmentBehavior() (store.EnrichmentBehavior, error) {
	ms := f.rateLimitMs
	return store.EnrichmentBehavior{MusicBrainzRateLimitMs: &ms}, nil
}
func (f *fakeManagerStore) LibraryEnrichmentPolicy(libraryID string) (store.LibraryEnrichmentPolicy, error) {
	return f.policies[libraryID], nil
}
func (f *fakeManagerStore) EnrichmentConsent() (store.EnrichmentConsent, error) {
	return store.EnrichmentConsent{Decided: true, Granted: !f.consentDeclined}, nil
}

// TestSettingsToProviderConfig covers the settings → ProviderConfig mapping: an
// enabled+keyed source contributes; a key-requiring source with no key does not;
// a base-URL override wins over the registry default, and an absent override
// falls back to it.
func TestSettingsToProviderConfig(t *testing.T) {
	rows := []store.MetadataProviderRow{
		{Slug: SlugTMDB, Enabled: true, APIKey: "tk", BaseURL: "http://tmdb.stub", ImageBaseURL: "http://img.stub"},
		{Slug: SlugMusicBrainz, Enabled: true},                // keyless, no override → default host
		{Slug: SlugFanartTV, Enabled: true, APIKey: ""},       // requires key but none → inactive
		{Slug: SlugTheAudioDB, Enabled: false, APIKey: "adk"}, // has key but disabled → inactive
	}
	fixed := FixedProviderInputs{MusicBrainzRateLimit: 2 * time.Second}
	cfg := SettingsToProviderConfig(rows, "en-GB", fixed)

	if cfg.TMDBAPIKey != "tk" {
		t.Errorf("TMDBAPIKey = %q, want tk", cfg.TMDBAPIKey)
	}
	if cfg.TMDBBaseURL != "http://tmdb.stub" {
		t.Errorf("TMDBBaseURL = %q, want override", cfg.TMDBBaseURL)
	}
	if cfg.TMDBImageBaseURL != "http://img.stub" {
		t.Errorf("TMDBImageBaseURL = %q, want the row's image-host override", cfg.TMDBImageBaseURL)
	}
	// A tmdb row with no image-host override falls back to the registry default.
	noOverride := SettingsToProviderConfig(
		[]store.MetadataProviderRow{{Slug: SlugTMDB, Enabled: true, APIKey: "tk"}}, "en-GB", fixed)
	if noOverride.TMDBImageBaseURL != registryTMDBImageBaseURL {
		t.Errorf("TMDBImageBaseURL = %q, want registry default %q", noOverride.TMDBImageBaseURL, registryTMDBImageBaseURL)
	}
	if cfg.MusicBrainzBaseURL != registryMusicBrainzBaseURL {
		t.Errorf("MusicBrainzBaseURL = %q, want registry default", cfg.MusicBrainzBaseURL)
	}
	if !cfg.MusicBrainzEnabled {
		t.Errorf("MusicBrainzEnabled = false, want true")
	}
	if cfg.FanartTVAPIKey != "" {
		t.Errorf("FanartTVAPIKey = %q, want empty (key-requiring, no key → inactive)", cfg.FanartTVAPIKey)
	}
	if cfg.TheAudioDBAPIKey != "" {
		t.Errorf("TheAudioDBAPIKey = %q, want empty (disabled → inactive)", cfg.TheAudioDBAPIKey)
	}
	if cfg.MetadataLanguage != "en-GB" || cfg.MusicBrainzRateLimit != 2*time.Second {
		t.Errorf("language/rate = %q/%v, want en-GB/2s", cfg.MetadataLanguage, cfg.MusicBrainzRateLimit)
	}
}

// TestManagerReload proves the manager reads settings, composes via the build
// func, and atomically swaps the Service's provider + enablement — and that
// repeated identical reloads are idempotent (no error, equivalent result).
func TestManagerReload(t *testing.T) {
	st := &fakeManagerStore{
		rows: []store.MetadataProviderRow{{Slug: SlugTMDB, Enabled: true, APIKey: "tk"}},
		lang: "en-US",
	}
	var builtCfgs []ProviderConfig
	build := func(cfg ProviderConfig) (MetadataProvider, Enablement) {
		builtCfgs = append(builtCfgs, cfg)
		return CompositeProvider{}, DeriveEnablement(cfg)
	}
	svc := NewService(nil, CompositeProvider{}, nil, Enablement{}, "", 0)
	if svc.EnrichmentEnabled() {
		t.Fatalf("precondition: service starts disabled")
	}
	mgr := NewManager(st, svc, build)

	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The swap took effect on the Service. A TMDB key enables every kind (the
	// legacy "a key turns music on too" rule preserved by BuildProvider), so both
	// video and music are on.
	if en := svc.snapshot().enablement; !en.Video || !en.Music {
		t.Errorf("after reload enablement = %+v, want video+music on", en)
	}
	if !svc.EnrichmentEnabled() {
		t.Errorf("EnrichmentEnabled = false after enabling video")
	}
	if len(builtCfgs) != 1 || builtCfgs[0].TMDBAPIKey != "tk" {
		t.Errorf("build received %+v, want one cfg with TMDBAPIKey=tk", builtCfgs)
	}

	// Idempotent: a repeated reload with unchanged settings rebuilds equivalently.
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload again: %v", err)
	}
	if len(builtCfgs) != 2 || builtCfgs[1] != builtCfgs[0] {
		t.Errorf("second build cfg = %+v, want equal to first", builtCfgs)
	}

	// Disable the source and reload: the swap flips enrichment off at runtime.
	st.rows[0].Enabled = false
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload after disable: %v", err)
	}
	if svc.EnrichmentEnabled() {
		t.Errorf("EnrichmentEnabled = true after disabling every source")
	}
}

// TestManagerReloadConsentGate proves the first-run consent gate (ADR-0032): with
// consent WITHHELD the Manager forces the global snapshot's Enablement off even
// though a usable provider is configured, so the Service reports enrichment
// disabled and makes no outbound calls; granting consent on the next Reload flips
// it on with no other change.
func TestManagerReloadConsentGate(t *testing.T) {
	st := &fakeManagerStore{
		rows:            []store.MetadataProviderRow{{Slug: SlugTMDB, Enabled: true, APIKey: "tk"}},
		lang:            "en-US",
		consentDeclined: true, // operator has not consented
	}
	build := func(cfg ProviderConfig) (MetadataProvider, Enablement) {
		return CompositeProvider{}, DeriveEnablement(cfg)
	}
	svc := NewService(nil, CompositeProvider{}, nil, Enablement{}, "", 0)
	mgr := NewManager(st, svc, build)

	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload (consent withheld): %v", err)
	}
	// A configured provider would normally enable both kinds; consent gates it off.
	if en := svc.snapshot().enablement; en.Video || en.Music {
		t.Errorf("enablement with consent withheld = %+v, want all off", en)
	}
	if svc.EnrichmentEnabled() {
		t.Errorf("EnrichmentEnabled = true with consent withheld, want false")
	}

	// Grant consent and reload: the SAME configuration now enriches.
	st.consentDeclined = false
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload (consent granted): %v", err)
	}
	if en := svc.snapshot().enablement; !en.Video || !en.Music {
		t.Errorf("enablement after granting consent = %+v, want video+music on", en)
	}
}

// TestManagerReloadRateLimit proves the MusicBrainz throttle is DB-authoritative:
// the Manager reads it from the store on each Reload, so a changed rate limit is
// threaded into the very next rebuild's ProviderConfig (no restart, no config
// capture at construction).
func TestManagerReloadRateLimit(t *testing.T) {
	st := &fakeManagerStore{
		rows:        []store.MetadataProviderRow{{Slug: SlugMusicBrainz, Enabled: true}},
		lang:        "en-US",
		rateLimitMs: 1000,
	}
	var builtCfgs []ProviderConfig
	build := func(cfg ProviderConfig) (MetadataProvider, Enablement) {
		builtCfgs = append(builtCfgs, cfg)
		return CompositeProvider{}, DeriveEnablement(cfg)
	}
	svc := NewService(nil, CompositeProvider{}, nil, Enablement{}, "", 0)
	mgr := NewManager(st, svc, build)

	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := builtCfgs[0].MusicBrainzRateLimit; got != time.Second {
		t.Errorf("first rebuild rate = %v, want 1s (from the store)", got)
	}

	// Change the stored throttle and reload: the next rebuild picks it up.
	st.rateLimitMs = 250
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload after rate change: %v", err)
	}
	if got := builtCfgs[1].MusicBrainzRateLimit; got != 250*time.Millisecond {
		t.Errorf("second rebuild rate = %v, want 250ms (DB-sourced hot-swap)", got)
	}

	// 0 disables throttling entirely.
	st.rateLimitMs = 0
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload after zeroing rate: %v", err)
	}
	if got := builtCfgs[2].MusicBrainzRateLimit; got != 0 {
		t.Errorf("third rebuild rate = %v, want 0 (throttling disabled)", got)
	}
}

// TestManagerPerLibraryResolution proves the installed per-Library resolver
// layers each Library's Enrichment policy over the global config (ADR-0027): an
// un-overriding Library resolves byte-for-byte to the global enablement, and a
// Library with enrich_enabled=false resolves to all-off — the two spine rules,
// observed through the Service the resolver feeds.
func TestManagerPerLibraryResolution(t *testing.T) {
	off := false
	st := &fakeManagerStore{
		rows: []store.MetadataProviderRow{{Slug: SlugTMDB, Enabled: true, APIKey: "tk"}},
		lang: "en-US",
		policies: map[string]store.LibraryEnrichmentPolicy{
			"lib-off": {EnrichEnabled: &off},
			// "lib-inherit" has no entry → empty policy → inherit global.
		},
	}
	svc := NewService(nil, CompositeProvider{}, nil, Enablement{}, "", 0)
	mgr := NewManager(st, svc, BuildFunc(func(cfg ProviderConfig) (MetadataProvider, Enablement) {
		return CompositeProvider{}, DeriveEnablement(cfg)
	}))
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	mgr.EnablePerLibraryResolution()

	// An un-overriding Library inherits the global enablement (video+music on).
	if en, err := mgr.EffectiveEnablement(context.Background(), "lib-inherit"); err != nil || !en.Video || !en.Music {
		t.Errorf("inherit library enablement = %+v (err %v), want video+music on", en, err)
	}
	if !svc.EnrichmentEnabledForLibrary(context.Background(), "lib-inherit") {
		t.Errorf("EnrichmentEnabledForLibrary(inherit) = false, want true")
	}

	// A switched-off Library resolves to all-off even though the server is enabled.
	if en, err := mgr.EffectiveEnablement(context.Background(), "lib-off"); err != nil || en.Video || en.Music {
		t.Errorf("off library enablement = %+v (err %v), want all off", en, err)
	}
	if svc.EnrichmentEnabledForLibrary(context.Background(), "lib-off") {
		t.Errorf("EnrichmentEnabledForLibrary(off) = true, want false (enrich_enabled=false)")
	}
	// The global snapshot is untouched — other server surfaces still see it enabled.
	if !svc.EnrichmentEnabled() {
		t.Errorf("global EnrichmentEnabled = false, want true (per-Library off must not disable the server)")
	}
}

// TestManagerLibraryCacheInvalidation proves the effective snapshot is cached and
// invalidated: a policy change is not seen until InvalidateLibrary (or a global
// Reload) drops the cached entry, matching the metadata-providers Reload contract.
func TestManagerLibraryCacheInvalidation(t *testing.T) {
	st := &fakeManagerStore{
		rows:     []store.MetadataProviderRow{{Slug: SlugTMDB, Enabled: true, APIKey: "tk"}},
		lang:     "en-US",
		policies: map[string]store.LibraryEnrichmentPolicy{}, // lib starts on inherit
	}
	var built int
	svc := NewService(nil, CompositeProvider{}, nil, Enablement{}, "", 0)
	mgr := NewManager(st, svc, BuildFunc(func(cfg ProviderConfig) (MetadataProvider, Enablement) {
		built++
		return CompositeProvider{}, DeriveEnablement(cfg)
	}))
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	mgr.EnablePerLibraryResolution()

	// First resolution builds + caches; a second resolution reuses the cache.
	snap1, _ := svc.snapshotFor(context.Background(), "lib")
	builtAfterFirst := built
	snap2, _ := svc.snapshotFor(context.Background(), "lib")
	if built != builtAfterFirst {
		t.Errorf("second resolution rebuilt (built %d→%d), want a cache hit", builtAfterFirst, built)
	}
	if !snap1.enablement.Video || !snap2.enablement.Video {
		t.Errorf("resolved enablement = %+v/%+v, want video on (inherited)", snap1.enablement, snap2.enablement)
	}

	// Change the policy to off, but WITHOUT invalidating: the cache still serves on.
	off := false
	st.policies["lib"] = store.LibraryEnrichmentPolicy{EnrichEnabled: &off}
	if snap, _ := svc.snapshotFor(context.Background(), "lib"); !snap.enablement.Video {
		t.Errorf("stale cache not served: enablement = %+v, want the cached video-on", snap.enablement)
	}

	// Invalidate → the next resolution rebuilds from the new policy (now off).
	mgr.InvalidateLibrary("lib")
	if snap, _ := svc.snapshotFor(context.Background(), "lib"); snap.enablement.Video || snap.enablement.Music {
		t.Errorf("after invalidation enablement = %+v, want all off (policy applied)", snap.enablement)
	}

	// A global Reload clears every cached entry (Model A: un-overriding Libraries
	// track global live).
	delete(st.policies, "lib") // back to inherit
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload after policy churn: %v", err)
	}
	if snap, _ := svc.snapshotFor(context.Background(), "lib"); !snap.enablement.Video {
		t.Errorf("after global Reload enablement = %+v, want video on (rebuilt from cleared cache)", snap.enablement)
	}
}

// fakeSeedStore records the settings SeedIfEmpty writes.
type fakeSeedStore struct {
	empty    bool
	upserts  map[string]store.MetadataProviderUpsert
	language string
	langSet  bool

	behavior    store.EnrichmentBehavior
	behaviorSet bool

	consentGranted *bool // the seeded first-run consent decision (nil = never written)
}

func newFakeSeedStore(empty bool) *fakeSeedStore {
	return &fakeSeedStore{empty: empty, upserts: map[string]store.MetadataProviderUpsert{}}
}

func (f *fakeSeedStore) MetadataSettingsEmpty() (bool, error) { return f.empty, nil }
func (f *fakeSeedStore) UpsertMetadataProvider(u store.MetadataProviderUpsert) error {
	f.upserts[u.Slug] = u
	return nil
}
func (f *fakeSeedStore) SetMetadataLanguage(language string) error {
	f.language, f.langSet = language, true
	return nil
}
func (f *fakeSeedStore) SetEnrichmentConsent(granted bool) error {
	f.consentGranted = &granted
	return nil
}
func (f *fakeSeedStore) SetEnrichmentBehavior(auto bool, intervalSeconds, rateLimitMs int) error {
	f.behavior = store.EnrichmentBehavior{
		AutoEnrichAfterScan:    &auto,
		EnrichIntervalSeconds:  &intervalSeconds,
		MusicBrainzRateLimitMs: &rateLimitMs,
	}
	f.behaviorSet = true
	return nil
}

// TestSeedIfEmpty proves the first-boot seed reproduces a pre-feature
// deployment's enablement in the DB, and is a no-op when settings already exist.
func TestSeedIfEmpty(t *testing.T) {
	t.Run("reproduces env enablement", func(t *testing.T) {
		s := newFakeSeedStore(true)
		seeded, err := SeedIfEmpty(s, SeedInput{
			TMDBAPIKey:         "tk",
			TMDBBaseURL:        "http://tmdb.stub",
			TMDBImageBaseURL:   "http://img.stub",
			MetadataLanguage:   "en-US",
			MusicBrainzEnabled: true,
			FanartTVAPIKey:     "fk",
			// TheAudioDB has no key → its source stays off (not seeded).
			AutoEnrichAfterScan:    true,
			EnrichIntervalSeconds:  21600,
			MusicBrainzRateLimitMs: 1000,
		})
		if err != nil || !seeded {
			t.Fatalf("SeedIfEmpty = %v, %v; want true, nil", seeded, err)
		}
		if !s.behaviorSet || !s.behavior.Auto() || s.behavior.IntervalSeconds() != 21600 || s.behavior.RateLimitMs() != 1000 {
			t.Errorf("behavior seed = %+v (set %v), want auto/21600s/1000ms", s.behavior, s.behaviorSet)
		}
		if u, ok := s.upserts[SlugTMDB]; !ok || !u.Enabled || u.APIKey != "tk" || u.BaseURL != "http://tmdb.stub" || u.ImageBaseURL != "http://img.stub" {
			t.Errorf("tmdb seed = %+v (ok %v), want enabled/tk/stub/img", u, ok)
		}
		if u, ok := s.upserts[SlugMusicBrainz]; !ok || !u.Enabled {
			t.Errorf("musicbrainz seed = %+v (ok %v), want enabled", u, ok)
		}
		if u, ok := s.upserts[SlugCoverArt]; !ok || !u.Enabled {
			t.Errorf("coverart seed = %+v (ok %v), want enabled (rides MusicBrainz)", u, ok)
		}
		if u, ok := s.upserts[SlugFanartTV]; !ok || !u.Enabled || u.APIKey != "fk" {
			t.Errorf("fanarttv seed = %+v (ok %v), want enabled/fk", u, ok)
		}
		if _, ok := s.upserts[SlugTheAudioDB]; ok {
			t.Errorf("theaudiodb seeded but had no key: %+v", s.upserts[SlugTheAudioDB])
		}
		if !s.langSet || s.language != "en-US" {
			t.Errorf("language seed = %q (set %v), want en-US", s.language, s.langSet)
		}

		// The seeded settings reproduce the pre-feature per-kind enablement.
		var rows []store.MetadataProviderRow
		for _, u := range s.upserts {
			rows = append(rows, store.MetadataProviderRow{
				Slug: u.Slug, Enabled: u.Enabled, APIKey: u.APIKey, BaseURL: u.BaseURL,
			})
		}
		en := DeriveEnablement(SettingsToProviderConfig(rows, s.language, FixedProviderInputs{}))
		if !en.Video || !en.Music {
			t.Errorf("reproduced enablement = %+v, want video+music on", en)
		}
	})

	t.Run("no-op when not empty", func(t *testing.T) {
		s := newFakeSeedStore(false)
		seeded, err := SeedIfEmpty(s, SeedInput{TMDBAPIKey: "tk"})
		if err != nil || seeded {
			t.Fatalf("SeedIfEmpty on non-empty = %v, %v; want false, nil", seeded, err)
		}
		if len(s.upserts) != 0 || s.langSet || s.behaviorSet {
			t.Errorf("seeded despite non-empty settings: upserts=%v langSet=%v behaviorSet=%v", s.upserts, s.langSet, s.behaviorSet)
		}
	})
}
