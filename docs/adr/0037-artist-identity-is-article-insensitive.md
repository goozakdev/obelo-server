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

## Amendment (2026-07-22): the word "and" folds too

"Marina and the Diamonds" and "Marina & the Diamonds" split the same way. `normalizeTitle`
already collapses "&" and "+" to a space, so the ampersand spelling keyed as
`artist:marina the diamonds` — but the *word* "and" survived normalization and keyed apart.

The rule gains one step, between normalization and the article strip:

`artistIdentityKey(name) = "artist:" + stripLeadingArticle(stripAndWords(normalizeTitle(name)))`

`stripAndWords` drops every standalone "and" word. The canonical direction is forced, not chosen:
stored keys from "&" spellings lost the ampersand at normalization, so "and" cannot be reinserted —
the "and"-less form is the only key derivable from stored text, and migration 0043 merges the
word-ful rows into it with the same machinery as 0042. The drop runs *before* the article strip so
a leading "And" mirrors a leading "&" ("And The X" → "the x" → "x"). A name that is only the word
("And") keeps it — a key never empties. Same accepted-quirk shape as "A-ha": the fold is keying
only, display names keep their spelling, and a false merge needs two real artists in one library
distinguished only by an "and" word.

## Consequences

- Album and Track identity keys embed the artist key as a prefix; migration 0042 rewrites those
  prefixes and merges same-titled Albums across a merged pair. Track (Title) ids never change.
- The merged Artist's display name is whichever the surviving row carried; the next rescan
  re-derives it from the first-seen track's tag, and an Admin rename (ADR-0019) still overrides.
- A duplicate rip of one album under both spellings can collide two Tracks on one key; the
  migration leaves the second on its old key and the next rescan settles it, exactly as a
  duplicate rip under a single artist behaves today.

## Amendment: the rule reaches the provider query too (2026-09-02)

This ADR made a leading article irrelevant to how Obelo *identifies* an Artist, and stopped
there. The provider query kept asking for the name exactly as tagged, so an operator whose
files say "The Eagles" got nothing from an album search that MusicBrainz answers under
"Eagles":

```
release-group?query=Hell Freezes Over AND artist:"The Eagles"                → 0
release-group?query=Hell Freezes Over AND artist:("The Eagles" OR "Eagles")  → 3, by "Eagles"
```

Half a rule is worse than none here, because the half that shipped is invisible: the
artist rows merge correctly, so nothing looks wrong, and the failure surfaces as albums
that "just don't match".

**The artist-narrowing clause therefore also tries the name with its leading English
article REMOVED.** It is one clause in `musicQuery`, so it reaches every place the artist
narrows a music search: the Edit-item and Needs-Fixing pickers, and the enrichment pass's
own track search.

Only removed — not added, which this amendment first claimed. `artist:"…"` is a phrase
query over an *analyzed* credit field, so a one-token phrase already matches inside a
longer credit:

```
release-group?query=Disintegration AND artist:"Cure"          → 4, credited "The Cure"
release-group?query=Different Light AND artist:"The Bangles"  → 0
release-group?query=Different Light AND artist:"Bangles"      → 2, credited "Bangles"
```

The matches of `artist:"The X"` are a strict subset of `artist:"X"`'s, so an
article-*added* alternative cannot return a row the bare one misses. It would also cost
the property that makes this safe: an artist with no article must emit exactly the
single-phrase clause it emits today, and a name that always gained an `OR "The …"` never
could.

Deliberately NOT extended to the `release:` clause. An album's leading article is part of
its title far more often than a band's is part of its name, and this ADR's evidence is
about artists. Nothing here argues for guessing at album titles.

Distinct from [ADR-0053](./0053-an-album-corroborates-its-artist.md), which is about
resolving the Artist *entity* and deliberately never reads the name at all. That one fixes
picking the wrong band; this one fixes narrowing a search by the right one.
