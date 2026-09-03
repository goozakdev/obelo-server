# An album resolves its own tracks

One track of Harry Connick Jr's *She* — "(I Could Only) Whisper Your Name" — sat in
the Needs Fixing queue with no metadata match. The album above it was matched. That
album's release names the recording, in order, with its MBID. Obelo never asked.

[ADR-0049](./0049-tagged-music-resolves-by-lookup-not-search.md) established that a
Track should be resolved by lookup wherever an exact id exists, and harvested three
ids out of the files. This ADR is about the fourth exact anchor, which is not in the
file at all: **the album's own tracklist.**

## What was actually broken

Three separate things, and only the first is the one the operator reported.

**A matched Album cannot decorate its own Tracks.** `mapAlbumTracks`
(enrich/cascade.go) already maps an album's provider tracklist onto its local tracks
and pins each one durably. It runs only when an Admin ticks "also apply to children"
on a parent correction. A normal enrichment pass never calls it: `collectMusicLeaves`
builds a `TitleRef` carrying `Album`, and `MusicBrainzProvider.Lookup` drops that
field on the floor (`p.trackDetails(ctx, ref.Track, ref.Artist)`). So the album's
record — and the release-group id sitting in `albums.musicbrainz_id` since ADR-0049 —
contributed nothing to resolving the twelve tracks underneath it.

**The pass's search is the shape the picker already abandoned.** `trackDetails` builds
`recording:"<title>" AND artist:"<artist>"` — an exact phrase, unescaped, first hit
wins, zero hits → `ErrNoMatch`. The interactive search box next to it was deliberately
moved off that shape to relevance-ranked escaped terms (`musicQuery`), because an
exact phrase misses on any punctuation MusicBrainz spells differently — which is
exactly what a parenthetical title like this one is. The automatic matcher was
strictly worse than the manual one it hands work to.

**The queue row searched for the wrong string.** A Track's Needs-Fixing row seeded its
recording search with the ALBUM title, so opening that row searched MusicBrainz for
every recording ever called "She". That is a UI bug with its own fix
(`.scratch/needs-fixing/issues/06`), not an architectural decision, and it is recorded
here only because it is what made the first defect visible.

## Decisions

- **The precedence gains a tier: record → tag → ALBUM TRACKLIST → search.** This
  narrows ADR-0049's "record → tag → search", and for the same reason that ADR gave:
  an exact id beats a guess, and search is the honest last resort. A human's
  correction still outranks what the file asserts, which still outranks what the album
  asserts about its contents, which still outranks a text search.

- **The tracklist tier SUPPLIES AN ID; it does not resolve a record.** The mapping's
  output is written into `ref.MusicbrainzID`, and the leaf then resolves through the
  existing `/recording/<mbid>` lookup like any other pinned id. One place turns an id
  into a record, and the new tier is a sentence in `trackRecordID`'s precedence rather
  than a second resolution path with its own error handling.

- **The rule is ALBUM-grained, not track-grained.** It has to be: "the one local track
  and the one tracklist position both still unclaimed are each other" cannot be
  evaluated one track at a time. The mapping is therefore computed once per Album, in
  `collectMusicLeaves`, where `enrichParent` already returns the Album's resolved
  external id and the call site currently discards it.

- **The match rule is position-and-title, then title, then leftover, then decline:**

  1. the tracklist entry at the file's own `(disc, track)` whose normalized title
     matches the local title — pin it;
  2. otherwise a normalized title matching exactly ONE entry anywhere in the tracklist
     — pin it, which is what rescues an album whose local numbering drifted;
  3. otherwise, if exactly one local Track and exactly one tracklist position are both
     still unclaimed after 1 and 2, pair them;
  4. otherwise decline, and let the track fall through to search.

  **Never position alone.** That is what `mapAlbumTracks` does today, and on a
  hand-numbered rip it silently pins the wrong recording — the confident-wrong-answer
  ADR-0049 called worse than no id at all. A position that agrees with a title is
  evidence; a position by itself is an assumption about someone else's numbering.

- **One rule, both callers.** `mapAlbumTracks` and the pass call the same function. A
  Cascade will therefore pin fewer tracks than it does today on an album whose
  numbering does not line up, and route the rest to the queue. That is the point: the
  ones it stops pinning are the ones it was getting wrong.

- **The tracklist comes from the release the FILE names, when it names one.**
  `albums.musicbrainz_release_id` is read from the `musicbrainz_albumid` tag,
  scanner-owned and re-derived on every scan, exactly as ADR-0049's three columns are.
  The reason it is worth a column is specific: Picard writes the recording id to ID3's
  binary `UFID` frame, which ffprobe does not surface, so **Picard-tagged MP3s are
  precisely the population stuck on search** — and Picard writes the release id to a
  `TXXX` frame, which ffprobe does surface, and which `releaseGroupID` already reads a
  sibling of. The exact anchor for the stuck population was one tag away.

- **A tag's release is used only if its parent release-group is the Album's.** One
  `/release/<id>?inc=recordings+release-groups` answers both questions in the call the
  tracklist needs anyway. A retagged or mis-tagged file that names a release of some
  other album is then ignored rather than renumbering the whole album against a
  stranger.

- **Absent a usable release id, the release is chosen by track-count fit, not by
  `limit=1`.** `releaseGroupTracklist` currently takes whichever release MusicBrainz
  returns first for a release-group. For a standard album that is harmless; for one
  with a deluxe edition, a remaster or a Japanese pressing, every position after the
  first bonus track belongs to a record nobody has. Picking the release whose track
  count matches the local album's — earliest release breaking ties — is the cheapest
  rule that is right more often than an arbitrary one.

