-- 0018_metadata_providers: DB-backed Enrichment provider settings (metadata-
-- providers 02). Moves provider configuration — which external sources are on,
-- their API keys, base-URL overrides, and the server-wide metadata language —
-- out of static env vars and into durable state (ADR-0007) that the running
-- server rebuilds from at runtime with no restart.
--
-- Only MUTABLE state lives here. A provider's kinds, role (authoritative vs.
-- supplement), whether it needs a key, and its default base URL are STATIC CODE
-- (the enrich registry), not columns — so adding those facts never needs a
-- migration, only a registry entry.

-- One row per provider, keyed by a stable provider slug (tmdb, musicbrainz,
-- coverart, fanarttv, theaudiodb). A missing row means "never configured" —
-- treated as disabled with no key (ADR-0001 offline-first). api_key is the
-- nullable secret (NULL/'' = none, and it is NEVER returned by the API — only a
-- hasKey boolean). base_url is a nullable override (NULL/'' = use the code
-- default from the registry), for pointing a source at a mirror or a test stub.
-- image_base_url is a second, nullable host override for the few sources that
-- serve artwork from a host distinct from their API (today only TMDB, whose
-- images come from image.tmdb.org, not api.themoviedb.org). NULL/'' = the
-- registry default; a provider with no image host ignores it.
CREATE TABLE IF NOT EXISTS metadata_providers (
    slug           TEXT PRIMARY KEY,
    enabled        INTEGER NOT NULL DEFAULT 0,
    api_key        TEXT,
    base_url       TEXT,
    image_base_url TEXT,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Server-wide Enrichment settings as a guarded singleton (id = 1) — the
-- preferred metadata language/region (e.g. en-US) applied across every source.
-- A singleton table keeps the typed-store style rather than introducing a
-- generic key/value store (the codebase has none, by design).
CREATE TABLE IF NOT EXISTS metadata_settings (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    metadata_language TEXT NOT NULL DEFAULT '',
    updated_at        TEXT NOT NULL DEFAULT (datetime('now'))
);
