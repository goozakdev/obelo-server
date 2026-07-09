-- 0011_enrichment_parents: extend the Enrichment decoration layer (external-
-- metadata-enrichment issue 03) from the leaf Title (Movie) to the TV/Music
-- browse-PARENT entities — Show, Season, Artist, Album — and give the leaf
-- Episode/Track an enriched DISPLAY title. Like 0010 this is purely additive
-- (new tables + one ADD COLUMN), never touches identity (identity_key / watch
-- state), and leaves every existing row byte-for-byte intact (ADR-0002).
--
-- The parents are not Titles (no titles row), so their descriptive metadata +
-- fetched artwork live in three GENERIC tables keyed by (entity_type, entity_id)
-- — one set of store methods serves all four parent kinds rather than four
-- near-identical per-table column sets. entity_type ∈ 'show'|'season'|'artist'|
-- 'album'; entity_id is that row's id.

-- 1. Generic descriptive + bookkeeping for a non-Title browse entity. All
--    columns have constant defaults; an absent row is treated as 'pending' (never
--    enriched) by the read side, so existing Shows/Artists/Albums need no backfill.
CREATE TABLE IF NOT EXISTS entity_enrichment (
    entity_type       TEXT NOT NULL,
    entity_id         TEXT NOT NULL,
    overview          TEXT NOT NULL DEFAULT '',   -- show synopsis / artist bio / album notes
    content_rating    TEXT NOT NULL DEFAULT '',   -- show maturity rating (TV-MA, …)
    network           TEXT NOT NULL DEFAULT '',   -- show network / album label
    -- external_id is the resolved provider id of this parent (e.g. the show's TMDB
    -- id), kept so a child Season/Episode lookup can resolve under it on a later
    -- only-new pass without re-fetching the parent.
    external_id       TEXT NOT NULL DEFAULT '',
    enrichment_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (enrichment_status IN ('pending', 'matched', 'unmatched', 'failed', 'disabled')),
    enriched_at       TEXT NOT NULL DEFAULT '',
    enrichment_source TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (entity_type, entity_id)
);

-- 2. Genres for a parent entity (show/artist/album), rebuilt wholesale on each
--    enrich (idempotent), ord preserving the provider's order. Indexed for the
--    cross-kind filter[genre] browse.
CREATE TABLE IF NOT EXISTS entity_genres (
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    genre       TEXT NOT NULL,
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entity_genres ON entity_genres(entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_entity_genres_genre ON entity_genres(genre);

-- 3. Fetched artwork for a parent entity (poster/background/cover/thumb). Unlike
--    the title-keyed `artwork` table this holds ONLY fetched images (source kept
--    for symmetry + a future local parent image); a local Album cover stays in
--    albums.artwork_path and WINS over a fetched cover at serve time (CONTEXT.md
--    "local wins"). One row per (entity, role, source) so re-enrich overwrites.
CREATE TABLE IF NOT EXISTS entity_artwork (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    role        TEXT NOT NULL,
    path        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'fetched' CHECK (source IN ('local', 'fetched')),
    UNIQUE (entity_type, entity_id, role, source)
);
CREATE INDEX IF NOT EXISTS idx_entity_artwork ON entity_artwork(entity_type, entity_id);

-- 4. A leaf Episode/Track gains an enriched DISPLAY title — the canonical episode
--    name ("The Suitcase" for a date-based episode whose parsed title is a raw
--    date) or a sparse-Track title fill. It is DISPLAY ONLY: identity_key, title
--    (the parsed value), season/episode/label and watch state are untouched, so a
--    rescan and the per-Title watch state are unaffected (ADR-0014). The read
--    paths prefer enriched_title when present, else the parsed title. Empty for a
--    Movie (its parsed title is the display title).
ALTER TABLE titles ADD COLUMN enriched_title TEXT NOT NULL DEFAULT '';
