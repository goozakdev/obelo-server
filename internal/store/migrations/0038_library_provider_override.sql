-- 0038_library_provider_override: the per-provider Supplement tri-state of the
-- per-Library Enrichment policy (ADR-0027, feature slice 05).
--
-- A Library may force an individual Supplement on or off, independent of the
-- server-wide setting — lean a documentary Library on OMDb/TheTVDB, or mute a noisy
-- source for one Library only. The tri-state is encoded by ROW PRESENCE: a row
-- present is a deliberate override (enabled 0/1); a row ABSENT means "inherit the
-- provider's global enabled state" — no sentinel ever means "inherit", the Model A
-- invariant (ADR-0027).
--
-- One row per (Library, provider); provider is a registry slug. ON DELETE CASCADE
-- drops a Library's overrides with it (foreign_keys is ON — see store.Open).
CREATE TABLE IF NOT EXISTS library_provider_override (
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,             -- registry slug
    enabled    INTEGER NOT NULL,          -- 0/1 forced state (row present = override)
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (library_id, provider)
);
