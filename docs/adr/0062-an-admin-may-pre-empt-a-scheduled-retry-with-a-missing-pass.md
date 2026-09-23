# An Admin may pre-empt a scheduled retry with a `missing` pass

[ADR-0048](./0048-a-transient-enrichment-failure-is-retried-not-parked.md) gave a
transient provider failure its own lane: it stays `'failed'`, but
`enrichment_retry_at` names when the server will ask again, on the backoff
schedule in `enrich.retryDelay`. That schedule tops out at a day between
attempts. An Admin who fixes the actual cause — a bad API key, a firewall rule,
a provider outage that has since cleared — has no way to say "ask now" for those
rows; the only levers are `new` (which does not see `'failed'` at all until its
retry comes due) and `full` (which re-asks the whole Library, `'matched'` rows
included, at ModeFull's cost).

[ADR-0051](./0051-a-settled-non-answer-is-re-asked-when-the-question-changes.md)'s
`recheck` looks close, but deliberately is not this: it re-asks `'unmatched'` and
a *parked* `'failed'` (no retry scheduled) because a matching improvement makes a
**settled** non-answer wrong. A `'failed'` row with a retry still in the future is
not settled — ADR-0048 calls it "IN-FLIGHT work the server already owns" — and
`recheck`'s whole selling point is that it costs nothing on a healthy Library, so
widening it to swallow in-flight retries would blur the exact line ADR-0048 drew.

## The third mode

`missing` sits between them: every visible `'pending'`, `'unmatched'`, or
`'failed'` item — `'failed'` **regardless of `retry_at`** — excluding only
`'matched'` and `'disabled'`. It is `new` plus `recheck`'s settled non-answers
plus the one population neither takes: a `'failed'` row still waiting out its
backoff. It does not apply ADR-0053's uncorroborated-match doubt; a `matched`
Artist is not missing just because `recheck` would also re-ask it.

|            | `new`      | `recheck`              | `missing`                | `full` |
|------------|------------|-------------------------|---------------------------|--------|
| `pending`  | yes        | yes                      | yes                        | yes    |
| retry due  | yes        | yes                      | yes                        | yes    |
| retry future | no       | no                       | **yes**                    | yes    |
| `unmatched`  | no       | yes                      | yes                        | yes    |
| `failed`, parked | no   | yes                      | yes                        | yes    |
| `matched`  | no         | no (unless doubted, ADR-0053) | no                    | yes    |
| `disabled` | no         | no                       | no                          | yes    |

The name says what it does, not what triggered it: an Admin reaching for this
button is asking "what in this Library has no match yet?", and a scheduled retry
still counts as no match — it is just a slower `pending`.

## Why a new mode and not a shorter retry ceiling

Shortening the backoff schedule would ask a struggling provider more often for
every Library, on every affected item, all the time. This is the opposite: a
manual, one-shot pre-emption of the schedule for the rows that need it, spent
exactly once, by a person who has reason to believe the cause has cleared. The
per-Library pass lock (ADR-0051's amendment) still serializes it with `new`,
`recheck`, and `full`, so it costs no more machinery than the modes already had.

## UI: two row-menu actions

The Library row's actions gain "Refresh missing metadata" (`missing`, starts at
once — it asks only items without a record, so a healthy Library costs nothing,
but during an outage it re-asks every item waiting on a retry) and "Refresh all
metadata" (`full`, behind the confirm dialog Delete uses, because it re-fetches every item
and can take a long time on a real Library). Both are POSTs that return as soon
as the pass is queued, per ADR-0051's amendment: a `started: false` ack means one
was already running and is shown as a message, not silence; a request error
surfaces the same way. Neither the client nor this pass waits for the other to
finish.
