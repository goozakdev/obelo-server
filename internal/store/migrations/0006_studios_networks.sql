-- Every Studio of a Movie and every Network of a Show, not just the first. The
-- single-value columns (titles.studio, entity_enrichment.network) stay as the
-- denormalized "first" for current readers; these tables hold the whole list, in
-- the provider's order, shaped like title_genres / entity_genres.

CREATE TABLE title_studios (
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    studio   TEXT NOT NULL,
    ord      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_title_studios_title ON title_studios(title_id);
CREATE INDEX idx_title_studios_studio ON title_studios(studio);

CREATE TABLE entity_networks (
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    network     TEXT NOT NULL,
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_entity_networks ON entity_networks(entity_type, entity_id);
CREATE INDEX idx_entity_networks_network ON entity_networks(network);

-- Carry every existing single value in BEFORE the touch triggers exist, so the
-- backfill does not advance every owner's updated_at and re-export the library.
INSERT INTO title_studios (title_id, studio, ord)
    SELECT id, studio, 0 FROM titles WHERE studio != '';
INSERT INTO entity_networks (entity_type, entity_id, network, ord)
    SELECT entity_type, entity_id, network, 0 FROM entity_enrichment WHERE network != '';

CREATE TRIGGER title_studios_touch_ai AFTER INSERT ON title_studios
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_studios_touch_au AFTER UPDATE ON title_studios
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = NEW.title_id;
END;
CREATE TRIGGER title_studios_touch_ad AFTER DELETE ON title_studios
BEGIN
    UPDATE titles SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = OLD.title_id;
END;

CREATE TRIGGER entity_networks_touch_ai AFTER INSERT ON entity_networks
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_networks_touch_au AFTER UPDATE ON entity_networks
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'show'   AND id = NEW.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'season' AND id = NEW.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'artist' AND id = NEW.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE NEW.entity_type = 'album'  AND id = NEW.entity_id;
END;
CREATE TRIGGER entity_networks_touch_ad AFTER DELETE ON entity_networks
BEGIN
    UPDATE shows   SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'show'   AND id = OLD.entity_id;
    UPDATE seasons SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'season' AND id = OLD.entity_id;
    UPDATE artists SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'artist' AND id = OLD.entity_id;
    UPDATE albums  SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE OLD.entity_type = 'album'  AND id = OLD.entity_id;
END;
