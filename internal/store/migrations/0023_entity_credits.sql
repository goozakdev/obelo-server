-- 0023_entity_credits: give a browse-PARENT entity (a Show now, an Artist later)
-- an ordered cast — the cast counterpart to entity_genres (0011) — so TV shows can
-- carry the same headshot cast strip movies do (cast-photos/02). Enrichment now
-- requests TMDB TV credits and captures the series main cast; this table is its
-- home. A leaf Movie/Episode's cast is UNTOUCHED — it stays in title_credits (0010,
-- keyed by title_id); this holds only a non-Title parent's cast, keyed by
-- (entity_type, entity_id) exactly as entity_genres is.
--
-- Each person_ref links a credit to the person's headshot in the EXISTING generic
-- entity_artwork table under the `person` entity type (0011/0022) — so an actor in
-- both a movie and a show shares ONE cached photo / one row (cross-kind dedupe). No
-- new image table or serve route is introduced.
--
-- Purely additive (a new table), never touches identity (identity_key / watch
-- state), and leaves every existing row byte-for-byte intact (ADR-0002). Rebuilt
-- wholesale on each unlocked parent enrich (WriteEntityEnrichment), like genres, so
-- no backfill is needed — a pre-cast-photos Show simply has no rows until re-enrich.
CREATE TABLE IF NOT EXISTS entity_credits (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    person_ref  TEXT NOT NULL DEFAULT '',
    person      TEXT NOT NULL,
    character   TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'cast',
    ord         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_entity_credits ON entity_credits(entity_type, entity_id);
