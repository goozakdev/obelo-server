-- Trust on first install (ADR-0069): the publisher name, public key and key id of
-- whoever signed a plugin's package, recorded whenever the signature verified.
-- Separate from plugins.publisher/key_id, which stay the pinned-verification
-- record. Nullable with no default: a row installed unsigned, or before this
-- migration, has no recorded key, and the two are deliberately not told apart.
-- 0001 is frozen, so the columns are added the ordinary way and the stored
-- definition is then rewritten to the CREATE TABLE a fresh database gets (the
-- same procedure as 0002, for the same reason: the two must compare equal).
ALTER TABLE plugins ADD COLUMN signer_name   TEXT;
ALTER TABLE plugins ADD COLUMN signer_key    TEXT;
ALTER TABLE plugins ADD COLUMN signer_key_id TEXT;

PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = 'CREATE TABLE plugins (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '''',
    version      TEXT NOT NULL DEFAULT '''',
    api_version  INTEGER NOT NULL DEFAULT 0,
    provides     TEXT NOT NULL DEFAULT ''[]'',
    -- publisher/key_id name the signer that vouches for this plugin;
    -- '''' for one installed without a signature check (a Bundled plugin, or a
    -- side-loaded dev build). origin records how it got here: e.g. ''admin''
    -- for one an Admin installed by hand, distinct from a Bundled plugin.
    publisher    TEXT NOT NULL DEFAULT '''',
    key_id       TEXT NOT NULL DEFAULT '''',
    origin       TEXT NOT NULL CHECK (origin IN (''admin'', ''bundled'')),
    source       TEXT NOT NULL DEFAULT '''',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_error   TEXT,
    installed_at TEXT NOT NULL DEFAULT (datetime(''now'')),
    -- signer_* are the trust-on-first-install record (ADR-0069): who signed the
    -- package, whenever the signature verified. NULL = no recorded key.
    signer_name   TEXT,
    signer_key    TEXT,
    signer_key_id TEXT
)' WHERE type = 'table' AND name = 'plugins';
PRAGMA writable_schema = RESET;
