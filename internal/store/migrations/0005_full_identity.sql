-- 0005_full_identity: deepen the Movie catalog to the full naming convention
-- (issue 05, docs/naming-convention.md). Builds on 0004's Title→Edition→File→
-- Stream. Everything still FK's up to libraries with ON DELETE CASCADE so
-- deleting a Library drops its whole catalog including the new attention rows.
--
-- New identity surface, all derived locally/offline (ADR-0002):
--   * Embedded external id ({tmdb-…}/{imdb-…}) recorded on the Title and used as
--     its identity key (the scanner computes the key; these columns are the
--     human-visible record + Enrichment hook, never an external fetch here).
--   * needs_review: a partial best-effort parse (e.g. a yearless movie) is filed
--     and flagged for the Admin attention surface (CONTEXT.md "Needs-review").
--   * ambiguous: two files in one folder parsed to the SAME Edition identity and
--     are not parts — surfaced, never silently picked (collision rule).
--   * extras: recognized clips (subfolder/suffix) attached to the parent Title,
--     never browsable Titles (CONTEXT.md).
--   * artwork: local poster/background associated with a Title (local wins; no
--     external enrichment in this slice, ADR-0001).
--   * unmatched_files: a recognized-media File with no extractable identity —
--     the Admin Unmatched list, manually matchable later, never auto-guessed
--     (CONTEXT.md "Unmatched"). Nothing recognized is silently dropped.

-- SQLite can't add a column with a non-constant default, but these defaults are
-- constant, so ALTER TABLE ADD COLUMN is fine and preserves 0004 data.
ALTER TABLE titles ADD COLUMN tmdb_id      TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN imdb_id      TEXT    NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN needs_review INTEGER NOT NULL DEFAULT 0;
ALTER TABLE titles ADD COLUMN ambiguous    INTEGER NOT NULL DEFAULT 0;

-- Partial index for the attention surface (titles flagged for review/ambiguous).
CREATE INDEX IF NOT EXISTS idx_titles_needs_review
    ON titles(library_id, needs_review, ambiguous);

-- Extras attach to a Title with an extra-type. They carry a path + light
-- technical attributes (probed like a File) but are deliberately NOT files rows
-- and NOT browsable — they never appear in the titles list.
CREATE TABLE IF NOT EXISTS extras (
    id          TEXT PRIMARY KEY,
    title_id    TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- extra_type ∈ trailer | behindthescenes | deleted | featurette | interview
    --            | short | scene | clip | other (naming-convention.md).
    extra_type  TEXT NOT NULL DEFAULT 'other',
    path        TEXT NOT NULL UNIQUE,
    container   TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    added_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_extras_title ON extras(title_id);

-- Local artwork associated with a Title. role ∈ poster | background. path is the
-- absolute on-disk image path; the API serves bytes from it (local wins).
CREATE TABLE IF NOT EXISTS artwork (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('poster', 'background')),
    path     TEXT NOT NULL,
    added_at TEXT NOT NULL DEFAULT (datetime('now')),
    -- One artwork per role per Title (local file is replaced in place on rescan).
    UNIQUE (title_id, role)
);

CREATE INDEX IF NOT EXISTS idx_artwork_title ON artwork(title_id);

-- The Unmatched list: a recognized-media File from which no minimal identity
-- could be extracted (CONTEXT.md "Unmatched"). Scoped to a Library so the Admin
-- attention surface lists per-Library. reason is a short human note (why it
-- couldn't be matched). Not a Title; never auto-guessed.
CREATE TABLE IF NOT EXISTS unmatched_files (
    id         TEXT PRIMARY KEY,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    path       TEXT NOT NULL UNIQUE,
    reason     TEXT NOT NULL DEFAULT '',
    added_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_unmatched_library ON unmatched_files(library_id);
