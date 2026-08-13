# Transcode governance: capped concurrency, reject-don't-queue, HW accel off by default

The server enforces a configurable global cap on concurrent transcodes. Direct play and direct stream (remux) are cheap and unmetered — only full transcodes count.

When a new playback needs a transcode and the cap is full, the server rejects it with a structured "server busy" response so the client can choose to retry at a lower quality. Interactive playback is never queued — no one waits in a spinner to press play.

Hardware acceleration (NVENC/VAAPI/QSV/VideoToolbox) is configurable but off by default; CPU libx264 is the always-available fallback. HW-accel detection/validation is a setup-time concern, not per-stream.

A hardware backend keeps frames in the **device domain end to end** — decode, scale, and encode — rather than copying them back to system memory between stages. Partial offload is not a lighter version of hardware acceleration; it is a different, much more expensive thing wearing the same name.

Per-User/per-Device concurrent-stream limits get a design hook (via the Device registry) but enforcement is deferred.

## Why
Transcoding is the only operation that can saturate the host. Queuing interactive playback produces a worse experience than an honest "busy" signal. HW accel is fiddly and hardware-specific, so it must be opt-in with a guaranteed software fallback.

The end-to-end-on-device rule was learned the expensive way. NVENC originally shipped a deliberately partial pipeline: GPU decode, then an auto-download of every full-size frame to system memory, a CPU `swscale` and `yuv420p` conversion, and a re-upload to encode. It was chosen for robustness across driver and container combinations, on the assumption that the copies cost "a little memory bandwidth."

Measured on a Threadripper 2950X, 1080p 8 Mbps H.264 transcoded down to SD:

| Pipeline | CPU |
| --- | --- |
| Software (libx264) | ~1500% |
| GPU decode + **CPU scale** + NVENC encode | ~450% |
| Full CUDA surface chain (`scale_cuda`) | ~50% |

Hardware decode was engaging throughout — `nvidia-smi dmon` showed a non-zero `dec` column in both hardware rows — so the ~9x between them is purely the copies, the scale, and the conversion. The middle row is the trap: it looks like hardware acceleration, reports as hardware acceleration on the admin surface, and still costs about a third of a software transcode. Against a global concurrency cap, that is the difference between a host serving a useful number of streams and one that saturates almost immediately.

The same shape held on VideoToolbox, where unified memory should have made the copy nearly free: 128% CPU on the partial path against 32% on the surface chain, same clip and target. Cheaper than NVENC's PCIe round-trip, as expected — but still 4x, which is not the rounding error "unified memory" suggests. Two backends, two memory architectures, same conclusion.

## Consequences
- The client API must define the "server busy" response and clients must handle it (reinforces the negotiation contract in [ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md)).
- A future change to add queuing would be a real behavior shift, not a tweak — hence recorded here.
- **This supersedes the "robust baseline" position** in `.scratch/transcode-hwaccel/PRD.md`, which named CPU-scale-then-HW-encode as the target and listed zero-copy surface chains as an explicit non-goal. The measurement above inverts that: the surface chain is the requirement, and a backend that cannot do it is the one needing justification.
- **A new hardware backend owes a device-domain scale filter**, not just an encoder. The `backend` descriptor seam in `internal/transcode/ffmpeg.go` carries `scaleFilter` and `uploadFilter` for exactly this; wiring `encoder` alone silently buys the middle row of that table.
- **The cost of the rule is pixel-format handling.** System-memory `-pix_fmt` no longer applies, so each backend must carry its own 8-bit guarantee in-domain (NVENC: `scale_cuda`'s `format=nv12`) or 10-bit sources fail encoder init and drop to CPU per-session. The per-session fallback keeps that a performance failure rather than a playback one.
- **All four backends are now on the device-domain path.** VideoToolbox was converted last and measured rather than assumed: on unified memory the partial path costs 128% CPU against 32% for the surface chain (1080p 8 Mbps H.264 to 480p, 600 frames). That is 4x rather than NVENC's 9x — the earlier guess that unified memory would make the copy cheap was directionally right and quantitatively wrong. Wall-clock barely moves (1.62s vs 1.55s) because the media engine is the bottleneck either way; what the copy costs is host capacity, which is exactly what the concurrency cap rations.
- **Pixel-format handling differs per backend and is not a matter of taste.** NVENC pins `format=nv12` on `scale_cuda` because `h264_nvenc` cannot encode 10-bit and would fail init on a p010 surface. VideoToolbox pins nothing: `scale_vt` exposes no `format` option, and the encoder converts internally — verified against a 10-bit HEVC source and against an uncapped transcode that emits no scale filter at all. So VideoToolbox has neither of the gaps NVENC still carries.
