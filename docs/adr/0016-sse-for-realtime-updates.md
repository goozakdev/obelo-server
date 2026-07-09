# Server-Sent Events for real-time updates, not WebSocket

Live updates (scan progress, library changes, and admin-only active-session / now-playing events) are delivered over a single Server-Sent Events stream: `GET /api/v1/events`, scoped to the authenticated user, with admin-only event types gated.

> **Amended (realtime-events feature):** the full event surface this ADR describes is now built. The shipped event types are `enrichProgress` (**broadcast**), `scanProgress` and `libraryUpdated` (**library-scoped**), and `sessionStarted` / `nowPlaying` / `sessionEnded` (**admin-only**). Gating is enforced inside the Broker per subscriber (audience evaluated against the subscriber's identity before enqueue), not merely hidden in the UI. **Library-scoping:** the library-scoped accessible-Library set is resolved from the subscriber's per-User library grants at subscribe time (access-control feature), so a Member's stream carries `scanProgress` / `libraryUpdated` only for the Libraries granted to them, and an Admin (all Libraries) receives every library-scoped event. This was wired into the `/events` handler with no Broker change — the Broker's per-subscriber audience gating was built for exactly this. The decision (SSE over WebSocket) is unchanged.

## Why
The data flow is one-directional (server→client); client→server is already covered by normal HTTP requests. SSE matches that shape, is plain HTTP (sails through the reverse proxy of [ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md) with no upgrade handling), and has built-in auto-reconnect. WebSocket's bidirectionality and framing/upgrade complexity buy nothing here.

## Consequences
- One streaming endpoint to build and secure; event types authorized per role.
- Every event maps to a resource a client could poll as a fallback (`/libraries`, `/libraries/{id}/scan`, `/sessions/{id}/*`) — SSE is an optimization, not the only path to state. (There is no `/sessions` collection endpoint yet; an Admin session-list is a known follow-up — see `api-contract.md` §"Real-time updates".)
