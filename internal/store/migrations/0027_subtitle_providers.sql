-- 0027_subtitle_providers: DB-backed external subtitle-provider settings (ADR-0021,
-- subtitles slice 05), mirroring the metadata-provider settings shipped in 0018.
-- Subtitle fetching reuses the enrichment pattern verbatim: which provider is on,
-- its API key, and a base-URL override move out of static env into durable state the
-- running server rebuilds from at runtime with no restart (the subfetch Manager).
--
-- Only MUTABLE state lives here. A provider's name, whether it needs a key, and its
-- default base URL are STATIC CODE (the subfetch registry), not columns — so adding
-- a provider is a registry entry, never a migration.

-- One row per subtitle provider, keyed by a stable slug (opensubtitles). A missing
-- row means "never configured" — treated as disabled with no key (ADR-0001
-- offline-first). api_key is the nullable secret (NULL/'' = none, NEVER returned by
-- the API — only a hasKey boolean). base_url is a nullable override (NULL/'' = the
-- code default from the registry), for pointing the source at a test stub.
CREATE TABLE IF NOT EXISTS subtitle_providers (
    slug        TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 0,
    api_key     TEXT,
    base_url    TEXT,
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Server-wide subtitle-fetch settings as a guarded singleton (id = 1). The one knob
-- this slice adds is auto_fetch_lang: the ISO-639-1 language a completed scan should
-- auto-fetch (mirrors metadata_settings.auto_enrich_after_scan). It defaults to ''
-- (OFF) — OpenSubtitles' small free-tier download quota makes fetch-everything a
-- footgun, so bulk auto-fetch is strictly opt-in (ADR-0021).
CREATE TABLE IF NOT EXISTS subtitle_settings (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    auto_fetch_lang  TEXT NOT NULL DEFAULT '',
    updated_at       TEXT NOT NULL DEFAULT (datetime('now'))
);
