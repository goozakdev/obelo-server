-- 0072_namespaced_record_ids: a record id is keyed by NAMESPACE, not by a column
-- named after one source (ADR-0060 decisions 2, 3, 4;
-- .scratch/bundled-plugins issue 14).
--
-- A Title kept its enrichment RECORD in three columns named for the sources that
-- existed when they were added — enrichment_tmdb_id, enrichment_imdb_id and
-- titles.musicbrainz_id — and a parent kept one untagged entity_enrichment.external_id.
-- A non-TMDB video lead therefore wrote its ids into the TMDB column (an AniDB-led
-- Library stored AniDB aids in enrichment_tmdb_id), and nothing could tell them
-- apart afterwards. A third-party source had nowhere to put an id at all.
--
-- IDENTITY IS UNTOUCHED. titles.tmdb_id / imdb_id are the Scanner's identity
-- columns, filled from folder tokens and spelled in identity_key (ADR-0002), and
-- artists.musicbrainz_id / albums.musicbrainz_id are tag identity (0054). Only the
-- three RECORD columns move.

-- 1. THE ROWS. One per (Title, namespace): a Title can hold several ids at once
-- (an AniDB record and an IMDb cross-reference an OMDb supplement reads), and adding
-- a namespace must never need a schema change.
CREATE TABLE IF NOT EXISTS title_external_ids (
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    namespace   TEXT NOT NULL,
    external_id TEXT NOT NULL,
    PRIMARY KEY (title_id, namespace)
);

-- Which of those rows is the Title's RECORD — the one a pin is keyed on. '' when
-- the Title has no record of its own.
ALTER TABLE titles ADD COLUMN enrichment_id_namespace TEXT NOT NULL DEFAULT '';

-- A parent keeps ONE id, because it has exactly one Authoritative provider
-- (ADR-0045's reason for its single column) — but that id now says whose it is.
ALTER TABLE entity_enrichment ADD COLUMN external_id_namespace TEXT NOT NULL DEFAULT '';

-- 2. THE BACKFILL, namespaced by the source that wrote the id where that is
-- PROVABLE (ADR-0060 decision 4).
--
-- A 'matched' row's enrichment_source was written in the SAME statement as its id,
-- so it names the id's namespace when it is an Authoritative one. The guard is
-- 'matched' because an Admin's override resets the status to 'pending' without
-- touching enrichment_source, so a stale source can sit beside a newer id only on a
-- row that is not 'matched'. Everything else falls back to the namespace the column
-- has always been READ as. This rescues the AniDB aids that leaked into the TMDB
-- column instead of making them TMDB ids for good.
INSERT OR REPLACE INTO title_external_ids (title_id, namespace, external_id)
SELECT id,
       CASE WHEN enrichment_status = 'matched'
                 AND enrichment_source IN ('tmdb', 'anidb', 'thetvdb')
            THEN enrichment_source ELSE 'tmdb' END,
       enrichment_tmdb_id
  FROM titles
 WHERE IFNULL(enrichment_tmdb_id, '') <> '';

-- enrichment_imdb_id was only ever an IMDb id, and musicbrainz_id only ever a
-- MusicBrainz one: no source rescue applies. OR IGNORE so a rescued row above can
-- never be displaced by a later statement (it cannot collide today; it is cheap to
-- make impossible).
INSERT OR IGNORE INTO title_external_ids (title_id, namespace, external_id)
SELECT id, 'imdb', enrichment_imdb_id
  FROM titles
 WHERE IFNULL(enrichment_imdb_id, '') <> '';

INSERT OR IGNORE INTO title_external_ids (title_id, namespace, external_id)
SELECT id, 'musicbrainz', musicbrainz_id
  FROM titles
 WHERE IFNULL(musicbrainz_id, '') <> '';

-- The RECORD is the row the old reads resolved against first: the TMDB-column row
-- (under whatever namespace it was rescued to), else the IMDb one; for a Track, the
-- MusicBrainz one.
UPDATE titles
   SET enrichment_id_namespace = CASE
         WHEN IFNULL(enrichment_tmdb_id, '') <> '' THEN
              CASE WHEN enrichment_status = 'matched'
                        AND enrichment_source IN ('tmdb', 'anidb', 'thetvdb')
                   THEN enrichment_source ELSE 'tmdb' END
         WHEN IFNULL(musicbrainz_id, '') <> '' THEN 'musicbrainz'
         WHEN IFNULL(enrichment_imdb_id, '') <> '' THEN 'imdb'
         ELSE '' END;

-- A parent, the same way: the matched source when it is an Authoritative namespace,
-- else the kind's default lead. A parent with no id has no namespace.
UPDATE entity_enrichment
   SET external_id_namespace = CASE
         WHEN IFNULL(external_id, '') = '' THEN ''
         WHEN enrichment_status = 'matched'
              AND enrichment_source IN ('tmdb', 'anidb', 'thetvdb', 'musicbrainz')
         THEN enrichment_source
         WHEN entity_type IN ('show', 'season') THEN 'tmdb'
         WHEN entity_type IN ('artist', 'album') THEN 'musicbrainz'
         ELSE '' END;

-- 3. A CHANGE TO A TITLE'S IDS IS A CHANGE TO THE TITLE. The Linked-Library export
-- still carries a Title's musicbrainz_id, and its delta is keyed on
-- titles.updated_at (0061); when the id lived on the row, writing it bumped the row.
-- The side table bumps its Title exactly as title_genres and title_credits do.
CREATE TRIGGER IF NOT EXISTS title_external_ids_touch_ai AFTER INSERT ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_external_ids_touch_au AFTER UPDATE ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;

CREATE TRIGGER IF NOT EXISTS title_external_ids_touch_ad AFTER DELETE ON title_external_ids
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

-- 4. THE COLUMNS GO. Two places holding one fact is how the bug this migration
-- rescues got in; leaving them would let a future reader consult half the truth.
ALTER TABLE titles DROP COLUMN enrichment_tmdb_id;
ALTER TABLE titles DROP COLUMN enrichment_imdb_id;
ALTER TABLE titles DROP COLUMN musicbrainz_id;
