# A record a Cascade wrote is the parent's choice, not the child's

[ADR-0045](./0045-enrichment-record-is-its-own-column.md) split a Title's
**identity** from the **record that decorates it**, and gave the second one a flag
saying it was *chosen* rather than *derived by a pass*. It listed the three ways
an Admin chooses — Fix info, an Episode pin, a **Cascade** — as one class, which
is right at the level it was deciding.

This ADR splits that class in two, because "chosen" turns out to have two owners:

- **Chosen** — the Admin opened *this item* and re-pointed *this item*: Fix info
  or Wrong item on the leaf, an Episode pin on the Slot, Fix info on the Album.
  The choice is the item's own.
- **Cascaded** — the Admin corrected a **parent** and ticked "apply to children".
  The child was never looked at; it was *mapped* under the parent's record,
  positionally or by title. The choice is the **parent's**, held by the child.

**A cascaded record is the parent's.** It follows the parent: the next Cascade
from that parent re-applies over it. A directly-chosen record is the child's and
outranks any Cascade, exactly as before.

## Why the child cannot own it

- **The Cascade's own skip rule says so.** CONTEXT.md **Cascade**: "a child's own
  Enrichment override or Locked field wins". That rule exists to settle a contest
  between *two* owners — the child's correction against the parent's. A value the
  Cascade itself wrote has the same owner as the Cascade, so counting it as "the
  child's own" makes the rule protect the parent's correction *from the parent*,
  which is not a contest and not what the sentence is about.
- **The same entry's second half is being broken today.** "Children that don't
  line up are surfaced in the Needs-Fixing queue, never silently changed" is a
  promise that a Cascade never leaves the Admin guessing. Reading a cascaded
  record as the child's own turns the *second* Cascade from a parent into
  `Updated: 0` on precisely the children the first one fixed — silently not
  changed, with nothing surfaced. The failure is the one the entry rules out,
  arrived at from the other side.
- **ADR-0019 already grades corrections by blast radius**, and is explicit about
  which ones are per-item: Fix label "is written as a Locked field, per-item, and
  never cascades". A Cascade is the one correction defined as *not* per-item. Its
  children are "re-resolve[d] ... under the applied parent" — a derivation of the
  parent's record, not a decision about the child.
- **CONTEXT.md Enrichment override** is "an Admin's correction of which external
  provider record **an entity** is enriched from ... so the Admin re-points *it*".
  On a cascaded child nobody re-pointed the child.

## What does not change

A cascaded record is still **durable**, which is the property ADR-0045 and
ADR-0019 ("each mapped child gets a durable per-child Enrichment override")
actually needed from the flag. An enrichment pass or a rescan still resolves the
child BY the cascaded id and never re-auto-matches it, and the Edit-item screen
still reports it as the active override. Only a Cascade *from the same parent*
may overwrite it — which is the parent revising its own decision.

So the answer is not "cascaded records are weaker". It is that "durable" and
"whose choice" were two different questions being answered by one bit.

## The shape: one column that says whose, not two that say what

`titles.enrichment_id_locked` and `entity_enrichment.external_id_locked` are
replaced by `titles.enrichment_id_origin` and
`entity_enrichment.external_id_origin`, each holding one of three values:

| value        | meaning                                              |
|--------------|------------------------------------------------------|
| `''`         | nobody chose it — an enrichment pass resolved it, a split sibling inherited it, a cleared pin wrote it back |
| `'chosen'`   | the Admin chose it **on this item**                  |
| `'cascaded'` | a parent's Cascade applied it **to** this item       |

The old boolean is a strict function of the new column (`origin <> ''`), exposed
as `store.RecordOrigin.Locked()`, so every reader that only ever asked "is this
durable" — `enrichParent`'s pinned-id lookup, the Edit-item active-override view —
reads the same answer it read before. The two skip rules,
`enrich.childHasOwnOverride` and `enrich.albumHasOwnOverride`, are the only
readers that ask the sharper question, and they ask it as `OwnChoice()`.

The writers name the origin explicitly: `store.SetTitleExternalMatch` and
`store.SetEntityExternalMatch` take a `RecordOrigin` argument, and the enrich
service's Admin-facing entry points (`MatchTitle`, `ApplyOverride`,
`ApplyEpisodeOverride`, `ApplyEntityOverride`) mean "the Admin chose this, on this
item" and pass `OriginChosen`; the Cascade goes through unexported twins that pass
`OriginCascaded`. Nothing infers the origin from context.

## Both levels, because the recursion has the same shape

