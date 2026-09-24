# Markers are Local, Detected or Fetched, and detection is a core feature, not a Plugin

A **Marker** is a timed span of a File that a player can offer to skip: an Intro, a Recap, a
Credits, or a Preview. There is deliberately no Commercial kind — v1 has no source that could
find one honestly. This ADR decides where Markers come from, which source wins when more
than one answers for the same File, how a Credits Marker changes the Watched threshold
(`internal/playback/service.go`), and why finding them by listening to the audio is core
server code rather than a Plugin.

## Decisions

**1. Three sources, in a fixed precedence: Local > Detected > Fetched.** **Local** markers
come from the File's own chapters or an edit-decision sidecar (`.edl`), read by the Scanner.
**Detected** markers come from Marker detection (decision 4), the Server's own audio
fingerprinting. **Fetched** markers come from a **Marker provider** Plugin, supplied with the episode length
they were timed for. The precedence exists because each source is measured on
something progressively less like *this exact File*: a Local marker is authored against
these very bytes, a Detected one is measured against this Server's own copy, and a Fetched
one is measured against whatever copy the provider happened to have.

**2. A Fetched marker is rejected if its stated length mismatches this File's.** A Marker
provider states the duration of the recording it measured; the Server refuses a Fetched
Marker timed for a recording more than a few seconds different in length. Unlike a mismatched Lyric
provider answer, which ADR-0063 keeps as Plain rather than discarding, a mismatched Marker
has no lesser shape to fall back to — a Marker is either trustworthy where it is timed or
not worth having, so the Server drops it outright.

**3. Only the Fetched source is a Plugin.** Marker detection needs the media bytes
themselves, which the wasm sandbox (ADR-0058) forbids a guest from ever holding, so it
cannot be one. Local and Detected are the Scanner and the Server's own core code
respectively; only Fetched, answered by a Plugin over the wire, fits the Plugin
contract at all.

**4. Marker detection is a background job, scheduled around transcoding, exempt from the
transcode cap.** It runs after a scan, one Season at a time, at the lowest priority. It never
starts while any Transcode is running and yields at the next safe point the moment one
starts. It never counts against the ADR-0009 concurrent-transcode cap — it is not a
Transcode and does not compete for the same governed resource, even though it is expensive
enough to want the same courtesy toward interactive playback. Per-library, it defaults on
for TV libraries and is absent (not merely off) for music, where the concept does not apply.
An Admin may also trigger "detect Markers now" for one Show.

**5. A Credits Marker starting at or past halfway through File duration becomes that File's
Watched ceiling.** In place of the ordinary ~90%-played Watched threshold (`WatchedCeiling`,
`internal/playback/service.go:64`), a File with a Credits Marker is marked watched — and its
resume position cleared — the moment playback crosses the Credits start. This is still a
server-side fact computed from position, not a client claim (`internal/playback/service.go`'s
`Watched threshold` doc comment already states that invariant; this decision only widens
what counts as the ceiling for a File that has a Credits Marker). The 50% floor exists
because crossing the ceiling clears resume: a Credits Marker sitting at, say, 10% of a
File's length — mis-detected, or a cold-open-heavy cut — would make the Server drop a
viewer's resume point almost as soon as they started, which is worse than not using the
Marker at all.

**6. Clients offer Skip by default, with a per-User server-side auto-skip setting per kind.**
The Credits button becomes "Next episode" whenever Up Next (ADR-0028) has one to offer.
Ships on the web client first; iPad follows.

## Why

TV-general Marker coverage without any form of detection would be thin: most TV releases
carry no chapters and no `.edl`, and a Marker provider only knows about episodes somebody
else already measured. Detection is what makes the feature work on an ordinary library
rather than only on releases somebody has already curated by hand. It has to be core rather
than a Plugin because finding an Intro or Credits by listening requires the decoded media
itself, which is precisely what ADR-0058's sandbox denies every guest — a Plugin gets JSON
in and JSON out, never bytes it could analyze. Scheduling it around Transcodes rather than
against the cap follows this ADR's own reasoning, not a borrowed one: a Transcode is the one
thing that can saturate the host, and detection is CPU work with no viewer waiting on it, so
treating it as the lowest-priority background citizen protects interactive playback the same
way ADR-0009's cap does, without needing to be a Transcode itself, or be governed by that
cap, to earn the same protection.

### Considered

- **Deferring detection.** The orchestrator's recommendation. Rejected: general-TV Marker
  coverage without it would be thin, for the reason above — most TV has no chapters to read
  and no provider that has measured it.
- **Fetched over Detected.** Considered and rejected in favor of Local > Detected > Fetched:
  a Detected Marker is measured against the Server's own copy of the File, which is closer to
  ground truth than a provider's measurement of a copy it never saw.
- **Counting detection as a transcode slot.** Rejected: detection is not a Transcode and
  competing for the same governed cap would starve it behind ordinary playback indefinitely,
  when yielding to an active Transcode already protects the thing the cap exists to protect.

## Consequences

- CONTEXT.md gains **Marker** (kinds and sources), **Marker provider**, and **Marker
  detection**; the **Watched threshold** entry is amended to name the Credits Marker
  exception (already reflected in CONTEXT.md by this bucket).
- `internal/playback/service.go`'s Watched-threshold logic gains a second path: the ordinary
  `WatchedCeiling`/`StartedFloor` position math for a File with no Credits Marker, and the
  Credits-start ceiling (gated by the 50% floor) for one that has one. Crossing either clears
  resume exactly as crossing `WatchedCeiling` does today.
- The per-library Marker detection toggle and the per-Show "detect now" action need an admin
  surface entry; no new ADR is required for either, since both are ordinary admin controls of
  the kind ADR-0029's transcoding surface already established a pattern for.
- A Marker provider's contract joins the closed Extension-point set named in ADR-0057
  decision 1 and ADR-0063's Consequences, as **Marker provider**.

## Back-pointers

None required by this ADR's own decisions. See [ADR-0009](./0009-transcode-governance.md)
for the concurrent-transcode cap Marker detection is deliberately exempt from,
[ADR-0028](./0028-up-next-anchors-on-most-recently-played.md) for the Up Next computation the
"Next episode" button reads, and `internal/playback/service.go` for the Watched threshold
this ADR amends.
