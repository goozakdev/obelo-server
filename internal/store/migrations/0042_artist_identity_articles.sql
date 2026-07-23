-- 0042_artist_identity_articles: make Artist identity article-insensitive
-- (ADR-0037). The scanner's artistIdentityKey() now strips a leading English
-- article ("the "/"an "/"a ") from the normalized album-artist name, so "The
-- Smashing Pumpkins" and "Smashing Pumpkins" resolve to ONE Artist. This
-- migration converges the rows already in the catalog to the new keys — merging
-- any pair the old rule split — so a rescan re-resolves to the SAME rows
-- (identity stability, ADR-0014) instead of duplicating.
--
-- Key shapes this rewrites (all scanner-composed, all artist-key-prefixed):
--   artists.identity_key = 'artist:<normalized album-artist>'
--   albums.identity_key  = '<artist key>|album:<...>' (or '|album-override:<...>')
--   titles.identity_key  = '<album key>|dNNtNN:<...>'  (kind 'track')
--
-- Stored keys are already lower-cased with single-space runs (normalizeTitle),
-- so exactly ONE article is stripped, longest prefix first, mirroring the Go
-- stripLeadingArticle() — the same single-pass rule migration 0024 applied to
-- the sort keys. The Go side strips AFTER normalization precisely so this
-- migration can derive the new key from the stored key text alone.
--
-- Merge policy when two rows now share a key ("The X" + "X"):
--   - the row already holding the target key survives (its id — and therefore
--     its watch state, collections, playlists, enrichment — is preserved); with
--     no such row, the smallest rekeyed id survives and is rekeyed in place;
--   - the loser's Albums move under the survivor; a same-titled Album merges
--     (its Tracks re-point; Track ids never change, so per-Title state is safe);
--   - Admin-pinned enrichment (entity_enrichment / field locks / artwork) moves
--     to the survivor unless the survivor already has its own row; derived rows
--     (genres, credits) are dropped for losers — the next enrich pass rebuilds
--     them wholesale anyway;
--   - in the pathological duplicate-rip case (the same album ripped under BOTH
--     spellings), two Tracks can map to one new key; UPDATE OR IGNORE leaves the
--     second on its old key, and the next rescan settles it exactly like a
--     duplicate rip under a single artist today.
--
-- The survivor keeps its own display name; the next rescan re-derives the name
-- from the first-seen track's tag as usual (display only — ADR-0019 renames are
-- protected by field locks, which move with the enrichment rows above).

-- 1. Every artist whose normalized name begins with an article, and its new key.
--    substr(identity_key, 8) is the name after the 7-char 'artist:' prefix; the
--    stripped remainders start at 12/11/10 ('the '/'an '/'a ' after the prefix).
CREATE TEMP TABLE artist_rekey AS
SELECT id, library_id, identity_key AS old_key,
       'artist:' || CASE
           WHEN substr(identity_key, 8) LIKE 'the %' THEN ltrim(substr(identity_key, 12))
           WHEN substr(identity_key, 8) LIKE 'an %'  THEN ltrim(substr(identity_key, 11))
           WHEN substr(identity_key, 8) LIKE 'a %'   THEN ltrim(substr(identity_key, 10))
       END AS new_key
  FROM artists
 WHERE substr(identity_key, 8) LIKE 'the %'
    OR substr(identity_key, 8) LIKE 'an %'
    OR substr(identity_key, 8) LIKE 'a %';

-- 2. One survivor per (library, new key): the artist already holding the target
--    key when one exists, else the smallest rekeyed id (deterministic).
CREATE TEMP TABLE artist_survivor AS
SELECT r.library_id, r.new_key,
       COALESCE(
           (SELECT a.id FROM artists a
             WHERE a.library_id = r.library_id AND a.identity_key = r.new_key),
           MIN(r.id)
       ) AS survivor_id
  FROM artist_rekey r
 GROUP BY r.library_id, r.new_key;

CREATE TEMP TABLE artist_loser AS
SELECT r.id AS loser_id, s.survivor_id
  FROM artist_rekey r
  JOIN artist_survivor s ON s.library_id = r.library_id AND s.new_key = r.new_key
 WHERE r.id <> s.survivor_id;

-- 3. Rewrite the artist-key prefix of every rekeyed artist's Album keys. Within
--    one artist the rewrite is injective (suffixes stay distinct), so the
--    UNIQUE(artist_id, identity_key) constraint cannot trip here.
UPDATE albums SET identity_key =
       (SELECT r.new_key FROM artist_rekey r WHERE r.id = albums.artist_id)
       || substr(identity_key,
                 length((SELECT r.old_key FROM artist_rekey r WHERE r.id = albums.artist_id)) + 1)
 WHERE artist_id IN (SELECT id FROM artist_rekey);

