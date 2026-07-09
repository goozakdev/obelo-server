-- 0029_remembered_audio: per-User Remembered audio (ADR-0023). An explicit audio
-- Stream pick is remembered and reapplied on the next play, keyed to what the pick
-- MEANS (normalized language + distinguishing traits — title tag, channel count,
-- commentary disposition), never a stream index, so it survives a re-rip or Edition
-- switch and degrades gracefully when nothing matches (the ADR-0014 spirit, applied
-- to audio).
--
-- Two levels:
--   * title_audio_memory — the pick on the Title it was made on (Movie or Episode).
--   * show_audio_memory   — the Show bubble-up: an Episode's non-commentary pick also
--     becomes the Show default for Episodes without their own pick. This is the FIRST
--     per-(User, Show) state slot (watch state today is per-Title).
--
-- Kept OUT of the watch_state table on purpose: watch_state is rewritten on every
-- progress tick (SaveWatchState), and a memory write is a separate, rarer event — a
-- dedicated table means row-existence IS presence (no "has remembered audio" flag)
-- and the two writers never clobber each other. A rescan touches neither table, so
-- re-scanning a library leaves Remembered audio intact (the memory keys to the stable
-- Title/Show identity row, not to a File or Stream).
--
-- Both cascade on the User (deleting a User drops their memory) and on the
-- Title/Show (a hard delete — e.g. its Library removed — drops the dangling pick);
-- a Title/Show going Missing is a soft-delete, not a row delete, so memory survives
-- it exactly like watch state.

CREATE TABLE IF NOT EXISTS title_audio_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id   TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- The remembered pick's MEANING (ADR-0023), normalized at write time:
    -- language is the ISO-639-1 code ('' = Unknown); label is the embedded title tag
    -- (e.g. "Director's Commentary", '' when untagged); channels is the layout count;
    -- commentary is the ffprobe comment disposition. Re-resolution matches these
    -- against the current File's Streams (exact-trait -> language -> default fallback).
    language   TEXT    NOT NULL DEFAULT '',
    label      TEXT    NOT NULL DEFAULT '',
    channels   INTEGER NOT NULL DEFAULT 0,
    commentary INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, title_id)
);

CREATE TABLE IF NOT EXISTS show_audio_memory (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    show_id    TEXT NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    language   TEXT    NOT NULL DEFAULT '',
    label      TEXT    NOT NULL DEFAULT '',
    channels   INTEGER NOT NULL DEFAULT 0,
    commentary INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (user_id, show_id)
);
