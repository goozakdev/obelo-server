-- 0003_libraries: the Library entity and its root-folder mapping.
--
-- A Library is a top-level collection of media of a single kind (CONTEXT.md),
-- backed by one or more root folders on disk; multiple roots merge into one
-- logical Library. Each root folder is owned by exactly one Library so a File's
-- identity is unambiguous (ADR-0002). No catalog rows yet — scanning lands in a
-- later slice; this slice only manages the Library entity and its folders.

CREATE TABLE IF NOT EXISTS libraries (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    -- kind is the media kind the Library holds. Modeled explicitly so later
    -- slices add 'tv'/'music'; only 'movie' is valid in this slice.
    kind       TEXT NOT NULL CHECK (kind IN ('movie')),
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One row per root folder. path is stored already normalized (cleaned, absolute)
-- by the library domain so the uniqueness constraint and overlap checks compare
-- canonical paths. Deleting a Library removes its folders (and its — currently
-- empty — catalog) via cascade. A folder belongs to exactly one Library: the
-- UNIQUE(path) guards exact duplicates; parent/child overlap across Libraries is
-- enforced in the domain layer before insert (a pure-SQL check can't express it).
CREATE TABLE IF NOT EXISTS library_roots (
    id         TEXT PRIMARY KEY,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_library_roots_library ON library_roots(library_id);
