package enrich

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marioquake/juicebox/internal/store"
)

// ManagerStore is the persistence the provider Manager reads to rebuild the
// active provider. *store.DB satisfies it; the narrow interface keeps the seam
// explicit and lets a test drive Reload without a live database. EnrichmentBehavior
// supplies the DB-authoritative MusicBrainz throttle so a saved rate-limit change
// takes effect on the next Reload (the rebuilt MusicBrainz provider gets the new
// interval) — the throttle is no longer captured from config at boot.
type ManagerStore interface {
	MetadataProviders() ([]store.MetadataProviderRow, error)
	MetadataLanguage() (string, error)
	EnrichmentBehavior() (store.EnrichmentBehavior, error)
}

// BuildFunc composes a MetadataProvider + its per-kind Enablement from a
// ProviderConfig. Production uses BuildProvider; a test substitutes a fake
// builder (app.WithProviderBuilder) so the write→rebuild→enrich loop runs with
// zero network.
type BuildFunc func(ProviderConfig) (MetadataProvider, Enablement)

// Manager owns the "read settings → build → hot-swap the running Service" cycle
// (metadata-providers 02). It is the runtime-reconfiguration seam: Reload is
// called once at boot (after first-boot seeding) and again after every
// successful settings save, so an Admin's change takes effect with no restart.
// The swap itself is atomic (Service.SetProvider), so an in-flight pass never
// sees a half-applied configuration.
type Manager struct {
	store ManagerStore
	svc   *Service
	build BuildFunc

	// mu serializes concurrent Reloads (e.g. two Admin saves racing) so the last
	// writer's snapshot is the one that stays live.
	mu sync.Mutex
}

// NewManager wires a Manager over the settings store, the running Service, and the
// composition function (BuildProvider in production, a fake in tests). The
// MusicBrainz throttle is read from the store on each Reload (not captured at
// construction), so a saved rate-limit change hot-swaps into the rebuilt provider.
func NewManager(store ManagerStore, svc *Service, build BuildFunc) *Manager {
	return &Manager{store: store, svc: svc, build: build}
}

// Reload reads the current settings, composes the provider + enablement, and
// atomically swaps them into the Service. It is idempotent — repeated calls with
// unchanged settings rebuild an equivalent provider and re-swap it (no churn to
// callers, which always read the live snapshot). Returns an error only if the
// settings read fails; the build itself is total (an unconfigured server yields
// an all-disabled provider that makes no calls, ADR-0001).
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows, err := m.store.MetadataProviders()
	if err != nil {
		return fmt.Errorf("enrich: manager reload: %w", err)
	}
	lang, err := m.store.MetadataLanguage()
	if err != nil {
		return fmt.Errorf("enrich: manager reload: %w", err)
	}
	// The MusicBrainz throttle is DB-authoritative: read it here so a saved
	// rate-limit change is picked up on this Reload and threaded into the rebuilt
	// provider (0 disables throttling).
	behavior, err := m.store.EnrichmentBehavior()
	if err != nil {
		return fmt.Errorf("enrich: manager reload: %w", err)
	}
	fixed := FixedProviderInputs{
		MusicBrainzRateLimit: time.Duration(behavior.RateLimitMs()) * time.Millisecond,
	}
	cfg := SettingsToProviderConfig(rows, lang, fixed)
	provider, enablement := m.build(cfg)
	m.svc.SetProvider(provider, enablement)
	return nil
}

// SeedInput is the first-boot seed source, decoupled from config.Config (ADR-0006
// — the enrich domain never imports config; app maps one to the other). It
// mirrors the config provider knobs so SeedIfEmpty can reproduce a pre-feature
// deployment's enablement in the DB.
type SeedInput struct {
	TMDBAPIKey         string
	TMDBBaseURL        string
	TMDBImageBaseURL   string
	MetadataLanguage   string
	MusicBrainzEnabled bool
	MusicBrainzBaseURL string
	CoverArtBaseURL    string
	FanartTVAPIKey     string
	FanartTVBaseURL    string
	TheAudioDBAPIKey   string
	TheAudioDBBaseURL  string
	// Behavior knobs (enrichment-runtime-settings): the server-wide Enrichment
	// behavior seeded from config on first boot, then DB-authoritative. Interval is
	// in seconds (0 disables the scheduled sweep); the rate limit is in milliseconds
	// (0 disables throttling).
	AutoEnrichAfterScan    bool
	EnrichIntervalSeconds  int
	MusicBrainzRateLimitMs int
}

