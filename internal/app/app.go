// Package app wires the modules into a running server: config -> store
// (open + migrate) -> server metadata -> HTTP API. Both the cmd binary and the
// test harness boot through here, so production and tests exercise identical
// wiring (the testing goal of the PRD).
package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/marioquake/juicebox/internal/access"
	"github.com/marioquake/juicebox/internal/api"
	"github.com/marioquake/juicebox/internal/auth"
	"github.com/marioquake/juicebox/internal/catalog"
	"github.com/marioquake/juicebox/internal/config"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/events"
	"github.com/marioquake/juicebox/internal/library"
	"github.com/marioquake/juicebox/internal/match"
	"github.com/marioquake/juicebox/internal/organize"
	"github.com/marioquake/juicebox/internal/playback"
	"github.com/marioquake/juicebox/internal/scanner"
	"github.com/marioquake/juicebox/internal/server"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subfetch"
	"github.com/marioquake/juicebox/internal/transcode"
	"github.com/marioquake/juicebox/internal/webui"
)

// App is a fully wired server: an http.Handler ready to serve, plus the
// resources it owns so callers can shut it down cleanly.
type App struct {
	Config   config.Config
	DB       *store.DB
	Auth     *auth.Service
	Library  *library.Service
	Scanner  *scanner.Service
	Catalog  *catalog.Service
	Match    *match.Service
	Playback *playback.Service
	Enrich   *enrich.Service
	Organize *organize.Service
	SubFetch *subfetch.Service
	Events   *events.Broker
	Handler  http.Handler

	// enrichQueue feeds the enrich worker; the scan auto-trigger and the scheduled
	// enrich both enqueue Library ids onto it (nil when no enrich worker runs).
	enrichQueue chan string
	// enrichReschedule wakes the scheduled-enrich goroutine so a saved
	// EnrichInterval change applies promptly (enabling from 0, or shrinking a long
	// interval, takes effect immediately rather than on the next tick). Buffered
	// size 1: the settings PUT pokes it non-blockingly via SettingsChanged.
	enrichReschedule chan struct{}

	// Background-goroutine lifecycle: cancel stops every long-running goroutine
	// (the periodic scan, the session reaper, the enrich worker + scheduled
	// enrich); each closes its done channel once it has fully exited, so Close
	// shuts them down cleanly. A goroutine that was never started leaves its done
	// channel nil (skipped by Close).
	cancel          context.CancelFunc
	schedDone       chan struct{}
	reaperDone      chan struct{}
	enrichDone      chan struct{}
	enrichSchedDone chan struct{}
}

// Option overrides a wiring decision at boot. The only overridable seams today
// are the two Enrichment network boundaries (MetadataProvider + ArtworkFetcher),
// so the black-box tests can inject fakes and drive enrichment with zero network
// while production wires the real TMDB provider + HTTP fetcher. Mirrors how the
// scanner takes a Prober; kept as functional options so adding a seam later
// doesn't churn the signature.
type Option func(*options)

type options struct {
	metadataProvider enrich.MetadataProvider
	artworkFetcher   enrich.ArtworkFetcher
	providerBuilder  enrich.BuildFunc
	// subtitleProviderBuilder overrides how the subtitle-fetch Manager composes a
	// SubtitleProvider from settings (default: subfetch.BuildProvider). It is the
	// test seam for the fetch flow: a black-box test maps settings → a fake
	// provider, so a fetch drives the whole flow with ZERO network.
	subtitleProviderBuilder subfetch.BuildFunc
}

// WithMetadataProvider overrides the Enrichment MetadataProvider (tests inject a
// fake). It pins a FIXED provider: the settings-driven manager does NOT rebuild
// or swap at boot, so the injected provider stays live and its per-kind
// enablement comes from config (as before this feature). Existing enrichment
// tests rely on this.
func WithMetadataProvider(p enrich.MetadataProvider) Option {
	return func(o *options) { o.metadataProvider = p }
}

// WithArtworkFetcher overrides the Enrichment ArtworkFetcher (tests inject a fake).
func WithArtworkFetcher(f enrich.ArtworkFetcher) Option {
	return func(o *options) { o.artworkFetcher = f }
}

