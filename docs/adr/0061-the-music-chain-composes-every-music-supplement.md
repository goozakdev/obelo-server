# The music chain composes every music Supplement

[ADR-0059](./0059-the-shipped-metadata-providers-are-bundled-plugins.md) decision 11 left "the
music chain's two hardwired Supplements" as a pre-existing gap for a third-party provider, and
decision 4 rejected a real Cover Art Archive plugin because the chain could not compose one. This
ADR closes that gap. See
`.scratch/bundled-plugins/issues/11-follow-up-the-music-chain-composes-supplements-generically.md`.

## Where it stood

`MusicChainProvider` had two fields, `Image` and `ImageBio`, and `BuildProvider` filled them
positionally from the first two active artwork-only music providers in registration order
(`musicChainSupplementSlots = 2`). The chain then applied rules written for the two shipped
sources: ask `Image` only for an artist and only with an MBID; ask `ImageBio` for an artist
and for a track; let `ImageBio`'s biography **replace** the lead's artist Overview. A third
music Supplement was registered, keyable, and never composed. The video chain, meanwhile, was
already a plain ordered fill: lead, then each Supplement in order, each self-gating by
no-match.

The two shipped manifests declare exactly the same thing (`supplement`, `artwork`,
`artwork-candidates`), so nothing a plugin declares could tell `Image` from `ImageBio`. The
only thing that distinguished them was their order.

## Decisions

**1. The music chain is the video chain's shape.** The lead runs, then every active music
Supplement in registration order, each filling only what is still empty. The shipped pair
keep their behaviour because each already self-gates: fanart.tv no-matches anything but an
artist with an MBID, TheAudioDB anything but an artist or a track, both without a network
call. So fanart.tv's artist photo still wins over TheAudioDB's, because it registers first and
fill-only keeps the first image per role, and a track still gets TheAudioDB's synopsis. Named
"image" and "biography" slots that a Supplement declares for were rejected: more contract
surface, and still one source per slot.

**2. A lead declares a synthesized Overview, and a synthesized Overview fills like an empty
one.** `MetadataRecord` gains `overviewSynthesized`. MusicBrainz sets it on its artist blurb
("English rock band from Oxford"), which it composes from type and area. The shared fill rule
takes a Supplement's Overview when the lead's is empty, or when the lead's is synthesized and
the Supplement's is not. A synthesized Supplement answer still fills an empty Overview, and
stays replaceable. This replaces "TheAudioDB's bio always wins", which was a rule about a name.
Two alternatives were rejected:

- A host rule that "an artist Overview is always replaceable" would override a future lead
  that holds a real biography.
- Strict fill-only would lose the biography, a visible regression.

The field is additive and zero-valued for every older guest.

**3. One fill rule for both chains.** `fillFromSupplement` is what both chains apply: Name
(display-only), Overview (as above), ContentRating, Genres, and Artwork by role. One addition:
a Supplement never fills the Name of a record found by search, because that Name is the
candidate's own title and it is what the host judges the record by (ADR-0050).

**4. Which providers the music chain composes.** It composes every active music provider,
other than the lead, that declares the `supplement` role or is artwork-only. The video chain
composes *every* other video provider, including an authoritative one behind a repointed lead.
Music deliberately does not. A music lead's lookup of a ref it holds no id for is a relevance
search, and a Supplement's search hit is filled in unjudged. No second Full music provider
ships, so this changes nothing today.

**5. Every music kind reaches the Supplements.** An album is no longer passed straight through,
so a Cover Art Archive-style Supplement can fill a cover the lead left empty. The skip rule
that spares Supplements a record the host will reject (`worthDecorating`) now gates every
music kind, since every music kind's search hit is judged by the same test.

**6. The artwork picker is lead-first, then every Supplement's.** For an artist, MusicBrainz
answers nothing and fanart.tv's thumbs lead as before. For an album, the lead's Cover Art
Archive covers lead and any album Supplement's follow. The lead's error is returned only when
nothing produced a candidate.

## Consequences

- **Behaviour changes for the shipped pair are cost, not output.** fanart.tv is now asked
  about tracks, albums and MBID-less artists, and TheAudioDB about albums. Each answers
  no-match inside its own guest with no fetch. MusicBrainz is now asked for artist artwork
  candidates, and answers an empty list without a fetch. A track whose lead already has an
  Overview now still asks TheAudioDB (none does today).
- Artist search hits the host is about to reject no longer cost fanart.tv and TheAudioDB a
  request each.
- Supplements are now asked with the caller's ref plus the lead's resolved id in its own
  namespace (ADR-0060), where before they got a narrowed ref. AlbumHints and the rest of the
  ref now reach them.
- ADR-0059 decision 4's reason for rejecting a Cover Art Archive plugin no longer holds. The
  decision itself stands: Cover Art Archive stays MusicBrainz's second URL until someone wants
  it as a Supplement.
- A Supplement author's contract is: answer no-match, cheaply, for what you don't serve. The
  authoring guide says so.
