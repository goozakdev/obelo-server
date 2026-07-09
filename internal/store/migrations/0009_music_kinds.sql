-- 0009_music_kinds: light up the Music media kind (issue tv-music/03). This adds
-- the explicit Artist and Album parent entities and hangs a Track Title off its
-- Album, building ON the kind groundwork already laid by 0008 (libraries.kind
-- and titles.kind already accept 'music'/'track'; library.Create already accepts
-- 'music'). It is additive — Movie and TV rows are untouched.
--
-- Two things happen here:
--
--   1. Add the explicit Artist and Album entities (confirmed modeling decision,
--      mirroring 0008's Show/Season): an Artist belongs to a Library; an Album
--      belongs to an Artist. Each has its own id and identity_key (derived from
--      embedded tags — Album Artist falling back to Artist) so a rescan
--      re-resolves to the same row (identity stability, ADR-0014). Albums group
--      by Album Artist (falling back to Artist) so a compilation / "Various
--      Artists" album stays ONE Album rather than fragmenting per track artist.
--
--   2. Hang a Track Title off its Album: titles gains a nullable album_id plus the
--      disc/track ordering columns. A Movie/Episode leaves them NULL/0; a Track
--      sets them. The Track still owns the existing Edition → File → Stream chain
--      UNCHANGED (it is a Title), so playback/watch-state/transcode need no schema
--      change. Watch state stays per-(User, Title).
--
-- SQLite cannot ALTER-add a column that REFERENCES another table with a
-- non-constant default cleanly across its FK checker mid-transaction the way a
-- plain column add can, AND titles already carries the TV season linkage from
-- 0008. We rebuild `titles` with the standard create→copy→drop→rename pattern
-- (preserving every column/default/index and every titles.id verbatim) to add the
-- album linkage alongside the existing season linkage. defer_foreign_keys=ON (the
-- only FK toggle settable inside a transaction, see 0008) defers the child-FK
-- checks (editions/watch_state/seasons → titles.id) to commit, by which point
-- every id is preserved so they all still resolve.
PRAGMA defer_foreign_keys = ON;

-- 1. The explicit Music parent entities. An Artist is the top-level browse unit of
--    a Music Library; an Album groups its Tracks. Both carry their own
--    identity_key (from tags) so a rescan re-resolves to the same row. An Album's
--    artwork is a local cover.jpg/folder.jpg (embedded cover art as fallback),
--    recorded as a path on the album row when present.
CREATE TABLE IF NOT EXISTS artists (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- name is the display Album Artist (falling back to Artist) for this grouping.
    name         TEXT NOT NULL,
    -- identity_key is the normalized Album-Artist (fallback Artist) name, scoped to
    -- the Library, so two spellings of one artist collapse and a rescan re-resolves.
    identity_key TEXT NOT NULL,
    sort_name    TEXT NOT NULL,
    -- hidden mirrors the titles/shows convention: an Artist with no visible Tracks
    -- is hidden from the list but stays fetchable so state recovers (ADR-0008).
    hidden       INTEGER NOT NULL DEFAULT 0,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (library_id, identity_key)
);
CREATE INDEX IF NOT EXISTS idx_artists_library   ON artists(library_id);
CREATE INDEX IF NOT EXISTS idx_artists_sort_name ON artists(library_id, sort_name, id);

CREATE TABLE IF NOT EXISTS albums (
    id           TEXT PRIMARY KEY,
    artist_id    TEXT NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    year         INTEGER,
    -- identity_key is "<artist identity>|<normalized album title>" so the same
    -- Album re-resolves on rescan and two artists may share an album title.
    identity_key TEXT NOT NULL,
    sort_title   TEXT NOT NULL,
    -- artwork_path is the local album cover (cover.jpg/folder.jpg) when present,
    -- empty otherwise. Local always wins; embedded cover art is the fallback the
    -- scanner records here too (ADR-0001, naming-convention.md "Local artwork").
    artwork_path TEXT NOT NULL DEFAULT '',
    hidden       INTEGER NOT NULL DEFAULT 0,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (artist_id, identity_key)
);
CREATE INDEX IF NOT EXISTS idx_albums_artist     ON albums(artist_id, sort_title, id);

-- 2. Rebuild `titles` to add the Album linkage + disc/track ordering, preserving
--    every existing column (including the 0008 TV columns), default, and index.
CREATE TABLE titles_new (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- kind is the leaf discriminator: 'movie' | 'episode' | 'track'.
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
    -- TV linkage (NULL for a Movie/Track), preserved verbatim from 0008.
    season_id      TEXT REFERENCES seasons(id) ON DELETE CASCADE,
    season_number  INTEGER NOT NULL DEFAULT 0,
    episode_number INTEGER NOT NULL DEFAULT 0,
    episode_label  TEXT NOT NULL DEFAULT '',
    -- Music linkage (NULL for a Movie/Episode). A Track references its Album; the
    -- Artist is reachable via the Album. ON DELETE CASCADE so dropping an
    -- Album/Artist drops its Tracks.
    album_id     TEXT REFERENCES albums(id) ON DELETE CASCADE,
    -- disc_number / track_number are the parsed Music ordering for a Track (from
    -- tags, path fallback). A Track lists in disc-then-track order. Both default 0
    -- for a Movie/Episode and for a Track with no disc/track tag.
    disc_number  INTEGER NOT NULL DEFAULT 0,
    track_number INTEGER NOT NULL DEFAULT 0,
    UNIQUE (library_id, identity_key)
);
INSERT INTO titles_new
    (id, library_id, kind, title, year, identity_key, sort_title, added_at,
     tmdb_id, imdb_id, needs_review, ambiguous, hidden,
     season_id, season_number, episode_number, episode_label)
    SELECT id, library_id, kind, title, year, identity_key, sort_title, added_at,
           tmdb_id, imdb_id, needs_review, ambiguous, hidden,
           season_id, season_number, episode_number, episode_label
      FROM titles;
DROP TABLE titles;
ALTER TABLE titles_new RENAME TO titles;

-- Recreate every titles index (the rebuild dropped them).
CREATE INDEX IF NOT EXISTS idx_titles_library      ON titles(library_id);
CREATE INDEX IF NOT EXISTS idx_titles_sort_title   ON titles(library_id, sort_title, id);
CREATE INDEX IF NOT EXISTS idx_titles_added        ON titles(library_id, added_at, id);
CREATE INDEX IF NOT EXISTS idx_titles_needs_review ON titles(library_id, needs_review, ambiguous);
CREATE INDEX IF NOT EXISTS idx_titles_hidden       ON titles(library_id, hidden);
CREATE INDEX IF NOT EXISTS idx_titles_season       ON titles(season_id, season_number, episode_number);
-- Track ordering within an Album (disc then track) for the Album track listing.
CREATE INDEX IF NOT EXISTS idx_titles_album        ON titles(album_id, disc_number, track_number);
