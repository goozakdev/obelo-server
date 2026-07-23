# Artist identity is article-insensitive

A Music Artist's identity key strips a leading English article ("The "/"An "/"A ") from the
normalized album-artist name, so two tag spellings of one band — "The Smashing Pumpkins" and
"Smashing Pumpkins" — resolve to ONE Artist row instead of splitting the discography across two.

## Why

Real libraries mix spellings. Rips from different sources tag the same band with and without its
article, and the old rule (`artist:` + case-folded, punctuation-collapsed name) treated those as
two Artists. The artist detail page then showed half a discography, and the artist wall listed the
band twice — adjacent, because the *sort* key already stripped articles (migration 0024). Sorting
had adopted article-insensitivity; identity had not, and the gap is exactly what users see.

Alternatives considered:

- **Retag the files** — pushes a server defect onto the user's library, and every future rip
  reintroduces it. Tags stay authority (amended ADR-0002); we normalize harder, we don't edit.
- **A manual merge/alias tool** — the general solution to a narrow, mechanical problem. Articles
  are deterministic; an Admin should not have to hand-merge every "The X" in the library. (A merge
  tool may still arrive someday for genuinely differently-named duplicates; nothing here blocks it.)
- **Merge at display time only** — leaves two rows under one face: watch state, enrichment, and
  collections would still split. Identity is where the problem lives.

## The rule

`artistIdentityKey(name) = "artist:" + stripLeadingArticle(normalizeTitle(name))`

The article is stripped **after** normalization, not before (as `sortTitle` does on raw names).
Post-normalization stripping makes the new key a pure text function of the OLD stored key, which is
what lets migration 0042 converge every existing row — merging split pairs while preserving the
surviving row's id, and with it watch state, playlists, collections, and pinned enrichment —
without re-probing a single file (identity stability, ADR-0014).

The cost of that choice: normalization turns punctuation into spaces first, so "A-ha" normalizes to
"a ha" and keys as "ha". That is a *keying* quirk, not a display change (the name still renders
"A-ha"), and a false merge requires two real artists in one library distinguished only by a leading
article-word — accepted as vanishingly rare against the very common split this fixes.

## Consequences

- Album and Track identity keys embed the artist key as a prefix; migration 0042 rewrites those
  prefixes and merges same-titled Albums across a merged pair. Track (Title) ids never change.
- The merged Artist's display name is whichever the surviving row carried; the next rescan
  re-derives it from the first-seen track's tag, and an Admin rename (ADR-0019) still overrides.
- A duplicate rip of one album under both spellings can collide two Tracks on one key; the
  migration leaves the second on its old key and the next rescan settles it, exactly as a
  duplicate rip under a single artist behaves today.
