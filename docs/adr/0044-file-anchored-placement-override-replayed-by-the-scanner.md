# Placement is a file-anchored Match override, replayed by the Scanner

An Admin can rearrange which File fills which **Slot** within an already-identified
Show (and later, Album). That correction is stored as a **sparse, file-anchored
Match override** — `(library, absolute path) → (season, episode, ordinal)` — which
the Scanner consults at resolve time exactly as it already consults the
folder-anchored overrides in `resolveShowFolder` / `music_resolve`. The resulting
Title's `identity_key` follows the **assigned** Slot, and its position in the
library follows with it. What decorates the Slot stays a separate decision (the
Episode pin), because a borrowed record's numbering would collide with the
library's own.

## Why the Scanner has to replay it

The three cardinalities an Admin needs to express — one File per Slot, several
Files on one Slot (a multi-part Edition), one File across several Slots (co-File
sibling Titles) — all decide *how many Title rows exist*. Only the resolve step
creates, merges and splits Title rows; `writeTitleRow` then overwrites
`season_id`, `season_number`, `episode_number` and `episode_label` from whatever
the resolve step produced, on every single upsert. An override that lived only as
columns read at render time could not create a row, and one applied only to live
rows would be silently undone by the next scheduled scan. So the override has to
be an *input to resolution*, which is the shape [ADR-0002](./0002-naming-convention-is-identity-authority.md)
already established for identity corrections.

## Why `identity_key` follows the assignment

[ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md) keys watch state to
parsed identity so that re-enrichment can never cost a viewer their history. That
still holds, but "the parsed key" is not available in two of the three
cardinalities: a split needs two distinct keys and the filename supplies one, and
a merge has two keys and needs one. Deriving the key from the assigned Slot is the
only rule that is total.

History survives anyway, because `watch_state` is keyed by `title_id`, not by
`identity_key`. Apply **updates the row in place**, so the id never moves; the next
scan replays the same override, computes the same key, and finds the same row.
This is deliberately unlike Wrong-item ([ADR-0019](./0019-item-editing-preserves-local-identity.md)),
which resets watch state — there the Admin has said the file is a *different work*,
whereas a Placement correction says the same work was filed in the wrong place.

Merging folds the parts' watch state onto the joint timeline the multi-part
Edition already defines: watched only if every part was watched, resume mapped
through `Edition.PartAt`. The alternative — watched if *any* part was — would
recreate exactly the bug the multi-part duration work was written to prevent.
Splitting copies the original state to both rows, which is what co-File siblings
already do for each other.

## Why the record stays a separate decision

The motivating case is a run of episodes the provider counts in a re-numbered
continuation series: five files at the end of *Batman: The Animated Series*'
season 3 are season **1** of *The New Batman Adventures* on TMDB. If a Slot simply
inherited its record's numbering, those five would land in the Show's real Season
1 and collide with it. So a Slot's **position** is always local and a Slot's
**record** is repointable — the Episode pin, which now exists solely for that
minority case rather than as the mechanism for rearranging files.

## Why sparse

Only Slots whose Placement differs from what the parse would produce are recorded,
following [ADR-0027](./0027-per-library-enrichment-policy-sparse-override.md)'s
precedent for the Enrichment policy. Correcting five files in a 65-file Show
writes five rows, not sixty-five. A filename later fixed on disk therefore takes
effect instead of being overruled by a stale record, a future improvement to the
Scanner reaches every File the Admin never touched, and only the five paths
actually corrected can orphan.

## Consequences

- **Two writers of Title structure.** Apply mutates rows directly *and* the
  Scanner rebuilds them from the same overrides, so the two must agree or a
  scheduled scan will silently rearrange a hand-sorted Show. The invariant is
  testable and must be tested directly: **apply-then-rescan is a no-op.** Applying
  through a Targeted scan instead was rejected as too slow (it re-probes every
  file), which is precisely why the invariant needs a test rather than a
  structural guarantee.
- **Seasons follow assignments, not folders.** A Season is the set of Episodes
  claiming that number, so a Season row can exist with no folder on disk, and one
  emptied by reassignment disappears. Season artwork and enrichment hang off that
  row as they always have.
- **A Slot may have no record.** When the Library's Authoritative provider cannot
  list episodes (only TMDB implements `EpisodeLister`), or enrichment is off, or
  the server is offline, Slots are bare numbers. Pure renumbering still works
  offline; titles and the discrepancy highlighting simply are not there.
- **Three states, one record.** Sparse storage means *absence* of a record is a
  meaningful answer — "derive it from the filename" — so a File the Admin
  deliberately took off its Slot cannot be represented by having no row: the next
  scan would re-place it from the parse. The Admin's decision about a File is
  therefore stored as one record with three states — **placed** (a row per Slot),
  **unassigned** (undecided, still queued) and **ignored** (settled, silent) —
  keyed on `(library, path)`. Ignoring is emphatically not `titles.hidden`, which
  is the derived all-Files-Missing cache and is reset on every upsert.
- **The storage and API shapes are kind-neutral** (`group`/`slot`/`ordinal`,
  addressed by container id) so the Music adapter is an adapter. The kind-specific
  parts — fetching Slots from a provider, replaying an override during resolve —
  stay in the TV and Music resolve paths, which have little code in common.
