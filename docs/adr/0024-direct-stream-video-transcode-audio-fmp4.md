# Direct-stream video, transcode audio — copy a client-decodable video codec, fMP4 HLS for HEVC

When the transcode tier is entered **only because the audio must change** (a codec the client cannot decode, or a container swap) but the client **can decode the source video codec** at its resolution/bitrate, the server **stream-copies the video** (`-c:v copy`) and transcodes only the audio to AAC — instead of re-encoding the video to h264. A copied non-h264 video is delivered as **fMP4** HLS (`-hls_segment_type fmp4`, an init segment + `.m4s` segments); h264 stays MPEG-TS, unchanged.

Concretely:

- **Copy decision** ([ffmpeg.go] `PlanVideo`): copy the source video when the client's capability profile declares the *source* codec and no resolution/bitrate cap binds — generalized from the old "copy only h264". Otherwise re-encode to h264 as before.
- **Container**: MPEG-TS reliably carries h264 but not HEVC in Safari's HLS (Apple's HLS authoring spec requires fMP4/CMAF for HEVC), so a copied **non-h264** video is delivered as fMP4; every existing path (h264 copy, any re-encode, audio renditions of a TS session) stays MPEG-TS by construction.
- **Playlist ownership**: a copied video has no forced-keyframe 4-second grid (you cannot force keyframes on a copy), so — exactly like remux and the audio-only transcode ([session.go] `ownsPlaylist`) — a video-copy session serves **ffmpeg's own media playlist** (accurate variable `#EXTINF`, and for fMP4 the `#EXT-X-MAP` init line ffmpeg writes) with **no seek realignment**. This reuses the remux runtime rather than the synthesized-playlist transcode runtime.
- **Governance**: a video-copy session does **no video encode**, so it is **unmetered** — not counted against the concurrent-transcode cap, like remux. The cap exists to bound video encodes saturating the host ([ADR-0009](./0009-transcode-governance.md), [ADR-0022](./0022-audio-stream-selection-in-band-hls-renditions.md)); a copy + a cheap AAC encode is not that. The initial negotiation can still busy-reject a *genuine* video re-encode as before.
- **Master playlist**: the video variant advertises a `CODECS` attribute (the copied video codec + `mp4a.40.2`) and the playlist version is bumped so Safari accepts the HEVC variant; a demuxed multi-audio HEVC session carries fMP4 audio renditions so the variant and its renditions share one segment type.

## Why

Re-encoding 4K HEVC → h264 in real time is infeasible on a self-hosted CPU and can't sustain playback (the reported bug: a 4K HEVC remux with TrueHD audio never plays — the TrueHD forces a transcode, which then re-encodes and downscales the 4K video). Even with hardware acceleration it is wasteful and needlessly downscales a stream a HEVC-capable client could play untouched. Copying the video is near-free, preserves full quality, and confines the cost to the audio the client actually can't decode — the true "direct stream" spirit ([CONTEXT.md](../../CONTEXT.md) *Direct stream*) extended to cover an accompanying audio transcode.

fMP4 (not MPEG-TS) is mandatory because Safari — the motivating client and one of three native-HLS targets — does not play HEVC in MPEG-TS segments; fMP4/CMAF is Apple's supported HEVC-HLS container.

## Consequences

- A new delivery shape — **video-copy + audio-transcode**, ffmpeg-owned playlist, unmetered — sits under the existing `transcode` tier as a `VideoCopy` sub-mode (the negotiation still routes there because the audio forces it). It reuses the remux runtime's playlist ownership and the transcode args' audio encode, so it adds a flag and two predicate tweaks rather than a new tier.
- fMP4 adds an **init segment** and `.m4s` segments to the HLS serving surface (new content-types + a by-name init-segment serve path); the MPEG-TS path is untouched.
- Applies only when the client declares the source video codec. A client that cannot decode the source video still gets the h264 re-encode (unchanged), so nothing regresses for non-HEVC-capable clients.
- The web capability profile's honesty matters here too: a browser that advertises HEVC it cannot actually decode would now get a copied HEVC stream it can't play — the same false-positive class as the AC3 audio fix. Browser HEVC is probed via `canPlayType('video/mp4; codecs="hvc1…"')`, which Safari answers honestly for the hardware it runs on.

## Addendum (2026-07): fMP4 is Safari-only — hls.js clients take copied HEVC over MPEG-TS

The fMP4 requirement above is **Apple's native player's**, not HLS's. An hls.js
(MSE) client demuxes HEVC-in-TS itself (hls.js ≥ 1.6), and strict MSE playback
turned out to *need* the TS pipeline: the fMP4 path keeps ffmpeg's hls muxer,
whose keyframe-grid cuts can drift from the server-synthesized playlist
(incomplete Matroska Cues; a post-seek grid re-anchored at the seek point), and
hls.js stalls on the divergence (`bufferStalledError` ~1s after a resume) where
Safari's native player tolerates it. The TS pipeline has none of that: the
segment muxer takes **dictated cut times** (`-segment_times` at the predicted
boundaries), so its segments match the synthesized playlist by construction,
from the top and across a seek realignment — verified byte-exact against a 78GB
UHD remux.

So the capability profile now carries `hevcInMpegts` (set by the web client to
"not on the native-HLS path"), and `UsesFMP4` — the single container authority
the job plan, session runtime, and playlists all read — keeps a copied HEVC on
MPEG-TS for such a client. Safari (no flag) keeps fMP4 exactly as specified
above. ffmpeg muxes HEVC into MPEG-TS fine (stream type 0x24); Apple's
"HEVC must be fMP4" rule binds only its own player.
