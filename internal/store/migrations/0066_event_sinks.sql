-- 0066_event_sinks: DB-backed Event sink settings (ADR-0057 decision 6,
-- .scratch/plugin-system issue 05), mirroring the metadata-provider settings of
-- 0018 and the subtitle-provider settings of 0027.
--
-- An Event sink is a Plugin like any other, so its settings are the SAME fixed
-- shape every Plugin gets (pluginapi.Settings): an enabled toggle, one secret, one
-- URL — plus the one field only a sink uses, the list of event types it subscribes
-- to. That list is the reason this is its own table rather than a column bolted
-- onto `subtitle_providers`: a sink and a provider share a shape, not a namespace,
-- and a sink slug colliding with a provider slug in one table would be an Admin's
-- signing secret landing on a metadata source.
--
-- Only MUTABLE state lives here. A sink's name, whether it needs a secret, and the
-- copy the settings screen shows are STATIC CODE (its Descriptor in the Plugin
-- registry), not columns — so adding a sink is a registration, never a migration.

-- One row per Event sink, keyed by a stable slug (webhook). A missing row means
-- "never configured" — disabled, with nothing to post to (ADR-0001 offline-first).
--
-- `secret` is the HMAC signing key. It is the nullable secret column every other
-- Plugin's credential uses, handled exactly as an API key is: NULL/'' = none, and
-- NEVER returned by the API — only a hasSecret boolean.
--
-- `url` is the sink's target. Unlike a provider's base_url it has no code default:
-- a sink with no URL has nowhere to post, which is why the settings endpoint
-- refuses to enable one without both a URL and a secret (the server must never
-- post unsigned, or post nowhere).
--
-- `events` is the subscribed-event list, stored as a comma-separated list of event
-- type ids in the order the Admin chose. A list rather than a table because it is
-- a handful of closed-vocabulary tokens read and replaced whole with the rest of
-- the row — the same reason a Library's kind is a column and not a join.
CREATE TABLE IF NOT EXISTS event_sinks (
    slug        TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 0,
    secret      TEXT,
    url         TEXT,
    events      TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
