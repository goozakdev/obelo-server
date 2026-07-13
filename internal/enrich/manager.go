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
	// LibraryEnrichmentPolicy reads a Library's SPARSE Enrichment policy (ADR-0027)
	// — the deltas the per-Library resolver layers over the global config. An
	// absent policy is the zero value (inherit everything).
	LibraryEnrichmentPolicy(libraryID string) (store.LibraryEnrichmentPolicy, error)
	// EnrichmentConsent reads the first-run consent decision (ADR-0032). Until it
	// is Granted the Manager forces every emitted Enablement off, so an undecided
	// or declined server makes ZERO outbound enrichment calls. Read on each Reload
	// (a consent change is applied like any other settings save).
	EnrichmentConsent() (store.EnrichmentConsent, error)
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
	// writer's snapshot is the one that stays live, and guards the per-Library
	// resolution state below (globalCfg + libCache).
	mu sync.Mutex

	// global is the server-wide Enrichment configuration from the last Reload — the
	// base the per-Library resolver (ADR-0027) layers each Library's Enrichment policy
	// over. It bundles the composed ProviderConfig with the per-slug ProviderState the
	// resolver needs (enabled + keyed + key). Its zero value is the fully-unconfigured
	// config; the resolver is only installed after the first Reload
	// (EnablePerLibraryResolution), so it is always populated before any per-Library
	// resolution runs.
	global GlobalEnrichment

	// libCache memoizes each Library's EFFECTIVE provider + enablement snapshot so a
	// pass doesn't rebuild it per run. It is INVALIDATED wholesale on a global
	// settings Reload (globalCfg changed) and per-Library on a policy change
	// (InvalidateLibrary), so it never serves a stale effective config.
	libCache map[string]providerSnapshot

	// consentGranted caches the first-run consent decision (ADR-0032) from the last
	// Reload. It is ANDed into every Enablement the Manager emits (consentGate), so
	// a server whose operator has not granted consent enriches NOTHING no matter
	// which path — global snapshot or per-Library pass — reads it. Guarded by mu
	// (set in Reload, read in resolveLibrary). False until the first Reload, so the
	// server is gated off until settings (and consent) are loaded at boot.
	consentGranted bool
}

// NewManager wires a Manager over the settings store, the running Service, and the
// composition function (BuildProvider in production, a fake in tests). The
// MusicBrainz throttle is read from the store on each Reload (not captured at
// construction), so a saved rate-limit change hot-swaps into the rebuilt provider.
// Per-Library policy resolution is OFF until EnablePerLibraryResolution is called
// (after the first Reload), so a Service given a fixed injected provider keeps
// using the global snapshot.
func NewManager(store ManagerStore, svc *Service, build BuildFunc) *Manager {
	return &Manager{store: store, svc: svc, build: build, libCache: map[string]providerSnapshot{}}
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
	// The first-run consent decision (ADR-0032) gates ALL outbound enrichment: read
	// it here so a consent change (applied via a Reload, like any settings save)
	// takes effect live, and cache it for the per-Library resolver below.
	consent, err := m.store.EnrichmentConsent()
	if err != nil {
		return fmt.Errorf("enrich: manager reload: %w", err)
	}
	m.consentGranted = consent.Granted

	cfg := SettingsToProviderConfig(rows, lang, fixed)
	provider, enablement := m.build(cfg)
	// AND consent into the global snapshot: without it, the composed provider still
	// exists but every kind is off, so the Service's global-snapshot readers
	// (SearchCandidates/ResolveIdentity/previewExternal/ArtworkCandidates) make no
	// call — the same all-off posture an unconfigured server has.
	m.svc.SetProvider(provider, m.consentGate(enablement))

	// A global settings change invalidates every Library's effective snapshot: an
	// un-overriding Library must pick the change up LIVE (Model A, ADR-0027), and
	// an overriding one is re-layered over the new base on its next resolution. The
	// per-slug ProviderState is derived from the same rows so the resolver can honor
	// the always-active-if-keyed authoritative and the supplement tri-state.
	m.global = GlobalEnrichment{Config: cfg, Providers: ProviderStatesFromRows(rows)}
	m.libCache = map[string]providerSnapshot{}
	return nil
}

// consentGate returns e unchanged when the first-run consent decision (ADR-0032)
// is Granted, or the all-off Enablement when it is not. It is the SINGLE point
// where consent is folded into what the Service sees: applied to the global
// snapshot in Reload and to every per-Library snapshot in resolveLibrary, so a
// withheld or declined consent yields ZERO outbound calls through every enrich
// path — no scattered per-call checks. Callers hold m.mu (consentGranted is
// written under it in Reload).
func (m *Manager) consentGate(e Enablement) Enablement {
	if !m.consentGranted {
		return Enablement{}
	}
	return e
}

