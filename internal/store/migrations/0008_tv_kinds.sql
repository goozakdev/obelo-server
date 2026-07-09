-- 0008_tv_kinds: light up the TV media kind (issue tv-music/01). This is the
-- one-time KIND GROUNDWORK plus the explicit TV parent entities.
--
-- Three things happen here, all additive — existing Movie rows are untouched and
-- the Movie path is unchanged:
--
--   1. Widen the `kind` CHECK constraints on `libraries` and `titles` from
--      ('movie') to the wider vocabulary. SQLite cannot ALTER a CHECK constraint
--      in place, so each table is rebuilt with the standard pattern (create new →
--      copy → drop old → rename). The rebuild preserves every column, default,
--      and index exactly; only the CHECK widens. A Library's kind is the coarse
--      'movie'|'tv'|'music'; a Title's kind is the leaf discriminator
--      'movie'|'episode'|'track'.
--
--   2. Add the explicit Show and Season entities (confirmed modeling decision):
--      a Show belongs to a Library; a Season belongs to a Show. Each has its own
--      id and identity_key (so a rescan re-resolves to the same row, identity
--      stability ADR-0014). They mirror the titles dedup model.
--
--   3. Hang an Episode Title off its Season: titles gains nullable season_id +
--      episode ordering columns. A Movie leaves them NULL/0; an Episode sets them.
--      The Episode still owns the existing Edition → File → Stream chain UNCHANGED
--      (it is a Title), so playback/watch-state/transcode need no schema change.
--
-- foreign_keys is ON (db.go pragma) and migrations run inside a transaction, so a
-- plain `PRAGMA foreign_keys=OFF` would be a no-op (that pragma cannot change
-- mid-transaction). Instead we DEFER enforcement to commit time with
-- `defer_foreign_keys=ON` (which IS settable inside a transaction): the
-- CHECK-widening rebuild of `titles` drops/recreates the table mid-transaction
-- but preserves every titles.id verbatim, so by commit every child FK
-- (editions/watch_state → titles.id) still resolves and the deferred check passes.
PRAGMA defer_foreign_keys = ON;

-- 1. The explicit TV parent entities first, so the rebuilt titles table can
--    reference seasons(id). A Show is the top-level browse unit of a TV Library;
--    a Season groups its Episodes. Both carry their own identity_key so a rescan
--    re-resolves to the same row. Specials are the Season with number 0.
CREATE TABLE IF NOT EXISTS shows (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    year         INTEGER,
    identity_key TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    tmdb_id      TEXT NOT NULL DEFAULT '',
    imdb_id      TEXT NOT NULL DEFAULT '',
    -- needs_review flags a Show filed from a partial parse (e.g. a yearless Show).
    needs_review INTEGER NOT NULL DEFAULT 0,
    -- hidden mirrors the titles convention: a Show with no visible Episodes is
    -- hidden from the grid but stays fetchable so state recovers (ADR-0008).
    hidden       INTEGER NOT NULL DEFAULT 0,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (library_id, identity_key)
);
CREATE INDEX IF NOT EXISTS idx_shows_library    ON shows(library_id);
CREATE INDEX IF NOT EXISTS idx_shows_sort_title ON shows(library_id, sort_title, id);

CREATE TABLE IF NOT EXISTS seasons (
    id            TEXT PRIMARY KEY,
    show_id       TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    -- season_number is the parsed number; 0 = Specials (Season 00 / Specials/).
    season_number INTEGER NOT NULL,
    -- identity_key is "<show identity>|s<NN>" so a rescan re-resolves the Season.
    identity_key  TEXT NOT NULL,
    hidden        INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (show_id, season_number)
);
CREATE INDEX IF NOT EXISTS idx_seasons_show ON seasons(show_id, season_number);

-- 2a. Rebuild `libraries` to widen kind → ('movie','tv','music'). library_roots
--     references libraries(id); the id values are copied verbatim so the FK holds.
CREATE TABLE libraries_new (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('movie', 'tv', 'music')),
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
INSERT INTO libraries_new (id, name, kind, created_at)
    SELECT id, name, kind, created_at FROM libraries;
DROP TABLE libraries;
ALTER TABLE libraries_new RENAME TO libraries;