// SeedStore is the persistence SeedIfEmpty writes through.
type SeedStore interface {
	MetadataSettingsEmpty() (bool, error)
	UpsertMetadataProvider(u store.MetadataProviderUpsert) error
	SetMetadataLanguage(language string) error
	SetEnrichmentBehavior(autoEnrichAfterScan bool, enrichIntervalSeconds, musicBrainzRateLimitMs int) error
}

// SeedIfEmpty seeds the DB-backed provider settings from a pre-feature
// deployment's config exactly once — only when the settings tables are empty
// (first boot). It reproduces the old env-driven enablement: a TMDB key enables
// tmdb; MusicBrainzEnabled enables musicbrainz + coverart; a fanart.tv /
// TheAudioDB key enables that source; the language is written to the singleton.
// After seeding (or on any boot where settings already exist) it is a no-op — the
// DB is authoritative and config provider values are ignored at runtime. Returns
// whether it seeded.
//
// Base URLs are captured verbatim from config so a deployment (or a test/e2e)
// pointing a source at a mirror or a local stub keeps working byte-for-byte; a
// row's empty base_url falls back to the registry default at build time.
func SeedIfEmpty(s SeedStore, in SeedInput) (bool, error) {
	empty, err := s.MetadataSettingsEmpty()
	if err != nil {
		return false, err
	}
	if !empty {
		return false, nil
	}

	// Video: a TMDB key both enables and authenticates the authoritative video
	// source (mirrors config.VideoEnrichmentEnabled).
	if in.TMDBAPIKey != "" {
		if err := s.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: SlugTMDB, Enabled: true, APIKey: in.TMDBAPIKey,
			BaseURL: in.TMDBBaseURL, ImageBaseURL: in.TMDBImageBaseURL,
		}); err != nil {
			return false, err
		}
	}
	// Music: MusicBrainz + Cover Art Archive need no key, so the explicit opt-in
	// (or a TMDB key, which historically turned on every kind) enables them.
	if in.MusicBrainzEnabled || in.TMDBAPIKey != "" {
		if err := s.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: SlugMusicBrainz, Enabled: true, BaseURL: in.MusicBrainzBaseURL,
		}); err != nil {
			return false, err
		}
		if err := s.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: SlugCoverArt, Enabled: true, BaseURL: in.CoverArtBaseURL,
		}); err != nil {
			return false, err
		}
	}
	// Music image supplements: enabled only when a key was configured.
	if in.FanartTVAPIKey != "" {
		if err := s.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: SlugFanartTV, Enabled: true, APIKey: in.FanartTVAPIKey, BaseURL: in.FanartTVBaseURL,
		}); err != nil {
			return false, err
		}
	}
	if in.TheAudioDBAPIKey != "" {
		if err := s.UpsertMetadataProvider(store.MetadataProviderUpsert{
			Slug: SlugTheAudioDB, Enabled: true, APIKey: in.TheAudioDBAPIKey, BaseURL: in.TheAudioDBBaseURL,
		}); err != nil {
			return false, err
		}
	}
	// The behavior knobs are seeded from config exactly once (like the provider
	// enablement above), after which the DB is authoritative and env is ignored.
	if err := s.SetEnrichmentBehavior(in.AutoEnrichAfterScan, in.EnrichIntervalSeconds, in.MusicBrainzRateLimitMs); err != nil {
		return false, err
	}
	// The language singleton is always written — this is also the marker that the
	// settings are no longer empty, so config is never re-consulted after boot.
	if err := s.SetMetadataLanguage(in.MetadataLanguage); err != nil {
		return false, err
	}
	return true, nil
}