// EnablePerLibraryResolution installs the per-Library resolver on the Service, so
// every Library-scoped pass resolves its effective provider through this Manager
// (its Enrichment policy layered over globalCfg, ADR-0027) instead of using the
// global snapshot. Call it AFTER the first Reload (globalCfg populated). It is a
// no-op to leave uninstalled — the Service then uses the global snapshot for every
// Library (the pre-policy behavior), which is what a fixed injected provider wants.
func (m *Manager) EnablePerLibraryResolution() {
	m.svc.resolveLibrary = m.resolveLibrary
}

// GlobalEnablement returns the server-wide per-kind Enablement (the base a
// Library inherits when its enrich-on/off key is unset). The API reports it so the
// Admin sees what "inherit" currently resolves to next to the override control.
func (m *Manager) GlobalEnablement() Enablement {
	m.mu.Lock()
	defer m.mu.Unlock()
	return DeriveEnablement(m.global.Config)
}

// GlobalMetadataLanguage returns the server-wide preferred metadata language (the
// base a Library inherits when its language key is unset). The API reports it so
// the Admin sees what "inherit" resolves to next to the language override control.
func (m *Manager) GlobalMetadataLanguage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.global.Config.MetadataLanguage
}

// UsableFullProviders lists the Full providers of a Library's coarse media kind
// that are currently USABLE as an Authoritative provider — the candidate set for
// the per-Library authoritative dropdown (ADR-0027). A Full provider is usable when
// it is KEYED (a key on file, or none required); it need NOT be globally enabled,
// because a pointed-at Full provider runs even when disabled for general use
// (always-active-if-keyed). Returned in registry order with the display name.
func (m *Manager) UsableFullProviders(kind string) []ProviderRef {
	m.mu.Lock()
	providers := m.global.Providers
	m.mu.Unlock()
	var out []ProviderRef
	for _, e := range FullProvidersForKind(kind) {
		if providers[e.Slug].Keyed {
			out = append(out, ProviderRef{Slug: e.Slug, Name: e.Name})
		}
	}
	return out
}

// ProviderRef is a provider's stable slug + display name, for the API's
// authoritative-candidate list (the UI renders a dropdown of these).
type ProviderRef struct {
	Slug string
	Name string
}

// SupplementRef is one togglable Supplement of a Library's kind, with the global
// enabled state its per-Library tri-state inherits when unset — the per-Supplement
// control's data (ADR-0027, issue 05). The current Authoritative provider is
// excluded (its off-switch is enrich_enabled, not a per-provider toggle).
type SupplementRef struct {
	Slug             string
	Name             string
	InheritedEnabled bool // the provider's server-wide enabled state (the inherit baseline)
}

// SupplementProviders lists a Library's togglable Supplements for its coarse media
// kind — the key-bearing providers of the kind EXCEPT the one currently leading
// (the Authoritative provider) — each with the global enabled state its tri-state
// inherits. The UI renders an inherit/on/off control per entry; the current
// override (if any) is read from the stored policy alongside.
func (m *Manager) SupplementProviders(ctx context.Context, libraryID, kind string) ([]SupplementRef, error) {
	res, err := m.resolvePolicy(libraryID)
	if err != nil {
		return nil, err
	}
	var authoritative string
	if kind == KindMusic {
		authoritative = DefaultAuthoritativeForKind(KindMusic)
	} else {
		authoritative = res.Config.videoAuthoritativeSlug()
	}
	m.mu.Lock()
	providers := m.global.Providers
	m.mu.Unlock()
	var out []SupplementRef
	for _, e := range SupplementProvidersForKind(kind) {
		if e.Slug == authoritative {
			continue // the leader isn't a supplement; its off-switch is enrich_enabled
		}
		out = append(out, SupplementRef{Slug: e.Slug, Name: e.Name, InheritedEnabled: providers[e.Slug].Enabled})
	}
	return out, nil
}

// EffectiveAuthoritative reports the slug of the Full provider currently LEADING a
// Library's Enrichment for its coarse media kind (resolving its policy over the
// current global config), plus any fallback: fallbackFrom names a chosen
// authoritative that is unreachable (so the Library fell back to the kind default),
// "" when the authoritative resolved normally. Read fresh each call (independent of
// the pass cache), so it never depends on invalidation ordering.
func (m *Manager) EffectiveAuthoritative(ctx context.Context, libraryID, kind string) (slug, fallbackFrom string, err error) {
	res, err := m.resolvePolicy(libraryID)
	if err != nil {
		return "", "", err
	}
	if kind == KindMusic {
		return DefaultAuthoritativeForKind(KindMusic), res.AuthoritativeFallback, nil
	}
	return res.Config.videoAuthoritativeSlug(), res.AuthoritativeFallback, nil
}

