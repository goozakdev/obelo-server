# Audio Stream selection: in-band HLS audio renditions, lazy, demuxed only for multi-audio

A viewer selects among a File's embedded audio Streams (there is no coined "Audio track" concept — the Stream is the selectable unit, see `CONTEXT.md`). Delivery:

- **HLS tiers (remux/transcode)**: every audio Stream is advertised in-band as an `#EXT-X-MEDIA:TYPE=AUDIO` rendition group in the master playlist introduced by [ADR-0020](./0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md). Switching is the player's native in-band mechanism — **no session restart, no server round-trip**.
- **Lazy rendition production**: a rendition's segments are produced only when its media playlist is first requested (same on-demand model as video segments). Streams whose codec the client accepts are stream-copied; incompatible ones (DTS, TrueHD) are transcoded to AAC.
- **Demux only when multi-audio**: a session for a File with 2+ audio Streams uses a video-only variant plus all audio (default included) as URI'd renditions. Single-audio Files keep today's muxed audio+video segments — that pipeline is untouched, mirroring how the master playlist itself appears only when needed.
- **Direct play carries only the default audio**: progressive playback has no rendition surface and the browser `audioTracks` API is unreliable. Explicitly selecting a non-default audio Stream escalates the session to HLS remux (one restart via re-negotiation with an `audioStreamId` parameter, mirroring `burnSubtitleId`); every subsequent switch is in-band. Ordinary default-audio playback stays on the cheap tier — the same escalate-only-on-explicit-selection posture ADR-0020 took for image subtitles.
- **Governance — in-session audio encodes are exempt from the transcode cap** (a scoped amendment of [ADR-0017](./0017-audio-only-playback-path.md)): a lazy audio encode inside an existing video session is unmetered, like remux. At session *start* the initially-selected Stream negotiates normally and can return `503 SERVER_BUSY` as usual.

## Why

The cheaper sibling design — restart + re-negotiate on every switch, exactly like image-subtitle burn-in — was considered and rejected: audio switching is a routine, sometimes-exploratory action (sampling a commentary, flipping dub/original), and two of three client targets are native-HLS players whose built-in audio menu expects in-band renditions. The master playlist ADR-0020 introduced makes the rendition group an extension of existing machinery rather than a new artifact.

Lazy production, not eager: a disc rip can carry 6+ audio Streams; eager encoding multiplies FFmpeg work per session for tracks nobody selects. Lazy costs only a brief buffer on first selection of a track.

The cap exemption resolves a genuine collision: [ADR-0017](./0017-audio-only-playback-path.md) counts audio-only encodes against the cap, but [ADR-0009](./0009-transcode-governance.md) rejects at *negotiation time* — an in-band switch has no negotiation request to reject, only a playlist GET that HLS players cannot surface a structured 503 from. The exemption is bounded: players consume one audio track at a time, so at most one audio rendition encodes per session (abandoned rendition jobs reap with the session), and an audio encode is a few percent of a core — the cap exists to stop *video* encodes saturating the host. ADR-0017's rule stays true where it was written: a standalone music session, where the audio encode *is* the session.

## Consequences

- The segmenter gains a **demuxed mode** (video-only variant + audio rendition playlists, segment cadence aligned) used only by multi-audio sessions; single-audio sessions are unchanged by construction.
- **Asymmetric switching cost, inverted from subtitles**: text subtitles switch instantly everywhere; image subtitles always restart; audio restarts *at most once* (leaving direct play) and is instant thereafter. The player UI should not warn on in-band audio switches but must handle the one escalating switch (and its possible `SERVER_BUSY`) like an image-sub selection.
- The negotiation API grows `audioStreamId` (initial selection and the direct-play escalation path); the Decision and catalog expose the per-File audio Stream list. Both are public client-API surface and versioned accordingly ([ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)).
- Fixes a latent divergence: negotiation already *reports* a chosen audio Stream (`preferredAudioLang` → default → first) but FFmpeg was never given a `-map` for it, so on multi-audio files the reported and audible streams could differ. With renditions (and `-map` on the escalation path), the reported Stream is the delivered one.
- ADR-0017 is amended (in-session exemption); ADR-0004's "no master playlist" mechanic is further amended in the same direction as ADR-0020 (the master playlist now also carries an AUDIO group). No-ABR intent remains intact — the video variant stays single.
