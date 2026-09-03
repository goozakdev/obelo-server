# A chosen edition is part of the record, and it licenses position

An operator with a classical album — Andrea Bocelli's *Viaggio Italiano* — did
everything right. They found the exact release on musicbrainz.org, pasted its
`/release/` URL into Fix info, and the album matched. Ten of its sixteen tracks stayed
unmatched anyway.

Two separate defects, and this ADR is both of them.

## The edition was taken and thrown away

`releaseGroupForRelease` resolves a pasted `/release/` URL to its parent release-group,
by design, because [ADR-0038](./0038-album-identity-release-group-wins.md) makes the
release-group the album's identity. The stored record is the release-group; the release
is discarded. There is nowhere to keep it: `entity_enrichment` has one `external_id`.

`albums.musicbrainz_release_id` cannot hold it either. That column is scanner-owned and
re-derived from the file's tags on every scan ([ADR-0049](./0049-tagged-music-resolves-by-lookup-not-search.md)),
so an Admin's choice written there would be erased by the next scan — the exact bug
[ADR-0045](./0045-enrichment-record-is-its-own-column.md) exists to prevent.

So the one moment a human names an edition, the system upgrades it to a release-group
and goes back to choosing the edition itself, by track-count fit
([ADR-0050](./0050-an-album-resolves-its-own-tracks.md)). The operator was asked for an
answer, gave it, and was ignored.

## "Never position alone" was applied one tier too high

ADR-0050 says a Track is never pinned by `(disc, track)` alone, because a bare position
is *our assumption about a stranger's numbering* — and on a hand-numbered rip that is a
confident wrong answer.

Here is what that rule rejected:

```
 1  Puccini: Turandot - Nessun Dorma            matched
 2  Cilea: L'Arlesiana - Lamento Di Federico    not-in-tracklist
 4  Verdi: Rigoletto - La Donna E Mobile        not-in-tracklist
 9  Core N`Grato                                matched
11  It`E Vurria Vasa                            not-in-tracklist
```

Composer prefixes, and backticks for apostrophes. The numbering is 1–16, sequential,
complete — and **every one of the 54 albums in that state across the library has clean
1..N numbering, no gaps, no duplicate positions, no unnumbered tracks.** The position was
right every time. The title comparison vetoed it.

The rule's justification does not survive the operator pinning the edition. Once a human
has said *this album is this release*, the ordering is no longer our assumption; it is
their assertion. ADR-0050's own precedence — a human's correction outranks what a file
asserts, which outranks what an album asserts, which outranks a guess — puts a chosen
edition above all three, and then declined to act on it.

## Decisions

- **The Admin's chosen release is part of the enrichment RECORD**, in its own column on
  `entity_enrichment`, beside the release-group `external_id`. Not in the scanner's
  column, for ADR-0045's reason; not instead of the release-group, for ADR-0038's.

- **Album identity is unchanged and stays the release-group.** The edition is a
  DECORATION refinement — it selects which tracklist decorates the album's tracks — and
  never enters an identity key. Pinning a different edition of the same album re-keys
  nothing and costs no watch state.

- **A pasted `/release/` URL keeps both.** It still resolves to its parent release-group,
  which is still what identity uses; the release is now also stored, because it is the
  more specific thing the human actually said.

- **The tracklist anchor's precedence is: chosen release → the file's tag release (when
  its parent is the album's release-group) → best fit by track count.** This is ADR-0050's
  ordering with the human's choice inserted where the precedence always said it belonged.

- **A chosen edition licenses position-alone mapping.** The rule becomes: position+title,
  then title anywhere, then the leftover pair, then — ONLY when the edition was chosen by
  a human — the entry at the track's own `(disc, position)`, regardless of title. A
  disagreeing title stops being a veto and becomes information.

- **It is licensed by the CHOICE, not by the confidence.** Not "when the counts match",
  not "when most titles agree" — those are the automatic matcher's heuristics wearing a
  new hat. The licence is that a human asserted this edition, which is the one fact the
  automatic path never has.

- **An edition can be chosen without leaving Obelo.** The release-group's releases are
  listed with date, country, format, track count and disambiguation. Requiring an
  operator to visit musicbrainz.org and paste a URL is what surfaced this, and a
  correction that can only be made on someone else's website is not a correction the
  product offers.

- **A track resolved this way is still `OriginDerived`.** It is derived from the album's
  record, not chosen on the track. A later pass may revise it, and an Admin's own choice
  on the track still outranks it (ADR-0045/0046).

## What was considered and rejected

- **Build the drag-and-drop album matcher instead** — which is what the operator asked
  for, and it is the wrong tool for this. Every one of the 54 albums is already numbered
  1..N correctly, so the matcher would ask them to drag track 2 onto slot 2, 207 times,
  and change nothing. A matcher earns its place when the local ORDER is wrong; here only
  the *titles* differ, and the position it would have the operator assert by hand is the
  position the file already claims. (ADR-0044's album adapter remains worth building for
  the case where the order really is wrong — an untagged pile with no usable numbering.
  It is not this case.)

- **Relax the title check generally.** It would fix these 54 and reintroduce exactly the
  silent mis-decoration ADR-0050 was written to stop, on every hand-numbered rip in every
  library. The distinction that makes it safe is who asserted the edition, and that
  distinction is now recorded.

- **Teach the normalizer to strip composer prefixes** (`Puccini: Turandot - …`). Tempting,
  and it would help some classical albums. But it is a guess at a naming convention that
  varies by tagger, by label and by era, it fails silently when it guesses wrong, and it
  does nothing for the backtick-for-apostrophe case sitting two lines below it. The
  operator's pin is exact; a prefix heuristic is not.

- **Store the edition in `albums.musicbrainz_release_id`.** One column, two owners, one of
  whom rewrites it from disk on every scan. See ADR-0045 for how that ends — and ADR-0049
  for the last time this exact temptation was refused.

## Consequences

- **One pick clears an album.** On the motivating library, 207 flagged tracks across 53
  albums become 53 decisions, each of which resolves its album's tracks outright.

- **A wrong pin now propagates confidently.** That is the trade, and it is deliberately
  the human's to make rather than the matcher's. The cascade reports how many tracks it
  mapped, so a pin that produces a nonsense count is visible immediately.

- **`not-in-tracklist` keeps its meaning**, narrowed: with an edition chosen, it means the
  pinned release genuinely has no entry at that position — a real disagreement about the
  album, not a disagreement about spelling.

- **The scanner's tag release id becomes the fallback it was always meant to be.** A
  Picard-tagged library still gets the exact edition for free; an untagged one now has a
  way to say it by hand.
