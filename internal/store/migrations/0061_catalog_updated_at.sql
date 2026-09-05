-- Catalog change stamps for the Library Export (ADR-0056 §4).
--
-- The Export is a flat, incremental feed keyed on (updated_at, type, id): the
-- mirror on the other Server asks "what changed since this cursor" and must get
-- exactly that. Until now no catalog table recorded WHEN a row last changed —
-- only added_at, which is deliberately preserved across rescans (a re-probed
-- File keeps its added_at so "Recently Added" does not lie). So every exportable
-- table gains updated_at.
--
-- WHY TRIGGERS AND NOT A COLUMN EVERY WRITER SETS. There are ~20 writer
-- functions across store/{catalog,tv,music,incremental,placement,enrich,
-- needsreview,identity_correction}.go, several reached through shared helpers
-- and several through dynamic column lists (WriteTitleMetadata builds its SET
-- clause at runtime). A stamp that depends on every one of them remembering is a
-- stamp that silently rots the first time somebody adds the twenty-first; and a
-- missed bump is invisible — the row simply never reaches the friend's Server
-- again. The database is the one place that sees every write, so the database
-- keeps the stamp. This is also what makes the "audit every write path" the
-- issue asks for finite: the audit is these triggers, and the per-writer tests
-- assert the trigger fired rather than that a caller remembered.
--
-- PROPAGATION. A change bumps the row itself and its ancestors UP TO THE TITLE:
-- a Stream change shows up as a File, an Edition and a Title change, which is
-- what ADR-0056 §4 asks for. It stops at the Title deliberately — a Season/Show
-- (or Album/Artist) is exported as its own entity with its own stamp, so rolling
-- every Episode's edit up into its Show would re-emit the whole Show subtree's
-- parents on every scan and buy the mirror nothing it did not already get.
--
-- Enrichment writes a Title's descriptive fields onto the titles row (covered by
-- the titles trigger) but a Show's/Artist's/Album's into entity_enrichment, and
-- genres/cast into the *_genres/*_credits side tables. Those are exported as
-- part of their owner's `data`, so a write there has to bump the owner.
--
-- RECURSION. SQLite's recursive_triggers pragma is OFF by default and this
-- server never turns it on (store/db.go sets journal_mode, foreign_keys and
-- busy_timeout and nothing else), so a trigger's own UPDATE never re-fires a
-- trigger — including its own. That is why each trigger walks the WHOLE chain to
-- the Title itself instead of leaning on the parent's trigger to continue it,
-- and why the self-bump (`UPDATE <table> SET updated_at ... WHERE id = NEW.id`)
-- cannot loop.
--
-- FORMAT. RFC3339 with milliseconds, UTC — the shape the memory tables already
-- use (0029/0030), NOT datetime('now'). The Export compares these values in SQL
-- as a keyset seek, and the two formats do not compare: 'T' (0x54) sorts after
-- ' ' (0x20), the bug migration 0041 records. The backfill therefore converts
-- added_at rather than copying it; a value strftime cannot parse falls back to
-- the raw string (it can only sort early, i.e. be re-exported once).

ALTER TABLE titles   ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE shows    ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE seasons  ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE artists  ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE albums   ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE editions ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE files    ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE streams  ADD COLUMN updated_at TEXT NOT NULL DEFAULT '';

UPDATE titles   SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE shows    SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE seasons  SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE artists  SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE albums   SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE editions SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
UPDATE files    SET updated_at = COALESCE(strftime('%Y-%m-%dT%H:%M:%fZ', added_at), added_at);
-- streams has no added_at of its own; it inherits its File's.
UPDATE streams  SET updated_at = COALESCE(
    (SELECT strftime('%Y-%m-%dT%H:%M:%fZ', f.added_at) FROM files f WHERE f.id = streams.file_id), '');

-- The Export's ordering index: per Library for the three tables that carry
-- library_id, and plain (updated_at, id) for the rest, which reach their Library
-- through a join and are seeked on the same tuple.
CREATE INDEX IF NOT EXISTS idx_titles_updated   ON titles(library_id, updated_at, id);
CREATE INDEX IF NOT EXISTS idx_shows_updated    ON shows(library_id, updated_at, id);
CREATE INDEX IF NOT EXISTS idx_artists_updated  ON artists(library_id, updated_at, id);
CREATE INDEX IF NOT EXISTS idx_seasons_updated  ON seasons(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_albums_updated   ON albums(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_editions_updated ON editions(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_files_updated    ON files(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_streams_updated  ON streams(updated_at, id);

-- --- Roots: bump self only ---------------------------------------------------

CREATE TRIGGER IF NOT EXISTS titles_touch_ai AFTER INSERT ON titles
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS titles_touch_au AFTER UPDATE ON titles
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS shows_touch_ai AFTER INSERT ON shows
BEGIN
    UPDATE shows SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS shows_touch_au AFTER UPDATE ON shows
BEGIN
    UPDATE shows SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS seasons_touch_ai AFTER INSERT ON seasons
BEGIN
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS seasons_touch_au AFTER UPDATE ON seasons
BEGIN
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS artists_touch_ai AFTER INSERT ON artists
BEGIN
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS artists_touch_au AFTER UPDATE ON artists
BEGIN
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS albums_touch_ai AFTER INSERT ON albums
BEGIN
    UPDATE albums SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS albums_touch_au AFTER UPDATE ON albums
BEGIN
    UPDATE albums SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
END;

-- --- Edition → Title ---------------------------------------------------------

CREATE TRIGGER IF NOT EXISTS editions_touch_ai AFTER INSERT ON editions
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS editions_touch_au AFTER UPDATE ON editions
BEGIN
    UPDATE editions SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.id;
    UPDATE titles   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS editions_touch_ad AFTER DELETE ON editions
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

-- --- File → Edition → Title --------------------------------------------------

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

-- --- Stream → File → Edition → Title -----------------------------------------

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

-- --- A Title's genres and cast live in side tables ---------------------------

CREATE TRIGGER IF NOT EXISTS title_genres_touch_ai AFTER INSERT ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_genres_touch_au AFTER UPDATE ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_genres_touch_ad AFTER DELETE ON title_genres
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_credits_touch_ai AFTER INSERT ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_credits_touch_au AFTER UPDATE ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_credits_touch_ad AFTER DELETE ON title_credits
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

-- --- A Show's / Season's / Artist's / Album's enrichment, genres and cast -----
--
-- These three tables are polymorphic (entity_type, entity_id), so each trigger
-- fans out to the four owning tables; the entity_type test makes three of the
-- four statements a no-op that never touches an index. 'person' rows (cast
-- headshots) own no exported entity and fall through all four.

CREATE TRIGGER IF NOT EXISTS entity_enrichment_touch_ai AFTER INSERT ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_enrichment_touch_au AFTER UPDATE ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_enrichment_touch_ad AFTER DELETE ON entity_enrichment
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_genres_touch_ai AFTER INSERT ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_genres_touch_au AFTER UPDATE ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_genres_touch_ad AFTER DELETE ON entity_genres
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_credits_touch_ai AFTER INSERT ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_credits_touch_au AFTER UPDATE ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;

CREATE TRIGGER IF NOT EXISTS entity_credits_touch_ad AFTER DELETE ON entity_credits
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;
