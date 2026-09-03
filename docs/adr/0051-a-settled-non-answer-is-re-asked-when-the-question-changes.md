# A settled non-answer is re-asked when the question changes

Seven slices landed that change how a Track resolves — an album's tracklist became a
resolution tier, the pass's search stopped being an exact phrase, and a matching rule
replaced a positional assumption. The operator did the obvious thing and rescanned
their library.

**Nothing moved.** 730 flagged tracks became 722. Every one of the 722 carried a blank
`enrichment_reason`, which is the proof: a reason is written when a leaf is *processed
and settles*, so a blank one means no leaf was processed at all. Not one of the new
code paths ran.

## Why the improvement was unreachable

`shouldProcessLeaf` admits two populations in `ModeNew`: `pending` leaves, and `failed`
leaves whose retry has come due. [ADR-0048](./0048-a-transient-enrichment-failure-is-retried-not-parked.md)
built that retry for **transient** failures — a 503, an unreachable host — and
deliberately excluded settled ones. `store.TitlesNeedingMatch` states the reasoning in
its own comment:

> 'unmatched' — the provider answered and had no record. **Nothing but a human will
> change that.**

That was true when it was written. This work made it false. A better matcher changes it
too, and nothing in the system re-asks.

The one existing lever is `ModeFull`, which re-resolves everything: on the library in
question, ~11,600 provider requests — 292 artists, 822 albums, 10,550 tracks — to
re-ask **722** questions, against the endpoint ADR-0049 documented shedding load
globally. Three hours of traffic for fifteen minutes of work.

## And no operator can press it anyway

`POST /libraries/{id}/enrich` has **no client method in the web app at all**. Nothing in
`web/src/` calls it. The only way an Admin causes a pass is by scanning, which fires
`enqueueEnrichAfterScan` → `ModeNew` — the mode that cannot see their problem. So the
remedy for a stale queue was reachable only by hand-issuing an HTTP request, and the
one action the UI does offer is the one that provably changes nothing.

A mode nobody can invoke is not a remedy. That is why this ADR covers a button as well
as a mode.

## Decisions

- **A third mode, `ModeRecheck`, re-asks exactly the settled non-answers**: leaves that
  are `unmatched`, plus `failed` leaves that are parked (no scheduled retry). It also
  includes everything `ModeNew` would have taken, because a pass that skipped new work
  to redo old work would be a strange thing to hand an operator.

- **It re-asks PARENTS, not only leaves.** Half of the motivating queue — 365 of 730
  tracks — sits under an album that is itself unmatched, and no amount of re-asking the
  track fixes that. `enrichParent`'s early return therefore admits the same two settled
  populations. A parent that already matched still short-circuits to its stored id, so
  a recheck costs nothing for the albums that are fine.

- **It re-asks; it does not reset.** No status is lowered to `pending`, nothing is
  cleared in advance. An item that is still unmatched afterwards is written as
  unmatched again, with a fresh `enrichment_reason` — which is what makes the reason
  column the honest record of the most recent attempt rather than of the first one.

- **An Admin's choice is never re-asked.** A locked or chosen record resolves by its
  pinned id exactly as it does in every other mode (ADR-0045/0046). Recheck changes
  which items are *visited*, never the precedence applied when they are.

- **The Movie SQL and the TV/Music walks must agree.** `store.TitlesForEnrichment`
  selects the Movie path's leaves with a query while `collectTVLeaves` /
  `collectMusicLeaves` walk their parent trees; `shouldProcessLeaf`'s comment already
  names them twins that must agree on what "due" means. A third mode is a third chance
  for them to drift, so they change together.

- **It is reachable from the Needs Fixing screen**, which is where the operator is
  already looking at the rows in question, and it reports what it did. The count of
  rows that cleared is the only feedback that distinguishes "the improvement did not
  apply to my library" from "the improvement never ran".

## What was considered and rejected

- **Lower `unmatched` to `pending` on upgrade.** A data migration that rewrites state to
  mean something it does not mean, firing on every deploy whether or not anything about
  matching changed, and destroying the distinction ADR-0048 exists to preserve.

- **Make `ModeNew` include `unmatched`.** Then every scheduled scan re-asks every
  permanent non-answer forever — an unbounded standing cost on a rate-limited host, to
  catch an improvement that lands a few times a year. The population this ADR cares
  about is *stale relative to a code change*, and a scan is not a code change.

- **Stamp a matcher version on every row and re-ask when it moves.** Precise, and it
  makes the trigger automatic. But it is per-row bookkeeping, a value someone must
  remember to bump, and a silent no-op when they forget — a lot of machinery to avoid a
  button.

- **Rely on `ModeFull`.** It is correct and it is what exists. It costs 16x the requests
  for this library, and the ratio gets worse as a library grows, which is precisely
  backwards.

## Consequences

- **The cost of adopting a matching improvement drops from the size of the library to
  the size of the problem** — on the motivating library, ~800 requests instead of
  ~11,600.

- **A blank `enrichment_reason` on a settled row now means "not visited since ADR-0050
  landed"**, and is the first thing to check when an improvement appears not to work. It
  was the diagnosis here.

- **Recheck can make the queue longer.** An item that re-asks and fails again is
  unchanged; an item whose *parent* newly matches may surface tracks that were
  previously invisible behind an unmatched album. That is honest, and it is the same
  arithmetic `mapAlbumTracks` already applies.

- **The web app gains its first enrichment-pass trigger.** Everything else about passes
  — scheduling, the auto-after-scan hook, `ModeFull` — stays exactly as it is; this adds
  one deliberate action, not a scheduler.

## Amendment: a pass is STARTED, never awaited (2026-09-02)

The first implementation of the button got this wrong, and it is worth recording why
rather than quietly fixing it.

`POST /libraries/{id}/enrich` ran the pass synchronously inside the request
(`EnrichLibraryProgress(r.Context(), …)`), and its doc comment said so. That was
harmless while the only caller was a human with curl. Pointing a browser at it made the
defect immediate: the recheck this ADR exists to enable is a *fifteen-minute* pass, the
fetch simply hung, and the operator — seeing nothing happen — reloaded the page. The
reload aborted the fetch, cancelled `r.Context()`, and killed the pass. Every leaf's
`enrichment_reason` was still blank afterwards, which is how we know not one item had
been processed.

So: **a pass is started, not awaited.** The endpoint enqueues onto the background enrich
worker that `App` already runs — the same worker the auto-after-scan trigger and
`enqueueEnrichFull` use, which already takes a mode — and returns immediately. Progress
arrives on the `enrichProgress` SSE stream that already exists, and `GET
/libraries/{id}/enrich` answers whether a pass is running, so a reloaded page rejoins it
instead of losing it.

This is not a new pattern. It is the one `handleScan` has always used —
`StartScan(context.Background(), …)`, return at once, progress over SSE, a pollable
`GET`. Enrichment had the worker and the event stream already; only the endpoint was
holding the request open. **A long-running job must not be tied to the lifetime of the
request that asked for it**, and the tell that this rule has been broken is an operator
being told, in effect, not to navigate away.

One consequence stands unfixed and is deliberately out of scope here: `collectMusicLeaves`
re-asks every unmatched PARENT before any leaf settles, so the first minutes of a recheck
produce no leaf-level progress even when everything is working. Honest progress reporting
makes that visible rather than mysterious, which is enough for now.
