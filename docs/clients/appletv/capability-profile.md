# Apple TV (libmpv) — recommended capability profile

What to send as `deviceProfile`/`constraints` in `POST /titles/{id}/playback`, and which tier each common file type will land on. Field shapes: `api-contract.md` §3.6. **The player is libmpv**, not AVPlayer — mpv demuxes and decodes through ffmpeg, so the profile is broad and nearly everything direct-plays.

## How matching works (server-side)

- **Containers** are normalized before comparison (`matroska`↔`mkv`, the `mov,mp4,m4a,…` family → `mp4`), so short tokens are safe. A container the profile omits forces at least directStream (remux) — with mpv there's no reason to omit any common one.
- **Video codecs** match per-codec with optional `maxResolution` caps; `maxLevel` and `hdr` are recorded but **not enforced** — don't rely on them.
- **`textSubtitleFormats` is enforced** ([ADR-0033](../../adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)): declaring `ass`/`srt` makes the decision's subtitle URLs point at **original-format bytes** (ASS styling intact — libmpv/libass renders them natively) instead of the WebVTT downgrade. Declare all three.
- `constraints.maxBitrate` (bits/sec) is what most often forces a transcode — set it from network conditions / user quality cap, `0` = uncapped.

## Recommended profile — Apple TV (all models)

```json
{
  "deviceProfile": {
    "containers": ["mkv", "mp4", "webm", "avi", "ts"],
    "videoCodecs": [
      { "codec": "h264" },
      { "codec": "hevc", "hdr": ["hdr10", "dolbyvision", "hlg"] },
      { "codec": "mpeg4" },
      { "codec": "vp9" },
      { "codec": "av1" }
    ],
    "audioCodecs": ["aac", "ac3", "eac3", "flac", "alac", "opus", "vorbis", "mp3", "dts", "truehd", "pcm_s16le"],
    "maxAudioChannels": 8,
    "textSubtitleFormats": ["ass", "srt", "vtt"],
    "hevcInMpegts": true
  },
  "constraints": {
    "maxBitrate": 0,
    "preferredAudioLang": "<user pref or device locale>",
    "preferredSubtitleLang": "<user pref>"
  }
}
```

Notes:
- **Everything in one static profile** — mpv software-decodes what the SoC can't hardware-decode. Hardware decode (VideoToolbox) covers H.264/HEVC on every Apple TV; VP9/AV1/mpeg4 fall back to software, fine at TV bitrates on A12+. Don't branch per model unless profiling shows AV1 software decode struggling on Apple TV HD — then drop `av1` there.
- **`hevcInMpegts: true`**: mpv happily plays HEVC in MPEG-TS HLS segments, so copied-HEVC transcode sessions can skip the fMP4 path. (An AVPlayer client must say `false`; mpv shouldn't.)
- **DTS/TrueHD declared**: mpv decodes them to PCM. Declaring them keeps the server from transcoding audio on directStream sessions. Passthrough to a receiver is a tvOS output question, not a negotiation one.
- No per-codec `maxResolution` caps: let the server serve the file; use `constraints.maxBitrate` for network-driven limits instead.
- Re-send changed `constraints` via a fresh negotiation (user quality setting, big bandwidth swings). Progressive direct play is bitrate-agnostic anyway — a cap mainly matters when it forces the transcode tier.

## Expected tier outcomes

| File | Tier | Delivery |
| --- | --- | --- |
| MKV/MP4/WebM, any listed codec | **directPlay** | Progressive byte-range `/sessions/{id}/stream` — original bytes, mpv demuxes; embedded subtitle/audio/video tracks all available in-container |
| Anything with `maxBitrate` below the file's bitrate | **transcode** | HLS (mpv plays HLS natively) |
| Corrupt/unlisted exotic codec | **transcode** or `501 TRANSCODE_REQUIRED` | Per negotiation |
| Image-subtitle burn-in (`burnSubtitleId`) | **transcode** | Only needed on already-transcoding sessions — on direct play mpv renders PGS/VOBSUB itself; never escalate just for subtitles |
| Audio-only (music Tracks) | direct or audio path | Same negotiation |

Consequences to design for:
- **Almost every session is directPlay** — cheap for the server (no cap slot), instant seek via byte-range, and all in-container tracks (audio/video/subtitle, incl. image subs) are handled by mpv locally with no server round-trip.
- The transcode tier appears mostly when the user caps quality on a fat file — handle `503 SERVER_BUSY` + `suggestedMaxBitrate` there (playbook §8).
- The decision's `subtitles[]` still matters on direct play for **sidecar/fetched** tracks (they're not in the container): load them with `sub-add <url>`, preferring the original-format URLs the broad profile earns.
