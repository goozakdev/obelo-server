# Editions are the bitrate ladder; co-packaged video Streams are content variants

> **Amends [ADR-0025](./0025-selectable-video-streams-in-container-restart-switch.md) (selectable
> video Streams)** on two factual claims: its *"multi-bitrate… for free"* reasoning, which holds for
> CPU but is backwards for bandwidth, and its objection that Editions *"split watch state"*, which
> is not true of the schema as built. Its decisions stand unchanged — the Video menu, the video
> Stream as the selectable unit, the restart switch, no in-band video, no ABR ladder. This ADR adds
> the authoring rule that decides *which* concept a given intent belongs to.

## Two claims in ADR-0025 do not survive the bandwidth case

ADR-0025 justifies the capability-defaulted video Stream this way:

> This delivers the "avoid transcoding" half of the multi-bitrate case *for free and without ABR* —
> a client that cannot decode a 4K HEVC Stream gets a co-packaged 1080p H.264 Stream **direct**,
> chosen once at negotiation.

Literally true, and it does say *"avoid transcoding"* — the "free" is CPU. But it reads as covering
the multi-bitrate case generally, and for **bandwidth it is backwards**. Direct play *"stream[s] the
File's bytes unchanged"* ([ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)),
and Matroska interleaves every Stream into one byte sequence. A client that direct-plays a
co-packaged 4K + 1080p File **receives both video Streams** and discards one after paying for it.
Co-package a 50 Mbps 4K with a 10 Mbps 1080p and the client watching the 1080p pulls 60.

For the case that motivates a ladder at all — a link that cannot carry the top rung — co-packaging
makes it *worse*, and it makes direct play useless exactly when direct play is the only cheap tier
left.

Second:

> Modeling the alternates as intra-File video Streams rather than as Editions is what makes audio
> genuinely *shared*: an Edition bundles its own audio, so B&W and colour as two Editions would
> duplicate the audio and split watch state.

The duplication is real. **The watch-state split is not.** `watch_state` is keyed
`UNIQUE (user_id, title_id)` (`internal/store/migrations/0007_watch_state.sql:31`), and
`WatchStateFor(userID, titleID)` / `SetWatchState(userID, titleID, …)` read and write it by Title.
Two Editions of one Title share resume and watched state **by construction**: watch 30 minutes of
the 4K Edition at home, travel, resume at 30 minutes on the 720p Edition over a WAN. Today.

## Decisions

- **A bitrate ladder is authored as Editions** — one File per rung. Bandwidth-driven rung selection
  at negotiation is **already built and needs no change**: `SelectEdition` runs `Negotiate` per
  Edition and prefers the direct-play bucket outright, so a binding bitrate cap knocks out the top
  rung and `better()` keeps the most pixels among those that fit. Uncapped, it picks the 4K.
- **Co-packaged video Streams remain what ADR-0025 built them for** — content variants that
  genuinely share one audio set (the B&W and colour cuts of Spider Noir). Not bitrate rungs.
- **The authoring rule.** ADR-0025 says packaging decides which concept applies; this decides
  packaging. *Will the viewer ever want a different rung of the same content?* → **Editions.**
  *Are these different cuts that share one audio set?* → **Streams.**
- **Automatic bandwidth-driven switching is the client's, and needs no server change.** `editionId`
  and `startPosition` already exist on the negotiate request. The Apple TV client's **ADR-0005**
  (*Automatic bitrate adaptation is Edition re-negotiation*, in the JuiceBoxPlayer repo) diverges
  from ADR-0025's refusal of automatic switching and explains why. It is deliberately not linked:
  this repo does not assume where a client is checked out, and `[Public]`-scope consumers are not
  its dependencies.
- **No ABR ladder in this ADR.** Deferred — and its audience is narrower than it looked (below).

## Why

**A ladder and a variant set are opposite shapes.** With a ladder you only ever want one rung, so
sharing audio across rungs you will never co-play buys nothing but the coupling that makes direct
play useless. B&W/colour is the mirror image: the same rung, one genuinely shared audio set, and
duplicating a 4GB TrueHD track is the dominant cost. ADR-0025 reasoned correctly about the second
case and its conclusion was read onto the first.

**The storage objection reverses at ladder scale.** Three rungs of a film — a ~60GB 4K, a ~10GB
1080p, a ~4GB 720p — carry ~74GB of video whichever way they are packaged. Editions add only the
duplicated audio: ~8GB against that baseline, about 11%. That is the entire cost of keeping direct
play at every rung, with zero server CPU and no new delivery path.

**Editions switch better, too.** A co-packaged rung change re-negotiates into a remux session
(`-map 0:v:N`, spawn ffmpeg, wait for segments). An Edition change lands in direct play — an HTTP
GET at an offset.

## Consequences

- **The ABR ladder's audience is web-only, which narrows the case for building it.** hls.js adapts;
  **libmpv does not.** Verified 2026-07-15: `ffmpeg -h demuxer=hls` exposes no adaptation AVOption,
  `ffprobe` on a two-variant master playlist surfaces the rungs as static parallel `AVProgram`s
  (chosen once, never switched), and mpv's only lever is the static `--hls-bitrate`. A ladder would
  therefore serve the SPA and never the Apple TV — one of three primary client targets, and the one
  most likely to be on a constrained link. Details in the client's ADR-0005.
- **Keyframe alignment is cheap insurance, no longer a deadline.** A seamless ladder needs IDR on a
  common grid across rungs; it cannot be fixed at delivery (*"you cannot force keyframes on a
  copy"*, [ADR-0024](./0024-direct-stream-video-transcode-audio-fmp4.md)) and needs a re-encode
  after the fact. Force it at authoring anyway — roughly 1–2% bitrate — but with the ladder
  web-only this buys an option, it does not meet a deadline.
- **If a ladder is ever built, alignment must be detected, not trusted.** Misaligned rungs play
  perfectly alone and glitch only at switch time, under load, in the field — the worst diagnostic
  shape available, from an encode months old. The detector already exists: `transcode/keyframes_mp4.go`
  and the Matroska Cues reader produce `SegmentBoundaries`, which *is* the alignment data. Probe
  each rung at scan, compare, mark the Title ladder-eligible or not, and surface why.
- **A ladder also revives the audio-container gate.** `hlsRemuxableAudio` (`negotiate.go`) admits
  only the MPEG-TS set — `aac, mp3, mp2, ac3, eac3` — so an HLS ladder costs lossless audio unless
  the fMP4 path (ADR-0024) is first extended to carry a *copied* FLAC/ALAC/Opus/DTS audio rendition.
  HLS keeps audio in a rendition group that does not switch with video variants
  ([ADR-0022](./0022-audio-stream-selection-in-band-hls-renditions.md)), so a lossless copy could
  ride underneath a ladder — but that gate has to move first.
- **One unverified shape survives, recorded so it is not rediscovered.** Because the demuxer exposes
  variants as parallel streams rather than one adaptive stream, a client could drive `vid` across
  them itself — adaptation logic above a demuxer that will not do it. Untested, and it still
  surrenders direct play and therefore lossless audio, so it is not a plan.
- **Documentation debt.** `docs/clients/appletv/integration-playbook.md` §8 and the capability-profile
  doc describe Edition choice as a quality/capability decision made once; they should note that a
  client may also re-negotiate `editionId` mid-session under bandwidth pressure.