- **A tracklist-derived record is `OriginDerived`.** Nobody chose it. It carries no
  protection, a later pass may revise it, and an Admin's correction overrules it
  without ceremony. It is explicitly NOT `OriginCascaded`: that value means "the
  parent's explicit choice, durable, revisable only by that parent" (ADR-0046), and a
  pass writing it would both lie about who decided and make every auto-match immune to
  the next pass.

- **A search hit must pass an acceptance test before it becomes a record.** The pass's
  search becomes `musicQuery(title, artist)` — escaped, relevance-ranked,
  artist-narrowed — and the top hit is accepted only when its normalized title matches
  the local one. Swapping the query without the test would trade an honest empty
  answer for a confident wrong one: an exact phrase that finds nothing is *telling the
  truth*, whereas a relevance search essentially always returns something and
  `Recordings[0]` would have been applied blind.

- **A settled failure records WHY, as a small closed enum.** `album-unmatched`,
  `not-in-tracklist`, `tag-id-unresolved`, `search-no-match`, `search-rejected`. The
  value is not decoration: each names a different next action — fix the Album, fix the
  Album's *release*, fix the file's tags, pick a recording by hand — and without it
  every one of them renders as the same sentence, "no metadata match", which tells the
  Admin nothing they did not already know from the row's existence. It is a closed set
  rather than free text so the copy lives in the client with the rest of the copy, and
  so no failure path can invent a category nothing renders.

- **Title normalization is one function, and it is not the identity normalizer.**
  Both the match rule and the acceptance test stand on it: case, punctuation,
  diacritics, bracketing, and the trailing decorations MusicBrainz and taggers disagree
  about (`feat. …`, `(Remastered 2011)`, `[Bonus Track]`). `scanner.normalizeTitle`
  exists but serves identity keys, where over-collapsing two distinct works is the
  failure that matters; here under-collapsing two spellings of one work is. Sharing it
  would couple a matching heuristic to a key format that must never move.

## What was considered and rejected

- **Infer the album from the folder and the file's neighbours** — the shape the
  operator originally proposed: if every other file in the folder belongs to *She*,
  this one does too. Rejected because Obelo already believes that. The file was filed
  in the right Album at the right track number; nothing about its membership was in
  doubt. The missing fact was the *recording*, and the album's tracklist names it
  exactly, where the folder can only suggest it. The idea's genuinely load-bearing
  half — pair the one leftover file with the one leftover position — survives as rule
  3 above, re-anchored on the album.

- **Call `CascadeEntity` from the pass.** The mapping is right there and already
  written. But a Cascade writes `OriginCascaded` and is defined as a parent revising
  its own decision; running it automatically would make every auto-matched track carry
  a durability it never earned, and the next pass would skip everything the first one
  touched.

- **Position-only matching, matching today's cascade.** Simplest, consistent, and
  wrong on exactly the albums that need help — the hand-numbered ones. Consistency
  with a bug is not a reason.

- **Keep `limit=1` and let the title check absorb a wrong release.** It would be
  *safe*: most positions fail the title test and fall through to search. But the album
  then gains nothing from being matched, which is the entire defect this ADR exists to
  fix.

- **Swap the exact phrase for a relevance query and take the top hit.** Better recall,
  and a new class of silent wrong overviews. The acceptance test is what makes the
  swap payable.

- **Retry with a second, looser query when the first returns nothing.** Best recall of
  the options considered. Rejected on ADR-0049's evidence: the search cluster is the
  dependency that sheds load globally, and a second request issued precisely during
  the failures is the wrong direction to push it. The album tier already removes most
  of the traffic that would have needed the retry.

- **A free-text failure reason.** Every writer invents its own phrasing, the client
  cannot key actions off it, and it becomes a log line stored in a column.

## Consequences

- **A matched album costs ONE provider call to resolve all of its tracks**, replacing
  one search per track. On the fragile endpoint, for the untagged libraries ADR-0049
  explicitly left unchanged. This is a reduction in load, not an addition.

- **The Cascade pins fewer tracks on a mis-numbered album, and queues the rest.** An
  existing library may gain attention rows on the next cascade. Each one is a track
  that was previously decorated by a recording nobody verified.

- **The release id appears only after a rescan**, and no backfill is possible — the
  id is in the files. Same as ADR-0049, for the same reason.

- **An album matched to the wrong RELEASE becomes visible.** Before, it was silent:
  the positions simply produced wrong titles or none. Now most of its tracks decline
  with `not-in-tracklist`, which is a diagnosis with an obvious action.

- **A hidden track, a bonus track, or a track the release genuinely omits still
  declines** — rule 3 fires only when exactly one of each is unclaimed, and an album
  with two strays leaves both for the Admin. That is deliberate: pairing two unknowns
  with two positions is a coin flip wearing a rule's clothing.

- **The reason must be rewritten on every settled outcome, never left behind.** A
  reason that outlives the failure it described is worse than none, because the row
  will confidently explain a problem that no longer exists. It is written wherever
  `enrichment_status` is written, and cleared on a match.

- **The Music matcher (ADR-0044's album adapter) is NOT what this needed.** The file
  the operator was looking at was arranged correctly; only its record was missing. A
  drag-and-drop screen is the answer to a *placement* problem, and whether music has
  one in volume is a question to re-ask against the queue that survives this work.
