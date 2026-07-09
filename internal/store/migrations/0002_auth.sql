-- 0002_auth: the authentication spine — password hashes, Devices, and tokens.
--
-- Builds on 0001's users table (ADR-0013 first-Admin bootstrap, ADR-0015
-- opaque DB-backed tokens). This slice mints a single Admin only; Members,
-- per-user library access, and rating ceilings arrive in later slices.

-- Each User carries a password hash. Stored as a self-describing PHC-like
-- string (algorithm + params + salt + digest), never plaintext. Added as a
-- nullable column so the 0001 schema migrates cleanly even though, in
-- practice, the first User is only ever created with a hash already set.
ALTER TABLE users ADD COLUMN password_hash TEXT;

-- A Device is a first-class, named client installation belonging to a User
-- (CONTEXT.md). It is keyed for dedup by (user_id, client_id): re-login from
-- the same stable clientId reuses/refreshes this row rather than creating a
-- duplicate. Deleting a Device cascades to its tokens, killing them instantly.
CREATE TABLE IF NOT EXISTS devices (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- client_id is the stable per-installation UUID the client persists.
    client_id   TEXT NOT NULL,
    name        TEXT NOT NULL,
    platform    TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    last_seen_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (user_id, client_id)
);

-- An opaque bearer token, stored only as a hash (ADR-0015). The raw token is
-- shown to the client exactly once at login and never persisted in the clear.
-- token_hash is the lookup key, so it is unique and indexed by the PK. A token
-- belongs to exactly one Device; deleting the Device (or logging out) deletes
-- the token, which is immediate revocation.
CREATE TABLE IF NOT EXISTS auth_tokens (
    token_hash  TEXT PRIMARY KEY,
    device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id);
CREATE INDEX IF NOT EXISTS idx_auth_tokens_device ON auth_tokens(device_id);
