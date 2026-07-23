# Album identity: an embedded release-group id wins over the title

A Music Album's identity key prefers the file's embedded MusicBrainz
release-group id when present; failing that, an embedded release-type tag joins
the normalized title. Same-titled releases by one artist — an LP and its title
single ("Rockin' the Suburbs") — resolve to separate Albums instead of one
merged, interleaved track list.

## Why

Album identity was `(artist key, normalized album title)` only, and
`normalizeTitle` collapses punctuation — so an LP tagged "Rockin’ the Suburbs"
(curly apostrophe) and a single tagged "Rockin' the Suburbs" keyed identically.
The merged Album showed both releases' track 1/2/3 interleaved.

Picard/MusicBrainz-tagged files already carry the answer:
`MUSICBRAINZ_RELEASEGROUPID` is distinct per release, and `RELEASETYPE`
distinguishes album/single/ep. The scanner captured both (ffprobe surfaces all
tags) and ignored them. Movie/TV identity already has the rule this needs: an
embedded external id (tmdb/imdb) overrides the normalized-title key
(`identityKey`, ADR-0002). This decision extends the same rule to music.

Alternatives considered:

- **Key on `MUSICBRAINZ_ALBUMID` (the release id)** — too fine: country
  pressings and reissues of one album would split. Release-group granularity
  matches what a user means by "an Album". (Cost: standard + deluxe editions
  share a release group and still merge — unchanged from the title-key
  behavior, accepted.)
- **Release type alone in the key** — fixes LP-vs-single but not two
  same-titled, same-type releases; kept as the fallback tier, not the rule.
- **Don't strip punctuation from album titles** — would split legitimately
  identical albums whose rips differ in punctuation, the very thing
  normalization exists for.

## The rule

```
AlbumKey = artistKey + "|album-mbrg:" + lowercase(releaseGroupID)   when tagged
         = artistKey + "|album:" + normalizeTitle(album)
                     + "|type:" + primaryReleaseType                when only a release type is tagged
         = artistKey + "|album:" + normalizeTitle(album)            otherwise (unchanged)
```

Tag spellings vary by container and both are read: Vorbis
`musicbrainz_releasegroupid`/`releasetype` (FLAC/OGG) and the ID3v2/MP4
`musicbrainz release group id`/`musicbrainz album type`. A multi-valued
release type ("album; compilation") keys on its first value.

## Consequences

- **No data migration.** Unlike the artist folds (ADR-0037, migrations
  0042/0043), the new key is NOT derivable from stored key text — it needs the
  tags. Music scans re-probe every file's tags already, so the next scan
  re-keys affected Albums/Tracks; the old rows lose their files to the by-path
  reclaim and go hidden (RecomputeHiddenTitles/RecomputeHiddenArtists).
  Watch state on re-keyed Tracks resets (new Title rows).
- A release group split across partially-tagged files (some tracks carry the
  MB tag, some don't) files as two Albums until tagging is fixed — the same
  stance the Movie embedded-id rule takes.
- An Admin folder override (`album-override:` namespace) still overrules
  everything, including the release-group grouping.
- Untagged libraries key exactly as before — zero churn.