-- 2b. Rebuild `titles` to widen kind → ('movie','episode','track') AND add the
--     TV parent linkage + episode ordering columns. Every existing column,
--     default, and the UNIQUE(library_id, identity_key) is preserved; the new
--     columns default to NULL/0 so Movie rows are unchanged.
CREATE TABLE titles_new (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- kind is the leaf discriminator: a Movie is 'movie', an Episode is
    -- 'episode', a Track is 'track'.
    kind         TEXT NOT NULL CHECK (kind IN ('movie', 'episode', 'track')),
    title        TEXT NOT NULL,
    year         INTEGER,
    identity_key TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    tmdb_id      TEXT NOT NULL DEFAULT '',
    imdb_id      TEXT NOT NULL DEFAULT '',
    needs_review INTEGER NOT NULL DEFAULT 0,
    ambiguous    INTEGER NOT NULL DEFAULT 0,
    hidden       INTEGER NOT NULL DEFAULT 0,
    -- TV linkage (NULL for a Movie). An Episode references its Season; the Show is
    -- reachable via the Season. ON DELETE CASCADE so dropping a Season/Show drops
    -- its Episodes.
    season_id    TEXT REFERENCES seasons(id) ON DELETE CASCADE,
    -- season_number / episode_number are the parsed ordering for an Episode
    -- (Season 00 = Specials). episode_number is the second number's value for the
    -- second Title of a multi-episode file (S01E05-E06 → two Titles, E05 and E06).
    season_number  INTEGER NOT NULL DEFAULT 0,
    episode_number INTEGER NOT NULL DEFAULT 0,
    -- episode_label is the human label for a degraded-offline episode: a date
    -- (YYYY-MM-DD) or an absolute number, surfaced when canonical SxxExx mapping
    -- is unavailable (needs Enrichment, out of scope). Empty for a normal episode.
    episode_label TEXT NOT NULL DEFAULT '',
    UNIQUE (library_id, identity_key)
);
INSERT INTO titles_new
    (id, library_id, kind, title, year, identity_key, sort_title, added_at,
     tmdb_id, imdb_id, needs_review, ambiguous, hidden)
    SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
           tmdb_id, imdb_id, needs_review, ambiguous, hidden
      FROM titles;
DROP TABLE titles;
ALTER TABLE titles_new RENAME TO titles;

-- Recreate every titles index the prior migrations defined (the rebuild dropped them).
CREATE INDEX IF NOT EXISTS idx_titles_library      ON titles(library_id);
CREATE INDEX IF NOT EXISTS idx_titles_sort_title   ON titles(library_id, sort_title, id);
CREATE INDEX IF NOT EXISTS idx_titles_added        ON titles(library_id, added_at, id);
CREATE INDEX IF NOT EXISTS idx_titles_needs_review ON titles(library_id, needs_review, ambiguous);
CREATE INDEX IF NOT EXISTS idx_titles_hidden       ON titles(library_id, hidden);
-- Episode ordering within a Season (Season/Episode list, Up Next ordering later).
CREATE INDEX IF NOT EXISTS idx_titles_season       ON titles(season_id, season_number, episode_number);

-- 2c. Rebuild `files` to relax UNIQUE(path) → UNIQUE(edition_id, path). A
--     multi-episode file (S01E05-E06) maps ONE on-disk File to TWO Episode
--     Titles; each Episode owns its own Edition→File rows, so the SAME path now
--     legitimately appears under two different editions. A path is still unique
--     WITHIN an edition (the real constraint). Every column/default is preserved.
CREATE TABLE files_new (
    id            TEXT PRIMARY KEY,
    edition_id    TEXT NOT NULL REFERENCES editions(id) ON DELETE CASCADE,
    path          TEXT NOT NULL,
    container     TEXT NOT NULL DEFAULT '',
    video_codec   TEXT NOT NULL DEFAULT '',
    audio_codec   TEXT NOT NULL DEFAULT '',
    width         INTEGER NOT NULL DEFAULT 0,
    height        INTEGER NOT NULL DEFAULT 0,
    bitrate       INTEGER NOT NULL DEFAULT 0,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT NOT NULL DEFAULT (datetime('now')),
    mtime         TEXT NOT NULL DEFAULT '',
    present       INTEGER NOT NULL DEFAULT 1,
    UNIQUE (edition_id, path)
);
INSERT INTO files_new
    (id, edition_id, path, container, video_codec, audio_codec, width, height,
     bitrate, duration_ms, size_bytes, added_at, mtime, present)
    SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
           bitrate, duration_ms, size_bytes, added_at, mtime, present
      FROM files;
DROP TABLE files;
ALTER TABLE files_new RENAME TO files;
CREATE INDEX IF NOT EXISTS idx_files_edition ON files(edition_id);
-- Path is no longer unique, but lookups by path (incremental change-detection,
-- fix-match reclaim) still want an index.
CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
