# Audio-only playback path for music Tracks

The three-tier playback negotiation ([ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)) was built for video and gated every tier on the presence of a video Stream: a File with no video resolved to a `ReasonNoVideo` "unsupported" outcome that no tier could satisfy. When the Music kind landed, this made a **Track** (an audio-only File — FLAC, MP3, etc.) unplayable: it could neither direct-play, remux, nor transcode.

Playback now negotiates on the **audio Stream alone** when a File has audio but no video. All three tiers apply to a Track exactly as to a Movie/Episode:

1. **Direct play** — supported container + audio codec.
2. **Direct stream (remux)** — only the container needs repackaging.
3. **Transcode** — unsupported audio codec/bitrate (e.g. FLAC/ALAC → AAC, or a channel downmix); the FFmpeg job maps **no video** (`-vn`) and emits only the audio encode, delivered over the same HLS path (audio-only HLS is valid).

`ReasonNoVideo` is retained, but **narrowed to its true meaning**: the structural-impossibility sentinel for a File with **neither** a video **nor** an audio Stream — the only genuinely unplayable case.

## Why
The original "playback is kind-agnostic" assumption was false for audio-only media: the negotiation and the FFmpeg argument builder both unconditionally required a video Stream. Music is a first-class kind ([CONTEXT.md](../../CONTEXT.md)), so a Track must play through the same tiers and governance as video rather than via a separate audio subsystem. Reusing the existing tier model (and the existing transcode cap) keeps one playback path for all kinds.

## Consequences
- The change is **additive**: the audio-only branch fires only when a File has no video Stream, so the video negotiation/transcode path is unchanged by construction.
- An audio-only transcode is a real FFmpeg encode and **counts against the concurrent-transcode cap** ([ADR-0009](./0009-transcode-governance.md)), consistent with video transcodes — cheap, but governed the same way.
  > **Amended ([ADR-0022](./0022-audio-stream-selection-in-band-hls-renditions.md)):** this rule applies to *standalone* audio sessions (music), where the audio encode is the session. A lazy audio-rendition encode *inside an existing video session* (alternate-audio selection) is exempt from the cap — there is no negotiation request to reject mid-session, at most one such encode runs per session, and the cap targets video saturation. See ADR-0022.
- The playback Decision for a Track carries an audio Stream and no video Stream; clients and the web player must tolerate a video-less Decision (the existing HLS player already does).
- This narrows `ReasonNoVideo` to "no playable stream at all," which is the correct structural-impossibility signal for edition selection.
