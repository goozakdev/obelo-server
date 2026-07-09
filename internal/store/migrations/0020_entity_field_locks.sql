-- 0020_entity_field_locks: extend the Locked-field + durable Enrichment-override
-- machinery from the leaf Title (slice item-editing/01) to the TV/Music browse
-- PARENT entities — Show, Artist, Album — whose enrichment lives in
-- entity_enrichment / entity_genres / entity_artwork with no lock table and no
-- durable-override anchor today (item-editing/02, ADR-0019). Like every
-- enrichment migration this is purely additive (one new table + one ADD COLUMN),
-- never touches identity (identity_key / watch state), and leaves every existing
-- entity_enrichment row byte-for-byte intact — external_id_locked backfills to 0
-- (auto), so an un-corrected parent behaves exactly as before.

-- 1. A Locked field on a browse-parent entity: an Admin hand-edited it, so
--    re-enrichment skips it — the entity_field_locks analogue of title_field_locks
--    (CONTEXT.md "Locked field"). Keyed by (entity_type, entity_id, field);
--    `field` names a scalar column ('overview', 'content_rating', 'network'), the
--    multi-valued 'genres', or an artwork role ('poster' | 'background' | 'cover').
--    entity_type ∈ 'show'|'season'|'artist'|'album' (Seasons are never edited in
--    v1, but the generic key admits them). The parent enrich path honors any lock
--    present here exactly as the leaf path honors title_field_locks.
CREATE TABLE IF NOT EXISTS entity_field_locks (
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    field       TEXT NOT NULL,
    PRIMARY KEY (entity_type, entity_id, field)
);

-- 2. external_id_locked marks entity_enrichment.external_id as an explicit,
--    Admin-pinned durable Enrichment override (Fix-info on a Show/Artist/Album)
--    rather than a transient per-pass auto-resolved id. When set, the enrich pass
--    looks the parent up BY the pinned external_id every pass (New or Full) and
--    never re-searches by name — so the correction survives later passes and
--    rescans, exactly like a leaf's pinned tmdb_id / musicbrainz_id (ADR-0019).
--    Defaults to 0 (auto) so existing rows are unchanged.
ALTER TABLE entity_enrichment ADD COLUMN external_id_locked INTEGER NOT NULL DEFAULT 0;
