-- 0017_playlists: User-owned, ordered, single-media-kind Playlists
-- (collections-playlists 03).
--
-- A Playlist is the User-owned, PRIVATE counterpart to the shared Collection
-- (0016): an ordered "watch later" queue keyed to the stable Title id (ADR-0014).
-- Unlike a Collection it HAS an owner (owner_user_id) — it belongs to one User and
-- is invisible to everyone else (404 hide-existence, like a playback session) — and
-- it is ordered (position) and single-media-kind (kind).
CREATE TABLE IF NOT EXISTS playlists (
    id            TEXT PRIMARY KEY,
    -- The owning User. The cascade gives "delete a User → their Playlists (and,
    -- via the items cascade below, their items) go with them" (story 36) for free.
    owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    -- The single media kind the Playlist holds: one of 'movie' | 'tv' | 'music'
    -- (NOT the raw Title kind — an Episode maps to 'tv', a Track to 'music'). It is
    -- NULL until the FIRST item fixes it (the Playlist is created empty/untyped);
    -- thereafter every appended Title must map to the same kind (single-kind rule,
    -- enforced in the organize service, story 26/27).
    kind          TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- The list read is "this User's Playlists"; index the owner lookup column.
CREATE INDEX IF NOT EXISTS idx_playlists_owner ON playlists(owner_user_id);

-- playlist_items is the ordered membership. Two cascades buy two behaviors for
-- free: deleting a Playlist drops its item rows (playlist_id cascade), and deleting
-- a Title — or the whole Library it sits in, which cascades to the Title — drops it
-- from every Playlist (title_id cascade). Unlike collection_items there is a
-- per-item PRIMARY KEY id and NO UNIQUE(playlist_id, title_id): a Playlist is a
-- SEQUENCE, not a set, so the SAME Title may appear more than once, each occurrence
-- its own item id (duplicates allowed, story 28). `position` carries the explicit
-- order (append puts a new item at MAX(position)+1). A member going Missing
-- (titles.hidden = 1, ADR-0008) is omitted from the resolved view but its row
-- PERSISTS here, so it reappears when the Files return.
CREATE TABLE IF NOT EXISTS playlist_items (
    id          TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    title_id    TEXT NOT NULL REFERENCES titles(id)    ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    added_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- The read path is "this Playlist's items in order": a scan of one Playlist's item
-- rows. Index the lookup column.
CREATE INDEX IF NOT EXISTS idx_playlist_items_playlist ON playlist_items(playlist_id);
