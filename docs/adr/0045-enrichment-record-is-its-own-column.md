# An Enrichment override outranks the id a folder name asserts

A Title's **identity** and the **provider record that decorates it** are two
separate claims with two separate owners, and they get two separate columns:

- `titles.tmdb_id` / `titles.imdb_id` — the id **local naming** asserts: the
  embedded `{tmdb-438631}` / `{imdb-tt1160419}` token, or a folder-anchored Match
  override. Owned by the Scanner, re-derivable from disk on every pass, and
  already spelled a second time in `identity_key` (`"tmdb:438631"`).
- `titles.enrichment_tmdb_id` / `titles.enrichment_imdb_id` (+
  `titles.musicbrainz_id`, which never had an identity twin) — the record
  **Enrichment** resolved, and the record an **Admin** chose with Fix info, an
  Episode pin, or a Cascade. `enrichment_id_locked` distinguishes the second from
  the first: it is set only by an Admin's own choice.

**When the two disagree, the enrichment column wins.** Every read that asks
"which record decorates this Title" resolves it as
`COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id)` — the override when there is
one, the folder's id otherwise.

The full precedence is three-deep, and only the top two are anybody's *decision*:

1. **the Admin's override** — Fix info, an Episode pin, a Cascade. Whose decision
   it is at that top rank is a further question this ADR does not answer, and
   [ADR-0046](./0046-a-cascaded-record-is-the-parents-choice.md) does: a record a
   Cascade wrote is the **parent's** choice held by the child, not the child's
   own, so it follows the parent instead of outranking it;
2. **the id the folder name asserts** — an embedded token or a Match override;
3. **the id an enrichment pass resolved on its own**, which is stored fill-only
   and so can never shadow either of the above.

An echoed id is not a claim, it is what a lookup came back with. That is why
`WriteTitleEnrichment` fills the enrichment column only when *both* halves of the
split are empty: a provider that answers a by-id lookup with a different id than
it was asked for must not be able to quietly re-point a Title nobody corrected.

This does not weaken [ADR-0002](./0002-naming-convention-is-identity-authority.md).
An embedded id keeps every bit of authority it ever had over **identity**: it
still decides `identity_key`, which Title a File lands on, and therefore which
watch state it carries. What it loses is a job it was never given — deciding
which record supplies the poster and the overview after an Admin has said, in so
many words, that the automatic answer was wrong. That is exactly the separation
[ADR-0019](./0019-item-editing-preserves-local-identity.md) draws between **Fix
info** ("only *which* record decorates the item") and **Wrong item** ("the file is
genuinely a different work"). An Admin who means the folder is wrong has Wrong
item, which re-keys identity *and* re-pins the record; Fix info means the folder
is right and the match is not, and it must therefore be able to outlive the
folder.

## Why this was already decided, everywhere else

A Show, Artist or Album has kept its Admin-chosen record in
`entity_enrichment.external_id` — its own column, in a table the Scanner never
writes, with its own `external_id_locked` flag — since 0011/0020, and
`catalog.showSeries` already resolves a Show enrichment-first and falls back to
`shows.tmdb_id` only for the never-enriched case. That precedence is this ADR,
written down for one entity type. It is also why Shows were structurally immune
to the bug (`.scratch/enrichment-override-durability/issues/01`) that blanked an
Episode pin's series on every scan, while Titles were not.

So the honest question was never "should we invent a separation" but "why is the
Title the one entity that never got one", and the answer is history: `titles`
predates enrichment by five migrations, and `0010_enrichment` reused the identity
columns it found rather than adding its own. Nothing about a Title differs in
kind. A leaf Title is, if anything, the entity where the conflation bites
hardest, because it is the only one whose record the Scanner also writes.

## Considered and rejected

- **Leave one column; tell the Admin that Fix info does not stick on a folder
  that names its own record.** Defensible under a maximal reading of ADR-0002,
  and cheap. Rejected because it makes the product's most common correction
  conditional on a detail of a folder name that the Admin may not have chosen and
  cannot see from the edit screen, and because it would have to be un-decided the
  moment anyone asked why Shows behave differently. A rule that needs a warning
  label at the point of use is usually the wrong rule.
- **Recommend a rename instead.** Renaming a folder is the user re-asserting
  identity ([ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md)) and drops
  the folder's Match override. Asking for it to fix a poster is asking for an
  identity change to buy a decoration change, which is the coupling this whole
  area exists to break.
- **One opaque `enrichment_external_id`, as `entity_enrichment` has.** Rejected
  for leaves: a video Title resolves against TMDB *and* OMDb/fanart.tv/
  OpenSubtitles, which key on different namespaces (ADR-0021), so a single column
  would have to carry a namespace tag to say which id it holds. The parent table
  gets away with one column because a parent has exactly one Authoritative
  provider.
- **A fourth column, `enrichment_musicbrainz_id`.** Rejected as a column that
  could only ever be a copy: music identity is embedded tags, not an id
  (ADR-0002), so nothing derives a MusicBrainz id from local naming and
  `titles.musicbrainz_id` has only ever had enrichment writers. It *is* the
  enrichment column already. `enrichment_id_locked` is namespace-neutral and
  covers it.

## Consequences

- **The Scanner owns `tmdb_id`/`imdb_id` outright** and writes them
  unconditionally again. The `CASE WHEN ? <> '' THEN ? ELSE tmdb_id END` guard
  that issue 01 added is gone: it existed only to order two writers of one
  column, and there are no longer two writers. The Scanner's tree write never
  touches the enrichment columns of an existing row at all, so an override cannot
  be reached by a scan even in principle — which is a stronger guarantee than the
  guard gave, and one that does not depend on remembering it.
- **"The Admin chose this" is now recordable rather than inferred.**
  `enrichment_id_locked` is the per-Title twin of `entity_enrichment.external_id_locked`.
  Cascade's `childHasOwnOverride` used to infer it from a non-empty id, which was
  wrong in the direction issue 03 describes; it reads the flag now. It is the only
  reader that needs to: everything else that reads the id columns is asking "which
  record does this Title resolve against", and that is what they hold whoever put
  the value there.

  **Superseded in shape by [ADR-0046](./0046-a-cascaded-record-is-the-parents-choice.md).**
  Recording *that* the record is a choice was not enough to tell a child's own Fix
  info from a value its parent's Cascade put there, so the flag is now a
  three-valued `enrichment_id_origin` / `external_id_origin` (`''` | `'chosen'` |
  `'cascaded'`). The bit this ADR wanted is `RecordOrigin.Locked()`, unchanged in
  meaning; the skip rules ask `OwnChoice()` instead.
- **Existing rows are split by their own `identity_key`.** An id that
  `identity_key` does not assert was never an identity claim, so it moves to the
  enrichment column. It is marked locked, because before this ADR nothing
  distinguished an Admin's pick from an auto-resolved id and the code that reads
  it treats every value as the Admin's — preserving that reading is the only
  migration that cannot silently discard someone's correction.
- **The API's `tmdbId`/`imdbId` keep meaning "the record this item resolves
  against"**, which is what the Edit-item picker already assumed when it fed
  `title.tmdbId` in as the currently-pinned id. Nothing in the contract changes
  shape.
