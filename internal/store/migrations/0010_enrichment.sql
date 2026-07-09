-- 0010_enrichment: the Enrichment decoration layer (external-metadata-enrichment
-- issue 01). Enrichment is the SEPARATE, OPTIONAL step (ADR-0002) that decorates
-- a Title the scanner already filed from local on-disk identity with descriptive
-- metadata + fetched artwork from an external source (TMDB for Movie). It NEVER
-- touches identity (identity_key / watch state are untouched) — every column here
-- is descriptive or bookkeeping, and every existing row is unchanged.
--
-- Three additive moves, no table rebuild (every change is an ADD COLUMN or a new
-- table, so the Movie/TV/Music rows the prior migrations wrote are byte-for-byte
-- intact and the offline scan path is unaffected):
--
--   1. titles gains nullable descriptive columns + enrichment bookkeeping.
--   2. New child tables title_genres / title_credits (rebuilt wholesale on each
--      unlocked enrich) and title_field_locks (a hand-edited field is Locked so
--      re-enrichment never overwrites it — CONTEXT.md "Locked field").
--   3. artwork gains a `source` discriminator ('local' | 'fetched') so a fetched
--      poster is a FALLBACK that local artwork still wins over (CONTEXT.md), and
--      so a rescan (which rewrites only local artwork) never drops fetched rows.

-- 1. Descriptive + bookkeeping columns on titles. All have constant defaults so
--    the ADD COLUMN backfills every existing row without a rebuild. The existing
--    tmdb_id / imdb_id columns are reused as the external-match anchor; a
--    musicbrainz_id is added for the Music kind (used by a later slice).
ALTER TABLE titles ADD COLUMN overview        TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN tagline         TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN content_rating  TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN release_date    TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN runtime_minutes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE titles ADD COLUMN studio          TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN musicbrainz_id  TEXT    NOT NULL DEFAULT '';
-- enrichment_status drives the pass + the attention surface:
--   pending   — scanned, never enriched (the default; an only-new pass picks it up)
--   matched   — enriched from an external record
--   unmatched — no external match found (browsable, sparse; Admin can fix by id)
--   failed    — the provider errored (transient; retried on the next pass)
--   disabled  — enrichment is off (no provider configured)
ALTER TABLE titles ADD COLUMN enrichment_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (enrichment_status IN ('pending', 'matched', 'unmatched', 'failed', 'disabled'));
ALTER TABLE titles ADD COLUMN enriched_at       TEXT NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN enrichment_source TEXT NOT NULL DEFAULT '';

-- An only-new enrich pass scans for pending titles; index it (scoped per Library).
CREATE INDEX IF NOT EXISTS idx_titles_enrichment ON titles(library_id, enrichment_status);

-- 2. Genres + credits (cast/crew), rebuilt wholesale on each unlocked enrich,
--    exactly like editions/artwork in writeTitleSubtree. ord preserves the
--    provider's billing order. genre is indexed for the filter[genre] browse.
CREATE TABLE IF NOT EXISTS title_genres (
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    genre    TEXT NOT NULL,
    ord      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_title_genres_title ON title_genres(title_id);
CREATE INDEX IF NOT EXISTS idx_title_genres_genre ON title_genres(genre);

CREATE TABLE IF NOT EXISTS title_credits (
    title_id  TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    person    TEXT NOT NULL,
    -- role is the job ("Actor", "Director"); character is the role played (cast
    -- only). kind groups them: 'cast' | 'crew'. ord is billing order.
    role      TEXT NOT NULL DEFAULT '',
    character TEXT NOT NULL DEFAULT '',
    kind      TEXT NOT NULL DEFAULT 'cast',
    ord       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_title_credits_title ON title_credits(title_id);

-- A Locked field: an Admin hand-edited it, so re-enrichment skips it (CONTEXT.md).
-- `field` names a scalar column ('overview', 'content_rating', …), the multi-
-- valued 'genres' / 'cast', or an artwork role ('poster' / 'background'). The
-- PUT /metadata write side lands in a later slice; the enrich pass already
-- honors any lock present here from the start.
CREATE TABLE IF NOT EXISTS title_field_locks (
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    field    TEXT NOT NULL,
    PRIMARY KEY (title_id, field)
);

-- 3. artwork.source: 'local' (scanner-recorded on-disk poster.jpg/cover.jpg) or
--    'fetched' (enrichment-downloaded into the artwork cache, ADR-0007). Existing
--    rows backfill to 'local' (they all came from the scanner). Local wins over
--    fetched for the same role (ArtworkByTitleRole orders local first); a rescan
--    rewrites only local rows, so fetched artwork survives rescans.
ALTER TABLE artwork ADD COLUMN source TEXT NOT NULL DEFAULT 'local';
-- The 0005 UNIQUE(title_id, role) becomes "one row per (role, source)" so a Title
-- may carry BOTH a local and a fetched poster (local wins at serve time). SQLite
-- can't ALTER a constraint in place, so rebuild artwork (preserving every row +
-- backfilling source='local'). defer_foreign_keys defers the artwork→titles FK
-- check to commit, by which point every id is preserved (same pattern as 0008).
PRAGMA defer_foreign_keys = ON;
CREATE TABLE artwork_new (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('poster', 'background')),
    path     TEXT NOT NULL,
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    source   TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'fetched')),
    UNIQUE (title_id, role, source)
);
INSERT INTO artwork_new (id, title_id, role, path, added_at, source)
    SELECT id, title_id, role, path, added_at, source FROM artwork;
DROP TABLE artwork;
ALTER TABLE artwork_new RENAME TO artwork;
CREATE INDEX IF NOT EXISTS idx_artwork_title ON artwork(title_id);
