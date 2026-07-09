-- 0021_watchlist: the per-User system "Watchlist" playlist.
--
-- The Watchlist is a first-class, always-present Playlist every User owns — the
-- durable home for "save this to watch later" and the anchor future features
-- (recommendations, "on your watchlist" rows) build on. It is a normal Playlist
-- row in every respect (owner-private, ordered, single-media-kind once typed) with
-- ONE marker: a non-NULL `system` slug ('watchlist'). That marker is what makes it
-- guaranteed-unique per User and what the organize service reads to forbid renaming
-- or deleting it (a system Playlist is not the User's to remove).
--
-- `system` is NULL for every ordinary Playlist and holds a stable slug for a system
-- one, so the design generalizes: a later feature can add another system Playlist by
-- picking a new slug without another migration.
ALTER TABLE playlists ADD COLUMN system TEXT;

-- At most ONE system Playlist of each slug per owner. Partial (WHERE system IS NOT
-- NULL) so ordinary Playlists — all with system = NULL — are unconstrained and a
-- User may still have many of them.
CREATE UNIQUE INDEX IF NOT EXISTS idx_playlists_owner_system
    ON playlists(owner_user_id, system) WHERE system IS NOT NULL;

-- Back-fill: give every EXISTING User their Watchlist now, so "always exists" holds
-- the instant this migration runs (Users created later get theirs seeded lazily on
-- first access, via the store's EnsureSystemPlaylist). The id is any unique opaque
-- text (ids are opaque, ADR-0014); hex(randomblob(16)) is a 32-char unique value.
-- created_at/updated_at fall to the table defaults.
INSERT INTO playlists (id, owner_user_id, name, system)
SELECT lower(hex(randomblob(16))), id, 'Watchlist', 'watchlist'
  FROM users;
