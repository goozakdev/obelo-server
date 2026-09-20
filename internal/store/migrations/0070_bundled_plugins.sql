-- 0070_bundled_plugins: where an Installed plugin CAME FROM, and the one thing a
-- server has to remember about a plugin it no longer has (ADR-0059 decisions 1-3,
-- .scratch/bundled-plugins issue 04).
--
-- The seven shipped metadata providers stop being compiled into the binary and
-- become Bundled plugins: WebAssembly modules the server carries, written into
-- <dataDir>/plugins/<id>/ on first boot exactly as an Admin's upload would be, and
-- RE-ASSERTED on every boot after that. Two facts make that lifecycle possible and
-- neither can be read off the files.

-- `origin` is who put this plugin here: 'bundled' for one the server shipped,
-- 'admin' for one a person uploaded or pasted a URL for. DEFAULT 'admin' is what
-- makes the migration correct for a server that already has plugins — every row
-- that existed before this column did was put there by an Admin, by construction,
-- because nothing else could put one there.
--
-- It decides exactly one thing, and the decision is the whole point: on every boot
-- the server replaces a 'bundled' row's files when it ships a newer version, and
-- NEVER TOUCHES an 'admin' row. An operator who uploads their own plugin under the
-- id `tmdb` has said something, and a server upgrade that overwrote it would be
-- the server winning an argument it was not invited to.
--
-- It is also the one sentence the Plugins screen shows in place of an upload name
-- or a URL: "Shipped with Obelo".
ALTER TABLE plugins ADD COLUMN origin TEXT NOT NULL DEFAULT 'admin';

-- The declined memory: a Bundled plugin an Admin uninstalled.
--
-- Uninstall deletes the row and the files (see store.DeletePlugin, which is
-- deliberately total). For an Installed plugin that is the end of it — the Admin
-- has the file and can upload it again. For a BUNDLED one it cannot be, because
-- the server would simply put it back on the next boot, and "uninstall" would mean
-- "until you restart". So the id is remembered HERE, and the re-assert skips it.
--
-- A TABLE rather than a tombstone row in `plugins`, for two reasons. A tombstone
-- would have to be filtered out of every read of that table — the Plugins screen,
-- the disabled-ids query the composition root asks before registering anything,
-- the duplicate check an install makes — and one forgotten filter is a phantom
-- plugin. And a declined id is not a plugin: there is nothing to enable, nothing
-- to configure and nothing to call. Two columns say everything there is to say.
--
-- The mark is cleared by exactly two things: POST /settings/plugins/{id}/reinstall-shipped,
-- which is the Admin changing their mind, and any install that claims the id,
-- which is the Admin replacing the shipped plugin with one of their own.
CREATE TABLE IF NOT EXISTS declined_plugins (
    id          TEXT PRIMARY KEY,
    declined_at TEXT NOT NULL DEFAULT (datetime('now'))
);
