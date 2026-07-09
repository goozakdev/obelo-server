-- 0031_uploaded_artwork: admit a third artwork source, 'uploaded' (ADR-0026).
--
-- An Admin can now supply an image the providers don't offer by uploading it in
-- an artwork tab (drag-drop / Browse). Uploading IS selecting: the bytes are
-- stored in the identity-keyed artwork cache and the role is Locked, reusing the
-- single-row-per-(role,source) model (no candidate pool). The upload is a
-- DISTINCT source — not 'local' (rescans reconcile local rows against disk and
-- could drop an upload) and not 'fetched' (which loses to Local). At serve time
-- 'uploaded' outranks EVERYTHING (uploaded > local > fetched); the read paths
-- (ArtworkByTitleRole / EntityArtworkByRole) carry the new ordering.
--
-- SQLite can't ALTER a CHECK in place, so rebuild both artwork tables preserving
-- every row (same pattern as 0010/0011). defer_foreign_keys defers the
-- artwork→titles FK check to commit, by which point every id is preserved.
PRAGMA defer_foreign_keys = ON;

-- 1. Leaf-Title artwork: grow source CHECK from ('local','fetched') to add
--    'uploaded'. role stays poster/background; UNIQUE(title_id, role, source) now
--    admits up to three rows per role (local + fetched + uploaded).
CREATE TABLE artwork_new (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('poster', 'background')),
    path     TEXT NOT NULL,
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    source   TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'fetched', 'uploaded')),
    UNIQUE (title_id, role, source)
);
INSERT INTO artwork_new (id, title_id, role, path, added_at, source)
    SELECT id, title_id, role, path, added_at, source FROM artwork;
DROP TABLE artwork;
ALTER TABLE artwork_new RENAME TO artwork;
CREATE INDEX IF NOT EXISTS idx_artwork_title ON artwork(title_id);

-- 2. Parent-entity artwork: same growth. Column order matches the live table
--    after 0011 (create) + 0013 (added_at): id, entity_type, entity_id, role,
--    path, source, added_at. Preserves person headshots and every parent image.
CREATE TABLE entity_artwork_new (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    role        TEXT NOT NULL,
    path        TEXT NOT NULL,
    source      TEXT NOT NULL DEFAULT 'fetched' CHECK (source IN ('local', 'fetched', 'uploaded')),
    added_at    TEXT NOT NULL DEFAULT '',
    UNIQUE (entity_type, entity_id, role, source)
);
INSERT INTO entity_artwork_new (id, entity_type, entity_id, role, path, source, added_at)
    SELECT id, entity_type, entity_id, role, path, source, added_at FROM entity_artwork;
DROP TABLE entity_artwork;
ALTER TABLE entity_artwork_new RENAME TO entity_artwork;
CREATE INDEX IF NOT EXISTS idx_entity_artwork ON entity_artwork(entity_type, entity_id);
