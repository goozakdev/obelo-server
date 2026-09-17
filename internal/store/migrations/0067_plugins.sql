-- 0067_plugins: the record of what an Admin INSTALLED (ADR-0058,
-- .scratch/plugin-system issue 10). The counterpart of 0018's metadata providers
-- and 0066's event sinks, for the Plugins that are not compiled into the binary.
--
-- THE DIRECTORY IS STILL THE TRUTH ABOUT WHAT IS INSTALLED. The loader reads
-- <dataDir>/plugins/<id>/ and needs none of this to work: a server whose database
-- lost these rows still loads every module on disk. What lives here is what the
-- FILES cannot say — the Admin's own enable switch, where the Plugin came from,
-- when it arrived, and the last thing that went wrong with it — so the Plugins
-- screen can answer those questions after a restart, before anything has been
-- called.

-- One row per Installed plugin, keyed by the manifest id, which is also the
-- directory on disk and the settings slug the fixed-shape settings row is written
-- under. The three columns copied from the manifest (name, version, api_version)
-- are DENORMALIZED on purpose: they are what the screen shows for a Plugin whose
-- module would not load at all, which is precisely the Plugin an Admin most needs
-- named.
--
-- `provides` is the Extension points the manifest declared, as a JSON array of the
-- contract's own tokens (metadata-provider / subtitle-provider / event-sink). A
-- JSON column rather than a join table for 0066's reason: a handful of
-- closed-vocabulary tokens, read and replaced whole with the rest of the row.
--
-- `enabled` is the ADMIN'S SWITCH, and it is not the loader's `disabled`. An
-- enabled Plugin can still be disabled — refused at load, or stopped after
-- repeated failures — and that combination is exactly the state that needs
-- explaining on screen. Switching a Plugin off un-registers it, so nothing
-- downstream can deliver to it; it is not a flag the delivery path consults.
--
-- `last_error` is the loader's last sentence, written back so the screen can show
-- it after a restart too. It is a CACHE of host state, never a source of truth:
-- clearing it does not re-enable anything, and the running loader overwrites it.
--
-- `source` is where the bytes came from — 'upload' for a file an Admin sent
-- through the browser, or the absolute manifest URL they pasted. It is what makes
-- a later catalog (issue 15) able to say "this one came from here" and what an
-- Admin reads when they no longer remember.
CREATE TABLE IF NOT EXISTS plugins (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    api_version  INTEGER NOT NULL DEFAULT 0,
    provides     TEXT NOT NULL DEFAULT '[]',
    enabled      INTEGER NOT NULL DEFAULT 1,
    last_error   TEXT,
    source       TEXT NOT NULL DEFAULT '',
    installed_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Per-Plugin settings that the FIXED shape cannot hold.
--
-- NOTHING WRITES THIS TABLE YET, and that is deliberate rather than an oversight.
-- Every Extension point today takes the same five-field Settings value of ADR-0057
-- (enabled / secret / url / url2 / events), and those already have a home: an
-- Installed Event sink's settings are an `event_sinks` row keyed by its slug, so
-- the settings API, the screen and the Manager work on it with no change at all.
-- The table exists now because the migration that adds it is the cheap half and
-- the schema is the part worth agreeing on early; issue 13 renders a
-- manifest-declared schema into it.
--
-- One row per (plugin, key), because a manifest-declared schema is a set of named
-- fields whose names this server cannot know at migration time — the one place in
-- this codebase where a key/value shape is the honest one rather than a shortcut
-- around typed columns.
--
-- `secret` marks a value that must never be returned by the API. It is handled
-- EXACTLY as `metadata_providers.api_key` and `event_sinks.secret` are — a
-- nullable column the settings surface masks to a hasSecret boolean — which is to
-- say the protection is masking and access control, not a column cipher. (Nothing
-- in this repo encrypts a credential column; `internal/rotation` is the
-- key-rotation envelope for the maintainer's own default keys, not storage
-- encryption. If "encrypted at rest" was ever meant literally, it is a gap that
-- predates this table and wants its own issue, and it wants fixing for all three
-- tables at once.)
--
-- The rows go when the Plugin does: uninstall deletes them by plugin_id, in the
-- same transaction as the `plugins` row.
CREATE TABLE IF NOT EXISTS plugin_settings (
    plugin_id  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT,
    secret     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (plugin_id, key)
);
