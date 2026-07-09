-- 0026_subtitle_tracks: the read-path foundation for Subtitle tracks
-- (.scratch/subtitles/, ADR-0020/0021). A Subtitle track is the client-facing,
-- selectable subtitle a viewer turns on — a source-tagged union over three
-- sources: an embedded subtitle Stream, a Sidecar subtitle, and a Fetched
-- subtitle (CONTEXT.md). This migration adds what the model was missing.

-- 1. Embedded subtitle Streams stay FFmpeg-pure and keep living in `streams`;
--    they only lacked the `forced` disposition (ffprobe already exposes it, the
--    scanner just dropped it — `default` was already captured as is_default).
--    A constant-default column, so existing rows backfill to 0 (not forced) and
--    a rescan captures the real value. No table rebuild needed.
ALTER TABLE streams ADD COLUMN forced INTEGER NOT NULL DEFAULT 0;

-- 2. `subtitles` is the persisted Subtitle-track model for the NON-stream
--    sources — Sidecar subtitles (this slice) and Fetched subtitles (slice 05).
--    Embedded tracks are projected from `streams` at read time, so they are not
--    duplicated here (and existing embedded-stream rows are preserved untouched).
--    The `source` discriminator mirrors artwork's 'local'|'fetched' pattern
--    (0010): a rescan rewrites only local (`sidecar`) rows while a `fetched` row
--    survives, and a local source outranks fetched at serve time. Title-scoped
--    (like artwork) so it survives the File rebuild a rescan performs and follows
--    a Title through a Match override (ADR-0014).
CREATE TABLE IF NOT EXISTS subtitles (
    id           TEXT PRIMARY KEY,
    title_id     TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- 'sidecar' = a subtitle file discovered next to the media; 'fetched' = an
    -- externally downloaded subtitle cached under the data dir (slice 05).
    source       TEXT NOT NULL CHECK (source IN ('sidecar', 'fetched')),
    -- 'text' subs are selectable/WebVTT-convertible; 'image' subs (PGS/VOBSUB)
    -- burn in on transcode (ADR-0020).
    kind         TEXT NOT NULL CHECK (kind IN ('text', 'image')),
    -- language is normalized to ISO-639-1 by the scanner ('' = Unknown).
    language     TEXT NOT NULL DEFAULT '',
    forced       INTEGER NOT NULL DEFAULT 0,
    is_default   INTEGER NOT NULL DEFAULT 0,
    -- codec/format token (srt/ass/vtt for text; vobsub/sup for image) — drives
    -- the WebVTT conversion (slice 02) and burn-in (slice 04) paths.
    codec        TEXT NOT NULL DEFAULT '',
    -- on-disk path: the sidecar file, or the cached fetched file. Never a library
    -- write for fetched (ADR-0021).
    path         TEXT NOT NULL DEFAULT '',
    -- provider candidate id for a fetched pick-lock (slice 05); '' otherwise.
    provider_id  TEXT NOT NULL DEFAULT '',
    added_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_subtitles_title ON subtitles(title_id);
