# Three-tier playback driven by client capability negotiation

On a playback request the client sends a capability profile (supported containers, codecs, max resolution, max bitrate, current bandwidth). The server picks the cheapest tier that the client can actually play:

1. **Direct play** — stream the File's bytes unchanged.
2. **Direct stream (remux)** — repackage the container / swap a track without re-encoding.
3. **Transcode** — re-encode video and/or audio in real time.

FFmpeg is the remux and transcode engine.

> **Amended ([ADR-0017](./0017-audio-only-playback-path.md)):** this ADR was written for video and originally gated every tier on a video Stream. The three tiers now apply equally to **audio-only** Files (music Tracks): negotiation falls back to the audio Stream when no video exists, and `ReasonNoVideo` is narrowed to mean "no video *and* no audio." See ADR-0017.

## Why
Target clients (iPhone, desktop, Apple TV) have very different codec support and stream over variable connections including cellular, so on-the-fly transcoding is mandatory for the product to work, not optional.

## Consequences
- FFmpeg is a hard runtime dependency of the server.
- The capability-negotiation request/response is a stable part of the public client API and must be versioned carefully — clients depend on it.
- Transcoding is CPU/GPU intensive; the server needs a notion of a bounded set of active transcode sessions (resource limits, hardware-acceleration config).
