-- 0014_user_library_access: per-User library-access grants (access-control 03).
--
-- A Member sees only the Libraries granted to them here; an entity in any other
-- Library is hidden as 404 (api-contract.md "404, not 403"). An Admin has NO
-- rows in this table — Admin = all Libraries is resolved by role, so a newly
-- added Library is implicitly the Admin's with no backfill, and granting to an
-- Admin is rejected upstream rather than recorded.
--
-- Both foreign keys cascade: deleting a User drops their grants (so re-creating
-- a username starts clean), and deleting a Library drops every grant that
-- pointed at it (no stale rows). The UNIQUE makes a (user, library) grant a set
-- — a duplicate grant is a harmless no-op (the replace-set write uses
-- INSERT OR IGNORE).
CREATE TABLE IF NOT EXISTS user_library_access (
    user_id    TEXT NOT NULL REFERENCES users(id)     ON DELETE CASCADE,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    UNIQUE (user_id, library_id)
);

-- The access read path is "the Libraries this User may see": a scan of one
-- User's grant rows, resolved once per request. Index the access path (user).
CREATE INDEX IF NOT EXISTS idx_user_library_access_user ON user_library_access(user_id);
