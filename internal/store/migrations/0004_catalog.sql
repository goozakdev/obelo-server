-- 0004_catalog: the Movie catalog produced by a scan — Title → Edition → File →
-- Stream — plus per-Library scan status.
--
-- The catalog hangs off a Library (0003). Identity (Title title+year) is derived
-- by the Scanner from local on-disk paths only (ADR-0002): no network, fully
-- deterministic. This slice models the simple case from docs/naming-convention.md:
-- one clean video file → one Title → one Edition → one File with its Streams.
-- Richer identity (multiple Editions, multi-part, extras, needs-review) is 05.
--
-- Everything FK's up to libraries with ON DELETE CASCADE so deleting a Library
-- drops its whole catalog (the cascade DeleteLibrary already relies on, 0003).

-- A Title is the logical media entity a user browses (CONTEXT.md): here a Movie.
-- Identity is (normalized title + year); the Library scopes it. identity_key is
-- the normalized dedup key the scanner computes (case/punct folded + year) so a
-- rescan of the same on-disk movie updates the same row instead of duplicating.
CREATE TABLE IF NOT EXISTS titles (
    id           TEXT PRIMARY KEY,
    library_id   TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    -- kind mirrors the Library kind; only 'movie' in this slice.
    kind         TEXT NOT NULL CHECK (kind IN ('movie')),
    -- title is the display title parsed from the folder/filename; year is the
    -- parsed (YYYY). year is part of identity (Dune 2021 != Dune 1984).
    title        TEXT NOT NULL,
    year         INTEGER,
    -- identity_key = normalized title + "|" + year, unique within a Library so a
    -- rescan re-resolves to the same Title (identity stability, ADR-0014).
    identity_key TEXT NOT NULL,
    -- sort_title is the lower-cased title used for stable, case-insensitive
    -- ordering and as the cursor sort key for sort=title.
    sort_title   TEXT NOT NULL,
    added_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (library_id, identity_key)
);

CREATE INDEX IF NOT EXISTS idx_titles_library ON titles(library_id);
-- Composite indexes backing the two stable sort orders + id tie-break used by
-- cursor pagination (sort key, then id) — no OFFSET.
CREATE INDEX IF NOT EXISTS idx_titles_sort_title ON titles(library_id, sort_title, id);
CREATE INDEX IF NOT EXISTS idx_titles_added       ON titles(library_id, added_at, id);

-- An Edition is a specific version of a Title at a quality/cut (CONTEXT.md). In
-- this slice every Title has exactly one Edition; the label is left simple.
CREATE TABLE IF NOT EXISTS editions (
    id       TEXT PRIMARY KEY,
    title_id TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- name is a human label ("1080p", "Director's Cut"); inferred richly in 05.
    name     TEXT NOT NULL DEFAULT '',
    added_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_editions_title ON editions(title_id);

-- A File is one physical file on disk (CONTEXT.md), carrying the technical
-- attributes ffprobe extracts. One File per Edition in this slice (multi-part is
-- 05). path is the absolute on-disk path; UNIQUE so a rescan updates in place.
CREATE TABLE IF NOT EXISTS files (
    id            TEXT PRIMARY KEY,
    edition_id    TEXT NOT NULL REFERENCES editions(id) ON DELETE CASCADE,
    path          TEXT NOT NULL UNIQUE,
    container     TEXT NOT NULL DEFAULT '',
    -- convenience denormalization of the primary video/audio for summaries; the
    -- authoritative per-stream detail lives in streams.
    video_codec   TEXT NOT NULL DEFAULT '',
    audio_codec   TEXT NOT NULL DEFAULT '',
    width         INTEGER NOT NULL DEFAULT 0,
    height        INTEGER NOT NULL DEFAULT 0,
    -- bitrate in bits/sec, duration in milliseconds, size in bytes.
    bitrate       INTEGER NOT NULL DEFAULT 0,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    added_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_files_edition ON files(edition_id);

-- A Stream is an elementary stream inside a File's container (CONTEXT.md,
-- FFmpeg's sense): video/audio/subtitle. stream_index is ffprobe's index within
-- the container, preserved for stable ordering and later playback selection.
CREATE TABLE IF NOT EXISTS streams (
    id           TEXT PRIMARY KEY,
    file_id      TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    stream_index INTEGER NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('video', 'audio', 'subtitle')),
    codec        TEXT NOT NULL DEFAULT '',
    language     TEXT NOT NULL DEFAULT '',
    -- video geometry (0 for non-video streams).
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    -- audio channel count (0 for non-audio).
    channels     INTEGER NOT NULL DEFAULT 0,
    is_default   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_streams_file ON streams(file_id);

-- Per-Library scan status, the pollable resource behind GET /libraries/{id}/scan
-- (acceptance criteria). One row per Library, created/updated by the scanner.
-- state ∈ idle (never scanned / finished) | running | error. Counts and the
-- error message describe the last/active scan.
CREATE TABLE IF NOT EXISTS scan_status (
    library_id    TEXT PRIMARY KEY REFERENCES libraries(id) ON DELETE CASCADE,
    state         TEXT NOT NULL DEFAULT 'idle' CHECK (state IN ('idle', 'running', 'error')),
    titles_found  INTEGER NOT NULL DEFAULT 0,
    files_found   INTEGER NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    started_at    TEXT,
    finished_at   TEXT
);