// WithProviderBuilder substitutes the function the provider Manager uses to
// compose a MetadataProvider + Enablement from settings (default:
// enrich.BuildProvider). It is the test seam for the write→rebuild→enrich loop: a
// black-box test maps settings → fake sub-providers, so a PUT that enables a kind
// makes the next pass enrich it with ZERO network. Unlike WithMetadataProvider,
// the manager stays active and rebuilds from the DB at boot and after each save.
func WithProviderBuilder(build enrich.BuildFunc) Option {
	return func(o *options) { o.providerBuilder = build }
}

// WithSubtitleProviderBuilder substitutes the function the subtitle-fetch Manager
// uses to compose a SubtitleProvider from settings (default:
// subfetch.BuildProvider). It is the test seam for the "search online → pick →
// track appears" flow: a black-box test maps settings → a fake provider, so the
// whole fetch flow runs with ZERO network. The Manager stays active and rebuilds
// from the DB at boot and after each settings save.
func WithSubtitleProviderBuilder(build subfetch.BuildFunc) Option {
	return func(o *options) { o.subtitleProviderBuilder = build }
}

// New boots the application from cfg: ensures the data directory exists and is
// writable, opens the SQLite database in WAL mode, runs migrations idempotently,
// and builds the HTTP handler. It does not start listening — the caller owns the
// http.Server lifecycle.
//
// New validates only what the handler wiring needs (the data directory). It does
// not require ListenAddr, because it never binds a listener — that is the serving
// caller's concern (main.go calls Config.Validate before ListenAndServe; the test
// harness drives the handler via httptest and has no listen address at all).
func New(cfg config.Config, opts ...Option) (*App, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("config: DataDir must not be empty")
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return nil, err
	}

	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: running migrations: %w", err)
	}

	meta := server.NewMetadata(db)

	authSvc, err := auth.NewService(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: initializing auth: %w", err)
	}
	// First-Admin bootstrap (ADR-0013): with zero Users, a one-time claim token
	// is generated and printed to the logs. It is held only in memory and never
	// persisted; an operator reads it from stdout/container logs to complete
	// POST /api/v1/setup. We log the token itself here (and nowhere else) — this
	// is the single, intentional disclosure point. Once an Admin exists the token
	// is empty and nothing is logged.
	if tok := authSvc.ClaimToken(); tok != "" {
		log.Printf("juicebox: no users yet — first-Admin claim token: %s", tok)
		log.Printf("juicebox: complete setup via POST /api/v1/setup with this claimToken")
	}

	librarySvc := library.NewService(db)

	// Access resolves each User's browse/play scope (granted Libraries + Rating
	// ceiling). Today it resolves every User to all-access; the read/play handlers
	// thread the Scope so the enforcing slices only change what Resolve returns.
	accessSvc := access.NewService(db)

	// Scanner derives the catalog from on-disk paths + ffprobe (ADR-0002); the
	// real Prober shells out to the ffprobe binary on PATH. Catalog is the
	// browse/read side over what the scanner persists.
	scannerSvc := scanner.NewService(db, scanner.FFprobe{})
	catalogSvc := catalog.NewService(db, cfg.ArtworkCacheDir())
	matchSvc := match.NewService(db)
	// Organize is the authored-grouping domain (Collections now; Playlists later),
	// over the same store. Its resolved member Titles are decorated by the api
	// layer with the catalog bulk readers, so a Collection grid matches a browse grid.
	organizeSvc := organize.NewService(db)
	// Playback is the negotiation domain (ADR-0003 tiers 1–2): it reads the catalog
	// tree (TitleByID) to negotiate against ffprobed attributes and owns the
	// ephemeral in-memory Playback sessions backing the stream/HLS URLs. The remux
	// (directStream) tier shells out to ffmpeg via the transcode.Runner seam,
	// writing HLS scratch under the data dir (ADR-0007). Stale scratch from a prior
	// run is cleared at boot — sessions are in-memory, so nothing on disk survives.
	scratchRoot := cfg.TranscodeScratchDir()
	if err := os.RemoveAll(scratchRoot); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: clearing transcode scratch %q: %w", scratchRoot, err)
	}
	// Governance (ADR-0009): the global concurrent-transcode cap and the HW-accel
	// knob (off by default → the CPU libx264 path). Direct play and remux stay
	// unmetered; only the transcode tier is bounded by the cap.
	playbackSvc := playback.NewService(db, transcode.FFmpeg{}, scratchRoot, playback.Governance{
		MaxConcurrentTranscodes: cfg.MaxConcurrentTranscodes,
		Accel:                   resolveAccel(cfg.HardwareAccel),
	})

	// Enrichment (external-metadata-enrichment): the separate, optional decorator
	// step (ADR-0002). Its two network seams default to the real TMDB provider +
	// guarded HTTP fetcher, but tests inject fakes via Options so the black-box
	// suite runs with zero network. Disabled (a logged no-op, status 'disabled')
	// until a provider key is configured (ADR-0001). Fetched artwork lands in the
	// durable artwork cache under the data dir (ADR-0007); unlike transcode scratch
	// it is NOT cleared at boot.
	// Compose the enrichment provider + its per-kind enablement snapshot. The
	// composition logic lives once, in enrich.BuildProvider (a future settings-
	// driven rebuild calls the same builder and hot-swaps via Service.SetProvider).
	// app.New maps config.Config → the builder's decoupled ProviderConfig, mirroring
	// how it maps config → playback.Governance (ADR-0006: the domain never imports
	// config). A test-injected fixed provider (WithMetadataProvider) bypasses the
	// builder but still takes its enablement from config, exactly as before.
	provider := o.metadataProvider
	enablement := enrich.Enablement{Video: cfg.VideoEnrichmentEnabled(), Music: cfg.MusicEnrichmentEnabled()}
	if provider == nil {
		provider, enablement = enrich.BuildProvider(enrich.ProviderConfig{
			TMDBAPIKey:           cfg.TMDBAPIKey,
			TMDBBaseURL:          cfg.TMDBBaseURL,
			TMDBImageBaseURL:     cfg.TMDBImageBaseURL,
			MetadataLanguage:     cfg.MetadataLanguage,
			MusicBrainzEnabled:   cfg.MusicBrainzEnabled,
			MusicBrainzBaseURL:   cfg.MusicBrainzBaseURL,
			CoverArtBaseURL:      cfg.CoverArtBaseURL,
			MusicBrainzRateLimit: cfg.MusicBrainzRateLimit,
			FanartTVAPIKey:       cfg.FanartTVAPIKey,
			FanartTVBaseURL:      cfg.FanartTVBaseURL,
			TheAudioDBAPIKey:     cfg.TheAudioDBAPIKey,
			TheAudioDBBaseURL:    cfg.TheAudioDBBaseURL,
		})
	}
	fetcher := o.artworkFetcher
	if fetcher == nil {
		fetcher = enrich.HTTPArtworkFetcher{}
	}
	if err := enrich.EnsureCacheDir(cfg.ArtworkCacheDir()); err != nil {
		_ = db.Close()
		return nil, err
	}
	enrichSvc := enrich.NewService(db, provider, fetcher, enablement, cfg.ArtworkCacheDir(), cfg.ArtworkCandidateCacheTTL)

	// First-boot seed + source-of-truth handoff (metadata-providers 02): if the
	// DB-backed provider settings have never been written, seed them from
	// config.Config so an existing env-configured deployment behaves identically.
	// Thereafter the DB is authoritative and the config provider values are ignored
	// at runtime (documented in config.go). Idempotent — seeds only when empty.
	seedInput := seedInputFromConfig(cfg)
	if _, err := enrich.SeedIfEmpty(db, seedInput); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: seeding metadata provider settings: %w", err)
	}
	// Upgrade backfill (enrichment-runtime-settings): a deployment that already ran
	// 0018 has a metadata_settings row (language set) but the three 0019 behavior
	// columns NULL — SeedIfEmpty won't fire (settings aren't empty), so fill any
	// still-NULL column from config here. Idempotent (COALESCE keeps an operator's
	// existing value), so a later boot never reverts a UI-saved change.
	if err := db.BackfillEnrichmentBehaviorIfUnset(
		seedInput.AutoEnrichAfterScan, seedInput.EnrichIntervalSeconds, seedInput.MusicBrainzRateLimitMs,
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: backfilling enrichment behavior: %w", err)
	}

	// The provider Manager reads settings → builds → atomically swaps the running
	// Service (metadata-providers 02). Production composes via enrich.BuildProvider;
	// a black-box test substitutes a fake builder (WithProviderBuilder) to drive the
	// write→rebuild→enrich loop with zero network. Every provider input is now
	// DB-backed — including the MusicBrainz throttle, which the Manager reads from
	// store.EnrichmentBehavior on each Reload (so app.New no longer passes it from
	// cfg), and every base URL including the TMDB image host.
	providerBuild := enrich.BuildFunc(enrich.BuildProvider)
	if o.providerBuilder != nil {
		providerBuild = o.providerBuilder
	}
	providerManager := enrich.NewManager(db, enrichSvc, providerBuild)
	// Apply the persisted settings at boot so the DB is authoritative — UNLESS a
	// fixed provider was injected (WithMetadataProvider), which pins that provider
	// and its config-derived enablement for the existing enrichment tests.
	if o.metadataProvider == nil {
		if err := providerManager.Reload(context.Background()); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("app: applying metadata provider settings: %w", err)
		}
	}

	// External subtitle fetching (ADR-0021), mirroring the enrichment wiring above:
	// a durable cache dir + a Service whose provider the Manager rebuilds from
	// DB-backed settings (seeded once from config), so a "search online" hot-swaps
	// with no restart and a disabled server does zero outbound work.
	if err := subfetch.EnsureCacheDir(cfg.SubtitleCacheDir()); err != nil {
		_ = db.Close()
		return nil, err
	}
	subFetchSvc := subfetch.NewService(db, cfg.SubtitleCacheDir())
	if _, err := subfetch.SeedIfEmpty(db, subfetch.SeedInput{
		OpenSubtitlesAPIKey:  cfg.OpenSubtitlesAPIKey,
		OpenSubtitlesBaseURL: cfg.OpenSubtitlesBaseURL,
		AutoFetchLang:        cfg.SubtitleAutoFetchLang,
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: seeding subtitle provider settings: %w", err)
	}
	subtitleBuild := subfetch.BuildFunc(subfetch.BuildProvider)
	if o.subtitleProviderBuilder != nil {
		subtitleBuild = o.subtitleProviderBuilder
	}
	subtitleManager := subfetch.NewManager(db, subFetchSvc, subtitleBuild)
	if err := subtitleManager.Reload(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: applying subtitle provider settings: %w", err)
	}

	// Realtime spine (ADR-0016): one Broker fans out enrichProgress to the SSE
	// /events stream. Created unconditionally (cheap) so /events always works; the
	// producers below publish onto it.
	broker := events.NewBroker()

	// Session lifecycle → realtime (issue 03): translate the Playback Manager's
	// observer transitions into the Broker's Admin-only session events. Wired via
	// the setter AFTER both the Service and Broker exist (the Broker is created
	// below the Service), mirroring how SetNow is applied post-construction rather
	// than threaded through NewService. The idle reaper (runSessionReaper →
	// ReapIdle → Reap) fires the same observer, so reaped sessions emit
	// sessionEnded automatically — no separate wiring.
	playbackSvc.Sessions().SetObserver(func(e playback.SessionEvent) {
		var eventType string
		switch e.Kind {
		case playback.SessionStarted:
			eventType = events.TypeSessionStarted
		case playback.SessionNowPlaying:
			eventType = events.TypeNowPlaying
		case playback.SessionEnded:
			eventType = events.TypeSessionEnded
		default:
			return
		}
		broker.PublishSessionEvent(eventType, events.SessionEvent{
			SessionID:  e.SessionID,
			UserID:     e.UserID,
			TitleID:    e.TitleID,
			PositionMs: e.PositionMs,
		})
	})

	// Enrichment triggering (external-metadata-enrichment issue 02, made runtime-
	// configurable by enrichment-runtime-settings). Auto-after-scan and the
	// scheduled enrich both feed one worker via enrichQueue; the worker AND the
	// scheduler now run UNCONDITIONALLY and read the CURRENT behavior settings from
	// the DB at trigger time, so toggling AutoEnrichAfterScan or changing
	// EnrichInterval in the UI takes effect with no restart. Every trigger also
	// gates on the live per-kind enablement (enqueueEnrichIfEnabled), so a fully
	// unconfigured server enqueues nothing and the idle worker just blocks on its
	// queue — no outbound call is ever made (ADR-0001 offline-first).
	runEnrichWorker := true

	app := &App{
		Config:           cfg,
		DB:               db,
		Auth:             authSvc,
		Library:          librarySvc,
		Scanner:          scannerSvc,
		Catalog:          catalogSvc,
		Match:            matchSvc,
		Playback:         playbackSvc,
		Enrich:           enrichSvc,
		Organize:         organizeSvc,
		SubFetch:         subFetchSvc,
		Events:           broker,
		enrichReschedule: make(chan struct{}, 1),
	}
	app.enrichQueue = make(chan string, 64)

	// The scan handler's auto-after-scan hook, wired unconditionally: it enqueues a
	// non-blocking background pass only when AutoEnrichAfterScan is CURRENTLY on
	// (live DB read) AND a kind is currently enabled, so a toggle in the UI applies
	// to the next scan with no restart and a disabled server does no background work.
	enrichTrigger := app.enqueueEnrichAfterScan

	apiHandler := api.Handler(api.Deps{
		Meta:            meta,
		Auth:            authSvc,
		Access:          accessSvc,
		Library:         librarySvc,
		Scanner:         scannerSvc,
		Catalog:         catalogSvc,
		Match:           matchSvc,
		Playback:        playbackSvc,
		Enrich:          enrichSvc,
		Organize:        organizeSvc,
		Events:          broker,
		EnrichTrigger:   enrichTrigger,
		ScanStatus:      db,
		Libraries:       db,
		Providers:       db,
		ProviderManager: providerManager,
		SettingsChanged: app.notifyEnrichReschedule,

		SubFetch:                subFetchSvc,
		SubtitleProviders:       db,
		SubtitleProviderManager: subtitleManager,
	})

	// Top-level composition (ADR-0012): /api/v1 stays the API's; every other
	// path is served from the embedded SPA bundle with an index.html fallback
	// for client-side routes. One process, one port (ADR-0006).
	app.Handler = webui.Handler(apiHandler)

	// Background goroutines share one cancellable context so Close stops them
	// together. Create it only if at least one is enabled (a zero interval/timeout
	// disables that goroutine), so a fully-quiet server starts none.
	if cfg.ScanInterval > 0 || cfg.SessionIdleTimeout > 0 || runEnrichWorker {
		ctx, cancel := context.WithCancel(context.Background())
		app.cancel = cancel

		// Scheduled scan (ADR-0008): an always-on safety net that re-walks every
		// Library on a configurable interval, so changes a manual scan never picked
		// up still get caught. It is incremental, so an unchanged library costs only
		// a directory walk (no re-ffprobe). A zero interval disables it. NO
		// filesystem watching — explicitly out of scope (ADR-0008).
		if cfg.ScanInterval > 0 {
			app.schedDone = make(chan struct{})
			go app.runScheduledScans(ctx, cfg.ScanInterval)
		}

		// Session reaper (issue 08): progress reports double as keepalive, so a
		// Playback session silent past the idle timeout is abandoned and ended (for
		// direct play there is no transcode scratch to free — just the map delete).
		// A zero timeout disables it. The sweep cadence is independent of the
		// timeout and bounds how long a reaped session lingers before its endpoints
		// answer 404.
		if cfg.SessionIdleTimeout > 0 {
			// Sweep no slower than the timeout itself, so a short test timeout gets a
			// correspondingly short sweep without a separate knob.
			every := config.SessionReapInterval
			if cfg.SessionIdleTimeout < every {
				every = cfg.SessionIdleTimeout
			}
			app.reaperDone = make(chan struct{})
			go app.runSessionReaper(ctx, cfg.SessionIdleTimeout, every)
		}

		// Enrich worker: drains enrichQueue, running one background ModeNew pass per
		// enqueued Library and publishing enrichProgress as it goes. Fed by the auto-
		// after-scan hook and the scheduled enrich below. Started whenever either
		// trigger is on (external-metadata-enrichment issue 02).
		if runEnrichWorker {
			app.enrichDone = make(chan struct{})
			go app.runEnrichWorker(ctx)
		}

		// Scheduled enrich (ADR-0001/0008 safety net): sweeps every Library on the
		// DB-configured interval, enqueuing a pass that backfills still-'pending'
		// Titles. Started UNCONDITIONALLY (enrichment-runtime-settings): the goroutine
		// reads EnrichInterval from the DB each cycle, so a runtime change (including
		// 0→enabled) applies with no restart — it parks on the wake channel while the
		// interval is 0 and is poked awake by the settings PUT.
		app.enrichSchedDone = make(chan struct{})
		go app.runScheduledEnrich(ctx)
	}

	return app, nil
}

