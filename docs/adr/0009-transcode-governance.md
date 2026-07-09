# Transcode governance: capped concurrency, reject-don't-queue, HW accel off by default

The server enforces a configurable global cap on concurrent transcodes. Direct play and direct stream (remux) are cheap and unmetered — only full transcodes count.

When a new playback needs a transcode and the cap is full, the server rejects it with a structured "server busy" response so the client can choose to retry at a lower quality. Interactive playback is never queued — no one waits in a spinner to press play.

Hardware acceleration (NVENC/VAAPI/QSV/VideoToolbox) is configurable but off by default; CPU libx264 is the always-available fallback. HW-accel detection/validation is a setup-time concern, not per-stream.

Per-User/per-Device concurrent-stream limits get a design hook (via the Device registry) but enforcement is deferred.

## Why
Transcoding is the only operation that can saturate the host. Queuing interactive playback produces a worse experience than an honest "busy" signal. HW accel is fiddly and hardware-specific, so it must be opt-in with a guaranteed software fallback.

## Consequences
- The client API must define the "server busy" response and clients must handle it (reinforces the negotiation contract in [ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)).
- A future change to add queuing would be a real behavior shift, not a tweak — hence recorded here.
