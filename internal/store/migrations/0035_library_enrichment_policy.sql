-- 0035_library_enrichment_policy: per-Library Enrichment policy (ADR-0027).
--
-- A Library may deviate from the server-wide Enrichment configuration through a
-- SPARSE set of deltas (Model A): unset keys inherit the global config LIVE, so
-- adding a provider or key server-wide reaches every non-overriding Library with
-- no per-Library chore. Only the deltas are stored — there is NO per-Library copy
-- of the global config.
--
-- This first slice carries the single simplest key, enrich_enabled, which stands
-- up the whole spine. Later slices add columns (metadata_language,
-- authoritative_provider) and a companion per-(Library, provider) override table.
--
-- enrich_enabled is NULLABLE and that is load-bearing: NULL (or an absent row)
-- means "inherit the global configuration"; a stored 0/1 is a DELIBERATE override.
-- Keeping "inherits the defaults" distinct from "deliberately froze the defaults"
-- is exactly the ambiguity Model A exists to avoid (ADR-0027), so no column ever
-- stores a value meaning "inherit" — inheritance is NULL / row-absence only.
--
-- One row per Library, keyed by library_id. ON DELETE CASCADE drops the policy
-- when its Library is deleted (no stale rows); foreign_keys is ON (see store.Open).
CREATE TABLE IF NOT EXISTS library_enrichment_policy (
    library_id     TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    enrich_enabled INTEGER,  -- NULL = inherit; 0/1 = deliberate override
    updated_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