// runSessionReaper sweeps idle Playback sessions on each tick until ctx is
// cancelled, ending any whose last progress report is older than idle. Errors
// are impossible here (the sweep is a pure in-memory map walk); the goroutine
// simply runs until shutdown.
func (a *App) runSessionReaper(ctx context.Context, idle, every time.Duration) {
	defer close(a.reaperDone)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.Playback.ReapIdle(idle)
		}
	}
}

// runScheduledScans incrementally scans every Library on each tick until ctx is
// cancelled. Errors are logged, never fatal — the safety net must keep running.
func (a *App) runScheduledScans(ctx context.Context, interval time.Duration) {
	defer close(a.schedDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			libs, err := a.DB.Libraries()
			if err != nil {
				log.Printf("juicebox: scheduled scan: listing libraries: %v", err)
				continue
			}
			// Same scanProgress publish the manual scan handler does, so the
			// scheduled path surfaces an advancing "scanning…" indicator the same
			// way (PRD story 4). LibraryID flows through the payload, so one
			// callback serves every Library in the sweep.
			onProgress := func(p scanner.Progress) {
				a.Events.PublishScanProgress(events.ScanProgress{
					LibraryID:   p.LibraryID,
					TitlesFound: p.TitlesFound,
					FilesFound:  p.FilesFound,
					Complete:    p.Complete,
				})
			}
			for _, lib := range libs {
				if ctx.Err() != nil {
					return
				}
				if _, err := a.Scanner.ScanModeProgress(ctx, lib.ID, scanner.ModeIncremental, onProgress); err != nil {
					// A cancelled ctx is shutdown, not a scan failure: don't log it as
					// an error or emit a spurious terminal event — just stop the sweep
					// (mirrors runEnrichPass's ctx.Err() guard).
					if ctx.Err() != nil {
						return
					}
					log.Printf("juicebox: scheduled scan of %q: %v", lib.ID, err)
					// Terminal-on-error: emit a Complete event so a client's
					// "scanning…" indicator clears instead of hanging.
					a.Events.PublishScanProgress(events.ScanProgress{LibraryID: lib.ID, Complete: true})
					continue
				}
				// A completed scan is a content-change point: nudge connected
				// clients to refetch the Library (library-scoped). Same publish the
				// manual scan handler does, so the scheduled path surfaces changes
				// the same way (PRD story 4).
				a.Events.PublishLibraryUpdated(lib.ID)
				// Auto-after-scan: enqueue a background Enrichment pass for any
				// newly-added/changed Titles (non-blocking; a live no-op when
				// AutoEnrichAfterScan is off or no kind is currently enabled).
				a.enqueueEnrichAfterScan(lib.ID)
			}
		}
	}
}

