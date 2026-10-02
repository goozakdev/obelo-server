# A Decision Stream reports the index an HLS player sees, beside the container's

A Decision's audio and video Streams carry an optional `playerIndex`: the Stream's index as a player
that opens the HLS playlist the Decision points at numbers it. It is present on every HLS Decision
(`directStream`, `transcode`) for the Streams the player sees. `index` keeps meaning the container's
FFmpeg index.

## The gap

`index` is the Stream's stored ffprobe index in the source File, and a Decision reports it unchanged on
every tier. On direct play that is also what a player sees, because the player opens the File. On a
demuxed HLS Decision it is not: the master playlist lists one audio rendition per audio Stream in File
order, then the subtitle renditions, then the single video variant, so a player demuxing it numbers
them 0, 1, … in that order. For a File with the video at container index 0 and audio at 1 and 2, the
Decision says audio 1, 2 while the player sees audio 0, 1. A client that joins its player's track list to
the Decision on index (libmpv clients do: its own `aid` restarts per type, so the container index is the
only stable key) then finds no Stream — and a Remembered audio pick on a non-default track is not
re-applied on replay, because the server answers that negotiation with exactly such a master.

## Decision

1. **Add `playerIndex`, do not redefine `index`.** `index` is also what Title detail's `streams[].index`
   reports, what existing clients correlate on, and what direct play uses. One field meaning two things
   by tier would break all three. A new optional field breaks nothing.
2. **Where it is set**: `audioStreams[]`, `videoStreams[]` (the played video only), `audioStream` and
   `videoStream`, on every `directStream` and `transcode` Decision. Never on the catalog's lists.
3. **How it is numbered**: the playlist's own order — AUDIO renditions, SUBTITLES renditions, then the
   one video variant. *Demuxed* (`playback.IsDemuxed`): audio *k* is `k`, the played video is the number of
   audio Streams plus deliverable text subtitle renditions. *Muxed variant* (single audio, and
   `remuxSelectedOnly` on a multi-audio File): the video is the number of deliverable text subtitle
   renditions and the played audio is that plus one — the variant carries the video, then the audio, and
   only the played audio Stream is reported. The `remuxSelectedOnly` master still lists an AUDIO group
   (`SessionAudioContext` ignores the flag) whose renditions are never produced; a player drops them, so
   they take no index (that dead group is a separate defect, left as it is). One function,
   `playback.HLSPlayerIndexes`, computes it from the same deliverable-text-subtitle filter
   (`playback.DeliverableTextSubtitles`) the master uses; a test builds each layout, serves it and holds
   the number to an ffprobe of the served playlist: TS and fMP4, transcode, two video Streams, and a
   non-default audio pick whose container index differs from the player's.
4. **Where it is omitted**: `directPlay` (the container is the player's view, so `index` is right) and
   audio-only (the player sees one audio Stream). A client falls back to `index` on `directPlay` only; on
   an HLS Decision, a Stream with no `playerIndex` is not in the player.
5. **Relay**: a linked-Library Decision is the sharer's, passed through whole (ADR-0056), so its
   `playerIndex` rides unchanged and still matches the master that is relayed.
6. **The subtitle set is frozen with the Decision.** The master is built when it is fetched, the index
   when the Decision is negotiated, and a sidecar can appear in between. The Session therefore keeps the
   Subtitle tracks the Decision offered and the master's SUBTITLES group is built from them, not from the
   store at fetch time, so the two cannot drift.
7. **No `features` key.** Keys exist so a client can avoid sending something an older server rejects or
   serving an affordance it lacks. This is an extra response field: an old client ignores it, and a new
   client reads absence as "use `index`" on `directPlay`, the one place that is true.

## Rejected

- **Rewriting `index` on HLS Decisions** — fixes a join with no client change, but one field then means
  two things and stops matching Title detail.
- **Making the master keep container order** — the player numbers renditions by playlist order and the
  master lists audio before video by construction; reordering the master is not available.
