# Juice Box — Apple TV client handoff bundle

Self-contained documentation for building the tvOS client in its **own repository**. Copy this directory into the tvOS repo (e.g. as `docs/backend/`) together with a copy of [`../../api-contract.md`](../../api-contract.md) — every document here assumes `api-contract.md` sits alongside it and refers to it for exact endpoint shapes.

**The player is libmpv, not AVPlayer.** That decision runs through every doc here: auth is a plain bearer header on all requests (mpv sends HTTP headers; no cookie tricks), MKV direct-plays without remuxing, embedded subtitles (ASS styling included, via libass) and track switching are handled in-player, and the server serves original-format subtitle files to capable clients (ADR-0033). Build libmpv as **LGPL** for App Store distribution.

| Doc | What it covers |
| --- | --- |
| [integration-playbook.md](./integration-playbook.md) | The choreography: cold start, auth (bearer header incl. mpv media requests), playback state machine, progress/keepalive + track-memory write-back, local track switching, SSE, and the error-recovery matrix. |
| [capability-profile.md](./capability-profile.md) | The exact libmpv `deviceProfile` to send (broad containers/codecs, `textSubtitleFormats` for original-format subs) and the tier each file type lands on. |
| [test-harness.md](./test-harness.md) | Booting a disposable Juice Box backend with generated media fixtures to develop and test against. |
| [design-language.md](./design-language.md) | The Juice Box visual language (flat wireframe, lime accent, media-first) adapted to the tvOS 10-foot, focus-driven UI — including the fully-custom player overlay libmpv requires. |

**Canonical source**: these files are generated from and maintained in the Juice Box server repo (`docs/clients/appletv/`, contract stamped at server commit `0eeda2c`). When the server's API changes, regenerate/update there and re-copy. If a doc here contradicts `api-contract.md`, the contract wins; if the contract contradicts the server, the server wins.