// enqueueEnrichIfEnabled enqueues a background pass only when enrichment is
// currently enabled in the live snapshot, so a disabled/unconfigured server does
// NO background work — its Titles stay 'pending' rather than being marked
// 'disabled' by a pointless pass (ADR-0001 offline-first). The manual pass
// (POST .../enrich) is unaffected; it goes straight through the Service, which
// records 'disabled' when off. Enablement changes at runtime via Manager.Reload.
func (a *App) enqueueEnrichIfEnabled(libraryID string) {
	if !a.Enrich.EnrichmentEnabled() {
		return
	}
	a.enqueueEnrich(libraryID)
}

// enqueueEnrichAfterScan is the auto-after-scan trigger (manual + scheduled scans).
// It reads the CURRENT AutoEnrichAfterScan flag from the DB so a UI toggle applies
// to the next scan with no restart, then defers to enqueueEnrichIfEnabled (which
// gates on live per-kind enablement). A read error is logged and treated as "off"
// so a transient DB hiccup never blocks or crashes the scan path.
func (a *App) enqueueEnrichAfterScan(libraryID string) {
	behavior, err := a.DB.EnrichmentBehavior()
	if err != nil {
		log.Printf("juicebox: reading auto-enrich setting: %v", err)
		return
	}
	if !behavior.Auto() {
		return
	}
	a.enqueueEnrichIfEnabled(libraryID)
}

