-- 0034_watch_state_played_at: add the played-recency signal to watch_state
-- (ADR-0028, up-next-resume-point/01).
--
-- played_at is the timestamp of the User's most recent PLAYBACK of a Title. It is
-- stamped ONLY by the playback progress path and NEVER by the manual mark-watched
-- toggle (PUT /titles/{id}/watchState), so a bookkeeping mark cannot move where the
-- viewer resumes. It is the recency signal the Up Next resume point anchors on: a
-- started Show's anchor is the Episode with the greatest played_at, and Up Next
-- resumes forward from there (ADR-0028). An Episode has a played_at iff it has a
-- resume position or was watched VIA PLAYBACK; a marks-only Show has no played_at,
-- so it has no anchor and degenerates cleanly to first-unwatched.
--
-- NULL means "never played" — deliberately distinct from updated_at, which every
-- write (a manual mark included) bumps and which remains Continue Watching's
-- most-recently-played sort key (the rejected alternative in ADR-0028 was to
-- overload updated_at itself).
ALTER TABLE watch_state ADD COLUMN played_at TEXT;

-- Backfill: seed played_at from updated_at for every row that already carries
-- evidence of playback — a live resume position OR a watched flag. The
-- played-vs-marked distinction is unrecoverable for pre-migration rows (a manual
-- mark and a play-to-completion are byte-identical here), so this heuristic may
-- over-attribute a historical manual mark as a play; per ADR-0028 that only affects
-- pre-migration rows and self-heals on the next real playback, which re-stamps
-- played_at exactly.
UPDATE watch_state
   SET played_at = updated_at
 WHERE resume_position_ms > 0 OR watched = 1;
