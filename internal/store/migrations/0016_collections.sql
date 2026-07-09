-- 0016_collections: Admin-curated, shared Collections (collections-playlists 01).
--
-- A Collection is a named, unordered, cross-kind grouping of Titles the Admin
-- hand-curates for discovery (CONTEXT.md "Collection"). It is shared — it belongs
-- to the server, not a User, so there is NO owner column (a Playlist, issue 03,
-- is the User-owned ordered counterpart and lives in its own tables).
CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    -- description carries an optional sentence of context for the row; stored as
    -- '' (never NULL) when absent so reads need no COALESCE.
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- collection_items is the membership join. Two cascades buy two behaviors for
-- free: deleting a Collection drops its membership rows (collection_id cascade),
-- and deleting a Title — or the whole Library it sits in, which cascades to the
-- Title — drops it from every Collection it was in (title_id cascade, story 11).
-- The UNIQUE(collection_id, title_id) makes a Collection a SET: re-adding an
-- existing member is a harmless no-op (the add path uses INSERT OR IGNORE, so it
-- is idempotent, story 6). Membership is keyed to the stable Title id (ADR-0014),
-- so a member survives an Edition swap / file rename like watch state does; and a
-- member going Missing (titles.hidden = 1, ADR-0008) is omitted from the resolved
-- view but its row PERSISTS here, so it reappears when the Files return.
CREATE TABLE IF NOT EXISTS collection_items (
    collection_id TEXT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    title_id      TEXT NOT NULL REFERENCES titles(id)      ON DELETE CASCADE,
    added_at      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (collection_id, title_id)
);

-- The read path is "this Collection's members": a scan of one Collection's item
-- rows, joined to titles for the sort_title ordering. Index the lookup column.
CREATE INDEX IF NOT EXISTS idx_collection_items_collection ON collection_items(collection_id);
