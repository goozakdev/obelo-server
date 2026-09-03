# Tagged music resolves by lookup, not by search

An enrichment pass over a Music Library asked MusicBrainz `/ws/2/recording?query=`
— a Lucene text search — **once per track**, forever, for every library. It did
that even when the file on disk was carrying the exact recording MBID in its tags.

This ADR is what a real 503 storm taught us about that.

## What the 503s actually were

An operator saw every track failing and reasonably concluded they had been
blocked. Asking MusicBrainz directly, with the same User-Agent the server sends:

```
HTTP/2 503
x-ratelimit-zone:      search-global
x-ratelimit-who:       search-shed
x-ratelimit-limit:     1200
x-ratelimit-remaining: 676
{"error": "The MusicBrainz web server is currently busy. Please try again later."}
```

Not blocked, and not throttled. `who` is a shed bucket, not a client address; the
quota is 56% unused; and the message is the "busy" one, not the "you are exceeding
the allowable rate limit" one. **MusicBrainz was shedding load globally on its
search cluster.** Interleaved probes confirmed the split — the search endpoint
failed roughly two calls in three while `/ws/2/recording/<mbid>` answered
normally.

Three things follow, and they are the whole of this ADR:

1. Lowering the client rate limit cannot help, because the limit being hit is not
   ours. It makes things slightly worse by extending the pass through the outage.
2. Search and lookup are not the same dependency. Obelo used the fragile one
   exclusively.
3. The server had all of that in the response and threw it away, so the operator
   had to guess — and guessed "blocked", which points at the wrong remedies
   entirely.

## Obelo already had the ids and dropped them

`releaseGroupID` (scanner/music.go) reads `musicbrainz_releasegroupid` from the
tags and uses it for **album identity** (ADR-0038). Nothing carried it any
further. `titles.musicbrainz_id` is written only by a pass's own result or an
Admin's Fix info; **no MBID had ever been read from a file's tags into the
enrichment path.**

So a Picard-tagged library — which knows the exact recording, release-group and
artist for every file — was resolved by fuzzy string matching against the one
MusicBrainz service that falls over under load. The id was sitting in the file the
whole time.

## Decisions

- **Tag ids are DECORATION anchors, in their own columns.** `titles.
  musicbrainz_recording_id`, `artists.musicbrainz_id`, `albums.musicbrainz_id` hold
  what the FILE asserts. They are scanner-owned, re-derived from disk on every
  scan, and never enter an identity key.

- **They are NOT `titles.musicbrainz_id`.** That column is the enrichment RECORD.
  A scanner writing to it would blank or overrule the Admin's correction on every
  scan — precisely the bug ADR-0045 was written to fix for `tmdb_id`, which cost
  this codebase an ADR and a migration the first time. The music side gets the
  separation for free by starting with it.

- **Precedence is record → tag → search** (`trackRecordID`). A human's correction
  outranks anything a file claims; a file's exact id outranks a guess. Search
  survives as the honest fallback for an untagged library, unchanged.

- **Ids are validated as UUIDs before use, and multi-valued tags take the first
  credit.** An unvalidated id is worse than no id: the lookup 404s, the provider
  maps 404 to `ErrNoMatch`, and the item is filed as "no such record" — a
  confident wrong answer where an empty id would have produced a correct search.

- **The recording id comes from the tag named "track", and the tag named "release
  track" is not it.** Picard writes the *recording* MBID to Vorbis
  `MUSICBRAINZ_TRACKID` and the release-specific *track* MBID to
  `MUSICBRAINZ_RELEASETRACKID`. Only the first resolves under `/recording/`. The
  keys are therefore matched EXACTLY — a prefix or substring test on
  "musicbrainz_track" swallows the wrong one and produces the confident-wrong-answer
  failure above. ID3's canonical home for the recording id is the binary `UFID`
  frame, which ffprobe does not surface, so MP3s tagged only that way keep the
  search path.

- **The artist id comes from the ALBUM artist**, falling back to the track artist.
  The Artist entity an Album files under is the album-artist
  (`MusicIdentityFromTags`), so the id must describe the same entity or a
  compilation decorates its "Various Artists" row from whichever track artist was
  seen first.

- **A 503 says why it happened.** `ProviderRefusal` captures the host's own
  message and its `x-ratelimit-*` headers, and the log states whether this is our
  usage or the host shedding load for everyone. "status 503" alone is not a
  diagnosis: it is the same three digits for rate-limited, blocked, and
  standing-in-a-global-queue, and those have opposite remedies.

- **In-request retries happen only when waiting can plausibly work** — our own
  rate limit, or an explicit `Retry-After`. Against a global shed the old
  four-attempt loop could not succeed: the shed outlives the pass, so all four
  failed, every track cost ~6 extra seconds, and three extra requests were added to
  a host already dropping load. Failing fast hands the item to the cross-pass
  backoff (ADR-0048), which is measured in minutes and is what actually recovers
  it.

- **The rate limiter is keyed by HOST, not held on the provider.** A rate limit is
  a property of the host, which counts requests per client and neither knows nor
  cares how this server divides its work. `Manager.resolveLibrary` builds a
  provider per Library, so the per-instance throttle meant a three-Library server
  sent 3 req/sec at one host with each Library correctly believing it was well
  behaved. The global snapshot's provider was a fourth.

## What was considered and rejected

- **Lower the default rate limit.** The evidence says the limit being hit is not
  ours. This is the remedy the missing diagnosis was pushing operators toward, and
  it trades throughput for nothing.

- **Fall back to search when a lookup fails.** Backwards. The lookup is the
  reliable path; falling back to the fragile one on its failure inverts the
  reliability ordering, and a failed lookup by an id the file asserts is a fact
  worth surfacing, not papering over.

- **Reuse `titles.musicbrainz_id` for the tag id.** One column, two owners, one of
  whom rewrites it on every scan. See ADR-0045 for how that ends.

- **Parse the release-group id back out of the album's identity_key**, where
  ADR-0038 already embeds it, instead of adding a column. String surgery on an
  identity key to recover a field we could simply store.

- **A provider-level circuit breaker** — stop calling a host entirely once it is
  clearly shedding, instead of per-item retries. Genuinely attractive during a long
  outage, and the natural next step, but it is a bigger mechanism than this needed
  and the per-item backoff already bounds the damage.

## Consequences

- **A tagged library makes far fewer search calls, and none for tracks that carry
  a recording id.** Matches also stop being fuzzy: an exact id cannot pick the
  wrong recording of a common song title, which was a silent source of wrong
  overviews.

- **The ids appear only after a rescan.** They are scanner-derived, so an existing
  library keeps searching until its next scan populates the columns. No backfill is
  possible — the ids are in the files, not in the database.

- **An untagged library is completely unchanged**, including its exposure to
  search outages. The fallback is the old path, exactly.

- **A retagged file changes its ids**, because the columns are re-derived from
  disk on every scan and written unconditionally for a Track — the same rule
  `tmdb_id` follows. Artist and Album ids are filled but never blanked by an empty
  one, because those rows are assembled from many files and an incremental scan may
  see only some of them.
