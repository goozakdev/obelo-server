# Persistent shell-owned player, not a player route

The web player was a **route**: `/titles/:titleId/play` mounted `PlayerScreen`, which owned the `<video>` element and the [Playback session](../../CONTEXT.md) lifecycle, so navigating away unmounted the player and stopped playback. To let playback persist as the user browses — and to give music a "keeps playing while I wander the app" experience — we **retire that route** and move the single media element + session into one **Now Playing bar** mounted once at the app shell, outside the router's `<Routes>`, driven by the already-global [`useQueue`](../../web/src/player/queue/useQueue.tsx) store. A "Play" affordance no longer navigates; it calls `queue.playNow(...)` and the bar begins playback in place.

## What this means for the UI

- **One element, one session.** The bar owns the only `<video>`/`<audio>` and the only `usePlayerSession`. The session ends on current-entry change (end-before-next), queue clear, logout, or tab close — no longer on navigation.
- **Video surfaces demote, they don't stop.** Watching a video shows a large in-app **immersive stage**; navigating to a browse page demotes the same element to a **custom in-app PiP window** (a styled corner box) that keeps playing. Handing off to the browser's **native Picture-in-Picture** is an opt-in pop-out, deferrable to a later slice.
- **Fullscreen wraps the stage, not the bare video.** The fullscreen control calls `requestFullscreen()` on the immersive-stage container so the app's custom controls (seek, ±10s, queue, etc.) remain overlaid in fullscreen rather than surrendering to the browser's default video chrome.
- **The bar appears only when a [Queue](../../CONTEXT.md) is active** and is a `position:fixed` overlay with a matching content spacer, so no per-screen shell (`app-shell` / `MusicShell`) has to be rewritten.
- **Reload restores paused.** The Queue already survives reload (`sessionStorage`); on restore the current entry loads seeked to its resume position but **paused**, because browsers block autoplay-with-sound without a fresh user gesture.

## Why

The route-owned player made "playback survives navigation" impossible by construction: the element died with the route. Every alternative that keeps a route (mirroring the bar to a route, or portaling the shell element into a route stage) reintroduces two-owner/one-session hazards — double negotiation, element hand-off bugs — for a route we don't otherwise need. A single shell-owned player driven by the store that already survives navigation is the simplest shape that makes the element outlive the page. The Queue store, model, and Play affordances were already global and store-driven, so the churn is concentrated in the player, not the app.

## Consequences

- **`PlayerScreen` and the `/titles/:titleId/play` route are removed.** Resume-seek, capability negotiation, HLS attach, and the negotiation-failure skip move into the shell player, re-keyed by the current entry (preserving the end-before-next invariant the queue player already relied on).
- **Every "Play" affordance stops navigating to the player** and instead builds the Queue and calls `playNow`; the immersive stage is opened as an overlay, not a route.
- **The in-player `QueuePanel` moves** out of the retired screen into a drawer opened from the bar's queue button.
- **Shuffle and Repeat** (music only) extend the pure Queue model, which previously stopped cleanly at the end with no loop; Repeat-all/one are the sanctioned exceptions (see [CONTEXT.md](../../CONTEXT.md)).
- The control set is **driven by the current entry's kind**: skips + fullscreen for video, shuffle + repeat for music, so a mixed-kind Queue swaps controls as it advances.
- **The immersive stage pushes a browser history entry** (not a content route) so Back / Esc / the mobile back-gesture dismiss it to the PiP window without stopping playback — restoring the "back exits fullscreen" reflex the route retirement would otherwise break.
