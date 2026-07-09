-- 0013_entity_artwork_added_at: give parent-entity artwork a write timestamp so a
-- browse client can cache-bust a Show / Season / Artist / Album poster exactly
-- when its fetched image changes. The title-keyed `artwork` table already has
-- `added_at` (0005/0010), which the Movie/Episode/Track poster version reads as
-- MAX(added_at); the parent `entity_artwork` table (0011) lacked it, so a
-- re-enrich that swapped a Show/Artist poster left the URL — and the browser's
-- cache — stale. A re-enrich replaces a role's row (DELETE + INSERT in
-- WriteEntityEnrichment), so a fresh `added_at` on each insert is the signal;
-- MAX(added_at) per entity is the opaque token the client appends as `?v=`.
--
-- SQLite forbids a non-constant default (datetime('now')) on ADD COLUMN, so we
-- add the column with a constant default and backfill existing rows once; the
-- single inserter (WriteEntityEnrichment) sets added_at = datetime('now')
-- explicitly going forward. Additive, no table rebuild; existing rows get a
-- one-time timestamp (their real fetch time is unknown, which is fine — the
-- token only needs to CHANGE on the next re-enrich, not be historically exact).
ALTER TABLE entity_artwork ADD COLUMN added_at TEXT NOT NULL DEFAULT '';
UPDATE entity_artwork SET added_at = datetime('now') WHERE added_at = '';
