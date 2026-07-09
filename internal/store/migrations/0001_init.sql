-- 0001_init: foundational schema for the walking skeleton.
--
-- Only the users table is needed this slice: the server handshake reports
-- setupRequired=true while zero Users exist (ADR-0013). Later slices add
-- devices, tokens, libraries, the catalog, watch state, etc. via new
-- migration files; this one stays frozen once shipped.

CREATE TABLE IF NOT EXISTS users (
    id         TEXT PRIMARY KEY,
    username   TEXT NOT NULL UNIQUE,
    -- role is constrained to known values; this slice only mints 'admin'.
    role       TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'member')),
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