// notifyEnrichReschedule pokes the scheduled-enrich goroutine awake so a saved
// EnrichInterval change applies promptly. Non-blocking (buffered size 1): a poke
// that races an already-pending one is simply coalesced. Wired into api.Deps as
// SettingsChanged; the settings PUT calls it after a successful save.
func (a *App) notifyEnrichReschedule() {
	select {
	case a.enrichReschedule <- struct{}{}:
	default:
	}
}

// enqueueEnrich requests a background Enrichment pass for a Library. It is
// non-blocking: if no worker is running (enrichment off) it is a no-op, and a
// full queue drops the request (the scheduled sweep / a later scan will catch
// it) rather than stalling the scan path.
func (a *App) enqueueEnrich(libraryID string) {
	if a.enrichQueue == nil {
		return
	}
	select {
	case a.enrichQueue <- libraryID:
	default:
		log.Printf("juicebox: enrich queue full, dropping %q", libraryID)
	}
}

// runEnrichWorker drains enrichQueue until ctx is cancelled, running one
// background ModeNew Enrichment pass per enqueued Library. Serialized by the
// enrich service's per-Library lock, so it never races a concurrent pass.
func (a *App) runEnrichWorker(ctx context.Context) {
	defer close(a.enrichDone)
	for {
		select {
		case <-ctx.Done():
			return
		case libID := <-a.enrichQueue:
			a.runEnrichPass(ctx, libID)
		}
	}
}

