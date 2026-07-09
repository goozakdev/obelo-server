# Subtitle delivery: in-band HLS rendition, out-of-band track for direct play, burn-in for image subs

Subtitles reach the client by three paths, chosen by subtitle kind and playback tier:

- **Text subtitles** — embedded text Streams and text Sidecar subtitles (SRT, WebVTT, mov_text, and ASS/SSA **downgraded to plain cues**) — are converted to **WebVTT** and delivered as *selectable* tracks:
  - **Direct play** (progressive `<video>`, byte-range): the WebVTT is served **out-of-band** as an HTML `<track>`, fetched independently of the video bytes.
  - **Remux / transcode** (HLS): the WebVTT is delivered **in-band** as an `#EXT-X-MEDIA:TYPE=SUBTITLES` rendition referenced from a **master playlist**. This introduces a master playlist (one video rendition + a subtitle group) — a scoped reversal of the transcode PRD's "no master playlist" — but stays **single-rendition**: no ABR.
- **Image subtitles** (PGS, VOBSUB, DVD) cannot become text (no OCR). They are **burned into the video frames** on transcode, and only on **explicit selection**. Selecting one **escalates the tier to transcode** — consuming a cap slot ([ADR-0009](./0009-transcode-governance.md)), so it can return `503 SERVER_BUSY` — and switching or clearing it **restarts the session**.
- **Forced** subtitles auto-display for **text subs only**. Forced *image* subs are not auto-burned in v1 (a subs-off viewer of a disc rip stays on the cheap tier).

## Why
Two of three client targets are native-HLS ([ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md): iPhone, Apple TV), where an in-band HLS subtitle rendition is the only reliable selectable-subtitle mechanism — sideloaded `<track>` on native HLS is unreliable. But that flakiness is *native-HLS-specific*: on progressive direct play, out-of-band `<track>` is reliable everywhere (including Safari) and avoids escalating the common "compatible file + subs" case into a managed FFmpeg session. Using each mechanism only where it is reliable beats both a single mechanism with a Safari hole and a universal-HLS tax on direct play. Image subs are bitmaps; burn-in is the only render path, and gating it on explicit selection keeps ordinary disc-rip playback on the cheap tiers.

## Consequences
- A **master playlist** is introduced for HLS sessions that carry subtitles (single video rendition + subtitle group) — a partial reversal of [ADR-0004](./0004-hls-for-adaptive-progressive-for-direct-play.md) and the transcode PRD. ABR remains out of scope.
- **Asymmetric selection cost**: text-subtitle selection is free and instant (swap `<track>` / select the HLS rendition); image-subtitle selection is a tier escalation + session restart. The web player must treat the two differently and surface a possible `SERVER_BUSY` when an image sub is chosen.
- WebVTT for the HLS path must be **segmented** to the video segment cadence (with `X-TIMESTAMP-MAP` for sync); the direct-play path serves a whole `.vtt`.
- ASS/SSA **styling** (karaoke, signs, positioning) is lost on the text path. Preserving it via **libass burn-in** is a future enhancement that reuses the image-sub burn-in machinery.
