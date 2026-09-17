-- 0068_plugin_kv: the plugin-scoped key-value namespace (ADR-0058 decision 5,
-- .scratch/plugin-system issue 11).
--
-- An Installed plugin has no filesystem, no environment and no way to keep
-- anything across a rebuilt instance — its linear memory dies with the instance,
-- which is the sandbox working as designed. This is the one durable thing it may
-- have: a small namespace for the state a source-shaped Plugin accumulates and
-- would otherwise re-derive on every boot (a paging cursor, an etag, a token's
-- expiry, a small response cache).
--
-- ADR-0007 puts state in SQLite and blobs on disk. A plugin's cursor IS state, and
-- the host caps what may be written here (a few KiB per value) precisely so this
-- table never becomes the blob store by accident.
--
-- THE PRIMARY KEY IS THE WHOLE SECURITY PROPERTY. plugin_id comes from the
-- manifest on disk — the directory the module was loaded from — and is prefixed by
-- the HOST, never by anything a guest says, so no spelling of a key can reach
-- another Plugin's value. Uninstall drops the namespace with one DELETE.
--
-- It is deliberately NOT the settings table. Settings are the Admin's, arrive
-- resolved from the host and are never written by a guest; this is the guest's own
-- scratch space and the host never reads it. Mixing the two would let a Plugin
-- rewrite its own enabled flag.
CREATE TABLE IF NOT EXISTS plugin_kv (
    plugin_id   TEXT NOT NULL,
    key         TEXT NOT NULL,
    value       BLOB NOT NULL,
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_id, key)
);
