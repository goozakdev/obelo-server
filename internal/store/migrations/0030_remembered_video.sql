-- 0030_remembered_video: per-User Remembered video (ADR-0025, ADR-0023 mirrored).
-- An explicit video Stream pick is remembered and reapplied on the next play, keyed
-- to what the pick MEANS (the embedded title tag — e.g. "Black & White"/"Colour" —
-- falling back to the resolution/codec traits), never a stream index, so it survives
-- a re-rip or remux that shuffled stream order and degrades gracefully when nothing
-- matches (the ADR-0014 spirit, applied to video, exactly as 0029 did for audio).
--
-- Two levels, the direct mirror of Remembered audio:
--   * title_video_memory — the pick on the Title it was made on (Movie or Episode).
--   * show_video_memory   — the Show bubble-up: an Episode's pick also becomes the
--     Show default for Episodes without their own pick, reusing the per-(User, Show)
--     slot 0029 introduced. A video pick has no commentary analogue, so — unlike
--     audio — nothing quarantines it: every Episode pick bubbles up.
--
-- Kept OUT of the watch_state table for the same reasons as audio memory: watch_state
-- is rewritten on every progress tick, a memory write is a separate rarer event, and a
-- dedicated table means row-existence IS presence (no "has remembered video" flag) so
-- the two writers never clobber each other. A rescan touches neither table, so
-- re-scanning a library leaves Remembered video intact (the memory keys to the stable
-- Title/Show identity row, not to a File or Stream).
--
-- Both cascade on the User (deleting a User drops their memory) and on the
-- Title/Show (a hard delete drops the dangling pick); a Title/Show going Missing is a
-- soft-delete, not a row delete, so memory survives it exactly like watch state.

CREATE TABLE IF NOT EXISTS title_video_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- The remembered pick's MEANING (ADR-0025), captured at write time: label is the
    -- embedded video title tag (e.g. "Black & White", '' when untagged — then the
    -- resolution/codec traits carry the meaning); codec is the normalized video codec
    -- (lowercased, e.g. "h264"); width/height are the resolution. Re-resolution matches
    -- these against the current File's video Streams (exact-trait -> label -> fallback).
    label      TEXT    NOT NULL DEFAULT '',
    codec      TEXT    NOT NULL DEFAULT '',
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, title_id)
);

CREATE TABLE IF NOT EXISTS show_video_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    show_id    TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    label      TEXT    NOT NULL DEFAULT '',
    codec      TEXT    NOT NULL DEFAULT '',
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, show_id)
);