// runEnrichPass runs one pass and publishes enrichProgress (per-Title) plus a
// terminal Complete event. Errors are logged, never fatal — enrichment is the
// optional decorator step (ADR-0001/0002).
func (a *App) runEnrichPass(ctx context.Context, libID string) {
	cb := func(p enrich.Progress) {
		a.Events.PublishEnrichProgress(events.EnrichProgress{
			LibraryID: p.LibraryID, Total: p.Total, Done: p.Done,
			Matched: p.Matched, Unmatched: p.Unmatched, Failed: p.Failed, Disabled: p.Disabled,
		})
	}
	res, err := a.Enrich.EnrichLibraryProgress(ctx, libID, enrich.ModeNew, cb)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("juicebox: enrich pass of %q: %v", libID, err)
		}
		return
	}
	a.Events.PublishEnrichProgress(events.EnrichProgress{
		LibraryID: libID, Total: res.Total, Done: res.Total,
		Matched: res.Matched, Unmatched: res.Unmatched,
		Failed: res.Failed, Disabled: res.Disabled, Complete: true,
	})
	// The pass changed the Library's metadata/artwork: nudge clients to refetch
	// (library-scoped), mirroring the manual enrich handler (PRD story 6).
	a.Events.PublishLibraryUpdated(libID)
}

