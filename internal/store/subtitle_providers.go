package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SubtitleProviderRow is one row of the subtitle_providers table — the MUTABLE
// per-provider fetch state (ADR-0021), the subtitle analogue of
// MetadataProviderRow. The provider's static facts (name, whether it needs a key,
// its default host) live in the subfetch registry, NOT here. APIKey is the secret
// and is NEVER surfaced by the API — only a hasKey boolean. Empty APIKey/BaseURL
// mean "none" / "use the registry default".
type SubtitleProviderRow struct {
	Slug      string
	Enabled   bool
	APIKey    string
	BaseURL   string
	UpdatedAt string
}

// SubtitleProviderUpsert is the desired mutable state to persist for one subtitle
// provider. The settings API resolves partial-update secret semantics
// (omit=unchanged / ""=clear / value=set) into the FULL desired state before
// calling — the store writes exactly what it is given.
type SubtitleProviderUpsert struct {
	Slug    string
	Enabled bool
	APIKey  string
	BaseURL string
}

// SubtitleProviders lists every persisted subtitle-provider row, ordered by slug.
// A provider with no row has never been configured — the caller treats it as
// disabled with no key (ADR-0001 offline-first).
func (db *DB) SubtitleProviders() ([]SubtitleProviderRow, error) {
	rows, err := db.Query(
		`SELECT slug, enabled, api_key, base_url, updated_at
		   FROM subtitle_providers ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: listing subtitle providers: %w", err)
	}
	defer rows.Close()

	var out []SubtitleProviderRow
	for rows.Next() {
		var (
			r               SubtitleProviderRow
			apiKey, baseURL sql.NullString
		)
		if err := rows.Scan(&r.Slug, &r.Enabled, &apiKey, &baseURL, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scanning subtitle provider: %w", err)
		}
		r.APIKey = apiKey.String
		r.BaseURL = baseURL.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertSubtitleProvider writes one provider's full mutable state, inserting or
// replacing it by slug. An empty APIKey/BaseURL is stored as NULL so "cleared" and
// "never set" read back the same (none / registry default).
func (db *DB) UpsertSubtitleProvider(u SubtitleProviderUpsert) error {
	_, err := db.Exec(
		`INSERT INTO subtitle_providers (slug, enabled, api_key, base_url, updated_at)
		      VALUES (?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(slug) DO UPDATE SET
		      enabled    = excluded.enabled,
		      api_key    = excluded.api_key,
		      base_url   = excluded.base_url,
		      updated_at = datetime('now')`,
		u.Slug, u.Enabled, nullString(u.APIKey), nullString(u.BaseURL))
	if err != nil {
		return fmt.Errorf("store: upserting subtitle provider %q: %w", u.Slug, err)
	}
	return nil
}

// SubtitleAutoFetchLang reads the server-wide auto-fetch-after-scan language from
// the singleton subtitle_settings row. A missing row yields "" (OFF) with no error.
func (db *DB) SubtitleAutoFetchLang() (string, error) {
	var lang string
	err := db.QueryRow(`SELECT auto_fetch_lang FROM subtitle_settings WHERE id = 1`).Scan(&lang)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: reading subtitle auto-fetch language: %w", err)
	}
	return lang, nil
}

// SetSubtitleAutoFetchLang writes the auto-fetch language into the guarded
// singleton row (id = 1). "" disables auto-fetch (the default posture).
func (db *DB) SetSubtitleAutoFetchLang(lang string) error {
	_, err := db.Exec(
		`INSERT INTO subtitle_settings (id, auto_fetch_lang, updated_at)
		      VALUES (1, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET
		      auto_fetch_lang = excluded.auto_fetch_lang,
		      updated_at      = datetime('now')`,
		lang)
	if err != nil {
		return fmt.Errorf("store: setting subtitle auto-fetch language: %w", err)
	}
	return nil
}

// SubtitleSettingsEmpty reports whether the subtitle-provider settings have never
// been written — no provider rows AND no singleton row. The first-boot signal: the
// app seeds from config.Config exactly once, only when this is true, after which
// the DB is authoritative (ADR-0021, the metadata-provider pattern).
func (db *DB) SubtitleSettingsEmpty() (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM subtitle_providers)
		      + (SELECT COUNT(*) FROM subtitle_settings)`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: checking subtitle settings empty: %w", err)
	}
	return n == 0, nil
}

// PickTitleSubtitle records a viewer-chosen Fetched subtitle for a Title as a
// source='fetched' row and LOCKS the choice for its (language, forced) slot, in one
// transaction (ADR-0021, mirroring PickTitleArtwork). The chosen candidate replaces
// any prior fetched row for the same language+forced slot — re-fetching a language
// is explicit and swaps the pick, never accumulates duplicates. A rescan leaves the
// row untouched (UpsertTitleTree deletes only source='sidecar'), so a fetched
// subtitle survives rescans; the row is Title-scoped so it follows a Match override.
// The caller has already downloaded+converted the bytes and confirmed the Title
// exists; path is the on-disk cache file, providerID the candidate's opaque id, and
// subID a fresh id for the row.
func (db *DB) PickTitleSubtitle(titleID, subID, lang string, forced bool, kind, codec, path, providerID string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin pick subtitle: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM subtitles
		   WHERE title_id = ? AND source = 'fetched' AND language = ? AND forced = ?`,
		titleID, lang, boolToInt(forced),
	); err != nil {
		return fmt.Errorf("store: clearing fetched subtitle %q: %w", lang, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO subtitles
		   (id, title_id, source, kind, language, forced, is_default, codec, path, provider_id)
		 VALUES (?, ?, 'fetched', ?, ?, ?, 0, ?, ?, ?)`,
		subID, titleID, kind, lang, boolToInt(forced), codec, path, providerID,
	); err != nil {
		return fmt.Errorf("store: inserting picked subtitle %q: %w", lang, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit pick subtitle: %w", err)
	}
	return nil
}
