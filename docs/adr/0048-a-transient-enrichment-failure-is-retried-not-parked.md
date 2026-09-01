# A transient enrichment failure is retried, not parked

An Enrichment pass asks a provider one question — *what is the record for this
item?* — and used to write down two different answers in the same box.

"There is no such record" is a real answer. It ends the matter; only a human
choosing a record changes it, which is why an unmatched Title goes on the Admin's
attention list and waits there.

"I could not reach the provider" is not an answer at all. Nothing was learned
about the item. But it was recorded as `enrichment_status = 'failed'`, and the
only-new pass — the one every scan and every scheduled sweep runs — selects
`WHERE enrichment_status = 'pending'`. So the row was never looked at again.

**A provider blip permanently un-enriched whatever the pass happened to be
holding**, until an Admin thought to run a full pass by hand.

## The gap between what the schema said and what the code did

Migration 0010 introduced the status vocabulary with this comment:

```
--   failed    — the provider errored (transient; retried on the next pass)
```

The retry was never implemented. The selection query has only ever matched
`'pending'`. Nothing failed loudly: the counts a pass reports were correct, the
Titles browsed fine (sparse), and they appeared on the attention list, which made
the outcome look deliberate — as if the server were asking for help rather than
having quietly given up. The comment describes this ADR; it took five years of
migrations to become true.

## The two questions a failure row has to answer

The fix is not a sixth status. `'failed'` already says what happened; what the
schema could not express is *what happens next*. Those are independent, and
conflating them is what produced a status value whose documented meaning and
actual behavior disagreed.

So the status is unchanged and a nullable-by-convention `enrichment_retry_at`
carries the second fact:

| | `retry_at = ''` | `retry_at = <instant>` |
|---|---|---|
| What happened | the provider gave a definitive refusal | the provider could not be reached |
| Who acts next | the Admin | the server |
| On the attention list | yes | not until it escalates |
| Picked up by an only-new pass | no | once the instant has passed |

Keeping it out of the status is also what makes the migration additive: the
`CHECK` constraint on `titles.enrichment_status` does not move, so nothing rebuilds
the largest table in the database to add a retry.

## Which failures are transient

The line is *does this error describe the provider, or does it describe our
request?* — because only the first has any chance of changing on its own.

- **Transient**: a failed round-trip (DNS, refused connection, TLS, timeout, a
  cancelled context), HTTP 408, 429 and every 5xx, and a response body that will
  not parse. None of these is a statement about the item.
- **Permanent**: 400, 401, 403, 422 — a malformed query or a credential the
  provider rejects. Retrying re-sends the same bad request. Only the Admin can fix
  a wrong key, and the attention list is how they find out.
- **Not a failure at all**: 404 and `ErrNoMatch`, which the providers already map
  to `'unmatched'`. The provider answered.

**The marker is opt-in.** An error carrying no classification is treated as
permanent and parks the item exactly as before. A failure mode nobody has thought
about therefore degrades to the old, *visible* behavior rather than to a silent
retry loop — the safe direction to be wrong in.

`ErrTransient` is matched, not concatenated: `transientError` implements `Is` and
leaves `Error()` alone, so the log still reads `enrich: tmdb /movie/550: status
503` and not that sentence wrapped in another one explaining the retry machinery.

## Decisions

- **There is no attempt cap.** "We could not reach the provider" never becomes
  evidence about the item, however many times it is said. The backoff climbs
  5m → 15m → 1h → 3h → 12h → 24h and then holds, so a provider that stays broken
  costs one call per item per day rather than one per pass.

- **The waits are a floor, not a schedule.** Nothing wakes up to run a retry. A
  pass runs when a scan finishes or the sweep ticks and picks up whatever has come
  due since. With a sweep interval longer than an early step, that step simply
  never binds — which is correct, and why the schedule needs no coordination with
  the sweep interval.

- **Six consecutive failures escalate to the attention list, without stopping the
  retries.** Trading "parked forever, visible" for "retrying forever, invisible"
  would not be a fix. `store.EnrichRetryEscalateAfter` is the length of the backoff
  schedule by construction: an item is surfaced exactly when its trouble has
  outlived every timescale a blip plausibly lasts. If the cause then clears on its
  own, the next pass settles it and it leaves the list with nobody having touched
  it.

- **A transient failure below that threshold is deliberately NOT on the attention
  list.** It is in-flight work. Listing it would fill the Needs-Fixing queue with
  rows that clear themselves before anyone reads them — and the queue's whole
  value is that everything on it is something a human must do.