// runScheduledEnrich is the always-on safety-net sweep, reworked to be dynamically
// reconfigurable (enrichment-runtime-settings). It does NOT capture a fixed
// interval: each cycle it reads EnrichInterval from the DB and either parks (when
// the interval is <= 0 / disabled) or waits that long and then sweeps every Library,
// enqueuing a pass that backfills still-'pending' Titles. It selects on {a timer
// sized to the current interval, the wake channel, ctx.Done()}: a settings PUT pokes
// the wake channel (via SettingsChanged), so re-enabling from 0 or shrinking a long
// interval applies immediately rather than on the next tick. Errors are logged,
// never fatal — the safety net must keep running.
func (a *App) runScheduledEnrich(ctx context.Context) {
	defer close(a.enrichSchedDone)
	for {
		secs := a.enrichIntervalSeconds()
		if secs <= 0 {
			// Disabled: park until a settings change wakes us (or shutdown).
			select {
			case <-ctx.Done():
				return
			case <-a.enrichReschedule:
				continue
			}
		}
		timer := time.NewTimer(time.Duration(secs) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.enrichReschedule:
			// Settings changed: re-read the interval and resize the timer.
			timer.Stop()
			continue
		case <-timer.C:
			// A change to 0 while we waited disables the sweep for this cycle.
			if a.enrichIntervalSeconds() <= 0 {
				continue
			}
			a.sweepEnrich(ctx)
		}
	}
}

// enrichIntervalSeconds reads the current scheduled-enrich cadence from the DB (0
// disables the sweep). A read error is logged and treated as disabled, so a
// transient DB hiccup parks the scheduler rather than spinning.
func (a *App) enrichIntervalSeconds() int {
	behavior, err := a.DB.EnrichmentBehavior()
	if err != nil {
		log.Printf("juicebox: reading enrich interval: %v", err)
		return 0
	}
	return behavior.IntervalSeconds()
}

// sweepEnrich enqueues a background enrich pass for every Library (the safety-net
// backfill), gated per-Library on the live enablement snapshot.
func (a *App) sweepEnrich(ctx context.Context) {
	libs, err := a.DB.Libraries()
	if err != nil {
		log.Printf("juicebox: scheduled enrich: listing libraries: %v", err)
		return
	}
	for _, lib := range libs {
		if ctx.Err() != nil {
			return
		}
		a.enqueueEnrichIfEnabled(lib.ID)
	}
}

