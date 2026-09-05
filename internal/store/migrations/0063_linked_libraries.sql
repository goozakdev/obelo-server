-- Linked Libraries: the mirror of another household's Library (ADR-0056 §1, §3).
--
-- A Library on this Server whose contents live on somebody else's arrives over a
-- Link (0062) and is written into THESE catalog tables, keyed by the sharer's
-- ids. That is the whole design: Home rows, search, Collections, Playlists,
-- counts, the access filter and Watch state are SQL over local tables, so a
-- mirrored Title has to BE a local row for any of them to keep working.
--
-- Three shapes are needed and this migration adds all three.
--
-- 1. `libraries` learns where it came from: source, link_id, remote_library_id,
--    and the export cursor the mirror resumes from.
-- 2. Every mirrored catalog row learns the id it came from: remote_id, unique so
--    a re-pull upserts in place rather than duplicating (ADR-0056 §3 — "matched
--    by remote_id, never by title", which is what makes a rename on the sharer's
--    side keep this household's Watch state).
-- 3. `files.path` is allowed to be empty, because a mirrored File names no disk
--    here. That one costs a table rebuild; see the long note below.

-- --- 1. Where a Library came from --------------------------------------------

-- source is the ONE column every writer filters on (ADR-0056 §1). A CHECK rather
-- than a convention: the Scanner, the enrichment pass and the attention queue all
-- branch on it, and a typo'd value would be a Library nothing ever visits again
-- with nothing to show for it.
ALTER TABLE libraries ADD COLUMN source TEXT NOT NULL DEFAULT 'local'
    CHECK (source IN ('local', 'linked'));

-- link_id is the Link this Library arrived over and remote_library_id is its id
-- on that Server. Both are NULL/'' for a local Library.
--
-- link_id carries NO foreign key to links(id), deliberately. The obvious
-- ON DELETE CASCADE would delete these rows the instant DeleteLink ran, and the
-- unlink needs them AFTER that: link.Service.Unlink deletes the Link row and then
-- hands the Link to OnUnlinked, which is what deletes the Libraries, their
-- mirrored rows and their Watch state (ADR-0056 §6 — unlinking is the only thing
-- that deletes what came over a Link, and it is one operation, not a cascade
-- nobody can see the shape of).
ALTER TABLE libraries ADD COLUMN link_id TEXT;
ALTER TABLE libraries ADD COLUMN remote_library_id TEXT NOT NULL DEFAULT '';

-- remote_checkpoint is the sharer's export cursor: the position this mirror has
-- consumed up to, to hand back as `since` on the next pull (ADR-0056 §4). It
-- lives on the Library and not on the Link because the feed is per-Library — one
-- Link can bring two Libraries that change at completely different rates.
ALTER TABLE libraries ADD COLUMN remote_checkpoint TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_libraries_source ON libraries(source);

-- One linked Library per (Link, remote library). A second pull of the same
-- granted Library must find the row it made last time, and a re-key (which keeps
-- the Link id) must not double the household's shelves.
CREATE UNIQUE INDEX IF NOT EXISTS idx_libraries_link_remote
    ON libraries(link_id, remote_library_id) WHERE link_id IS NOT NULL;

-- --- 2. Where a mirrored row came from ---------------------------------------
--
-- remote_id is NULL on every locally-scanned row and carries the sharer's id on
-- every mirrored one. NULL rather than '' precisely so the unique indexes below
-- can be partial: SQLite treats NULLs as distinct, so a million local rows do not
-- collide with each other on an empty string.
--
-- The Episode and Track the issue names are `titles` rows (kind 'episode' /
-- 'track'), so eight tables carry the column, not ten.
ALTER TABLE titles   ADD COLUMN remote_id TEXT;
ALTER TABLE shows    ADD COLUMN remote_id TEXT;
ALTER TABLE seasons  ADD COLUMN remote_id TEXT;
ALTER TABLE artists  ADD COLUMN remote_id TEXT;
ALTER TABLE albums   ADD COLUMN remote_id TEXT;
ALTER TABLE editions ADD COLUMN remote_id TEXT;
ALTER TABLE streams  ADD COLUMN remote_id TEXT;
-- files gets its remote_id in the rebuild below, in the table definition itself.

-- The uniqueness is (library_id, remote_id) where the table carries a library_id
-- and (remote_id) where it does not. The narrower form is not available for
-- seasons/albums/editions/streams — they reach their Library through a join — and
-- the wider one is strictly stronger anyway: a remote id is the other Server's
-- UUID, so two Links can no more collide on one than two rows here can.
CREATE UNIQUE INDEX IF NOT EXISTS idx_titles_remote
    ON titles(library_id, remote_id)  WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_shows_remote
    ON shows(library_id, remote_id)   WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_artists_remote
    ON artists(library_id, remote_id) WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_seasons_remote
    ON seasons(remote_id)  WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_albums_remote
    ON albums(remote_id)   WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_editions_remote
    ON editions(remote_id) WHERE remote_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_streams_remote
    ON streams(remote_id)  WHERE remote_id IS NOT NULL;

-- --- 3. A mirrored File names no disk -----------------------------------------
--
-- `files` has carried UNIQUE (edition_id, path) since 0008. A mirrored File has
-- no path — the bytes are on the other household's disk and arrive through the
-- relay (ADR-0056 §5) — so its path is the empty string, and a multi-part Title
-- (two Files under ONE Edition, which is exactly what part_ordinal exists for)
-- would then be two rows colliding on ('', edition). The constraint has to become
-- a PARTIAL unique index that ignores the empty path, and SQLite cannot drop a
-- table constraint in place: the table is rebuilt.
--
-- THE REBUILD HAS TO CARRY `streams` OUT AND BACK, and that is not defensiveness.
-- foreign_keys is ON for every connection this server opens (store/db.go), and
-- with it on a DROP TABLE performs an implicit DELETE FROM first — which fires
-- streams' ON DELETE CASCADE and empties it. Migration 0008 did this same rebuild
-- without the backup and would have taken every Stream row with it on any
-- database that had one. So: park streams in a plain table with no constraints,
-- do the rebuild, put them back byte for byte.
--
-- The three streams triggers come off first and go back on last, for two reasons.
-- They fire on the restore (0061's stamps are AFTER INSERT), which would re-stamp
-- every File, Edition and Title in the database and make every linked Server
-- re-pull its whole mirror for nothing; and streams_touch_ad reads `files` in the
-- middle of the DROP that is removing it.

DROP TRIGGER IF EXISTS streams_touch_ai;
DROP TRIGGER IF EXISTS streams_touch_au;
DROP TRIGGER IF EXISTS streams_touch_ad;

CREATE TABLE streams_rebuild_backup AS SELECT * FROM streams;

CREATE TABLE files_new (
    id            TEXT PRIMARY KEY,
    edition_id    TEXT NOT NULL REFERENCES editions(id) ON DELETE CASCADE,
    -- path is the absolute on-disk path for a locally-scanned File and '' for a
    -- mirrored one. Still NOT NULL: "no path" is a fact about linked Libraries,
    -- not a missing value.
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
    part_ordinal  INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL DEFAULT '',
    remote_id     TEXT
);

INSERT INTO files_new
    (id, edition_id, path, container, video_codec, audio_codec, width, height,
     bitrate, duration_ms, size_bytes, added_at, mtime, present, part_ordinal, updated_at)
    SELECT id, edition_id, path, container, video_codec, audio_codec, width, height,
           bitrate, duration_ms, size_bytes, added_at, mtime, present, part_ordinal, updated_at
      FROM files;

DROP TABLE files;
ALTER TABLE files_new RENAME TO files;

-- The cascade emptied streams; put it back exactly as it was, stamps included.
DELETE FROM streams;
INSERT INTO streams
    (id, file_id, stream_index, kind, codec, language, width, height, channels,
     is_default, forced, title, commentary, hearing_impaired, updated_at)
    SELECT id, file_id, stream_index, kind, codec, language, width, height, channels,
           is_default, forced, title, commentary, hearing_impaired, updated_at
      FROM streams_rebuild_backup;
DROP TABLE streams_rebuild_backup;

-- The uniqueness 0008 wanted, minus the empty path a mirrored File carries.
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_edition_path
    ON files(edition_id, path) WHERE path <> '';
CREATE INDEX IF NOT EXISTS idx_files_edition ON files(edition_id);
CREATE INDEX IF NOT EXISTS idx_files_path    ON files(path);
CREATE INDEX IF NOT EXISTS idx_files_updated ON files(updated_at, id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_files_remote
    ON files(remote_id) WHERE remote_id IS NOT NULL;

-- 0061's stamps, recreated verbatim for the new table.
CREATE TRIGGER IF NOT EXISTS files_touch_ai AFTER INSERT ON files
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = NEW.edition_id);
END;

CREATE TRIGGER IF NOT EXISTS files_touch_au AFTER UPDATE ON files
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = NEW.edition_id);
END;

CREATE TRIGGER IF NOT EXISTS files_touch_ad AFTER DELETE ON files
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.edition_id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT title_id FROM editions WHERE id = OLD.edition_id);
END;

CREATE TRIGGER IF NOT EXISTS streams_touch_ai AFTER INSERT ON streams
BEGIN
    UPDATE streams  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = NEW.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = NEW.file_id);
END;

CREATE TRIGGER IF NOT EXISTS streams_touch_au AFTER UPDATE ON streams
BEGIN
    UPDATE streams  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = NEW.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = NEW.file_id);
END;

CREATE TRIGGER IF NOT EXISTS streams_touch_ad AFTER DELETE ON streams
BEGIN
    UPDATE files    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.file_id;
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT edition_id FROM files WHERE id = OLD.file_id);
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = (SELECT e.title_id FROM editions e JOIN files f ON f.edition_id = e.id
                  WHERE f.id = OLD.file_id);
END;
