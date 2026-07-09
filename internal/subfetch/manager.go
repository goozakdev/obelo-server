package subfetch

import (
	"context"
	"fmt"
	"sync"

	"github.com/marioquake/juicebox/internal/store"
)

// ManagerStore is the persistence the Manager reads to rebuild the active
// provider. *store.DB satisfies it; the narrow interface keeps the seam explicit
// and lets a test drive Reload without a live database.
type ManagerStore interface {
	SubtitleProviders() ([]store.SubtitleProviderRow, error)
}

// BuildFunc composes a SubtitleProvider from the persisted provider rows.
// Production uses BuildProvider; a test substitutes a fake builder
// (app.WithSubtitleProviderBuilder) so the settings→rebuild→fetch loop runs with
// zero network.
type BuildFunc func(rows []store.SubtitleProviderRow) SubtitleProvider

// Manager owns the "read settings → build → hot-swap the running Service" cycle
// (ADR-0021, mirroring enrich.Manager). Reload is called once at boot (after
// first-boot seeding) and again after every settings save, so an Admin's change —
// enabling the provider, changing the key — takes effect with no restart. The swap
// is atomic (Service.SetProvider), so an in-flight fetch never sees a half-applied
// configuration.
type Manager struct {
	store ManagerStore
	svc   *Service
	build BuildFunc

	// mu serializes concurrent Reloads so the last writer's snapshot stays live.
	mu sync.Mutex
}

// NewManager wires a Manager over the settings store, the running Service, and the
// composition function (BuildProvider in production, a fake in tests).
func NewManager(store ManagerStore, svc *Service, build BuildFunc) *Manager {
	return &Manager{store: store, svc: svc, build: build}
}

// Reload reads the current provider settings, composes the provider, and
// atomically swaps it into the Service. It is idempotent — repeated calls with
// unchanged settings rebuild an equivalent provider and re-swap. Returns an error
// only if the settings read fails; the build is total (an unconfigured server
// yields the disabled provider that makes no calls, ADR-0001).
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows, err := m.store.SubtitleProviders()
	if err != nil {
		return fmt.Errorf("subfetch: manager reload: %w", err)
	}
	m.svc.SetProvider(m.build(rows))
	return nil
}

// BuildProvider composes the active SubtitleProvider from the persisted rows: the
// OpenSubtitles provider when its row is enabled AND (it requires a key) a key is
// on file, otherwise the disabled nil-object provider that makes zero calls. This
// is the one place a subtitle provider is constructed in production; the Manager
// calls it on every Reload.
func BuildProvider(rows []store.SubtitleProviderRow) SubtitleProvider {
	for _, r := range rows {
		if r.Slug != SlugOpenSubtitles {
			continue
		}
		entry, ok := RegistryEntryFor(r.Slug)
		if !ok {
			continue
		}
		if !r.Enabled || (entry.RequiresKey && r.APIKey == "") {
			continue
		}
		base := r.BaseURL
		if base == "" {
			base = entry.DefaultBaseURL
		}
		return NewOpenSubtitlesProvider(r.APIKey, base)
	}
	return disabledProvider{}
}

// SeedInput is the first-boot seed source, decoupled from config.Config (ADR-0006
// — the subfetch domain never imports config; app maps one to the other). It
// mirrors the config subtitle-provider knobs so SeedIfEmpty can reproduce a
// pre-feature deployment's enablement in the DB.
type SeedInput struct {
	OpenSubtitlesAPIKey  string
	OpenSubtitlesBaseURL string
	// AutoFetchLang is the auto-fetch-after-scan language, "" = off (the default).
	AutoFetchLang string
}

// SeedStore is the persistence SeedIfEmpty writes through.
type SeedStore interface {
	SubtitleSettingsEmpty() (bool, error)
	UpsertSubtitleProvider(u store.SubtitleProviderUpsert) error
	SetSubtitleAutoFetchLang(lang string) error
}

// SeedIfEmpty seeds the DB-backed subtitle-provider settings from config exactly
// once — only when the settings tables are empty (first boot). An OpenSubtitles key
// in config both enables and authenticates the provider (mirroring how a TMDB key
// seeds enrichment). After seeding (or on any boot where settings already exist) it
// is a no-op — the DB is authoritative and config is ignored at runtime. Returns
// whether it seeded. The auto-fetch language is always written (it is also the "not
// empty" marker), defaulting to "" (off).
func SeedIfEmpty(s SeedStore, in SeedInput) (bool, error) {
	empty, err := s.SubtitleSettingsEmpty()
	if err != nil {
		return false, err
	}
	if !empty {
		return false, nil
	}
	if in.OpenSubtitlesAPIKey != "" {
		if err := s.UpsertSubtitleProvider(store.SubtitleProviderUpsert{
			Slug: SlugOpenSubtitles, Enabled: true,
			APIKey: in.OpenSubtitlesAPIKey, BaseURL: in.OpenSubtitlesBaseURL,
		}); err != nil {
			return false, err
		}
	}
	// The auto-fetch language singleton is always written — this is also the marker
	// that settings are no longer empty, so config is never re-consulted after boot.
	if err := s.SetSubtitleAutoFetchLang(in.AutoFetchLang); err != nil {
		return false, err
	}
	return true, nil
}
