# Server identity (id + name) and mDNS advertisement — making ADR-0005's discovery promise real

> **Amends [ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md).** Does not supersede it:
> the remote-access and TLS-via-reverse-proxy postures stand unchanged. This ADR implements the
> discovery half, which was decided but never built.

[ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md) states:

> LAN discovery: the server advertises itself via mDNS/Bonjour for zero-config pairing on Apple
> clients, with manual address entry as the fallback.

**None of that shipped.** `go.mod` declares three direct dependencies (`uuid`, `crypto`,
`sqlite`) and no mDNS library; there is no `mdns|bonjour|zeroconf|dns-sd` anywhere in the Go
source. The "fallback" is the only path that exists — and it reads as a kept promise to anyone
building a client against the ADR. The Apple TV client is now being built, which is exactly the
"Apple clients" case the ADR was written for, so the gap is no longer theoretical.

A second ADR-0005 promise was also unbuilt — *"The server provides a configurable external URL"* —
and this ADR flagged it as out of scope but not to be left sitting unexamined. It has since been
examined: **the claim was retired unbuilt on 2026-07-15**, because the server emits no absolute
self-referential URL for an external URL to correct — every URL it emits is relative, so a
proxied client stays on the proxy's origin on its own. See the retirement note in
[ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md) for the evidence and for the
origin-root assumption it documents. Nothing here depends on it either way.

## Scope

- **LAN only.** mDNS is a link-local mechanism. A reverse-proxied or VPN-reachable instance is
  not discoverable and never will be — manual entry stays the permanent path for remote access,
  not a stopgap.
- **Advertisement only.** The server announces; it does not browse, pair, or authenticate
  differently. Discovery replaces *typing an address*, nothing more — a discovered server still
  requires a full `POST /auth/login`.

## Decisions

- **A Server gains an identity: a stable `id` and a display `name`.** The `id` is a UUID
  generated once on first boot and persisted in `JUICEBOX_DATA_DIR` alongside the SQLite file —
  the same durability boundary as every other piece of server state, so "reset the data dir" (the
  documented cheapest reset) correctly mints a new identity. The `name` comes from a new
  `JUICEBOX_SERVER_NAME`, defaulting to the host's name.
- **Both are added to `GET /server`.** This is **additive** — per the contract's own rule,
  *"Additive changes (new fields/endpoints) never bump it"* — so no `/api/v2`, and existing
  clients are unaffected:

  ```json
  { "id": "…", "name": "Living Room", "version": "0.1.0",
    "supportedVersions": [1], "features": { … }, "setupRequired": false }
  ```

- **The server advertises `_juicebox._tcp` on its listen port**, with a deliberately small TXT
  record (per RFC 6763, TXT is a hint; the client confirms against the real protocol):

  ```
  txtvers=1  id=<uuid>  name=<display name>  path=/api/v1
  ```

  A discovered server is always plain `http` — the server binds plain HTTP, and a TLS-terminating
  proxy is not on the local link by definition.
- **Advertisement is best-effort and never fatal.** A failure to register logs and is ignored;
  the server serves regardless. Discovery is a convenience, and an unadvertised server is still
  fully usable via manual entry.

## Why

The picker — "choose Juice Box from a list" instead of thumbing `http://192.168.1.50:8080` into
an on-screen keyboard with a Siri Remote — is the visible win, and it alone justifies the work.
That is the worst first-run experience the client has, and it lands at the exact moment a user
decides whether the product feels real.

**But the identity is the larger prize, and it is why this is more than a UX fix.** Today, a DHCP
lease change silently breaks every client: the stored base URL points nowhere, and the user
re-types an address. The bearer token is still perfectly valid — it is bound to a Device row in
this server's database ([ADR-0015](./0015-opaque-db-backed-tokens.md)), not to an address — but
the client has no way to *find* the server it is still authenticated to. With `id` in the TXT
record, the client browses, matches the identity it stored, updates the base URL, and keeps its
token. The user never notices.

**Identity also makes "is this the same server?" answerable at all**, which today it is not.
`GET /server` returns a version and some flags; two different servers on two different LANs are
indistinguishable. That question underpins any future multi-server client, and it costs one
column to answer.

**`id` and `name` are separate on purpose.** The `id` is machine-facing, stable forever, and
never shown. The `name` is human-facing and freely changeable — renaming a server must not orphan
a single token. Collapsing them into one field would couple those lifetimes and guarantee the
bug.

## Consequences

- **One new Go dependency** (a zeroconf/mDNS responder) in a module that currently has three
  direct ones. Weighed and accepted: the alternative is hand-rolling a multicast DNS responder.
- **`GET /server` grows two fields**, and its handler needs the server metadata that
  `handleServerInfo` already receives. `serverInfoResponse` (`internal/api/server_info.go`) gains
  `Id` and `Name`.
- **First boot gains a side effect** — minting and persisting the identity — joining the claim
  token ([ADR-0013](./0013-first-admin-claim-token-bootstrap.md)) as things that happen once. It
  must be generated *before* the first advertisement.
- **Resetting `JUICEBOX_DATA_DIR` changes the server's identity**, so clients treat it as a new
  server and must re-login. Correct — a wiped data dir has no Users, no Devices, and no tokens to
  honor — and consistent with `test-harness.md`'s reset story.
- **`GET /server` is `[Unauthenticated]`**, so `id` and `name` are public to anyone who can reach
  the port, as is the mDNS record to anyone on the link. Neither is a secret: the id is an opaque
  UUID granting nothing, and the name is chosen by the operator. No access-control change.
- **`CONTEXT.md` gains the term _Server identity_.**
- Multi-server clients become possible without further server work. Not built, not promised — just
  no longer blocked.
