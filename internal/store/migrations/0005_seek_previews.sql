-- Seek preview generation (ADR-0070): the per-Library toggle and the record of each
-- File's generated set. Tile sheets themselves live on disk under
-- <data>/seekpreviews/<file id>/ (ADR-0007); this is only what the manifest needs
-- and the change signal that decides when a set is stale.

-- The per-Library Seek preview generation toggle. Only a local Movie or TV Library
-- has one; such a Library with no row is ON (the default), so a row exists only
-- once an Admin has set it.
CREATE TABLE library_seek_previews (
    library_id TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    enabled    INTEGER NOT NULL
);

-- One row per File that has been tried. file_id deliberately has no foreign key:
-- a rescan deletes and re-inserts a changed File's row under the same id, and that
-- must not take its record with it. A row whose File no longer exists is a purged
-- File's leftover; the generator removes it with the set on disk.
--
-- mtime and size_bytes are the File's at generation (or at failure): a File whose
-- row now differs has changed, so its set is stale and a failure is no longer
-- final. status 'failed' keeps reason (one line) and zero geometry.
CREATE TABLE seek_previews (
    file_id      TEXT PRIMARY KEY,
    mtime        TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('ready', 'failed')),
    reason       TEXT NOT NULL DEFAULT '',
    interval_ms  INTEGER NOT NULL DEFAULT 0,
    frame_width  INTEGER NOT NULL DEFAULT 0,
    frame_height INTEGER NOT NULL DEFAULT 0,
    grid_columns INTEGER NOT NULL DEFAULT 0,
    grid_rows    INTEGER NOT NULL DEFAULT 0,
    frame_count  INTEGER NOT NULL DEFAULT 0,
    sheet_count  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);
