-- 0007_watch_state: per-(User, Title) playback state — resume position and
-- watched/unwatched (issue 08, ADR-0014 identity-keyed watch state).
--
-- Watch state belongs to the User, never to the Title (CONTEXT.md). It is keyed
-- to the Title's STABLE identity row (titles.id), which slice 06 guarantees
-- survives an Edition swap or a file rename/move (the same on-disk movie
-- re-resolves to the same titles row). So watch history follows the Title across
-- those mutations for free: nothing here references an Edition or a File.
--
-- One row per (user, title) — the UNIQUE makes the upsert (ON CONFLICT) a clean
-- last-write-wins on position, the concurrency model the issue calls for (two
-- Devices reporting progress just overwrite each other, no locking/merge).
--
-- Both FKs cascade: deleting a User drops their whole watch history; dropping a
-- Title (e.g. its Library is deleted) drops the state that pointed at it. A
-- Title going Missing (soft-delete) is NOT a delete, so watch state survives it
-- and recovers when the files return — exactly the CONTEXT.md "Missing" promise.
CREATE TABLE IF NOT EXISTS watch_state (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    title_id           TEXT NOT NULL REFERENCES titles(id) ON DELETE CASCADE,
    -- resume_position_ms is where the User left off, in milliseconds. 0 means no
    -- resume offset (either never started, finished/watched, or a stop below the
    -- ~2% floor that we deliberately do not record). A row with watched=1 always
    -- has resume_position_ms=0 (crossing the ceiling clears the resume).
    resume_position_ms INTEGER NOT NULL DEFAULT 0,
    -- watched is the server-applied Watched threshold outcome (~90%), or a manual
    -- override via PUT /titles/{id}/watchState. Clients never compute it.
    watched            INTEGER NOT NULL DEFAULT 0,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (user_id, title_id)
);

-- Continue Watching is "this User's in-progress titles, most-recently-played
-- first": a scan of one User's rows filtered to resume_position_ms > 0 and
-- ordered by updated_at. Index the access path (user, then recency).
CREATE INDEX IF NOT EXISTS idx_watch_state_user ON watch_state(user_id, updated_at);
