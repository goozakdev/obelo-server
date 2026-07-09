-- 0019_enrichment_settings: three server-wide Enrichment BEHAVIOR knobs made
-- runtime-configurable (enrichment-runtime-settings), extending the singleton
-- metadata_settings row shipped in 0018. Like the provider settings, these move
-- out of static env/config into durable state the running server reads at
-- runtime with no restart; env only SEEDS them on first boot.
--
--   auto_enrich_after_scan    — whether a completed scan enqueues a background
--                               Enrichment pass (a bool stored 0/1).
--   enrich_interval_seconds   — the scheduled safety-net enrich cadence, in
--                               seconds; 0 disables the scheduler.
--   musicbrainz_rate_limit_ms — the MusicBrainz throttle, in milliseconds; 0
--                               disables throttling.
--
-- All three are NULLABLE with no default: NULL means "unset", which is DISTINCT
-- from a real 0 (0 is a MEANINGFUL "disabled" value for the interval / rate
-- limit). Go resolves a NULL column to the config-derived seed value on the
-- first boot that sees it (SeedIfEmpty on a fresh install; an idempotent
-- per-column backfill on an upgrade boot where the 0018 row already exists),
-- after which the DB is authoritative and env is ignored at runtime.
ALTER TABLE metadata_settings ADD COLUMN auto_enrich_after_scan INTEGER;
ALTER TABLE metadata_settings ADD COLUMN enrich_interval_seconds INTEGER;
ALTER TABLE metadata_settings ADD COLUMN musicbrainz_rate_limit_ms INTEGER;
