# An Album corroborates its Artist

An operator reported that albums by "The Eagles" would not match, and that searching
MusicBrainz for "Eagles" found them immediately. The article looked like the bug. It was
the symptom.

## What Obelo had actually done

`artists.external_id` for their Eagles was `a4852e21-7f09-470b-b5ae-9740d939d183`. Asking
MusicBrainz what that is:

```
name:           The Eagles
disambiguation: 1960s UK instrumental group
type:           Group          area: United Kingdom      began: 1958
```

Not the band. `artistDetails` searches `artist:"<name>"` — an exact phrase — takes
`out.Artists[0]`, and stores it with no acceptance test of any kind. The only MusicBrainz
artist *literally named* "The Eagles" is a 1958 British instrumental outfit; the American
band's name is "Eagles". So the lookup found an exact match, was confident, and was wrong
— and the Artist row was written `matched`, which is why nothing ever flagged it.

Everything downstream then failed correctly. Three live queries:

```
release-group?query=Hell Freezes Over AND artist:"The Eagles"   → no results
release-group?query=Hell Freezes Over AND artist:"Eagles"       → 3, top: Hell Freezes Over
release-group?query=Hell Freezes Over AND arid:a4852e21-…       → 0
```

The albums are not by the band Obelo picked. Thirteen unmatched tracks on *Hell Freezes
Over* were a symptom of a bad Artist, three levels up.

## Why the obvious repairs do not work

- **Article-insensitivity.** [ADR-0037](./0037-artist-identity-is-article-insensitive.md)
  already made a leading article irrelevant to Artist *identity*, and that rule never
  reached the provider query. Fixing that finds "Eagles" — and "The Eagles" still exists,
  still matches the local name exactly, and still wins.
- **An acceptance test on the name**, the twin of the one
  [ADR-0050](./0050-an-album-resolves-its-own-tracks.md) put on the track search. It would
  *accept* this: the names are identical.
- **A relevance-score threshold.** An exact name match scores 100.

Every discriminator built out of the artist's NAME fails, because the name is not what is
wrong. The two bands are told apart by their discographies, and Obelo is holding one.

## Decisions

- **An Artist is resolved through one of its Albums.** Take an album the library actually
  has, identify that release-group, and read the artist credit off it. The precedence
  becomes: the artist MBID the FILES assert (ADR-0049, unchanged and still first) → an
  album's release-group → a name search.

- **The album's own tag id is used when it has one.** A release-group MBID from the tags
  resolves with one `/release-group/<id>?inc=artist-credits` lookup and no search at all —
  the same lookup-beats-search preference ADR-0049 established, applied one level up.

- **Otherwise the album is searched for UNNARROWED, and its title must pass the same
  acceptance test a track's does.** Narrowing that search by the artist would reintroduce
  the very name we are trying not to trust. Reusing `normalizeMatchTitle` means a
  corroborating album is one whose title actually matches, not merely the top hit.

- **The name search survives as the last resort**, unchanged, for an Artist whose albums
  are all unidentifiable — a soundtrack filed under the film's name, an "Unknown Artist"
  pile. Those are exactly the cases where corroboration has nothing to offer, and they
  are no worse off than today.

- **This subsumes the article problem.** Corroboration never reads the artist's name, so
  an artist MusicBrainz spells without the article needs no special handling. ADR-0037's
  rule is satisfied by not asking the question.

- **A corroborated Artist is still `OriginDerived`.** Nobody chose it; a later pass may
  revise it, and an Admin's Fix-info still outranks it (ADR-0045/0046).

- **Cost is neutral.** The corroborating search REPLACES the artist name search rather
  than joining it; an album with a tag release-group id makes it cheaper still.

## What was considered and rejected

- **Article-insensitive querying alone**, which is what the report literally asked for. It
  fixes the artists that have no same-named twin and leaves the reported one broken.

- **Verifying the artist by name after the fact.** Same failure, later.

- **Checking the candidate's release-groups against the library's albums** — corroboration
  in reverse, and strictly more expensive: it costs a request per candidate to reject
  them, where searching for the album we hold costs one to find the answer.

- **Flagging "artist matched, no album matched" and leaving the matching alone.** A
  genuine improvement to visibility — sixteen artists in the motivating library carry that
  signature — but it asks a human to repair every wrong match by hand, forever, and does
  nothing for the next library.

## Consequences

- **A wrong Artist stops being invisible.** The failure mode this fixes was silent by
  construction: the Artist read `matched`, so no queue row ever mentioned it, and the
  damage surfaced as unmatched tracks three levels down.

- **A corroborated Artist is only as good as its corroborating Album.** The acceptance
  test on the album title is what bounds that, and a failed test falls through to the name
  search rather than guessing.

- **Existing wrong matches do not repair themselves.** They are `matched`, so only a
  recheck pass re-asks them ([ADR-0051](./0051-a-settled-non-answer-is-re-asked-when-the-question-changes.md))
  — and a recheck only visits settled NON-answers. A confidently wrong `matched` Artist is
  reached by a full pass, or by an Admin's correction. That gap is worth naming: this ADR
  stops new ones, and the sixteen already in the library are a separate cleanup.

- **Artist metadata quality rises where it was quietly wrong.** The artist photo, bio and
  genres on those rows were another band's.
