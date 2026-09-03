-- 0058_remote_role: admit a third User role, 'remote' — the role a linked Server
-- holds on this one (ADR-0054, .scratch/linked-servers issue 01).
--
-- ADR-0054's whole point is that a peer Server is NOT a new entity: it is a User,
-- so the per-User grant set, the Rating ceiling, access.Service.Resolve, the
-- Device/token binding and the 404-not-403 posture all apply with no new
-- enforcement code. The only thing that has to move for that to be true is the
-- schema's opinion about what a User is, and it has exactly two opinions:
--
--   1. role IN ('admin', 'member')  — widened to admit 'remote'.
--   2. every User has a password    — implicit today, and now made EXPLICIT in
--      the opposite direction: a CHECK that a User has a non-empty password hash
--      UNLESS its role is 'remote'.
--
-- The second constraint is the interesting one. A `remote` User's only credential
-- is the token an Invite leaves behind (ADR-0055); it has no password, and
-- POST /auth/login refuses the role outright. Writing that as a CHECK rather than
-- as a service-layer rule means the passwordless state is reachable for exactly
-- one role and no other — a `member` can never be created without a password by
-- accident, which is the failure a bare "password_hash is nullable" column would
-- have permitted silently for the rest of the server's life.
--
-- COALESCE(password_hash, '') and not a bare `password_hash <> ''`: the column
-- arrived nullable in 0002, and in SQLite a CHECK passes when its expression is
-- NULL (only a false result fails one). `password_hash <> '' OR role = 'remote'`
-- would therefore ADMIT a NULL-hash member — the exact hole this constraint
-- exists to close. Every row CreateAdmin/CreateUser has ever written carries a
-- real hash, so the rebuild's copy below cannot trip on existing data.
--
-- SQLite cannot ALTER a CHECK in place, so users is rebuilt with the standard
-- pattern (create new -> copy -> drop old -> rename), preserving every column,
-- default and value exactly; only the two CHECKs are new. Column order matches
-- the live table after 0001 (id, username, role, created_at) + 0002
-- (password_hash) + 0015 (rating_ceiling). No index is dropped: username's
-- uniqueness is inline and users carries no explicit index.
--
-- foreign_keys is ON (db.go pragma) and migrations run inside a transaction, so
-- DEFER enforcement to commit time exactly as 0008/0031 do: the rebuild drops and
-- recreates users mid-transaction while devices, auth_tokens,
-- user_library_access, watch_state, playlists, remembered_audio/video,
-- stream_tokens and device_auth_requests all reference users(id). Every id is
-- preserved verbatim by the copy, so by COMMIT every child FK still resolves and
-- the deferred check passes.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE users_new (
    id         TEXT PRIMARY KEY,
    username   TEXT NOT NULL UNIQUE,
    -- 'remote' joins the vocabulary: a linked Server holds it (ADR-0054). It is a
    -- ROLE and not a flag on a member deliberately — every guard then reads as
    -- `role == remote`, one field, the way ADMIN_GRANT reads today.
    role       TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'member', 'remote')),
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    password_hash TEXT,
    rating_ceiling TEXT,
    -- Only a 'remote' User may lack a password. See the note above on COALESCE.
    CHECK (COALESCE(password_hash, '') <> '' OR role = 'remote')
);
INSERT INTO users_new (id, username, role, created_at, password_hash, rating_ceiling)
    SELECT id, username, role, created_at, password_hash, rating_ceiling FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
