package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/marioquake/juicebox/internal/config"
	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/rotation"
	"github.com/marioquake/juicebox/internal/server"
	"github.com/marioquake/juicebox/internal/store"
)

// rotationFetchTimeout bounds a single rotation fetch so a hung endpoint never
// stalls the poll loop (and, transitively, never wedges anything waiting on it).
// The endpoint answers a few hundred bytes; 15s is generous for a slow network.
const rotationFetchTimeout = 15 * time.Second

// keyRotator is the app-side orchestration of the optional key-rotation channel
// (ADR-0032, layer 2): it wraps the pure rotation.Client with the policy the client
// deliberately omits — the first-run consent gate (no external contact before the
// operator opts in, issue 01), the durable cache under the data dir (ADR-0007), and
// the propagation of a fetched default into the running provider (a DB upsert +
// Manager.Reload) so a rotated key is picked up on the next poll.
//
// It exists only for an OFFICIAL build (one with a build-injected app encryption
// key) that has not fully opted out; newKeyRotator returns nil otherwise, and the
// app then starts no rotation goroutine and makes zero maintainer contact.
type keyRotator struct {
	cfg      config.Config
	db       *store.DB
	manager  *enrich.Manager
	client   rotation.Client
	interval time.Duration

	// mu serializes refreshOnce so the periodic loop and the test-facing
	// RefreshRotationKeysNow can never race on the provenance fields or the cache.
	mu sync.Mutex
	// plantedTMDB / plantedFanart record the last DEFAULT key this rotator wrote into
	// each provider's DB row (seeded to the bootstrap key, then updated to each
	// adopted rotation key). They are the provenance guard: the rotator overwrites a
	// provider's key ONLY when the DB still holds a value it planted (or is empty), so
	// an operator's own key entered through the admin UI (a value the rotator never
	// planted) is never clobbered — BYOK wins, exactly as the resolver promises.
	plantedTMDB   string
	plantedFanart string
	// warned makes failure logging fire ONCE per failure streak (reset on success),
	// so a persistently-down endpoint polled every N hours never turns into a log
	// storm (ADR-0032 fail-safe: log once, fall through to bootstrap).
	warned bool
	// now supplies the cache fetch-time; a field so a test could pin it (defaults to
	// time.Now).
	now func() time.Time
}

// newKeyRotator builds the rotator, or returns nil when the rotation channel is off
// for this deployment (ADR-0032). It is off when: the enc key is absent (a build-
// from-source binary can't decrypt any payload — the honest "no bundled keys" path);
// JUICEBOX_KEY_ROTATION=off; no endpoint URL; or the operator has supplied BOTH
// default-provider keys via BYOK (env), which bypasses the channel entirely for zero
// maintainer contact. The url/encKey overrides (WithKeyRotation) let a black-box
// test inject a stub endpoint + known key without ldflags; production passes empty
// overrides and falls back to cfg.KeyRotationURL + the build-injected AppEncKey.
func newKeyRotator(cfg config.Config, db *store.DB, manager *enrich.Manager, urlOverride, encKeyOverride string) *keyRotator {
	url := cfg.KeyRotationURL
	if urlOverride != "" {
		url = urlOverride
	}
	encKey := config.AppEncKey()
	if encKeyOverride != "" {
		encKey = encKeyOverride
	}

	// A URL override (test) forces the channel on; otherwise honor the disable flag.
	enabled := cfg.KeyRotationEnabled || urlOverride != ""
	if !enabled || url == "" || encKey == "" {
		return nil
	}
	// Full BYOK (both env keys set) bypasses the channel outright — one of the two
	// independent ways (with the disable flag) to reach zero maintainer contact. A
	// partial BYOK still runs, so the un-BYOK'd provider can still rotate; the
	// per-provider resolver + provenance guard keep the BYOK'd one untouched.
	if cfg.TMDBAPIKey != "" && cfg.FanartTVAPIKey != "" {
		return nil
	}

	return &keyRotator{
		cfg:      cfg,
		db:       db,
		manager:  manager,
		interval: cfg.KeyRotationInterval,
		client: rotation.Client{
			URL:        url,
			EncKeyB64:  encKey,
			AppVersion: server.Version,
			UserAgent:  "juicebox/" + server.Version,
			HTTP:       &http.Client{Timeout: rotationFetchTimeout},
		},
		// Seed the provenance guard with the bootstrap keys: on a fresh official
		// install the seed planted these into the DB provider rows, so a first
		// rotation may safely replace them.
		plantedTMDB:   config.BootstrapTMDBKey(),
		plantedFanart: config.BootstrapFanartKey(),
		now:           time.Now,
	}
}

