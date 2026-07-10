-- 0033_logo_artwork_role: admit 'logo' as a leaf-Title artwork role.
--
-- TMDB serves title logos (the transparent wordmark art) alongside posters and
-- backdrops; enrichment now fetches one per Movie/Show and the Edit-item dialog
-- gets a Logo picker tab, exactly like Poster/Background. Parent-entity logos
-- (Shows) need no schema change — entity_artwork has no role CHECK — but the
-- leaf `artwork` table pins role to ('poster','background'), so a Movie logo
-- would be rejected at INSERT.
--
-- SQLite can't ALTER a CHECK in place, so rebuild the table preserving every
-- row (same pattern as 0031). defer_foreign_keys defers the artwork→titles FK
-- check to commit, by which point every id is preserved.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE artwork_new (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('poster', 'background', 'logo')),
    path     TEXT NOT NULL,
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    source   TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local', 'fetched', 'uploaded')),
    UNIQUE (title_id, role, source)
);
INSERT INTO artwork_new (id, title_id, role, path, added_at, source)
    SELECT id, title_id, role, path, added_at, source FROM artwork;
DROP TABLE artwork;
ALTER TABLE artwork_new RENAME TO artwork;
CREATE INDEX IF NOT EXISTS idx_artwork_title ON artwork(title_id);