The defect is not a leaf-only defect. An Artist → Albums Cascade writes each
**Album** through `ApplyEntityOverride` → `SetEntityExternalMatch`, which set
`external_id_locked = 1`, and `albumHasOwnOverride` read that flag back as the
Album's own Fix info — so a second Artist Cascade skipped every Album the first
one reached, *and* its tracks with it (the recursion never enters a skipped
album). Fixing only the leaves would have left the larger blast radius broken.
Both columns therefore get the same three-valued origin, and both skip rules read
`OwnChoice()`.

## Backfilled rows read as `chosen`

Migration 0051 maps every existing `..._locked = 1` to `'chosen'`. Nothing else
was possible: the history is not in the database — before this ADR the two
provenances were the same bit — and it cannot be reconstructed, since a cascaded
record and a directly-chosen one are byte-identical in every other column.

`chosen` is the right direction for the same reason 0050's backfill was: the two
wrong answers are not symmetric. Reading an old row as `cascaded` would let the
next Cascade silently overwrite a Fix-info correction an Admin really made —
destructive, invisible, and unrecoverable. Reading it as `chosen` reproduces
exactly the behaviour that install had yesterday: the row keeps being skipped.
That is the bug, preserved, and it is visible (the Cascade summary counts it) and
repairable (Fix info, Wrong item, or a cleared pin on the child re-states its
record and settles the origin honestly).

**What an existing install gets**: the fix applies to every record written after
upgrading. Children a Cascade wrote *before* the upgrade go on being skipped by
later Cascades from their parent, exactly as they were.
`internal/store/record_origin_migration_internal_test.go` runs the real migration
over a pre-0051 row and pins that reading;
`enrich.TestABackfilledRowKeepsItsOldReading` pins what the skip rule then does
with it.

## Considered and rejected

- **A second boolean beside the lock** (`enrichment_id_cascaded`), the cheaper
  half of what the issue proposed. Rejected: two booleans spell four states, one
  of which (`cascaded && !locked`) is meaningless and unenforceable, and every
  future reader has to remember to consult both — the same "forgot the second
  half" shape as the bug being fixed. One fact with three values cannot be got
  half-right.
- **Keep `..._locked` and add the origin beside it.** Rejected for the mirror of
  ADR-0045's own reason. 0045 removed a column carrying two independent claims;
  keeping both here would add two columns carrying one claim, which every writer
  would then have to hold in agreement. The boolean is derived, so it is a method,
  not a column.
- **Let a Cascade leave the child's origin untouched** (write the record, record
  nothing), so a cascaded child looks derived. Rejected: it would make the
  cascaded record non-durable, and a later enrichment pass could re-auto-match the
  child back to the record the Admin corrected away from — the exact regression
  ADR-0019's "durable per-child Enrichment override" forbids, traded for a bug
  that at least fails safe.
- **Have the Cascade force its way through every child, and rely on the Needs-Fixing
  queue to report what it overwrote.** Rejected: it deletes the promise that a
  child's own Fix info survives its parent's correction, which is the reason the
  skip rule exists (ADR-0019, issue 03), and "we told you afterwards" is not a
  substitute for not clobbering.
- **Re-open ADR-0045's claim that a Cascade is the Admin's own choice.** Not
  rejected so much as unnecessary: it is true, and 0045 was deciding *chosen vs.
  derived by a pass*, where a Cascade plainly falls on the chosen side. This ADR
  refines the class it put the Cascade in rather than moving it out.

## Consequences

- **A Cascade is repeatable.** Re-pointing a Show or an Artist with "apply to
  children" a second time re-applies to the children the first run reached, and
  the summary counts them.
- **`store.Title.EnrichmentIDLocked` and `store.EntityEnrichment.ExternalIDLocked`
  are gone**, replaced by `EnrichmentIDOrigin` / `ExternalIDOrigin` of type
  `store.RecordOrigin`. A reader that wants the old bit calls `.Locked()`; a
  reader that wants "did the Admin choose this *here*" calls `.OwnChoice()`. The
  two questions are no longer spellable the same way by accident.
- **Nothing in the API contract changes shape.** The Edit-item active-override
  view still reports a cascaded record as the item's pinned override, because it
  is one — it is simply the parent's.
- **`RekeyTitleIdentity` clears the origin along with the record**, and
  `applyPinsTx`' Clear branch releases it, both as they cleared the lock before: a
  Wrong item is a clean slate and a cleared pin is a withdrawn choice, whichever
  of the two provenances put the value there.