// runKeyRotation is the poll loop: a startup fetch, then a re-poll every interval
// (ADR-0032). It parks on the wake channel when the interval is 0 (re-poll
// disabled) so a consent grant still triggers a fetch. Every fetch is best-effort —
// errors are logged once and never propagate — so a maintainer outage never
// degrades the server beyond metadata freshness (ADR-0001) and boot is never
// blocked (the loop runs entirely in this goroutine).
func (a *App) runKeyRotation(ctx context.Context) {
	defer close(a.rotationDone)

	// Startup fetch (consent-gated inside refreshOnce; a no-op until the operator
	// has opted in).
	a.refreshRotationSafe(ctx)

	for {
		if a.keyRotator.interval <= 0 {
			// No periodic re-poll: park until a settings change (e.g. consent granted)
			// wakes us, or shutdown.
			select {
			case <-ctx.Done():
				return
			case <-a.rotationWake:
				a.refreshRotationSafe(ctx)
			}
			continue
		}
		timer := time.NewTimer(a.keyRotator.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.rotationWake:
			timer.Stop()
			a.refreshRotationSafe(ctx)
		case <-timer.C:
			a.refreshRotationSafe(ctx)
		}
	}
}

// refreshRotationSafe runs one refresh and logs a failure at most once per failure
// streak (reset on the next success), so a persistently-unreachable endpoint polled
// on a multi-hour cadence never spams the log (ADR-0032: log once, no retry storm).
func (a *App) refreshRotationSafe(ctx context.Context) {
	if a.keyRotator == nil {
		return
	}
	err := a.keyRotator.refreshOnce(ctx)
	a.keyRotator.mu.Lock()
	defer a.keyRotator.mu.Unlock()
	if err != nil {
		if !a.keyRotator.warned {
			log.Printf("juicebox: metadata credentials: rotation fetch failed, using the last cached/bootstrap key: %v", err)
			a.keyRotator.warned = true
		}
		return
	}
	a.keyRotator.warned = false
}

// RefreshRotationKeysNow forces one synchronous rotation refresh — fetch, decrypt,
// cache, propagate, and (if a default key changed) rebuild the running provider.
// It is the deterministic test seam the harness drives to simulate a poll without
// waiting on the interval timer; production uses the periodic loop. Nil-safe: a
// disabled channel (no rotator) returns nil. Errors are returned as-is (the loop
// swallows-and-logs them; a test asserts on them).
func (a *App) RefreshRotationKeysNow(ctx context.Context) error {
	if a.keyRotator == nil {
		return nil
	}
	return a.keyRotator.refreshOnce(ctx)
}

// refreshOnce performs one full refresh cycle. It is consent-gated (no external
// contact — the rotation endpoint included — before the operator opts in, issue
// 01), fail-safe (any fetch/decrypt error leaves the last cache and running provider
// untouched), and idempotent (a payload equal to the current default is a no-op, so
// re-polls don't churn the DB or Reload). Serialized by mu.
func (kr *keyRotator) refreshOnce(ctx context.Context) error {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	// Consent gate (ADR-0032, issue 01): the endpoint is an external metadata contact
	// like any provider, so no fetch happens before consent is recorded. Not an error
	// — simply not yet permitted; the loop retries when consent is granted (poked via
	// rotationWake).
	consent, err := kr.db.EnrichmentConsent()
	if err != nil {
		return fmt.Errorf("reading enrichment consent: %w", err)
	}
	if !consent.Granted {
		return nil
	}

	fetchCtx, cancel := context.WithTimeout(ctx, rotationFetchTimeout)
	defer cancel()
	keys, err := kr.client.Fetch(fetchCtx)
	if err != nil {
		return err
	}

	// Persist to the durable cache with the fetch time (ADR-0007) so restarts and
	// brief outages reuse the last good keys rather than dropping to bootstrap.
	if err := rotation.SaveCache(kr.cachePath(), rotation.Cache{
		TMDB: keys.TMDB, Fanart: keys.Fanart, FetchedAt: kr.now().UTC(), V: rotation.SupportedVersion,
	}); err != nil {
		return fmt.Errorf("caching rotation keys: %w", err)
	}

	changed, err := kr.propagate(config.RotationKeys{TMDB: keys.TMDB, Fanart: keys.Fanart})
	if err != nil {
		return err
	}
	if changed {
		// Rebuild the running provider so the rotated key takes effect immediately
		// (the next enrich pass uses it) — this is what "picked up on the next poll"
		// means. Reload also re-reads consent, so the swap stays consent-consistent.
		if err := kr.manager.Reload(ctx); err != nil {
			return fmt.Errorf("reloading provider after rotation: %w", err)
		}
		log.Printf("juicebox: metadata credentials: adopted a rotated default key from the rotation endpoint")
	}
	return nil
}