// Close releases resources owned by the App: it stops the background goroutines
// (the scheduled scan, the session reaper, the enrich worker + scheduled
// enrich), waits for each to exit, closes the realtime Broker (releasing any SSE
// subscribers), and closes the database.
func (a *App) Close() error {
	if a.cancel != nil {
		a.cancel()
		if a.schedDone != nil {
			<-a.schedDone
		}
		if a.reaperDone != nil {
			<-a.reaperDone
		}
		if a.enrichDone != nil {
			<-a.enrichDone
		}
		if a.enrichSchedDone != nil {
			<-a.enrichSchedDone
		}
		a.cancel = nil
	}
	if a.Events != nil {
		a.Events.Close()
	}
	if a.DB != nil {
		return a.DB.Close()
	}
	return nil
}

// resolveAccel turns the operator-facing HardwareAccel knob into the concrete,
// VALIDATED transcode.Accel the playback Service runs with — the setup-time
// detection/validation pass (ADR-0009: detection is a setup-time concern, never
// per-stream). It maps config → the preference (accelFromConfig), then runs the
// detector ONCE: the preference is validated against the host (encoder present +
// a real test-encode) and resolved to a concrete backend, or warned-and-fallen-
// back to the always-available CPU path. A misconfigured/absent backend can never
// take down playback — the server always boots and plays. A bounded context keeps
// a hung ffmpeg from blocking boot; the probes are sub-second in practice.
//
// A single startup log line records the resolution (loudly when an explicitly-
// configured backend fell back to CPU), so an operator can confirm the GPU is
// actually being used or see why it isn't.
func resolveAccel(h config.HWAccel) transcode.Accel {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res := transcode.NewDetector("").Resolve(ctx, accelFromConfig(h))
	if res.Warn {
		log.Printf("juicebox: hardware acceleration: WARNING — %s", res.Reason)
	} else {
		log.Printf("juicebox: hardware acceleration: %s", res.Reason)
	}
	return res.Accel
}

// seedInputFromConfig maps the config provider knobs to enrich.SeedInput — the one
// place the config vocabulary meets the Enrichment domain (enrich never imports
// config, ADR-0006). Used only for the first-boot seed: after the DB-backed
// provider settings exist, config's provider values are ignored at runtime.
func seedInputFromConfig(cfg config.Config) enrich.SeedInput {
	return enrich.SeedInput{
		TMDBAPIKey:         cfg.TMDBAPIKey,
		TMDBBaseURL:        cfg.TMDBBaseURL,
		TMDBImageBaseURL:   cfg.TMDBImageBaseURL,
		MetadataLanguage:   cfg.MetadataLanguage,
		MusicBrainzEnabled: cfg.MusicBrainzEnabled,
		MusicBrainzBaseURL: cfg.MusicBrainzBaseURL,
		CoverArtBaseURL:    cfg.CoverArtBaseURL,
		FanartTVAPIKey:     cfg.FanartTVAPIKey,
		FanartTVBaseURL:    cfg.FanartTVBaseURL,
		TheAudioDBAPIKey:   cfg.TheAudioDBAPIKey,
		TheAudioDBBaseURL:  cfg.TheAudioDBBaseURL,
		// Behavior knobs seeded from config on first boot (then DB-authoritative).
		// Durations collapse to the DB's integer units: seconds for the interval,
		// milliseconds for the throttle.
		AutoEnrichAfterScan:    cfg.AutoEnrichAfterScan,
		EnrichIntervalSeconds:  int(cfg.EnrichInterval / time.Second),
		MusicBrainzRateLimitMs: int(cfg.MusicBrainzRateLimit / time.Millisecond),
	}
}

// accelFromConfig maps the operator-facing config.HWAccel knob to the transcode
// tier's preference Accel — the one place the config vocabulary meets the encode
// backend (config stays low-level and never imports transcode). HWAccelOff (and
// the zero value) is the always-available CPU path; every other value passes its
// 1:1 transcode.Accel through to the detector (resolveAccel), which validates it
// and resolves auto / an explicit backend to a concrete, validated value.
func accelFromConfig(h config.HWAccel) transcode.Accel {
	switch h {
	case config.HWAccelAuto:
		return transcode.AccelAuto
	case config.HWAccelNVENC:
		return transcode.AccelNVENC
	case config.HWAccelVAAPI:
		return transcode.AccelVAAPI
	case config.HWAccelQSV:
		return transcode.AccelQSV
	case config.HWAccelVideoToolbox:
		return transcode.AccelVideoToolbox
	default: // HWAccelOff, "" (zero value), or any unexpected value → CPU.
		return transcode.AccelCPU
	}
}
