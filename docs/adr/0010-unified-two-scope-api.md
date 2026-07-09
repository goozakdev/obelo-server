# One unified HTTP/JSON API with public and admin scopes

There is a single API. It has two scopes:

- **Public scope** — browse, search, playback/negotiation, watch state, playlists. Shared by the management web app and all client apps (iPhone, desktop, Apple TV).
- **Admin scope** — libraries, scans, users, devices, active session monitoring/control, settings. Gated to Admins; used only by the web app.

The management web app is a first-class client of the public scope — not served by a separate internal API.

## Why
Dogfooding: if browse/play is awkward for the web app it is awkward for every client, and a unified surface surfaces that immediately. A separate web-app API would let the public client API rot and double the maintained surface.

## Consequences
- The public scope must be treated as a real, versioned, stable product from day one (the web app and future/third-party clients all depend on it) — reinforces the versioning concern in [ADR-0003](./0003-three-tier-playback-with-capability-negotiation.md).
- Authorization must enforce scope + role per endpoint, not per-client-type.