// cachePath is the durable rotation-cache location under the data dir.
func (kr *keyRotator) cachePath() string { return kr.cfg.MetadataKeysPath() }

// propagate applies the freshly fetched keys as the default-credential layer,
// updating each managed provider's DB row where the rotator owns the key. It
// returns whether any provider's key actually changed (so the caller only Reloads
// when needed). The resolver decides WHICH default wins (operator → rotation →
// bootstrap); the provenance guard in applyDefault decides WHETHER the rotator may
// write it (never over an operator's admin-UI key).
func (kr *keyRotator) propagate(rot config.RotationKeys) (bool, error) {
	rows, err := kr.db.MetadataProviders()
	if err != nil {
		return false, fmt.Errorf("reading provider rows: %w", err)
	}
	byslug := make(map[string]store.MetadataProviderRow, len(rows))
	for _, r := range rows {
		byslug[r.Slug] = r
	}

	tmdbKey, tmdbSrc := kr.cfg.ResolveTMDBKey(rot)
	tmdbChanged, err := kr.applyDefault(enrich.SlugTMDB, byslug, tmdbKey, tmdbSrc, &kr.plantedTMDB)
	if err != nil {
		return false, err
	}
	fanartKey, fanartSrc := kr.cfg.ResolveFanartTVKey(rot)
	fanartChanged, err := kr.applyDefault(enrich.SlugFanartTV, byslug, fanartKey, fanartSrc, &kr.plantedFanart)
	if err != nil {
		return false, err
	}
	return tmdbChanged || fanartChanged, nil
}

// applyDefault writes one provider's resolved DEFAULT key into its DB row, honoring
// the precedence chain and the provenance guard. It returns whether the stored key
// changed. It writes nothing (and reports no change) when: the operator's own BYOK
// key won (operator source — never touch it); no default exists for the provider
// (none source); the current DB key is an operator admin-UI override (a value this
// rotator never planted); or the resolved key already matches what's stored. When it
// does write, it preserves the operator's enabled + base-URL choices, enabling the
// provider only when creating its row fresh so a newly-arrived default turns the
// kind on.
func (kr *keyRotator) applyDefault(slug string, byslug map[string]store.MetadataProviderRow, key string, src config.CredentialSource, planted *string) (bool, error) {
	if src == config.CredentialOperator || key == "" {
		return false, nil
	}
	row, exists := byslug[slug]
	current := ""
	if exists {
		current = row.APIKey
	}
	// Provenance guard: only overwrite an empty slot or a key we planted. A different
	// non-empty value is an operator admin-UI key — BYOK wins, so leave it and stop
	// managing this provider's key.
	if current != "" && current != *planted {
		return false, nil
	}
	if key == current {
		*planted = key // re-affirm (e.g. bootstrap already in place); no write needed
		return false, nil
	}
	up := store.MetadataProviderUpsert{
		Slug:         slug,
		APIKey:       key,
		Enabled:      true, // creating fresh: a bundled default should turn the kind on
		BaseURL:      row.BaseURL,
		ImageBaseURL: row.ImageBaseURL,
	}
	if exists {
		up.Enabled = row.Enabled // preserve an operator's enable/disable choice
	}
	if err := kr.db.UpsertMetadataProvider(up); err != nil {
		return false, fmt.Errorf("upserting %s default key: %w", slug, err)
	}
	*planted = key
	return true, nil
}
