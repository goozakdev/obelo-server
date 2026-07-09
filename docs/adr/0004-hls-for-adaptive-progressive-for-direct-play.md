# HLS for remux/transcode delivery; progressive HTTP for direct play

Direct play is delivered as progressive HTTP byte-range requests against the File. Anything remuxed or transcoded is delivered as HLS: FFmpeg generates segments on demand, listed in a media playlist. Seeking maps a timestamp to a segment and, for transcode, realigns the FFmpeg job near that point.

No MPEG-DASH in v1.

> **Amended ([ADR-0020](./0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md)):** the HLS delivery here was a bare media playlist with no master playlist. Subtitle delivery introduces a **master playlist** (one video rendition + an `#EXT-X-MEDIA:TYPE=SUBTITLES` group) for HLS sessions carrying subtitles. This stays single-rendition — no ABR — so the "no ABR ladder" intent of this ADR is unchanged; only the "no master playlist" mechanic is. See ADR-0020.
>
> **Further amended ([ADR-0022](./0022-audio-stream-selection-in-band-hls-renditions.md)):** the master playlist also carries an `#EXT-X-MEDIA:TYPE=AUDIO` group for multi-audio Files, whose sessions demux into a video-only variant + per-Stream audio renditions (produced lazily). The video variant remains single — still no ABR. See ADR-0022.

## Why
Two of three primary client targets are Apple (iPhone, Apple TV) where HLS is the native/required adaptive format; browsers can play HLS via MSE. Supporting DASH as well would roughly double the delivery surface for little benefit.

## Consequences
- Single segmented-delivery code path to build and test.
- Seeking within a live transcode requires segment/timestamp realignment logic, not just a byte offset.
