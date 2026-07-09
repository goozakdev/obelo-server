-- 0006_incremental_rescan: make rescans incremental and safe for ongoing library
-- maintenance (issue 06, ADR-0008 incremental scan + soft-delete of Missing,
-- ADR-0014 identity stability + Match overrides keyed to the folder path).
--
-- Three additions, all preserving 0004/0005 data (constant defaults, so ALTER
-- TABLE ADD COLUMN is safe under SQLite):
--
--   * files.mtime / files.present — per-file change-detection + soft-delete.
--     mtime (RFC3339 UTC) and the existing size_bytes are the cheap change
--     signal: an incremental scan re-ffprobes a File only when its mtime/size
--     differ from what is stored. present=0 marks a File Missing (absent from
--     disk) instead of deleting it — an unmounted drive must not wipe the
--     catalog (ADR-0008). Re-adding the file flips it back to present=1.
--
--   * titles.hidden — derived all-Files-Missing state. A Title whose every File
--     is Missing is hidden from browse (excluded from ListTitles) but still
--     fetchable by id so its state is recoverable when the files return. The
--     scanner recomputes this after each scan; it is a cache of "all files
--     present=0", not an independent flag.
--
--   * match_overrides — an Admin identity correction (fix-match) keyed to the
--     FOLDER PATH (its physical anchor, ADR-0014). It overrules the
--     convention-derived guess and PERSISTS across rescans. Renaming/moving the
--     folder leaves the override with no matching folder on disk: the next scan
--     marks it orphaned (not silently lost), surfaced in the Admin attention
--     list alongside needs-review / Unmatched.

-- Per-file change-detection + soft-delete state.
ALTER TABLE files ADD COLUMN mtime   TEXT    NOT NULL DEFAULT '';
ALTER TABLE files ADD COLUMN present INTEGER NOT NULL DEFAULT 1;

-- Derived "all Files Missing" state for a Title (hidden from browse, recoverable).
ALTER TABLE titles ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;

-- Partial index so the browse list can cheaply exclude hidden Titles.
CREATE INDEX IF NOT EXISTS idx_titles_hidden ON titles(library_id, hidden);

-- Match overrides: an app-managed identity correction keyed to the on-disk
-- folder path (ADR-0002, ADR-0014). One override per (library, folder). The
-- corrected identity mirrors the columns the scanner would otherwise derive:
-- a corrected title+year, or an embedded-style external id, collapsed into the
-- identity_key the catalog dedups on. orphaned=1 when the folder no longer
-- exists on disk (surfaced, never silently dropped).
CREATE TABLE IF NOT EXISTS match_overrides (
    id            TEXT PRIMARY KEY,
    library_id    TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- folder_path is the absolute on-disk folder the override anchors to. For a
    -- bare-file Title the anchor is the file's own path.
    folder_path   TEXT NOT NULL,
    -- The corrected identity the scanner must use instead of its parse.
    title         TEXT NOT NULL,
    year          INTEGER,
    tmdb_id       TEXT NOT NULL DEFAULT '',
    imdb_id       TEXT NOT NULL DEFAULT '',
    identity_key  TEXT NOT NULL,
    -- orphaned=1 once a scan finds no folder at folder_path (the user renamed/
    -- moved it). Surfaced in the Admin attention list.
    orphaned      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (library_id, folder_path)
);

CREATE INDEX IF NOT EXISTS idx_match_overrides_library ON match_overrides(library_id);
