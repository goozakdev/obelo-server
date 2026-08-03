# The transcode tier is advertised from a startup-resolved ffmpeg availability

Whether this host has a usable ffmpeg is resolved **once at startup** — the binary is located and actually run — and that single answer feeds both the transcode Runner and the handshake's `features.transcode` flag. A server with no working ffmpeg still boots and still serves direct play; it simply stops claiming the remux/transcode half of [ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md).

Negotiation is unchanged: it still reports the tier the *media and the profile* require, so a client can still be told `transcode` on a server that cannot serve it. The flag — not the decision — is where "can this host do it" is answered.

## Why

`features.transcode` was hardcoded `false` while the tier worked, and the contract tells every client to branch on flags rather than version strings — so the more disciplined the client, the more wrong its behaviour. Computing the flag was the fix, but "compute it from what?" had no answer: nothing in the server knew whether ffmpeg existed. [ADR-0009](./0009-transcode-governance.md) resolves *which encoder* to use, and its answer is never "none" (CPU libx264 is the guaranteed fallback), so it cannot answer the question underneath it.

The flag also had to become genuinely informative, not merely correct. A deployment with no ffmpeg is a real state and one a client wants to know about: an AVPlayer-style profile that cannot demux Matroska has nowhere to go there, and the right behaviour is to hide the affordance rather than offer it and collect an error.

Resolution is setup-time for the same reason [ADR-0009](./0009-transcode-governance.md) gives for hardware-accel detection ("a setup-time concern, not per-stream"), with one added constraint: `GET /server` is unauthenticated and every client hits it on connect, so it must never spawn a process.

Locating the binary is not enough — a file on `PATH` that cannot execute (a truncated download, a wrong-architecture binary, a dangling symlink into a vanished container layer) passes a `LookPath` check and then fails at the first playback, which is precisely the state the flag exists to warn clients away from. So the probe runs `ffmpeg -version`.

## Consequences

- **A feature flag is now host-dependent.** Two identical builds can advertise different feature maps. `transcode` is the only such flag, and the only one whose truth `TestFeaturesMatchRoutes` cannot check by probing the route table — which is why it is excluded there and pinned by its own both-directions test instead.
- **ffmpeg's "hard runtime dependency" (ADR-0003) is now degraded, not fatal.** Its absence costs the remux and transcode tiers and is logged loudly at boot; it does not stop the server.
- **One resolution, not two.** The Runner invokes the ffmpeg that was resolved, so the server cannot advertise one binary's availability while spawning another. The pinned-answer seam therefore controls both, and a test that says "this deployment has no ffmpeg" gets a server that both advertises and behaves that way.
- The failure mode for a client that ignores the flag is unchanged and honest: a transcode-tier media request fails immediately with an enveloped `500 INTERNAL` when ffmpeg cannot be spawned. It does not hang waiting for output that will never arrive.
- This says nothing about *which* encoder or whether hardware acceleration is available — that is [ADR-0009](./0009-transcode-governance.md), and the two resolutions stay separate.