// resolvePolicy reads a Library's policy fresh and resolves it over the current
// global config — the shared read used by the display accessors (enablement,
// authoritative). It never touches the pass cache.
func (m *Manager) resolvePolicy(libraryID string) (Resolution, error) {
	m.mu.Lock()
	global := m.global
	m.mu.Unlock()
	policy, err := m.store.LibraryEnrichmentPolicy(libraryID)
	if err != nil {
		return Resolution{}, fmt.Errorf("enrich: resolving library %q policy: %w", libraryID, err)
	}
	return ResolveLibraryEnrichment(global, policy), nil
}

// resolveLibrary returns a Library's EFFECTIVE provider + enablement, memoized in
// libCache. On a miss it reads the Library's policy, resolves it over globalCfg
// (ResolveLibraryEnrichment), and builds the effective provider through the same
// BuildProvider seam the global path uses — pairing it with the resolved
// enablement (which encodes the policy's rules, e.g. enrich_enabled=false ⇒ off).
// It is the closure the Service calls via snapshotFor.
func (m *Manager) resolveLibrary(ctx context.Context, libraryID string) (providerSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if snap, ok := m.libCache[libraryID]; ok {
		return snap, nil
	}
	policy, err := m.store.LibraryEnrichmentPolicy(libraryID)
	if err != nil {
		return providerSnapshot{}, fmt.Errorf("enrich: resolving library %q policy: %w", libraryID, err)
	}
	res := ResolveLibraryEnrichment(m.global, policy)
	provider, _ := m.build(res.Config) // effective enablement comes from the resolver, not the build
	// Carry the effective config so the pass can honor per-item override precedence
	// (issue 06): a pinned Title resolves via its record's provider while reachable,
	// and an orphaned pin (provider made unreachable by this policy) is flagged.
	// consentGate forces the Library off when consent is not granted (ADR-0032), so
	// a per-Library pass, MatchTitle, and the background triggers all no-op the same
	// way the global snapshot does — the gate applies uniformly regardless of policy.
	snap := providerSnapshot{provider: provider, enablement: m.consentGate(res.Enablement), config: res.Config}
	m.libCache[libraryID] = snap
	return snap, nil
}

// InvalidateLibrary drops one Library's cached effective snapshot, so its next
// resolution rebuilds from the current policy. The policy-change path calls it
// before re-enriching that Library, so the re-enrich pass sees the new policy.
func (m *Manager) InvalidateLibrary(libraryID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.libCache, libraryID)
}

// EffectiveEnablement returns a Library's effective per-kind Enablement for
// DISPLAY (the API reports it so the Admin sees what a Library will enrich under
// its policy). It reads the Library's policy FRESH each call and resolves over the
// current globalCfg — independent of the pass cache, so it never depends on
// invalidation ordering. Cheap: no provider is built.
func (m *Manager) EffectiveEnablement(ctx context.Context, libraryID string) (Enablement, error) {
	res, err := m.resolvePolicy(libraryID)
	if err != nil {
		return Enablement{}, err
	}
	return res.Enablement, nil
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
	// ConsentGranted seeds the first-run Enrichment consent decision (ADR-0032) on
	// first boot: nil leaves it UNDECIDED (the fresh-install default — the SPA
	// prompts and the server makes no outbound calls), while a non-nil value records
	// that decision (a headless deploy pre-consenting via JUICEBOX_ENRICHMENT_CONSENT,
	// or the test harness granting it). An upgrade never reaches here (settings
	// aren't empty); its row was grandfathered to granted by migration 0040.
	ConsentGranted *bool
}

// SeedStore is the persistence SeedIfEmpty writes through.
type SeedStore interface {
	MetadataSettingsEmpty() (bool, error)
	UpsertMetadataProvider(u store.MetadataProviderUpsert) error
	SetMetadataLanguage(language string) error
	SetEnrichmentBehavior(autoEnrichAfterScan bool, enrichIntervalSeconds, musicBrainzRateLimitMs int) error
	SetEnrichmentConsent(granted bool) error
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
	// Seed the first-run consent decision only when one was supplied (ADR-0032). A
	// nil ConsentGranted leaves the column NULL — UNDECIDED — so a fresh install
	// prompts and makes no outbound calls until the operator answers; a non-nil
	// value records a headless pre-consent.
	if in.ConsentGranted != nil {
		if err := s.SetEnrichmentConsent(*in.ConsentGranted); err != nil {
			return false, err
		}
	}
	// The language singleton is always written — this is also the marker that the
	// settings are no longer empty, so config is never re-consulted after boot.
	if err := s.SetMetadataLanguage(in.MetadataLanguage); err != nil {
		return false, err
	}
	return true, nil
}
