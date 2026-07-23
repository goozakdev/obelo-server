-- 0043_artist_identity_and: fold the word "and" out of Artist identity keys
-- (ADR-0037 amendment). normalizeTitle collapses "&"/"+" to a space, so "Marina
-- & the Diamonds" already keys as 'artist:marina the diamonds' — but the WORD
-- "and" survived normalization, keying "Marina and the Diamonds" apart from it.
-- The scanner's artistIdentityKey() now drops every standalone "and" word (then
-- re-strips a leading article the drop may expose: "And The X" ~ "& The X");
-- this migration converges the stored rows the same way 0042 did for articles.
--
-- The canonical direction is forced: stored keys from "&" spellings lost the
-- ampersand at normalization, so "and" cannot be reinserted — the "and"-less
-- form IS the only key derivable from stored text, and the word-ful rows merge
-- into it. The merge machinery below is identical to 0042 (survivor keeps its
-- id — and with it watch state, playlists, collections, pinned enrichment;
-- album/track key prefixes rewrite; same-titled albums merge; the duplicate-rip
-- Track collision is tolerated via OR IGNORE and settles on the next rescan).

-- 1. Every artist whose key contains a standalone "and" word, and its new key.
--    The name is space-padded so LIKE '% and %' spots the word in any position;
--    replace() runs twice so consecutive "and and" collapse fully; a name that
--    is ONLY "and" is kept whole (never an empty key). The article CASE then
--    re-strips a leading article the drop may have exposed — post-0042 keys are
--    already article-stripped ONLY when the article was the first word.
CREATE TEMP TABLE artist_rekey AS
WITH dropped AS (
    SELECT id, library_id, identity_key AS old_key,
           trim(replace(replace(' ' || substr(identity_key, 8) || ' ',
                                ' and ', ' '),
                        ' and ', ' ')) AS name
      FROM artists
     WHERE ' ' || substr(identity_key, 8) || ' ' LIKE '% and %'
),
guarded AS (
    SELECT id, library_id, old_key,
           CASE WHEN name = '' THEN substr(old_key, 8) ELSE name END AS name
      FROM dropped
),
rekeyed AS (
    SELECT id, library_id, old_key,
           'artist:' || CASE
               WHEN name LIKE 'the %' THEN ltrim(substr(name, 5))
               WHEN name LIKE 'an %'  THEN ltrim(substr(name, 4))
               WHEN name LIKE 'a %'   THEN ltrim(substr(name, 3))
               ELSE name
           END AS new_key
      FROM guarded
)
SELECT id, library_id, old_key, new_key
  FROM rekeyed
 WHERE new_key <> old_key;

-- Steps 2-8 are the 0042 machinery verbatim: see that migration's comments for
-- the reasoning behind each statement and its ordering.

-- 2. One survivor per (library, new key).
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

-- 3. Rewrite the artist-key prefix of every rekeyed artist's Album keys.
UPDATE albums SET identity_key =
       (SELECT r.new_key FROM artist_rekey r WHERE r.id = albums.artist_id)
       || substr(identity_key,
                 length((SELECT r.old_key FROM artist_rekey r WHERE r.id = albums.artist_id)) + 1)
 WHERE artist_id IN (SELECT id FROM artist_rekey);

-- 4. Rewrite the same prefix on Track keys, before any album changes hands.
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

-- 5. Move loser artists' Albums under the survivor (collisions merge in step 6).
UPDATE OR IGNORE albums SET artist_id =
       (SELECT l.survivor_id FROM artist_loser l WHERE l.loser_id = albums.artist_id)
 WHERE artist_id IN (SELECT loser_id FROM artist_loser);

-- 6. Merge each Album left behind into the survivor's same-key Album.
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

-- 7. Migrate loser artists' entity side rows, then delete the loser rows.
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

-- 8. Rekey the rekeyed artists that survived in place.
UPDATE artists SET identity_key =
       (SELECT r.new_key FROM artist_rekey r WHERE r.id = artists.id)
 WHERE id IN (SELECT r.id FROM artist_rekey r
               JOIN artist_survivor s ON s.survivor_id = r.id);

DROP TABLE artist_rekey;
DROP TABLE artist_survivor;
DROP TABLE artist_loser;
DROP TABLE album_merge;
