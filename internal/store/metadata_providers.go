package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MetadataProviderRow is one row of the metadata_providers table — the MUTABLE
// per-provider Enrichment state (metadata-providers 02). A provider's static
// facts (which kinds it serves, its role, whether it needs a key, its default
// base URL) live in the enrich registry, NOT here. APIKey is the secret and is
// NEVER surfaced by the API — only a hasKey boolean. Empty APIKey/BaseURL mean
// "none" / "use the registry default" (a NULL or ” column, treated the same).
// ImageBaseURL is a second host override for sources whose artwork host differs
// from their API host (today only TMDB); "" = the registry default.
type MetadataProviderRow struct {
	Slug         string
	Enabled      bool
	APIKey       string
	BaseURL      string
	ImageBaseURL string
	UpdatedAt    string
}

// MetadataProviderUpsert is the desired mutable state to persist for one
// provider. The caller (the settings API) resolves the partial-update secret
// semantics (omit=unchanged / ""=clear / value=set) into the FULL desired state
// before calling UpsertMetadataProvider — the store writes exactly what it is
// given.
type MetadataProviderUpsert struct {
	Slug         string
	Enabled      bool
	APIKey       string
	BaseURL      string
	ImageBaseURL string
}

// MetadataProviders lists every persisted provider row, ordered by slug. A
// provider with no row has never been configured — the caller treats it as
// disabled with no key (ADR-0001 offline-first); this returns only the rows that
// exist.
func (db *DB) MetadataProviders() ([]MetadataProviderRow, error) {
	rows, err := db.Query(
		`SELECT slug, enabled, api_key, base_url, image_base_url, updated_at
		   FROM metadata_providers ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: listing metadata providers: %w", err)
	}
	defer rows.Close()

	var out []MetadataProviderRow
	for rows.Next() {
		var (
			r                             MetadataProviderRow
			apiKey, baseURL, imageBaseURL sql.NullString
		)
		if err := rows.Scan(&r.Slug, &r.Enabled, &apiKey, &baseURL, &imageBaseURL, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning metadata provider: %w", err)
		}
		r.APIKey = apiKey.String
		r.BaseURL = baseURL.String
		r.ImageBaseURL = imageBaseURL.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertMetadataProvider writes one provider's full mutable state, inserting the
// row or replacing it by slug (SQLite ON CONFLICT). An empty APIKey/BaseURL is
// stored as NULL so "cleared" and "never set" are indistinguishable on read
// (both mean none / registry default).
func (db *DB) UpsertMetadataProvider(u MetadataProviderUpsert) error {
	_, err := db.Exec(
		`INSERT INTO metadata_providers (slug, enabled, api_key, base_url, image_base_url, updated_at)
		      VALUES (?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(slug) DO UPDATE SET
		      enabled        = excluded.enabled,
		      api_key        = excluded.api_key,
		      base_url       = excluded.base_url,
		      image_base_url = excluded.image_base_url,
		      updated_at     = datetime('now')`,
		u.Slug, u.Enabled, nullString(u.APIKey), nullString(u.BaseURL), nullString(u.ImageBaseURL))
	if err != nil {
		return fmt.Errorf("store: upserting metadata provider %q: %w", u.Slug, err)
	}
	return nil
}

// MetadataLanguage reads the server-wide preferred metadata language from the
// singleton metadata_settings row. A missing row (settings never seeded/saved)
// yields "" with no error — the caller falls back to a default.
func (db *DB) MetadataLanguage() (string, error) {
	var lang string
	err := db.QueryRow(`SELECT metadata_language FROM metadata_settings WHERE id = 1`).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: reading metadata language: %w", err)
	}
	return lang, nil
}

// SetMetadataLanguage writes the server-wide preferred metadata language into the
// guarded singleton row (id = 1), inserting or replacing it.
func (db *DB) SetMetadataLanguage(language string) error {
	_, err := db.Exec(
		`INSERT INTO metadata_settings (id, metadata_language, updated_at)
		      VALUES (1, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      metadata_language = excluded.metadata_language,
		      updated_at        = datetime('now')`,
		language)
	if err != nil {
		return fmt.Errorf("store: setting metadata language: %w", err)
	}
	return nil
}

// EnrichmentBehavior is the trio of server-wide Enrichment BEHAVIOR knobs stored
// on the singleton metadata_settings row (enrichment-runtime-settings): whether a
// completed scan auto-enriches, the scheduled-sweep cadence (seconds), and the
// MusicBrainz throttle (milliseconds). Each field is a POINTER so a NULL column
// ("unset") is distinguishable from a real 0 — 0 is a meaningful "disabled" value
// for the interval and rate limit. After the first-boot seed / upgrade backfill the
// columns are never NULL; the resolver accessors below default a stray NULL to a
// safe value anyway.
type EnrichmentBehavior struct {
	AutoEnrichAfterScan    *bool
	EnrichIntervalSeconds  *int
	MusicBrainzRateLimitMs *int
}

// Auto resolves the auto-enrich-after-scan flag, defaulting an unset column to
// true (the config default).
func (b EnrichmentBehavior) Auto() bool {
	return b.AutoEnrichAfterScan == nil || *b.AutoEnrichAfterScan
}

// IntervalSeconds resolves the scheduled-enrich cadence, defaulting an unset
// column to 0 (scheduler disabled — the safe posture).
func (b EnrichmentBehavior) IntervalSeconds() int {
	if b.EnrichIntervalSeconds == nil {
		return 0
	}
	return *b.EnrichIntervalSeconds
}

// RateLimitMs resolves the MusicBrainz throttle, defaulting an unset column to 0
// (no throttle).
func (b EnrichmentBehavior) RateLimitMs() int {
	if b.MusicBrainzRateLimitMs == nil {
		return 0
	}
	return *b.MusicBrainzRateLimitMs
}

// EnrichmentBehavior reads the three behavior knobs from the singleton
// metadata_settings row. A missing row (settings never seeded) or a NULL column
// comes back as a nil field — "unset" — which the caller resolves (the boot
// backfill fills it from config; the resolver accessors default it at read time).
func (db *DB) EnrichmentBehavior() (EnrichmentBehavior, error) {
	var (
		auto           sql.NullBool
		interval, rate sql.NullInt64
	)
	err := db.QueryRow(
		`SELECT auto_enrich_after_scan, enrich_interval_seconds, musicbrainz_rate_limit_ms
		   FROM metadata_settings WHERE id = 1`).Scan(&auto, &interval, &rate)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrichmentBehavior{}, nil
	}
	if err != nil {
		return EnrichmentBehavior{}, fmt.Errorf("store: reading enrichment behavior: %w", err)
	}
	var out EnrichmentBehavior
	if auto.Valid {
		v := auto.Bool
		out.AutoEnrichAfterScan = &v
	}
	if interval.Valid {
		v := int(interval.Int64)
		out.EnrichIntervalSeconds = &v
	}
	if rate.Valid {
		v := int(rate.Int64)
		out.MusicBrainzRateLimitMs = &v
	}
	return out, nil
}

// SetEnrichmentBehavior writes all three behavior knobs into the guarded singleton
// row (id = 1) as concrete (non-NULL) values, inserting the row or updating ONLY
// these three columns on conflict — metadata_language (and the provider rows) are
// left untouched. The caller resolves any partial-update (omit=unchanged) semantics
// into full concrete values before calling this (mirroring UpsertMetadataProvider).
func (db *DB) SetEnrichmentBehavior(autoEnrichAfterScan bool, enrichIntervalSeconds, musicBrainzRateLimitMs int) error {
	_, err := db.Exec(
		`INSERT INTO metadata_settings (id, auto_enrich_after_scan, enrich_interval_seconds, musicbrainz_rate_limit_ms, updated_at)
		      VALUES (1, ?, ?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      auto_enrich_after_scan    = excluded.auto_enrich_after_scan,
		      enrich_interval_seconds   = excluded.enrich_interval_seconds,
		      musicbrainz_rate_limit_ms = excluded.musicbrainz_rate_limit_ms,
		      updated_at                = datetime('now')`,
		autoEnrichAfterScan, enrichIntervalSeconds, musicBrainzRateLimitMs)
	if err != nil {
		return fmt.Errorf("store: setting enrichment behavior: %w", err)
	}
	return nil
}

// BackfillEnrichmentBehaviorIfUnset fills ONLY the NULL behavior columns from the
// given config-derived seed values, preserving any column an operator already set
// (from a prior boot's seed or a UI save). It handles the upgrade case — a
// deployment that ran 0018 has a metadata_settings row with the three 0019 columns
// NULL, so SeedIfEmpty won't fire (settings aren't empty) yet the columns still need
// their first value. Idempotent: COALESCE keeps a non-NULL column, so re-running on
// a fully-set row is a no-op (it never reverts a UI change on restart). Also creates
// the row if somehow absent, so the runtime readers always see resolved values.
func (db *DB) BackfillEnrichmentBehaviorIfUnset(autoEnrichAfterScan bool, enrichIntervalSeconds, musicBrainzRateLimitMs int) error {
	_, err := db.Exec(
		`INSERT INTO metadata_settings (id, auto_enrich_after_scan, enrich_interval_seconds, musicbrainz_rate_limit_ms, updated_at)
		      VALUES (1, ?, ?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      auto_enrich_after_scan    = COALESCE(auto_enrich_after_scan, excluded.auto_enrich_after_scan),
		      enrich_interval_seconds   = COALESCE(enrich_interval_seconds, excluded.enrich_interval_seconds),
		      musicbrainz_rate_limit_ms = COALESCE(musicbrainz_rate_limit_ms, excluded.musicbrainz_rate_limit_ms)`,
		autoEnrichAfterScan, enrichIntervalSeconds, musicBrainzRateLimitMs)
	if err != nil {
		return fmt.Errorf("store: backfilling enrichment behavior: %w", err)
	}
	return nil
}

// sqliteDatetimeLayout is the text format SQLite's datetime('now') writes (UTC,
// no zone suffix). The consent timestamp is stored with it and parsed back with
// the same layout, treated as UTC.
const sqliteDatetimeLayout = "2006-01-02 15:04:05"

// EnrichmentConsent is the persisted first-run Enrichment consent decision
// (ADR-0032), read from the singleton metadata_settings row. Decided distinguishes
// "the operator has answered" from the undecided default (a NULL granted column):
// while undecided the SPA shows the first-run prompt and the server makes NO
// outbound enrichment calls. It is the SINGLE authoritative gate both the enrich
// Manager (which ANDs Granted into every emitted Enablement) and, later, the
// key-rotation fetch consult.
type EnrichmentConsent struct {
	Decided bool
	Granted bool
	At      time.Time // when decided; zero while undecided
}

// State renders the decision as the wire string the API/SPA branch on: "unset"
// (show the first-run prompt), "granted", or "declined".
func (c EnrichmentConsent) State() string {
	switch {
	case !c.Decided:
		return "unset"
	case c.Granted:
		return "granted"
	default:
		return "declined"
	}
}

// EnrichmentConsent reads the consent decision from the singleton
// metadata_settings row. A missing row (settings never seeded) or a NULL granted
// column comes back Decided=false — undecided — so a fresh install is gated off
// until the operator answers the prompt.
func (db *DB) EnrichmentConsent() (EnrichmentConsent, error) {
	var (
		granted sql.NullBool
		at      sql.NullString
	)
	err := db.QueryRow(
		`SELECT enrichment_consent_granted, enrichment_consent_at
		   FROM metadata_settings WHERE id = 1`).Scan(&granted, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrichmentConsent{}, nil
	}
	if err != nil {
		return EnrichmentConsent{}, fmt.Errorf("store: reading enrichment consent: %w", err)
	}
	var out EnrichmentConsent
	if granted.Valid {
		out.Decided = true
		out.Granted = granted.Bool
	}
	if at.Valid {
		if t, perr := time.Parse(sqliteDatetimeLayout, at.String); perr == nil {
			out.At = t.UTC()
		}
	}
	return out, nil
}

// SetEnrichmentConsent records the operator's consent decision on the guarded
// singleton row (id = 1), inserting the row or updating ONLY the two consent
// columns on conflict — the language, provider, and behavior settings are left
// untouched (mirroring SetEnrichmentBehavior). Writing either decision marks the
// consent Decided, so the first-run prompt does not fire again.
func (db *DB) SetEnrichmentConsent(granted bool) error {
	_, err := db.Exec(
		`INSERT INTO metadata_settings (id, enrichment_consent_granted, enrichment_consent_at, updated_at)
		      VALUES (1, ?, datetime('now'), datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      enrichment_consent_granted = excluded.enrichment_consent_granted,
		      enrichment_consent_at      = excluded.enrichment_consent_at,
		      updated_at                 = datetime('now')`,
		granted)
	if err != nil {
		return fmt.Errorf("store: setting enrichment consent: %w", err)
	}
	return nil
}

// MetadataSettingsEmpty reports whether the Enrichment provider settings have
// never been written — no provider rows AND no singleton language row. It is the
// first-boot signal: the app seeds the settings from config.Config exactly once,
// only when this is true, after which the DB is authoritative (metadata-providers
// 02).
func (db *DB) MetadataSettingsEmpty() (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM metadata_providers)
		      + (SELECT COUNT(*) FROM metadata_settings)`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: checking metadata settings empty: %w", err)
	}
	return n == 0, nil
}

// nullString maps an empty string to a SQL NULL, so a cleared secret / base-URL
// override is stored as NULL rather than ” (both read back as "").
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
