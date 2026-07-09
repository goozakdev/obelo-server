# Opaque DB-backed auth tokens, not JWTs

Auth tokens are opaque random strings stored hashed in the SQLite database and validated by lookup on each request. They are not self-contained JWTs.

## Why
Q8 / the Device model require that revoking a Device kills its token immediately. JWTs are self-contained and valid until expiry, making instant revocation hard (needs a blocklist that reintroduces per-request state anyway). An opaque token makes revocation a one-row delete, and per-request DB validation is trivial at household scale on the single embedded DB ([ADR-0007](./0007-sqlite-plus-filesystem-caches.md)).

## Consequences
- Every authenticated request does a token lookup — fine at this scale, and it lets us cheaply attach the Device/User and enforce scope/role per request.
- No stateless multi-node validation, but the monolith ([ADR-0006](./0006-docker-first-modular-monolith.md)) doesn't need it.
- A client persists a stable `clientId` (UUID); re-login reuses the existing Device rather than creating duplicates.
