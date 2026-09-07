-- Linked entity artwork: the artwork ROLES a mirrored Show/Artist/Album
-- advertises, and a cache-bust VERSION for them (.scratch/linked-servers issue
-- 19, ADR-0056 §5).
--
-- The problem this closes: a browse builder advertises a Show poster / Artist
-- image / Album cover URL only when a LOCAL `entity_artwork` row exists, and the
-- mirror (store/mirror.go) copies none — so a mirrored entity's image is never
-- offered to the client and the byte relay (link.RelayArtwork), which already
-- works, is never asked. The sharer's Export now carries `artworkRoles` +
-- `artworkVersion` for each of those entities, and the mirror lands them HERE.
--
-- Why a dedicated table and NOT `entity_artwork`: that table's `path` is
-- NOT NULL and carries a servable file, behind a source CHECK ('local',
-- 'fetched', 'uploaded'). A mirrored entity has NO local file — its bytes live on
-- the other household's disk and arrive through the relay — so a marker row with
-- an empty path would fight every reader that opens the path there. This is a
-- SIGNAL-ONLY table: it says "the sharer advertises these roles, at this
-- version", and nothing in it is ever served from disk.
--
-- entity_id is THIS Server's local id for the mirrored Show/Artist/Album (the one
-- the mirror minted and keyed the sharer's onto by remote_id), so the browse read
-- path joins it on the same id the decorators already hold. The mirror REPLACES an
-- entity's whole row-set on every pull (delete-then-insert): the sharer dropping
-- all of an entity's art clears the rows and the mirror stops advertising it — a
-- tombstone by absence, consistent with the mirror's own write model.
--
-- version is the sharer's entity-level cache-bust token (its newest artwork
-- added_at). It is repeated on every role row of an entity — one entity has one
-- version — so the browse read can MAX() it exactly as EntityArtworkVersionsForMany
-- does over entity_artwork.
CREATE TABLE IF NOT EXISTS linked_entity_artwork (
    entity_type TEXT NOT NULL,             -- 'show' | 'artist' | 'album'
    entity_id   TEXT NOT NULL,             -- this Server's local id for the mirrored entity
    role        TEXT NOT NULL,             -- 'poster' | 'background' | 'logo' | 'cover'
    version     TEXT NOT NULL DEFAULT '',  -- the sharer's entity-level cache-bust token
    PRIMARY KEY (entity_type, entity_id, role)
);

-- The table carries no foreign key (its entity_id is not one column reachable by
-- a single reference — it names one of three tables). These AFTER DELETE triggers
-- are its cleanup: they clear an entity's signal rows whenever the entity itself
-- goes, which happens three ways — a full pull pruning a row the sharer no longer
-- has (store/mirror.go prune), the ON DELETE CASCADE that unlinking runs when its
-- linked Library is deleted (ADR-0056 §6), and any future direct delete. Without
-- them a re-grant/relink would leave dead signal rows keyed to local ids this
-- Server never mints again.
CREATE TRIGGER IF NOT EXISTS shows_linked_artwork_ad AFTER DELETE ON shows
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'show' AND entity_id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS artists_linked_artwork_ad AFTER DELETE ON artists
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'artist' AND entity_id = OLD.id;
END;

CREATE TRIGGER IF NOT EXISTS albums_linked_artwork_ad AFTER DELETE ON albums
BEGIN
    DELETE FROM linked_entity_artwork WHERE entity_type = 'album' AND entity_id = OLD.id;
END;