- **Any settled outcome resets the streak to zero.** A Title that fails five
  times, recovers, and fails again months later is at the start of a new streak,
  not one step from the ceiling. One SQL fragment, `clearEnrichmentRetry`, is
  included by every statement that settles a row or hands it back as `'pending'`,
  so exactly one rule governs when a streak survives: only a transient failure
  keeps it.

- **Parents carry the same bookkeeping as leaves.** This is where the old
  behavior did the most damage. `enrichParent` returns early for any parent that is
  not `'pending'`, so a single 503 on a Show parked it *and* left every Season and
  Episode beneath it un-enriched — with nothing on any list saying why, because a
  parent is not a Title and never reaches `TitlesNeedingMatch`.

- **A cancelled context is transient.** It is how a pass ends at shutdown, and
  marking whatever the pass was holding as permanently failed meant a restart
  during a large pass silently parked it.

- **An unparseable `retry_at` counts as due.** It can only be a corrupted write,
  and the two ways to be wrong are not symmetric: retrying early costs one call,
  never retrying strands the item invisibly — a row with a retry scheduled is kept
  off the attention list until it escalates.

- **The clock is injectable on the Service** (`SetClock`) and the store only
  *compares* timestamps handed to it. The pass owns "now", so the SQL selection and
  the in-Go `shouldProcessLeaf` gate — the Movie path queries for its leaves, the
  TV/Music paths walk their parent trees — cannot disagree about what is due.

- **`Retrying` is reported apart from `Failed`** in the pass Result, the SSE
  progress event and the enrich response. The two need different words in front of
  an operator: "8 failed" invites them to go and fix eight things, "8 will be
  retried" tells them to wait. Collapsed into one counter, the fix would be
  invisible on every surface that reports a pass.

## The migration hands every stranded row one retry

Every `'failed'` row in an existing database was parked by a pass that could not
tell an outage from a refusal, so the population is a mix and the server cannot
now say which was which. Migration 0053 schedules all of them for one immediate
retry (`retry_at` = a past sentinel, `attempts` = 0).

That re-asks the provider the question exactly once. A genuinely transient failure
resolves on the next pass; a genuinely permanent one fails again and parks itself
properly under the new rules. The alternative — leaving them alone — would apply
the fix only to failures occurring after the upgrade and leave the existing
attention list full of items nobody was ever going to retry.

## What was considered and rejected

- **Make the only-new pass select `'failed'` too.** One line, and it retries a
  rejected API key on every scan for every Title in the library, forever, with no
  backoff. It also destroys the attention list: a permanent failure would be
  indistinguishable from a transient one, so either everything is listed or
  nothing is.

- **A `'retrying'` status value.** Cleaner to read, but it rewrites the `CHECK`
  constraint on `titles.enrichment_status`, which in SQLite means rebuilding the
  table every other table points at. And it is the wrong shape: it answers "what
  happens next" in a column whose job is "what happened", so the two facts could
  never be set independently.

- **Retry inside the pass instead of across passes.** MusicBrainz already does
  this for a 503 (`getJSON`, four attempts), and it is the right tool for a
  sub-second throttle. It cannot help with an outage that outlasts the pass, and
  extending it would turn one slow provider into an arbitrarily long pass holding
  a per-Library lock.

- **A cap after which the item parks.** It reintroduces the original bug on a
  delay, and puts the server in the position of concluding something about an item
  from evidence that is entirely about the network.

## Consequences

- **A pass now makes provider calls for items it previously skipped.** A library
  with a large backlog of old `'failed'` rows will, on the first pass after the
  upgrade, re-ask the provider about all of them. That is the intended one-time
  cost of the backfill; steady state is bounded by the backoff.

- **`enrichment_status = 'failed'` no longer means "on the attention list".**
  Anything reading that column directly to decide whether an item needs a human
  must also read `enrichment_retry_at`. `TitlesNeedingMatch` is the one place that
  encodes the rule, and callers should keep going through it.

- **Artwork fetches are NOT covered.** A transient failure downloading a poster is
  swallowed by `cacheArtwork` and the Title is still recorded `'matched'`, so it
  keeps whatever artwork it had — permanently, until a full pass. That is the same
  class of bug in a different place, and it needs a different fix (the item is
  settled, not failed); it is deliberately out of scope here.

- **The retry state is per-item, so a library-wide outage schedules N retries
  rather than one back-off for the provider.** Acceptable at the scale a
  self-hosted server runs at, and it keeps the mechanism free of any cross-item
  coordination — but a provider-level circuit breaker is the natural next step if
  the per-item call volume during a long outage ever matters.
