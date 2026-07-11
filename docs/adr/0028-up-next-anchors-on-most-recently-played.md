# Up Next anchors on the most-recently-played Episode, not the lowest unwatched

Up Next — and the Show detail page's next-episode block, which now renders the
same computation — anchors on a Show's most-recently-**played** Episode and
resumes forward from there: the anchor Episode itself if it is still in progress,
otherwise the next unwatched Episode *after* the anchor in Show order, wrapping to
the first unwatched from the start once the end is reached. A fully-watched Show
has no resume point and drops out. This replaces the previous rule, which always
surfaced the lowest-numbered unwatched Episode.

## Why

A viewer who deliberately skips an Episode or a whole Season they don't care about
must not be nagged to play it every time they open the Show — they should pick up
after where they last played. The lowest-unwatched rule marched them backward into
the skipped gap forever. The wrap ensures a skipped Episode still resurfaces
exactly once, after the viewer reaches the end of the Show.

## The played-vs-touched distinction (`played_at`)

The anchor must follow actual **playback**, not a manual "mark as watched" — if I
mark a later Episode watched to correct my records, that must not move where I
resume. Previously the two were indistinguishable: both the playback progress path
and the manual watched toggle went through one `SaveWatchState` upsert and bumped
one `watch_state.updated_at`, and a manually-marked-watched Episode
(`watched=1, resume=0`) is byte-identical to one played to completion.

So we add a `played_at` column stamped **only** by the playback progress path and
**never** by the manual watched toggle, kept in lockstep with the existing 2%
floor: an Episode has a `played_at` iff it has a resume position or was watched via
playback. The anchor is the Episode with the max `played_at` for the Show. A
marks-only Show has no `played_at`, so it has no anchor and degenerates cleanly to
first-unwatched. `played_at` propagates to multi-episode-file siblings on the
playback path, exactly as resume/watched already do.

Considered and rejected: making the manual toggle skip the `updated_at` bump to
reuse the one column. Rejected because `updated_at` legitimately means "last
modified" (and is Continue Watching's sort key); overloading it to sometimes-not-
update is muddier than a dedicated recency signal.

## Consequences

- Up Next stays a pure computed view — no stored "next Episode" pointer, only the
  new recency column.
- Home's Up Next row shows only Shows whose anchor is already **watched**; a Show
  whose anchor is still in progress belongs to Continue Watching, so the two Home
  rows stay disjoint and never double-list the same Episode.
- The Show detail page renders the same anchor with mode-dependent controls:
  - **Not started** (no watched, no in-progress Episode) → the Show description +
    **Play** (series from the first Episode).
  - **In-progress anchor** → the anchor Episode's S/E, title, and synopsis +
    **Continue** (resume at the stored position) and **Restart** (from 0, which
    resets the stored resume to 0). Restart is a playback action, so it re-stamps
    `played_at` and keeps the anchor on that Episode.
  - **Watched (or absent) anchor** → the next-episode block + **Play**.
  - **Fully watched** → reverts to the Show description with no Play; restarting a
    completed series is not a supported flow.
- Episode ordering for both the forward walk and the wrap reuses the existing Up
  Next order: regular Seasons in S/E order first, Specials (Season 0) deferred to
  the very end.