-- 4. Rewrite the same prefix on Track keys, BEFORE any album changes hands (the
--    correlation runs titles → albums.artist_id → artist_rekey). OR IGNORE
--    covers the duplicate-rip collision described above.
UPDATE OR IGNORE titles SET identity_key =
       (SELECT r.new_key FROM artist_rekey r
         JOIN albums al ON al.artist_id = r.id
        WHERE al.id = titles.album_id)
       || substr(identity_key,
                 length((SELECT r.old_key FROM artist_rekey r
                          JOIN albums al ON al.artist_id = r.id
                         WHERE al.id = titles.album_id)) + 1)
 WHERE album_id IN (SELECT al.id FROM albums al
                     WHERE al.artist_id IN (SELECT id FROM artist_rekey));

-- 5. Move loser artists' Albums under the survivor. OR IGNORE skips an Album
--    whose key already exists there (same album under both spellings) — those
--    merge in step 6.
UPDATE OR IGNORE albums SET artist_id =
       (SELECT l.survivor_id FROM artist_loser l WHERE l.loser_id = albums.artist_id)
 WHERE artist_id IN (SELECT loser_id FROM artist_loser);

-- 6. Merge each Album left behind into the survivor's same-key Album: re-point
--    its Tracks (Title ids — and thus watch state / playlists / collections —
--    unchanged), migrate its entity side rows, then delete it.
CREATE TEMP TABLE album_merge AS
SELECT la.id AS loser_album_id, sa.id AS survivor_album_id
  FROM albums la
  JOIN artist_loser l ON l.loser_id = la.artist_id
  JOIN albums sa ON sa.artist_id = l.survivor_id AND sa.identity_key = la.identity_key;

UPDATE titles SET album_id =
       (SELECT m.survivor_album_id FROM album_merge m WHERE m.loser_album_id = titles.album_id)
 WHERE album_id IN (SELECT loser_album_id FROM album_merge);

UPDATE OR IGNORE entity_enrichment SET entity_id =
       (SELECT m.survivor_album_id FROM album_merge m WHERE m.loser_album_id = entity_enrichment.entity_id)
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);
DELETE FROM entity_enrichment
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);

UPDATE OR IGNORE entity_field_locks SET entity_id =
       (SELECT m.survivor_album_id FROM album_merge m WHERE m.loser_album_id = entity_field_locks.entity_id)
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);
DELETE FROM entity_field_locks
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);

UPDATE OR IGNORE entity_artwork SET entity_id =
       (SELECT m.survivor_album_id FROM album_merge m WHERE m.loser_album_id = entity_artwork.entity_id)
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);
DELETE FROM entity_artwork
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);

DELETE FROM entity_genres
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);
DELETE FROM entity_credits
 WHERE entity_type = 'album' AND entity_id IN (SELECT loser_album_id FROM album_merge);

DELETE FROM albums WHERE id IN (SELECT loser_album_id FROM album_merge);

-- 7. Migrate loser ARTISTS' entity side rows the same way, then delete the loser
--    rows themselves. Every Album has moved by now, so the albums→artists
--    ON DELETE CASCADE has nothing left to take with it.
UPDATE OR IGNORE entity_enrichment SET entity_id =
       (SELECT l.survivor_id FROM artist_loser l WHERE l.loser_id = entity_enrichment.entity_id)
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);
DELETE FROM entity_enrichment
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);

UPDATE OR IGNORE entity_field_locks SET entity_id =
       (SELECT l.survivor_id FROM artist_loser l WHERE l.loser_id = entity_field_locks.entity_id)
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);
DELETE FROM entity_field_locks
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);

UPDATE OR IGNORE entity_artwork SET entity_id =
       (SELECT l.survivor_id FROM artist_loser l WHERE l.loser_id = entity_artwork.entity_id)
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);
DELETE FROM entity_artwork
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);

DELETE FROM entity_genres
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);
DELETE FROM entity_credits
 WHERE entity_type = 'artist' AND entity_id IN (SELECT loser_id FROM artist_loser);

DELETE FROM artists WHERE id IN (SELECT loser_id FROM artist_loser);

-- 8. Rekey the rekeyed artists that survived in place (no pre-existing holder of
--    the target key — otherwise that holder survived and this row is gone).
UPDATE artists SET identity_key =
       (SELECT r.new_key FROM artist_rekey r WHERE r.id = artists.id)
 WHERE id IN (SELECT r.id FROM artist_rekey r
               JOIN artist_survivor s ON s.survivor_id = r.id);

-- Temp tables live on the pooled connection past this transaction; drop them.
DROP TABLE artist_rekey;
DROP TABLE artist_survivor;
DROP TABLE artist_loser;
DROP TABLE album_merge;
