# Automatic step-down is client-driven over a server-provided ladder

> **Amends [ADR-0035](./0035-editions-are-the-bitrate-ladder.md)** — automatic switching stays the
> client's, but is no longer each client's private policy: the Server now hands every decision a
> Step-down ladder, and the rules for walking it are one shared client spec.
> **Amends [ADR-0009](./0009-transcode-governance.md)** — a negotiation may *replace* a session
> inside the cap check, and audio-only transcodes are no longer metered.

When playback falters — the link cannot carry the bitrate, the device cannot keep up with the
decode, or it cannot decode the codec at all — the client drops to a cheaper way of playing the
same Title on its own, even when that means starting a Transcode. The **client** decides when and
how far to step; the **Server** decides what steps exist. There is still no in-band ABR ladder.

## Decisions

1. **Every playback decision carries a Step-down ladder** — every rung this client may play, best
   first, with the one being played marked `current`. Rungs above it are bounded by the Capability
   profile and the Playback ceiling, never by the client's own bitrate or resolution constraints,
   so a play that started low can still step back up. The ladder is ordered by delivered picture
   height, then bitrate, and is derived from what the Server's planner would actually play for each
   candidate request (Editions that play without a video re-encode, and fixed H.264 Transcode rungs
   down to 480p), so a rung is never a prediction. Music gets its own short ladder: original, AAC 256 kbps, AAC 128 kbps.
   *Amended 2026-10-23:* this replaces "Edition rungs first, then fixed Transcode rungs"; see
   `docs/api-contract.md` §3.6 for the exact shape.
   A rung is only the choice to send back (`editionId` and/or constraints) plus a label; the Server
   holds no ladder state. The full list is returned, not just "next", so a client facing a cliff can
   skip rungs.
2. **Three triggers, two remedies.** Bandwidth (stalls, draining buffer) → step to the rung that
   fits measured throughput. Resource (dropped frames, decoder falling behind) → step one rung.
   Codec incompatibility (decoder error, no frames) → **not a step**: withdraw the codec from the
   Capability profile and re-negotiate at the *same* quality, because the claim was wrong, not the
   quality too high.
3. **Step back up for bandwidth only**, one rung at a time, after sustained headroom (120 s at
   ≥ 1.5× the higher rung, buffer full, no stalls) with a 300 s cooldown between up-steps — the
   values the Apple and Android clients already share and Android measured (30–240 s all held). Resource and codec
   steps describe the device and never revert within a play.
4. **Decode exclusions persist on the Device, client-side** — "HEVC Main10 fails here", "can't keep
   up with 4K HEVC" — and are withheld from every later Capability profile until the app or OS
   version changes or the user resets them. Bandwidth never persists: the next play starts from
   current measured throughput. Nothing goes in server-side Watch state; these are device facts.
5. **A negotiation may name `replacesSessionId`.** Under the same lock that checks the transcode
   cap and the Playback ceiling's stream cap, the replaced session is counted as already gone, the
   new one is minted, and the old one ends after a short grace period (or on its `DELETE`) so the
   client can bridge the switch. Without this, a client at a full cap — or a `remote` User limited to
   one stream — is refused by *its own* session. If the cap is genuinely full, a step into Transcode
   still gets `SERVER_BUSY`; the client tries a lower Edition rung if one exists, else stays put with
   a non-blocking notice. Still reject-don't-queue; no priority for steps.
6. **Audio-only transcodes are unmetered**, like video-copy (ADR-0024) and remux. The cap bounds
   video encodes; an AAC encode is not one, and metering it lets music listeners block a film's step.
7. **Relay:** the sharer builds the ladder; this Server trims rungs above the home User's Playback
   ceiling and forwards `replacesSessionId` translated to the sharer's session. Online items list
   only the rungs the Server can actually produce.
8. **Manual quality pins.** "Auto" is the default; a picked rung disables bandwidth steps both ways
   for that play only. Codec and resource steps still override a pin — otherwise nothing plays — and
   every automatic step is announced briefly.
9. **One normative client spec**, `docs/clients/step-down.md`: triggers, thresholds, per-platform
   signals (libmpv — Apple TV and Android both play on it — and hls.js), step-up rules, music's Track-boundary timing. Client
   playbooks link to it rather than restate it.

## Considered options

- **Server-driven stepping** (clients report stalls on the progress heartbeat, the Server orders a
  re-negotiation). Rejected: only the client sees a stall or a decode error in time, and a 10–15 s
  heartbeat cannot save a stall.
- **An HLS ABR ladder.** Still rejected for ADR-0035's reasons: libmpv does not adapt, so it would
  serve the web client only, and it costs Direct play and lossless audio.
- **Next-rung-only hint.** Rejected: a steep drop would cost one session restart per rung.

## Consequences

- The decision response grows a `stepDown` list and the negotiate request a `replacesSessionId`;
  both are public client contract.
- Music steps on bandwidth land at the next Track boundary unless the buffer is already dry; an
  audio decoder failure skips the Track at once (see below).
- The Apple TV client's ADR-0005 (Edition re-negotiation) is subsumed by the shared spec and should
  point at it.
- **Supersedes the clients' shared Edition-only ABR rules** (Android `docs/beyond-parity/03-abr.md`,
  mirrored by Apple): rule 1 (Edition is the only lever) and rule 5 (ABR only off-Transcode) give way
  to Transcode rungs; rule 3 (a pin makes ABR inert) gives way to codec/resource overriding a pin;
  rule 17 (a failed switch fails the play) gives way to the `SERVER_BUSY` fallback. **Rule 15
  (DELETE the old session before negotiating, else a one-stream User is refused) stays in force
  until the Server ships `replacesSessionId`**, and only then flips to negotiate-first. The shared
  Edition-step notice strings stay until the client spec sets new ones for both clients at once.
- **Cast steps on bandwidth only, and only down.** A Cast sender sees the receiver's buffering state
  but no decode or resource signal, so it never records a Decode exclusion and never takes a resource
  step. It steps one rung down per debounced buffering episode and **never steps back up**
  automatically (amends decision 3 for Cast): the sender's view of the receiver's throughput is too
  coarse to judge headroom. When playback hands back to the local player, the local player starts
  again from the viewer's Quality cap and the full ladder.
- **Music has no codec step.** An audio decoder failure on a Track skips that Track instead of
  re-negotiating (amends decision 2 for music): no audio Decode exclusion is recorded. Music keeps
  its Track-to-Track overlap on Transcode rungs too, so gapless playback survives a bandwidth step.
