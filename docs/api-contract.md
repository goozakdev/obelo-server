# API Contract

The single HTTP/JSON API with public and admin scopes ([ADR-0010](./adr/0010-unified-two-scope-api.md)), consumed by the web app and all clients. Treated as a versioned product because clients and server update independently.

> **Generated from source at commit `843c7ea` (2026-09-04)** — which completes **linked servers** ([ADR-0054](./adr/0054-a-linked-server-is-a-user-with-the-remote-role.md), [ADR-0055](./adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md), [ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)): the `remote` role and the Playback ceiling on §3.2, the invite and its redemption on §3.1, the Library Export on §3.3, the new §3.11 **Links** with the one-hop relay, the `linked`/`available` fields on Libraries and **every browse row** (§3.3, §3.4, §3.5), the `linkState` event, and eleven new error codes. Extracted from handler/DTO structs in `internal/api` and verified against a live instance. Every JSON field name below is a verbatim struct tag; examples are captured or derived from real responses. If code and this doc disagree, the code wins — regenerate by re-running the extraction against `internal/api/*.go` and `internal/events/broker.go`.

**Reading this catalog:**
- Every path lives under **`/api/v1`** (`APIPrefix`, `internal/api/api.go`). The prefix is stripped before dispatch; unknown paths under it return the enveloped `404 NOT_FOUND`, never a plain-text 404.
- Each endpoint is tagged **[Public]** (any authenticated User), **[Admin]** (requires `role: "admin"`), or **[Unauthenticated]**. Two routes carry a fourth tag, **[Stream token]** — they take no bearer and no cookie, only the session-scoped media credential in their path (§Auth, [ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)).
- **An Apple TV / native client needs only the [Public] endpoints** plus `/server`, `/setup`, `/auth/*`, `/devices`. The [Admin] scope is otherwise the management web app's (ADR-0010) — with one deliberate exception: an iPhone/iPad app that scans an invite QR posts it to `POST /links` (§3.11), which requires an Admin bearer on the **user's own** Server.

---

## Part 1 — Invariants (the stable promises)

### Style & versioning

- **REST-ish JSON over HTTP**, resource-oriented, **camelCase** field names.
- **Versioning via URL path** — `/api/v1/…`. One integer major version. Additive changes (new fields/endpoints) never bump it; only breaking changes mint `/api/v2`, and the server may serve both during a transition.
- **Handshake** — `GET /server` returns server version, supported API versions, and a **feature-flags** map. Clients branch on feature flags, not version strings. A flag means "this server serves these routes"; `TestFeaturesMatchRoutes` holds the map to that meaning by probing the routes. The one exception is **`transcode`**, which advertises the transcode *delivery tier* rather than a route: it is computed at startup from whether this host has a usable ffmpeg ([ADR-0040](./adr/0040-transcode-tier-advertised-from-startup-resolved-ffmpeg-availability.md)), so it is the one flag two identical builds can disagree about. `true` means the `directStream`/`transcode` half of negotiation can actually run here; `false` means this deployment has no working ffmpeg, so **only direct play works** — hide the affordance rather than offer it and collect a `500`. A client that cannot direct-play a File should read a `false` flag as "unplayable on this server" rather than negotiate. Note this is orthogonal to `/transcoding`, the admin observability snapshot ([ADR-0029](./adr/0029-transcoding-observability-admin-surface.md)), which is served either way.
- **Success content type**: `application/json; charset=utf-8`, except `204 No Content` (empty body) and the media byte endpoints (images, video, HLS artifacts, WebVTT).

### Transport — HTTP always, HTTPS optionally alongside it

The server speaks **plain HTTP on `OBELO_LISTEN_ADDR`** (default `:8080`), and *may additionally* terminate **TLS itself** on a second port ([ADR-0041](./adr/0041-native-tls-optional-alongside-plain-http.md)). Native TLS is **off by default** and opt-in:

| Variable | Default | Meaning |
| --- | --- | --- |
| `OBELO_TLS_MODE` | `off` | `off` \| `files` \| `acme`. `files` serves HTTPS from an operator-supplied certificate; `acme` obtains one automatically over ACME/TLS-ALPN-01. Any other value is a startup error, never a silent fallback to `off`. |
| `OBELO_TLS_CERT` | — | Absolute path to the PEM certificate chain (leaf first). Required in `files` mode. |
| `OBELO_TLS_KEY` | — | Absolute path to the PEM private key. Required in `files` mode. |
| `OBELO_TLS_LISTEN_ADDR` | `:8443` | `host:port` for HTTPS. Must differ from `OBELO_LISTEN_ADDR` — both listeners run at once. |
| `OBELO_TLS_DOMAINS` | — | Comma-separated DNS names, matched exactly (no wildcards). **Required** in `acme` mode and deliberately without a permissive default: it is the CA host policy, and an absent one would let any SNI name trigger an issuance. |
| `OBELO_ACME_EMAIL` | — | Optional contact registered with the ACME account, used by the CA for expiry notices. |
| `OBELO_ACME_DIRECTORY` | Let's Encrypt production | ACME directory URL. Point at `https://acme-staging-v02.api.letsencrypt.org/directory` while setting up; production allows only five failed authorizations per name per hour. |

What a client can rely on:

- **HTTPS is additive.** Turning it on never stops the plain-HTTP listener; the LAN keeps the transport it has, and the `_obelo._tcp` advertisement of [ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md) is unaffected. No public CA will certify a LAN address or a `.local` name, which is why this is a permanent arrangement rather than a migration step.
- **One handler, two listeners.** Both serve the identical API — same routes, same auth, same responses. Nothing in the request path learns which listener it arrived on except the media cookie, which is named and flagged for the scheme the request actually arrived on (§Authentication). A session, token, or device works over either.
- **No redirect.** The server never redirects HTTP to HTTPS and emits no absolute URLs; a client that connects to the plain port stays there.
- **HTTP/2** is negotiated on the TLS listener (ALPN `h2`), which suits HLS: many small segment fetches multiplex over one connection. The plain-HTTP listener is HTTP/1.1.
- **TLS 1.2 is the floor**, matching Apple's ATS requirement.
- **No `Strict-Transport-Security`, on either listener**, and no `upgrade-insecure-requests` in the CSP. HSTS is sticky per hostname and the plain-HTTP LAN path still exists, so emitting it could lock a household out of its own server. Clients must not infer HTTPS support from headers; they connect to the port they were given.
- **`acme` needs exactly one forwarded port.** The TLS-ALPN-01 challenge completes on the HTTPS port; the server never listens on port 80 and never serves an HTTP-01 challenge path. Certificates are obtained on the first handshake for a listed name and renewed automatically; the ACME account key and issued keys live in `OBELO_DATA_DIR/acme` (mode `0700`) and must persist across restarts.
- **The two modes fail in opposite directions, on purpose.** In `files` mode a missing, unreadable, or mismatched certificate is a **startup failure** with an error naming the path, because that is a typo the operator can fix and booting anyway would serve plain HTTP to someone who believes they have TLS. Renewed files are re-read while the server runs (no restart); a failed re-read keeps the previous certificate serving. In `acme` mode a certificate that cannot be obtained — CA unreachable, DNS not yet pointed, no port-forward, rate-limited — is **never** a startup failure: the server boots, the plain-HTTP listener serves as usual, the failure is logged, and issuance is retried on later handshakes. A client sees this as an HTTPS port that refuses to complete a handshake while the plain-HTTP port works normally.

### Error envelope

Every error — including the catch-all 404/405 — returns:

```json
{ "error": { "code": "STRING_ENUM", "message": "human readable", "details": { } } }
```

`details` is omitted when empty. A wrong method on a known path returns `405 METHOD_NOT_ALLOWED` with an `Allow` header.

Complete `code` enum (`internal/api/errors.go`, in file order): `NOT_FOUND`, `METHOD_NOT_ALLOWED`, `INTERNAL`, `BAD_REQUEST`, `UNAUTHORIZED`, `FORBIDDEN`, `FOLDER_OVERLAP`, `SCAN_RUNNING`, `SLOT_COLLISION`, `OUTSIDE_SHOW`, `EMPTY_SLOT`, `NO_FILES`, `SETUP_CLOSED`, `INVALID_CLAIM_TOKEN`, `INVALID_CREDENTIALS`, `AUTHORIZATION_PENDING`, `SLOW_DOWN`, `EXPIRED_TOKEN`, `INVALID_DEVICE_CODE`, `INVALID_USER_CODE`, `TOO_MANY_ATTEMPTS`, `DEVICE_AUTH_BUSY`, `USERNAME_TAKEN`, `LAST_ADMIN`, `ROLE_CHANGE`, `NOT_REMOTE_USER`, `INVALID_ORIGIN`, `INVALID_INVITE`, `LINK_PROTOCOL`, `RESYNC`, `BAD_INVITE`, `INVITE_EXPIRED`, `LINK_UNREACHABLE`, `LINK_SERVER_MISMATCH`, `LINK_REVOKED`, `LINKED_LIBRARY`, `ADMIN_GRANT`, `UNKNOWN_LIBRARY`, `LINKED_GRANT`, `ADMIN_CEILING`, `UNKNOWN_RATING`, `UNKNOWN_RESOLUTION`, `UNKNOWN_TITLE`, `KIND_MISMATCH`, `ITEM_SET_MISMATCH`, `SYSTEM_PLAYLIST`, `TRANSCODE_REQUIRED`, `SERVER_BUSY`, `STREAM_LIMIT`, `SERVICE_UNAVAILABLE`, `ENRICH_UNAVAILABLE`, `ENRICH_BUSY`, `PROVIDER_UNKNOWN`, `PROVIDER_KEY_REQUIRED`, `PROVIDER_INVALID_BASE_URL`, `PROVIDER_INVALID_LANGUAGE`, `PROVIDER_INVALID_SETTING`, `PROVIDER_NOT_AUTHORITATIVE`, `SEARCH_UNAVAILABLE`, `WRONG_KIND`, `UNSUPPORTED_MEDIA_TYPE`, `PAYLOAD_TOO_LARGE`, `TAILNET_INVALID_HOSTNAME`, `TAILNET_INVALID_CONTROL_URL`. (Everything from `SCAN_RUNNING` through `DEVICE_AUTH_BUSY`, and the two `TAILNET_*`, were documented at their own endpoints but missing from this list until 2026-09-03; the linking codes are new and are broken out below.)

**Linking codes** ([ADR-0054](./adr/0054-a-linked-server-is-a-user-with-the-remote-role.md)–[ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)), split by which side of the Link emits them. They are listed together because the two halves ship in one binary — every Server can be either side — and a client reading a refusal has to know which household it is about.

| Code | Status | Emitted by | Means |
| --- | --- | --- | --- |
| `ROLE_CHANGE` | 422 | sharer | A role change to or from `remote` was attempted. A `remote` User is created `remote` and dies `remote` (ADR-0054 §1). |
| `NOT_REMOTE_USER` | 422 | sharer | `POST /users/{id}/invite` named a User whose role is not `remote`. An invite exists only for a linked Server. |
| `INVALID_ORIGIN` | 422 | sharer | An origin in the invite request is not an absolute `http(s)` origin with no path, or the list is empty. Nothing is minted. |
| `INVALID_INVITE` | 400 | sharer | `POST /auth/link/redeem` presented a code that is unknown, expired, or already spent — **one byte-identical body for all three**, so the live code space cannot be mapped. |
| `LINK_PROTOCOL` | 409 | both | The two Servers speak different `linkProtocolVersion`s. `details` is named from the answering side: `{ supported, requested }` on the sharer's redeem, `{ theirs, ours, upgrade: "theirs"\|"ours" }` on the receiver's `POST /links`. Checked **before** the code is spent. |
| `RESYNC` | 410 | sharer | `GET /libraries/{id}/export` was handed a `since` older than the tombstone retention (30 days). The position is gone, not the request malformed; the mirror answers with a full pull. |
| `LINKED_GRANT` | 422 | sharer | A grant set for a `remote` User named a Library that itself arrived over a Link. A mirror is never re-shared onward (ADR-0054 §4); the whole set is rejected and the prior grants stand. |
| `BAD_INVITE` | 400 | receiver | The pasted string is not a readable invite — wrong scheme, undecodable base64, a missing field — **or** the sharer refused it (`INVALID_INVITE`). One code, because the operator's move is the same: ask for the string again. |
| `INVITE_EXPIRED` | 410 | receiver | A well-formed invite whose 24 hours ran out. Ask for a fresh one; do not re-paste this. |
| `LINK_SERVER_MISMATCH` | 409 | receiver | `POST /links/{id}/rekey` was given an invite for a **different** Server. A Link is bound to one peer for its whole life. |
| `LINK_UNREACHABLE` | 503 | receiver | No origin in the invite answered, over either dialer — or, on a play, the sharing Server is not answering now. Retryable; nothing is deleted. |
| `LINK_REVOKED` | 409 / 503 | receiver | The sharer answered `401`: the `remote` User or its Device is gone over there. 409 from `POST /links/{id}/sync`, 503 from a play. The fix is a fresh invite, never a retry. |
| `LINKED_LIBRARY` | 409 | receiver | A **write** was aimed at a Library that is a mirror of another household's. The Server that owns the files is the identity authority for them (ADR-0056 §1); a correction belongs on that machine. Renaming is the one exception. |
| `STREAM_LIMIT` | 429 | either | The User is at their Playback ceiling's `maxStreams`. `details: { "active", "limit" }`. On a relayed play the **sharer's** refusal passes through verbatim. |
| `UNKNOWN_RESOLUTION` | 422 | either | `PUT /users/{id}/playbackCeiling` named a `maxResolution` that is not a settable rung (`720p`, `1080p`, `2160p`). |

### Request bodies

Every JSON body is capped at **1 MiB** and decoded with **unknown fields rejected** — an extra key, malformed JSON, or an oversized body is `400 BAD_REQUEST` `"invalid JSON body"`. Do not send fields the endpoint doesn't define. (Exceptions: the scan/enrich trigger bodies are best-effort — a malformed body falls back to the default mode.)

### Authentication

Four credential transports, each honored only where stated:

1. **Bearer token** (the universal credential): `Authorization: Bearer <token>`. The token is **opaque and DB-backed** — validated on every request, so revocation (logout, device delete) is immediate ([ADR-0015](./adr/0015-opaque-db-backed-tokens.md)). Every endpoint accepts it. When bearer and another credential are both present, bearer wins. Auth failures set `WWW-Authenticate: Bearer` and return `401 UNAUTHORIZED`.
2. **Media cookie** (media/browser credential): carries the *same* opaque token. Set by `POST /auth/login` (`HttpOnly`, `SameSite=Lax`, `Path=/api/v1`, 30-day MaxAge), cleared by `POST /auth/logout`. Honored **only** by the read-only media/stream GETs and the SSE stream — exactly: `GET /titles/{id}/artwork/{role}`, `GET /titles/{id}/subtitles/{subId}.vtt`, `GET /shows/{id}/artwork/{role}`, `GET /seasons/{id}/artwork/{role}`, `GET /artists/{id}/artwork/{role}`, `GET /albums/{id}/artwork`, `GET /people/{personRef}/artwork/{role}`, `GET /sessions/{id}/stream`, `GET /sessions/{id}/hls/*`, `GET /events`, `GET /providerImage?ref=` (which is additionally Admin-only). No JSON/mutation endpoint honors it.

   **Its name depends on the listener**, and a client that hardcodes one name is wrong:

   | Request arrived over | Cookie name | `Secure` |
   | --- | --- | --- |
   | TLS (`r.TLS`, or a trusted proxy's `X-Forwarded-Proto: https`) | `__Secure-ms_media` | yes |
   | plain HTTP | `ms_media_plain` | no |

   A response only ever sets the name belonging to its own listener; requests are accepted under either, plus the pre-split name `ms_media` (read for compatibility, never written, expires on its own). **The split is load-bearing, not cosmetic.** A cookie jar is partitioned by neither scheme nor port, so while both listeners wrote one name at one path they contended for a single jar entry — and [RFC 6265bis §5.7](https://datatracker.ietf.org/doc/html/draft-ietf-httpbis-rfc6265bis) ("leave secure cookies alone", implemented by every current browser) resolves that contention by discarding the *non-secure* origin's `Set-Cookie`. One HTTPS login therefore broke browser media on the plain-HTTP origin permanently and silently: the plain listener kept issuing a correct cookie, the browser kept throwing it away, every `<img>`/`<video>` `401`ed, and the rest of the page kept working on its origin-scoped bearer. Logging in again over HTTP could not fix it — that was the write being refused.
3. **`?token=` query param**: honored by **exactly one** endpoint — `GET /files/{id}/download` (external players fed a `.xspf` can send neither header nor cookie). The URL-borne token is an accepted tradeoff there; it is still DB-validated and revocable.
4. **Stream token** — the **third media credential**, alongside the bearer and the media cookie ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)). A separate secret, carried in the URL **path**, honored **only** by the two routes built for it: `GET /stream/{streamToken}/stream` and `GET /stream/{streamToken}/hls/{file}`. Gated by the **`streamToken`** feature flag. Its scope is the whole point of it, so state it in the same breath as the mechanism:
   - **One session.** It authorises the media artifacts of exactly one Playback session — not the JSON API, not metadata, not another session, not another User's session.
   - **Read-only.** It reaches only GET media routes; a non-GET is `405` before the token is even examined. There is no token-authenticated progress report, session delete, or mint.
   - **Expiring.** 4 hours from minting, reported as `streamTokenExpiresAt`.
   - **Revoked by `DELETE /sessions/{id}`** — and by the idle reaper, which is the same cascade. Whichever comes first, the token or the session, kills both.
   - **Not an account token.** It is a different table and a different lookup: a stream token is refused as a bearer everywhere, and a bearer pasted into the path is refused here. Minting one always requires a bearer.

   Obtained from a Decision (`streamToken` + `streamTokenExpiresAt`, §3.6) or minted on demand with `POST /sessions/{id}/stream-token`. It exists for the player that **hands the URL to somebody else** — an AirPlay receiver, a cast target, a smart-TV app — where the fetching party is software we do not own, cannot be made to send an `Authorization` header, and may or may not carry a cookie depending on an OS and a firmware we do not control.

**Native clients (Apple TV / libmpv):** the tvOS client plays through **libmpv**, whose HTTP layer (ffmpeg) can attach arbitrary headers to every media request — so a native client simply sends **`Authorization: Bearer <token>` on media requests too** (mpv: `http-header-fields`), and needs neither the cookie nor `?token=`. The media cookie exists for players that *cannot* set headers — the browser's `<video>`/`<img>`/`EventSource` (and it would equally serve an AVPlayer-based client via cookie injection). Do not put `?token=` on stream/HLS URLs — the server only accepts it on `/files/{id}/download`.

**The stream token does not soften that refusal — it is what removed the need to bend it.** The two are different credentials in different places, and the distinction is the entire reason one is allowed in a URL and the other is not. `?token=` carries the **account** bearer: permanent (there is no `expires_at` on it), authorising every read and every mutation in the API, for as long as the Device lives. That is why it is confined to a single read-only download route, and why no media route accepts it — a URL is logged by this server, by any proxy, and by whatever fetches it, and an account credential must not be in one. A stream token is scoped to one session's bytes, read-only by construction, expiring, and revoked with its session, so the same URL exposure costs one film for one sitting instead of an account. **A client that needs a self-authenticating media URL uses a stream token; it never puts `?token=` on a stream or HLS URL.**

**Bootstrap** ([ADR-0013](./adr/0013-first-admin-claim-token-bootstrap.md)): `GET /server` reports `setupRequired: true` while zero Users exist; the server logs a **claim token** at boot; `POST /setup` with `{claimToken, username, password}` creates the first Admin. Setup does *not* log you in — call `/auth/login` after.

### Access control

- Enforced **server-side on every endpoint**. A Member sees only granted Libraries and only Titles at or below their Rating ceiling; listings are filtered in SQL so counts and pagination stay correct; an Admin sees everything.
- **404-not-403 (hide-existence)**: an entity outside the caller's access — an ungranted Library's Title, another User's playback session, another User's Playlist (no Admin override), a Collection with zero visible members — returns `404`, indistinguishable from not-existing.
- **Un-rated content stays visible.** A Title with no Content rating, or a label outside the known ladder (MPAA + US TV), is never hidden by a ceiling — a capped Member's un-enriched Library does not look empty.

### Pagination — the actual state

Cursor pagination (opaque `cursor` param, keyset seek — never offset) is implemented **only on the three top-level grids**: `GET /libraries/{id}/titles` and its TV/music delegates (shows, artists). Params `limit` (default 20, max 100) and `cursor`; the response carries `nextCursor` (absent on the last page). **Not paginated**: seasons/episodes, albums/tracks (full listings), `/home` (each row capped at 20), `/search` (`limit` caps each group independently, default 20, max 100).

One route paginates by the same mechanism with **different bounds and an extra field**: `GET /libraries/{id}/export` (§3.3), which is server-to-server rather than screen-shaped — `limit` defaults to 200 and is **clamped** (not refused) at 500, and every page carries a `checkpoint` beside the optional `nextCursor`. See §3.3 for why the two differ.

### Timestamps & conventions

- Timestamps are RFC3339 UTC (`2026-07-14T10:00:00Z`).
- Durations/positions are **milliseconds** (`durationMs`, `positionMs`, `resumePositionMs`); bitrates are **bits/sec**.
- Response arrays are **never `null`** — an empty list is `[]`. Many scalar fields are `omitempty`: absent means zero/empty/false. `isDefault` and `forced` are always emitted.
- The device-profile HEVC flag is spelled **`hevcInMpegts`** (lowercase "ts").

### Watched threshold

The **server** applies the threshold, never the client: crossing ~**90%** of duration marks a Title watched (clears resume, advances TV Up Next); a stop below the ~**2%** floor stores no resume. Clients report raw position only. Manual override via `PUT /titles/{id}/watchState`. Concurrency is last-write-wins per (User, Title).

### Known gaps (deliberate, tracked)

- **No `GET /sessions` collection** — only per-session sub-resources exist. An Admin session list is a known follow-up; the admin-only session SSE events are deliverable without it.
- The SSE stream sends **no `id:` lines, no `retry:` hint, and no heartbeat** beyond the initial `: connected` comment — clients must rely on EventSource/HTTP-level reconnect and treat every event as a refetch nudge (each maps to a pollable resource).

---

## Part 2 — Real-time events (SSE)

### GET /events — [Public]

Single server→client SSE stream ([ADR-0016](./adr/0016-sse-for-realtime-updates.md)). Auth: bearer **or** media cookie (a browser `EventSource` cannot set headers).

Wire format: status 200 with `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. First bytes are the comment `: connected`. Each event:

```
event: <type>
data: <json>
```

No `id:`, no `retry:`, no heartbeat. The subscriber's identity (user, admin flag, accessible-Library set) is resolved **once at subscribe time**; per-subscriber buffers hold 32 events and a full buffer **drops** events (publish never blocks) — SSE is an optimization; every event maps to a pollable resource.

**Audience gating** happens before enqueue: *broadcast* → everyone; *admin-only* → Admins only; *library-scoped* → subscribers whose accessible-Library set contains the event's Library (Admins always).

| Event | Audience | Payload | Poll fallback |
| --- | --- | --- | --- |
| `enrichProgress` | broadcast | `{ "libraryId", "total", "done", "matched", "unmatched", "failed", "disabled", "retrying", "complete" }` | `/libraries`, `/libraries/{id}/titles` |
| `scanProgress` | library-scoped | `{ "libraryId", "titlesFound", "filesFound", "complete", "scope"?, "added"?, "removed"? }` — `scope` is the Targeted-scan entity label (absent for full scans); `added`/`removed` only on the terminal targeted event | `GET /libraries/{id}/scan` |
| `libraryUpdated` | library-scoped | `{ "libraryId" }` — a refetch nudge, not a diff. Also emitted for a **linked** Library after a mirror pull that actually applied rows (a pull that carried nothing publishes nothing — there would be no refetch behind the nudge). A `remote` User subscribing to `/events` is audience-gated to its own grants, which is exactly the nudge the mirror on the other side wants (ADR-0056 §4) | `/libraries`, `/libraries/{id}/titles` |
| `sessionStarted` | admin-only | `{ "sessionId", "userId", "titleId" }` | — (no session list yet) |
| `nowPlaying` | admin-only | `{ "sessionId", "userId", "titleId", "positionMs" }` | — |
| `sessionEnded` | admin-only | `{ "sessionId", "userId", "titleId" }` | — |
| `tailscaleState` | admin-only | `{}` — a refetch nudge carrying **no state**, fired on every Tailnet node transition ([ADR-0043](./adr/0043-tailnet-remote-access-via-embedded-tsnet.md)) | `GET /settings/tailscale` |
| `linkState` | admin-only | `{ "linkId" }` — a refetch nudge fired on every **Link** state transition ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §6). It carries the id and deliberately **not** the new state, for `tailscaleState`'s reason — a second copy of a condition is the one on the screen when the two disagree — while the id is there because a household can hold several Links and only one moved | `GET /links` |

---

## Part 3 — Endpoint catalog

### 3.1 Handshake & auth spine

#### GET /server — [Unauthenticated]

```json
{
  "id": "90f0aa62-4769-4788-b63a-d9e3cb4ad51c",
  "name": "Living Room",
  "version": "0.1.0",
  "supportedVersions": [1],
  "features": {
    "auth": true, "libraries": true, "scanner": true, "directPlay": true,
    "watchState": true, "home": true, "search": true, "collections": true,
    "playlists": true, "realtimeEvents": true, "deviceAuth": true,
    "mediaCookieRefresh": true, "streamToken": true, "transcode": true,
    "tailscale": false,
    "serverLinking": true, "linkedLibraries": true
  },
  "setupRequired": false,
  "linkProtocolVersion": 1,
  "tailnetURL": null
}
```

Every flag above reads `true` on a normally-provisioned server except the two that describe **this deployment** rather than the API surface:

- **`transcode`** varies by host — `false` on a deployment with no usable ffmpeg (see the handshake note in Part 1).
- **`tailscale`** varies by **build** ([ADR-0043](./adr/0043-tailnet-remote-access-via-embedded-tsnet.md)): `true` only in a binary compiled with `-tags tailscale`, which is what the Docker image and the release binaries are and what a plain `go build` is not. It says whether this server *can* join a Tailnet, **not** whether remote access is switched on — that is `GET /settings/tailscale` (§3.9). A client that sees `false` should explain that remote access is unavailable in this build rather than offer a control that can only fail; the `/settings/tailscale` routes are served either way, answering with an error that names the build, precisely so the failure is not a bare `404` that reads like a mistyped path.

The two linking flags describe the two halves of ADR-0055, and every Server ships both:

- **`serverLinking`** — this Server can **be linked to**: it serves `POST /users/{id}/invite`, `POST /auth/link/redeem` and `GET /libraries/{id}/export` (the sharing side). A home Server reads this flag on the other Server's handshake **before** it posts an invite code, so a mismatch costs the Admin nothing.
- **`linkedLibraries`** — this Server can **hold Links**: it serves `/links` (§3.11) and its Libraries and browse summaries may carry `linked` / `available`. A client branches on it to show the Linked-servers surface and to expect those two fields; absent and `false` are indistinguishable, which is what makes the flag safe to read against an older server.

`linkProtocolVersion` is the **top-level** link-protocol version — one small integer, `1` today. It is deliberately *not* a feature flag and clients must not branch on it: it exists so two **Servers** can refuse each other clearly. The invite, the redeem request, the export and the relay all stamp it, and the receiving Server compares it before anything is redeemed, answering `409 LINK_PROTOCOL` naming which side needs an upgrade. Bumping it is a deliberate act with a compatibility note. A server predating linking omits the key; the receiving side reads that as version `0` — "speaks no version of this protocol" — which lands on the same sentence.

`tailnetURL` is the origin a signed-in client should use to reach this server **from away** — `"https://obelo.tail1a2b.ts.net"` — or `null` ([ADR-0043](./adr/0043-tailnet-remote-access-via-embedded-tsnet.md)). Four rules, and each exists because getting it wrong is silent:

- **It is authenticated-only, but the ROUTE stays unauthenticated.** A request with no bearer, or with a malformed, expired or revoked one, gets `200` with `tailnetURL: null` — **never `401`**. The handshake has never answered `401` and must not start: clients drive token-drop from that status, so a `401` here presents as a revoked token and signs the user out. Only the field is gated. (An unauthenticated caller must not learn the name either: a scanner reaching a port-forward would learn that a tailnet exists and what it is called.)
- **`null` or absent OBLIGES the client to CLEAR its stored address.** Not "keep the last value you saw" — that is the natural implementation and it is wrong in the case that matters: an operator who switches remote access off would otherwise leave every paired client probing a dead name on every cold start, forever, with no way to stop it.
- **It is an ORIGIN and it carries the scheme the server ACHIEVED**, never the one the operator requested: scheme + host, no path, no trailing slash, and no port (the tailnet listeners are `:80`/`:443`, the defaults for their schemes). `http://` means tailnet HTTPS is not bound — do not "upgrade" it, because `:443` is not listening.

  **The scheme also decides the transport, so it is worth surfacing rather than just obeying.** HTTP/2 is negotiated by ALPN during the TLS handshake, so an `http://` tailnet origin is HTTP/1.1 permanently — every request queues against the per-origin connection limit instead of multiplexing, which is felt most on many-small-fetch screens (artwork grids, HLS playlists) and most of all on the remote path, where latency is highest. An `https://` origin gets HTTP/2. Apple clients additionally **cannot use an `http://` origin at all** — App Transport Security refuses cleartext to a globally-resolvable name, and the request never leaves the app. So a `http://` value is a working address and a degraded one; the operator fixes it by enabling tailnet HTTPS, not by anything a client can do.
- **It is the MagicDNS name, never a tailnet IP.** The addresses are what the name exists to replace and they move; no certificate can match an IP literal; and Apple's App Transport Security refuses cleartext to a `100.64.0.0/10` literal regardless, so an IP is not even a workaround.

Because `tailnetURL` is explicitly nullable and never omitted, `null` is this server *saying* it has no tailnet address. An older server omits the key entirely — decode both the same way and take the same action.

`id` and `name` are the **Server identity** ([ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md)). Both are `omitempty` and both are **additive** — a server predating ADR-0034 omits them, so treat them as optional rather than as an error.

- **`id`** — an opaque UUID, minted once into the server's data dir and stable for its lifetime. It is *not* derived from a key or from hardware, so it survives both. **Its purpose is to make an address change survivable:** a client that stored the id can rediscover the server at a new address (DHCP lease change) and keep its token, because the token is bound to a Device row on this server ([ADR-0015](./adr/0015-opaque-db-backed-tokens.md)), never to an address. It is also the only way to answer "is this the same server I logged into?". Wiping the data dir mints a new id — correct, since that server has no Users, Devices, or tokens to honor.
- **`name`** — the operator's display name (`OBELO_SERVER_NAME`, defaulting to the host's name). Cosmetic; nothing keys on it, and renaming never invalidates a token. That is exactly why it is a separate field from `id`.

Neither is a secret: this endpoint is unauthenticated, the id grants nothing, and the name is operator-chosen.

Errors: `500 INTERNAL`.

#### LAN discovery — mDNS/Bonjour

Not an HTTP endpoint, but part of how a native client reaches this contract ([ADR-0005](./adr/0005-discovery-and-tls-via-reverse-proxy.md), implemented by [ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md)). The server advertises on the local link:

```
service:  _obelo._tcp        (in the local domain, on the listen port)
TXT:      txtvers=1  id=<uuid>  name=<display name>  path=/api/v1
```

Verify with `dns-sd -B _obelo._tcp local` / `dns-sd -L "<name>" _obelo._tcp local`.

- **TXT is a hint, not a contract** (RFC 6763). Confirm everything against `GET /server` once connected; `id`/`name` appear in both, and the handshake is authoritative.
- **The SRV target is always a `.local` name**, and its A/AAAA records carry the server host's own LAN addresses, most-likely-reachable first. Resolve and try them in order. (Servers predating the 2026-08-02 amendment to ADR-0034 could advertise a single-label host name that only unicast DNS would answer, and in a container often advertised nothing at all.)
- **A discovered server is always plain `http`.** The server binds plain HTTP and a TLS-terminating reverse proxy is by definition not on the local link, so no scheme is advertised.
- **Discovery is LAN-only, permanently.** mDNS is link-local by construction: a reverse-proxied or VPN-reachable instance is not discoverable and never will be. **Manual address entry is the permanent path for remote access, not a stopgap** — every client needs it.
- **Advertisement is best-effort.** A server that failed to register still serves normally; it just has to be addressed manually. Absence of a Bonjour record is not evidence the server is down — use `GET /server`, the cheapest liveness probe.

#### POST /setup — [Unauthenticated]

Body `{ "claimToken", "username", "password" }` (all required) → `201` `{ "user": { "id", "username", "role" } }`. One-shot; does not log in and sets no cookie.
Errors: `400 BAD_REQUEST` (bad body / missing fields / collision), `403 SETUP_CLOSED`, `403 INVALID_CLAIM_TOKEN`, `500`.

#### POST /auth/login — [Unauthenticated]

```json
{ "username": "admin", "password": "…",
  "device": { "name": "Living Room TV", "platform": "tvos", "clientId": "stable-uuid" } }
```

`device.clientId` is **required** — persist a stable UUID per install; re-login with the same `clientId` reuses/refreshes the existing Device instead of duplicating it.

→ `200`:

```json
{
  "token": "opaque-session-token",
  "user": { "id": "…", "username": "admin", "role": "admin" },
  "device": { "id": "…", "name": "Living Room TV", "platform": "tvos",
              "clientId": "stable-uuid",
              "createdAt": "2026-07-15T01:26:44Z", "lastSeenAt": "2026-07-15T01:26:44Z" }
}
```

Side effect: sets the media cookie with the same token.
Errors: `400 BAD_REQUEST` (`"device.clientId is required"` / bad body), `401 INVALID_CREDENTIALS`, `429 TOO_MANY_ATTEMPTS`, `500`.

**Rate limited.** Repeated *failures* — never successes — are counted per username **and** per client IP, and either counter going over refuses further attempts for a fixed window with `429 TOO_MANY_ATTEMPTS` and a `Retry-After` (seconds). The refusal is identical for a known and an unknown username, so it is not a username oracle, and it precedes the credential check, so the correct password is refused too while the window is open. Clients should surface the wait rather than retry-loop; a retry loop is what the limit exists to stop.

#### Device authorization grant — signing a TV in from a phone ([ADR-0036](./adr/0036-device-authorization-grant-for-tv-sign-in.md))

Gated by the **`deviceAuth`** feature flag. Branch on the flag, never on a version — a server without these routes 404s them, and the correct fallback is `POST /auth/login`, which every client keeps anyway (typing a password is the permanent manual path, not a deprecated one).

Three endpoints, and **two codes that are not interchangeable**:

- **`deviceCode`** — the poll secret. 256-bit, held by the signing-in Device, shown to nobody, stored only as a hash. The only thing that can collect a session.
- **`userCode`** — 4 characters from an unambiguous alphabet (no `0`/`O`, no `1`/`I`/`L`, no `U`). Displayed and carried in the QR. **Powerless alone**: approving requires a bearer token. This is why 4 characters is safe — see the ADR.

The flow: the TV `POST /auth/device/code` → shows `userCode` + a QR of `verificationUriComplete` → polls `POST /auth/device/token` on `interval` → a phone opens the URL, signs in if needed, and `POST /auth/device/approve`s → the TV's next poll returns a session.

##### POST /auth/device/code — [Unauthenticated]

```json
{ "device": { "name": "Living Room TV", "platform": "tvos", "clientId": "stable-uuid" } }
```

`device.clientId` is **required**, same rule as login. The Device is declared **here**, at the start — it is what the approving phone is shown and what the Device row is minted from at redemption. The poll carries only `deviceCode`, so nothing can swap the identity out from under a human who already approved one.

→ `201`:

```json
{
  "deviceCode": "opaque-256-bit-secret",
  "userCode": "K7R9",
  "verificationUri": "http://192.168.1.50:8096/link",
  "verificationUriComplete": "http://192.168.1.50:8096/link/K7R9",
  "expiresIn": 300,
  "interval": 2
}
```

`verificationUri*` are built from the **inbound request's** Host/scheme (honouring `X-Forwarded-Host`, and `X-Forwarded-Proto` from a peer in `OBELO_TRUSTED_PROXIES`), because the server cannot know its own address — only the one it was reached on, which is the one the phone beside the TV needs. Encode `verificationUriComplete` in the QR; show `verificationUri` + `userCode` as text for anyone typing it.
Errors: `400 BAD_REQUEST` (`"device.clientId is required"`), `429 TOO_MANY_ATTEMPTS`, `503 DEVICE_AUTH_BUSY`, `500`.

The two refusals are **not** interchangeable, and a client should act differently on each. `503 DEVICE_AUTH_BUSY` means the server's code space is full: nothing the caller did caused it, codes expire on their own, and a retry in a minute may well work. `429 TOO_MANY_ATTEMPTS` means *this source address* is over its quota on starting flows; it carries `Retry-After` (whole seconds) and retrying before then spends the same budget and makes the refusal last longer. A TV that treats the two the same will be right about one of them and wrong about the other. Reaching the 429 in normal use means the client is retry-looping the endpoint — the quota is far above any household's real rate. Note that behind a reverse proxy the operator has not declared in `OBELO_TRUSTED_PROXIES`, the server sees every request as coming from the proxy (`X-Forwarded-For` is ignored unless the peer is on that allowlist), so the quota is shared by everyone behind it.

##### POST /auth/device/token — [Unauthenticated]

Body `{ "deviceCode": "…" }`. Poll no faster than `interval`.

→ `200` — **byte-identical to `POST /auth/login`'s response** (`{token, user, device}`), cookie included. That is deliberate: this is a second way to *obtain* a session, not a second kind of session, so a client reuses one code path.

Every other answer is a `400` carrying the state (the grant models "keep waiting" as an error, so a client must not treat 2xx as the only terminal answer):

| code | meaning |
|---|---|
| `AUTHORIZATION_PENDING` | Nobody has approved yet. Keep polling. The usual answer. |
| `SLOW_DOWN` | Polling faster than `interval`. Back off; not fatal. |
| `EXPIRED_TOKEN` | The code aged out. Start a new flow. |
| `INVALID_DEVICE_CODE` | Unknown, or already redeemed — a device code is **one-shot**. |

##### POST /auth/device/approve — [Public]

Body `{ "userCode": "K7R9" }`. Case-insensitive; spaces and hyphens are ignored (`k7-r9` works). Confusable characters are **not** repaired — the alphabet excludes them, so a code containing one was misread and guessing which glyph was meant would authorize the wrong request.

**Approval is immediate — there is no confirmation step** (ADR-0036 records why, and what it costs).

→ `200` `{ "device": { "name": "Living Room TV", "platform": "tvos" } }` — the Device just authorized. With no confirmation beforehand, this is the user's only chance to notice a mis-entered code; show it. (No `id`: the Device row does not exist until the TV redeems.)
Errors: `404 INVALID_USER_CODE` — unknown, expired, **and** already-used, deliberately indistinguishable so the live code space cannot be mapped. `429 TOO_MANY_ATTEMPTS` — the per-User brute-force limit. `401`, `500`.

There is no deny operation. The recourse for an unintended approval is `DELETE /devices/{id}`, which revokes instantly.

#### POST /auth/link/redeem — [Unauthenticated]

Gated by the **`serverLinking`** feature flag. **Called by another Obelo Server, never by an app** — it is how a home Server spends the one-time invite a sharing Admin sent and comes away with an ordinary Device-bound bearer ([ADR-0055](./adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md) §1, §4). Nothing in a phone or a TV posts here; a client that has scanned an invite posts the whole string to **its own** Server's `POST /links` (§3.11) and this call happens between the two machines.

```json
{ "code": "opaque-256-bit-secret",
  "linkProtocolVersion": 1,
  "server": { "id": "<the redeeming server's own id>", "name": "Brandon's server" } }
```

`server` is [ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md)'s Server identity doing the job it was minted for, one hop out: it becomes the **Device** row on this side, upserted by `clientId` exactly as a login is, so a re-link from the same household reuses the row instead of leaving a trail of dead ones. There is no `platform` field — it is always `"server"`, set by the auth layer, so a linked Server cannot present itself as a phone.

→ `200`:

```json
{ "token": "opaque-session-token",
  "user":   { "id": "…", "username": "Brandon's server", "role": "remote" },
  "device": { "id": "…", "name": "Brandon's server", "platform": "server",
              "clientId": "<the redeeming server's id>",
              "createdAt": "…", "lastSeenAt": "…" },
  "linkProtocolVersion": 1 }
```

The first three fields are **byte-identical to `POST /auth/login`'s response**, and deliberately so: what a Link holds is an ordinary Device-bound token, not a second kind of credential. **The media cookie is not set** — the caller is a process on another household's machine with no cookie jar and no use for one.

The code is **single-use and valid 24 hours**. It is stored only as its SHA-256, like a device code, and it is dead the moment it is spent.

Errors:
- `409 LINK_PROTOCOL` — `details: { "supported", "requested" }`, named from *this* (the answering) side. Checked **before** the code is examined, so a mismatch never costs the Admin their invite.
- `400 INVALID_INVITE` — unknown, expired, already redeemed, or a missing server id: **one byte-identical body for all of them**, so the live code space cannot be mapped. Every one of them means "ask for a fresh invite".
- `429 TOO_MANY_ATTEMPTS` with `Retry-After` — failed redemptions are counted per client address in a counter **of their own**, separate from the login limiter: a stranger guessing at invites must not lock the household out of password login, and a fumbled password must not spend a friend's redemption budget. The limiter runs before the code is looked at, so it is not an oracle. The security property here is the 256 bits, not the limiter.
- `500`.

#### POST /auth/logout — [Public] (bearer only)

No body → `204`. Revokes exactly the calling token and clears the media cookie.

#### POST /auth/media-cookie — [Public] (bearer only)

Gated by the **`mediaCookieRefresh`** feature flag. Branch on the flag, never on a version — a server without the route 404s it, and the correct fallback is to skip the call (media byte-serving keeps today's behaviour until the next real login).

No body → `204`. **Re-issues** the media cookie carrying the **requesting bearer's** session token — byte-for-byte the login cookie (same name-for-the-scheme, `HttpOnly`, `SameSite=Lax`, `Path=/api/v1`, MaxAge, and `Secure`-only-under-TLS attributes), or the browser would store a second cookie instead of overwriting the one being replaced. It is the login cookie machinery minus the credential check: the request's validated bearer identity **is** the authorization.

**Bearer only** — it does **not** honor the media cookie itself (only the read-only media GETs do), so a lone/stale cookie can never authorize re-issuing itself. Unauthenticated or invalid-bearer requests are `401` and set no cookie.

Why it exists: the web instant user switch swaps the *bearer* token from JS but **cannot** touch the `HttpOnly` cookie, so after a switch browser media bytes would still serve under the **previous** user's cookie. A client calls this (after adopting the new bearer, before resuming media) so the cookie identity always matches the active bearer — closing that identity leak (the client ADR-0009 hard-teardown class).

#### GET /devices — [Public]

→ `200` `{ "devices": [ { "id", "name", "platform", "clientId", "createdAt"?, "lastSeenAt"? } ] }` — the **caller's** Devices only.

#### DELETE /devices/{id} — [Public] (self) / [Admin] (any)

→ `204`. Revokes that Device's token immediately. A foreign Device (non-admin caller) is `404 NOT_FOUND` `"device not found"` — forbidden is deliberately indistinguishable from missing.

### 3.2 Users (admin scope)

All bearer + admin. Non-admin → `403 FORBIDDEN`.

**Three roles**: `admin`, `member`, and — since [ADR-0054](./adr/0054-a-linked-server-is-a-user-with-the-remote-role.md) — **`remote`**, which is what a *linked Server* holds on this one. A `remote` User is a User in every enforcement seam (grants, Rating ceiling, `access.Scope`, Device/token binding, the 404 posture) and differs in four ways a client can observe:

- **No password.** `POST /users` with `role: "remote"` takes none and **refuses a supplied one** (`400`); `POST /auth/login` refuses the role with any password, answering the same `401 INVALID_CREDENTIALS` an unknown username gets, on the same timing path. Its only credential is what redeeming an invite leaves behind.
- **Not promotable.** Any role change to or from `remote` is `422 ROLE_CHANGE`.
- **Not in the roster.** It is absent from every "who is on this Server" surface a non-Admin can see, and present on the Admin Users page — which is where the operator manages what it can reach.
- **No watch state.** Relay playback under it starts and ends Sessions and feeds `nowPlaying`, and writes nothing per-Title. The person watching is on the other Server, and their watch state stays there.

| Endpoint | Body → Response |
| --- | --- |
| `POST /users` — [Admin] | `{ "username", "password", "role": "admin"\|"member"\|"remote" }` → `201` bare `{ "id", "username", "role" }`. For `role: "remote"` the `username` is the label the sharing Admin picks ("Brandon's server") and `password` **must be omitted**. Errors: `400` (missing username, unknown role, a password for `remote`, none for `admin`/`member`), `409 USERNAME_TAKEN`. |
| `GET /users` — [Admin] | → `200` `{ "users": [ { "id", "username", "role", "lastSeenAt"? } ] }`. `lastSeenAt` is the newest `lastSeenAt` across that User's Devices, RFC3339 UTC, and is **omitted — never `""`** — for a User with no Device at all. That absence is the only way to tell a `remote` User that has redeemed its invite from one that never has ("Linked, seen 2h ago" vs "Never linked"). It is best-effort: if the lookup fails the field is off *every* row rather than the roster failing over a decoration. |
| `GET /users/{id}` — [Admin] | → `200` `{ "id", "username", "role", "libraryIds": [], "ratingCeiling": "", "maxResolution": "", "maxBitrate": 0, "maxStreams": 0 }` — `libraryIds` never null (`[]` for an admin), `ratingCeiling` `""` = uncapped, and the three **Playback ceiling** fields zero/empty = uncapped (always zero for an Admin, who may not carry one). Errors: `404`. |
| `DELETE /users/{id}` — [Admin] | → `204`. Errors: `404`, `409 LAST_ADMIN`. Deleting a `remote` User is the sharer's **kill switch**: the cascade removes its Device and token, the next request from the other Server `401`s, and that Server marks its Link `revoked` (§3.11). |
| `PUT /users/{id}/password` — [Admin] | `{ "password" }` → `204`. Errors: `400`, `404`. |
| `PUT /users/{id}/libraryAccess` — [Admin] | `{ "libraryIds": [ … ] }` (full replace-set) → `204`. Errors: `404`, `422 ADMIN_GRANT`, `422 UNKNOWN_LIBRARY`, `422 LINKED_GRANT` (the target is `remote` and the set names a Library that itself arrived over a Link — a mirror is never re-shared onward). On any 422 the prior grant set is unchanged. |
| `PUT /users/{id}/ratingCeiling` — [Admin] | `{ "rating": "PG-13" }` (`""` clears) → `204`. Errors: `404`, `422 ADMIN_CEILING`, `422 UNKNOWN_RATING`. |
| `PUT /users/{id}/playbackCeiling` — [Admin] | `{ "maxResolution": "1080p", "maxBitrate": 8000000, "maxStreams": 2 }` → `204`. Errors: `404`, `422 ADMIN_CEILING`, `422 UNKNOWN_RESOLUTION`, `400` (a negative bitrate or stream count). See below. |
| `POST /users/{id}/invite` — [Admin] | `{ "origins": [ "https://media.example.org", "http://obelo.tail1a2b.ts.net" ] }` → `201` `{ "invite": "obelo-link:…", "expiresAt": "…" }`. Target must be `remote`. Errors: `404`, `422 NOT_REMOTE_USER`, `422 INVALID_ORIGIN`. See below. |

#### PUT /users/{id}/playbackCeiling — the Playback ceiling

A per-User cap on **how** a Title may play, never on **what** may be seen ([ADR-0054](./adr/0054-a-linked-server-is-a-user-with-the-remote-role.md) §2). It applies to every role **except Admin**: a sharer says "1080p, two at a time" about a linked Server, and a household Admin sometimes says it about a kid's iPad.

The body is the **whole ceiling, not a patch** — every omitted field decodes to its zero value and clears that dimension, exactly as `libraryAccess` is a replace-set, so re-sending the same body is idempotent:

- **`maxResolution`** — one of exactly three settable rungs, `"720p"`, `"1080p"`, `"2160p"`; `""` = uncapped. Case and spacing fold; anything else is `422 UNKNOWN_RESOLUTION`. (The negotiator's own ladder is wider — `144p…4320p` plus `sd/hd/fhd/2k/4k/uhd` — but only these three are *settable*.)
- **`maxBitrate`** — bits/sec; `0` = uncapped.
- **`maxStreams`** — concurrent Playback sessions; `0` = uncapped.

`maxResolution` and `maxBitrate` are **clamped into the session's constraints before negotiation**: the effective constraint is the stricter of what the client asked for and what the User is allowed, and zero on either side means "the other one". The existing tiering then does what it always does — a 1080p Edition direct-plays, a 4K-only File transcodes down under [ADR-0009](./adr/0009-transcode-governance.md) governance and is refused `503 SERVER_BUSY` when the budget is full. **A ceiling never hides a Title**; it changes how something plays, not whether it exists.

`maxStreams` is enforced at session creation, under the same lock as the session map insertion, so two simultaneous negotiations cannot both slip past a limit of one. At the cap the negotiation answers `429 STREAM_LIMIT` with `details: { "active", "limit" }`; ending a session frees the slot, and the reaper defines "unended". A **relayed** play counts against the home User's `maxStreams` here *and* against the `remote` User's on the sharer — the two Servers each count their own, which is the point of the lever.

#### POST /users/{id}/invite — minting the one string

The `remote` User's credential comes into being here and nowhere else ([ADR-0055](./adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md) §1–§2). Creating the User mints nothing; re-keying a Link means minting again, and nothing about the User changes.

`origins` are the addresses the other Server should **try**, in order, and they are **typed by the sharing Admin** because this Server does not know its own public address and deliberately never emits one ([ADR-0005](./adr/0005-discovery-and-tls-via-reverse-proxy.md), the retired External URL). Each must be an absolute `http(s)` origin with **no path**; a lone trailing `/` is folded away, the host is lowercased, exact duplicates are dropped, and the **order is preserved** because it is meaningful. A path prefix, a malformed origin, or an **empty list** is `422 INVALID_ORIGIN` — an invite with no address could only ever fail on the other household's machine, with nothing on this side to explain why.

`invite` is one self-contained string, not "a hostname and a code":

```
obelo-link:<base64url(JSON)>

{ "v": 1, "id": "<this server's id>", "name": "<this server's name>",
  "origins": [ … ], "code": "…", "exp": "<RFC3339>" }
```

It is **not** an `https://` URL: there is no hosted page to open it against, and the action it triggers belongs on the *redeeming* Server, which the string cannot name. The origins are not secret and the code is single-use for 24 hours, so the whole thing is safe in a chat — the same reasoning that makes a User code safe. The raw code exists **only in this response**; minting a new invite invalidates any unredeemed one for the same User. The web app additionally renders the string as a QR, which is a convenience over the string and never the mechanism.

### 3.3 Libraries & scanning

`libraryJSON`: `{ "id", "name", "kind", "createdAt"?, "rootFolders": [ { "id", "path" } ], "linked"?, "available"?, "linkedServer"? }`.

**`linked` and `available` describe a mirror** ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §1), and both are absent on an ordinary local Library — a Server that has never linked emits exactly the wire it always did.

- **`linked: true`** — this Library's contents live on another household's Server and arrived over a Link (§3.11). It has no `rootFolders`, the Scanner never sees it, and it is granted, rating-capped, searched, collected, playlisted and watch-stated here exactly like any other Library. `linked` is `omitempty`: it is never sent as `false`.
- **`available`** — sent **only** for a linked Library, and then always (a `*bool`, so `false` is a statement rather than an absence). It is `true` while the Link's state is `connected`. `false` means the sharing Server is not answering: the shelf stays, its Titles stay listed, and a play answers `503 LINK_UNREACHABLE`. **Grey it, do not hide it** — Continue Watching must survive a friend's reboot.
- **`linkedServer`** — the sharing Server's display name (as this household recorded it on the Link). Present only on a linked row; `omitempty`-dropped on a local one, like the other two. It rides the row itself rather than the Admin-only `GET /links` join, so a **Member** can name whose shelf a mirror is — the provenance a member-facing surface shows beside the link mark ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §6). Exposing the sharer's chosen name to non-admin members is intended.

The same three fields ride on `titleSummaryJSON`, `showSummaryJSON` and `artistSummaryJSON` (§3.4), so a grid can badge a mirrored row — and name its Server — without a second request. `GET /links` (§3.11) still carries the name for the admin Links surface; `linkedServer` is what makes it reachable to everyone else.

**Every write aimed at a mirror is `409 LINKED_LIBRARY`** — 37 routes across scan, targeted scan, enrichment, the enrichment policy, fix-match, override deletion, the file matcher, per-entity editing, artwork upload, and subtitle fetch. Not `403` and not `404`: the caller is an Admin, the Library is theirs to see and grant, and it plainly exists — it is the *state* of it that refuses, which is what Conflict means. The Server that owns the files is the identity authority for them ([ADR-0002](./adr/0002-naming-convention-is-identity-authority.md), [ADR-0019](./adr/0019-item-editing-preserves-local-identity.md)), so a correction belongs on that machine. The Admin attention reads (`needs-review`, `unmatched`, `enrichment-attention`, `show-problems`, the matcher) answer **empty** for a mirror rather than refusing — an empty list is the true answer, since that queue is the sharer's.

| Endpoint | Notes |
| --- | --- |
| `POST /libraries` — [Admin] | `{ "name", "kind": "movie"\|"tv"\|"music", "rootFolders": ["/abs/path"] }` → `201` libraryJSON. Errors: `400`, `409 FOLDER_OVERLAP`. A linked Library is never created here — it appears when a Link is made (§3.11). |
| `GET /libraries` — [Public] | → `200` `{ "libraries": [ … ] }`, filtered to the caller's grants. |
| `GET /libraries/{id}` — [Public] | → `200` libraryJSON. Ungranted/unknown → `404`. |
| `PATCH /libraries/{id}` — [Admin] | `{ "name"?, "addRootFolders"?: [ … ] }` (partial; `kind` immutable) → `200` libraryJSON. Errors: `400`, `404`, `409 FOLDER_OVERLAP`. On a **linked** Library a `name` is accepted and `addRootFolders` is `409 LINKED_LIBRARY`: what this household calls somebody else's shelf is this household's business; giving it a folder is not, because it has none. |
| `DELETE /libraries/{id}` — [Admin] | → `204`. `409 LINKED_LIBRARY` for a mirror — deleting the shelf alone would leave a Link syncing into nothing; `DELETE /links/{id}` is what removes it (§3.11). |
| `GET /libraries/{id}/export` — [Admin] / **`remote`** | The Library Export. See below. |

#### GET /libraries/{id}/export — [Admin] / [`remote`]

`?since=<cursor>&cursor=<cursor>&limit=<n≤500>`

The **one flat, incremental feed** a sharing Server offers for one Library ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §4). It is *not* the browse API and no client should call it: browse is nested, capped per row, carries the caller's Watch state, and has no way to ask "what changed" — a full walk of a TV library through it is one request per Show and then one per Season. The export is also the **single place** the sharing Server decides what another Server may learn about a Title, rather than an audit of every browse handler.

Readable by a `remote` User (for its granted Libraries) and by Admins. A Member gets `404`, like any route outside their scope. A **linked** Library answers `404` for *every* caller including an Admin — a mirror is never re-shared onward ([ADR-0054](./adr/0054-a-linked-server-is-a-user-with-the-remote-role.md) §4).

```json
{
  "linkProtocolVersion": 1,
  "library": { "id": "…", "kind": "movie", "name": "Films" },
  "entities": [
    { "type": "title", "id": "…", "parentId"?: "…",
      "updatedAt": "2026-09-01T10:00:00Z", "deletedAt"?: "…", "data": { … } }
  ],
  "nextCursor"?: "opaque",
  "checkpoint": "opaque"
}
```

- **`type`** ∈ `title | show | season | episode | artist | album | track | edition | file | stream`. One table serves three of them: a Movie is a `title` with no `parentId`, an Episode an `episode` under its Season, a Track a `track` under its Album.
- **`data`** is the entity's *public* fields only — what the browse API already exposes, plus genres and cast. Deliberately absent: **every path and folder name** (a test greps the whole body for `/`-rooted values), `mtime`, artwork rows (artwork is relayed and cached, §3.11), and every enrichment **bookkeeping** column — attempts, retry times, reasons, the id origin, the episode pin, `reviewed`. Those are the sharing Server's notes about how it reached its own answer. Files carry container, codecs, resolution, bitrate, duration and size: what a Capability profile needs, nothing that names disk.
- **`deletedAt`** is the tombstone. Soft-deleted Files ([ADR-0008](./adr/0008-incremental-scan-soft-delete-missing-files.md)) carry it, and so does a Title with no live Files. Editions and Streams have no soft delete of their own and are rebuilt with fresh ids by the sharing Server's scanner, so their removal carries no tombstone — it is visible only as an **absence in a full pull**, which is why a mirror re-walks a Library in full whenever an incremental page mentions an Edition, File or Stream.
- **Ordering is keyset on `(updatedAt, type, id)`**, and `since`/`cursor` are the same opaque shape: `since` starts a walk, `cursor` continues one, and the walk position wins when both are sent. **`checkpoint` is on every page** — the position after this page's last entity, equal to `nextCursor` when more follow — so a mirror that stops early resumes exactly where it stopped, and an empty page hands back the bound it was given rather than "the beginning".
- Propagation stops at the Title: a Stream change bumps its File, its Edition and its Title, so a codec edit surfaces as a Title change. A Season does **not** bump its Show and an Album does not bump its Artist — each is exported as its own entity with its own stamp.

Errors: `400 BAD_REQUEST` `"invalid cursor"` (garbled), `410 RESYNC` (a `since` older than the 30-day tombstone retention — the position is gone, so pull in full), `404` (unknown, ungranted, or linked), `401` anonymous. `limit` over 500 is **clamped, not refused**: the caller is another Server, and a smaller page costs it one round trip where a `400` costs it the sync.

**Scan status shape** (`scanStatusJSON`, shared by trigger + status + targeted scans):

```json
{ "libraryId": "…", "state": "idle|running|error", "titleCount": 0,
  "titlesFound": 0, "filesFound": 0, "errorMessage"?, "startedAt"?, "finishedAt"?, "scope"? }
```

`titleCount` (top-level Movies/Shows/Albums, the User's sense of "titles") is populated only on the status GET; `titlesFound`/`filesFound` are the scanner's leaf counts. `scope` is the Targeted-scan entity label while one runs.

| Endpoint | Notes |
| --- | --- |
| `POST /libraries/{id}/scan` — [Admin] | Optional `?mode=full` or `{ "mode": "full" }` (default incremental). → **`202`** with status (`state:"running"`). **Async**: runs on a background context — client disconnect does not cancel. Idempotent: an in-flight scan returns `202` with its status. Completion auto-enriches + emits `libraryUpdated`. Errors: `404`. |
| `GET /libraries/{id}/scan` — [Public] | → `200` status with `titleCount`. The poll fallback for `scanProgress`. |
| `POST /{titles\|shows\|albums\|artists}/{id}/scan` — [Admin] | Targeted scan ([ADR-0030](./adr/0030-targeted-scan-scope-from-existing-file-folders.md)). No body. → `202` status (SSE `scanProgress` carries `scope`). Errors: `404 "item not found"`, `409 NO_FILES` (all Files Missing), `500`. Shares the per-Library scan lock. |

### 3.4 Browse — movies / TV / music

**`titleSummaryJSON`** — the browse-grid row, reused by search (movies/episodes/tracks) and collection/playlist members:

```json
{ "id": "…", "kind": "movie|episode|track", "title": "…",
  "year"?, "needsReview"?, "ambiguous"?, "tmdbId"?, "imdbId"?, "addedAt"?,
  "resumePositionMs"?, "watched"?, "overview"?, "contentRating"?, "releaseDate"?,
  "runtimeMinutes"?, "studio"?, "genres"?: [], "enrichmentStatus"?, "artworkVersion"?,
  "linked"?, "available"?, "linkedServer"? }
```

`linked`/`available`/`linkedServer` mean exactly what they mean on `libraryJSON` (§3.3): this row lives in a mirror of another household's Library, whether that Server is answering, and the sharing server's display name (present only on a linked row). All three are absent on a local Title.

**Every browse row carries the same three fields**, so a client never has to infer the mark — or look up whose shelf it is — from the screen the viewer arrived through: `showSummaryJSON` and `artistSummaryJSON` here, the Album entries and the `album` object of the music endpoints below, the Episode rows of `GET /seasons/{id}/episodes`, and `homeTitleJSON` (§3.5). The three shapes that carry none are the ones that cannot need it — `seasonJSON` (no Library of its own; the Show beside it, or the Episode rows under it, say so), the `resumePoint` block (inside a Show that says so), and `titleDetailJSON` (reached from a row that says so, and its `POST /titles/{id}/playback` answers `LINK_UNREACHABLE` / `LINK_REVOKED` in its own right).

`resumePositionMs`/`watched` are the **calling User's** watch state. `enrichmentStatus` ∈ `pending|matched|unmatched|failed|disabled` — note `failed` does **not** imply the item needs a human: a transient provider failure is recorded `failed` with a retry scheduled, and only a `failed` item with no retry (or one that has escalated) reaches the attention list ([ADR-0048](./adr/0048-a-transient-enrichment-failure-is-retried-not-parked.md)). `artworkVersion` is an opaque cache-bust token.

#### GET /libraries/{id}/titles — [Public]

One route, three shapes by Library kind:

- **movie** → `{ "titles": [ titleSummaryJSON ], "nextCursor"? }`. Query: `limit` (default 20, max 100), `cursor`, `sort` (`title` default; `dateAdded`/`addedAt`/`-addedAt`/`recent` = newest-first), `filter[genre]` (exact string).
- **tv** → `{ "shows": [ showSummaryJSON ], "nextCursor"? }`. Query: `limit`, `cursor`, `filter[genre]` (no `sort`).
- **music** → `{ "artists": [ artistSummaryJSON ], "nextCursor"? }`. Query: `limit`, `cursor`, `filter[genre]`.

Errors: `400 BAD_REQUEST` `"invalid cursor"`, `404` (unknown/ungranted library).

**`showSummaryJSON`** (grid): `{ "id", "libraryId"?, "kind": "show", "title", "year"?, "needsReview"?, "tmdbId"?, "imdbId"?, "identityKey"?, "addedAt"?, "unwatchedEpisodeCount"?, "overview"?, "genres"?, "contentRating"?, "network"?, "enrichmentStatus"?, "posterUrl"?, "backgroundUrl"?, "logoUrl"? }` — artwork URLs carry `?v={version}` cache-busters; `lockedFields`/`enrichmentOverride`/`cast` appear only on the seasons detail.

**`artistSummaryJSON`** (grid): `{ "id", "libraryId"?, "kind": "artist", "name", "enrichmentStatus"?, "artworkUrl"? }` — `overview`/`genres`/`backgroundUrl`/`logoUrl` appear only on the albums detail.

#### GET /titles/{id} — [Public]

The full nested Title detail (`titleDetailJSON`): Editions → Files → Streams, plus artwork, subtitles, metadata, per-user watch state.

```json
{
  "id": "…", "libraryId": "…", "kind": "movie", "title": "Back to the Future", "year": 1985,
  "needsReview"?, "ambiguous"?, "hidden"?, "tmdbId"?, "imdbId"?,
  "resumePositionMs"?, "watched"?, "addedAt": "…",
  "editions": [ { "id": "…", "name": "SD", "files": [ {
      "id": "…", "path": "/…/file.mkv", "container": "matroska",
      "videoCodec"?, "audioCodec"?, "width"?, "height"?, "bitrate"?, "durationMs"?, "sizeBytes"?, "missing"?,
      "streams": [ { "index": 0, "kind": "video|audio|subtitle", "codec": "…", "language"?, "width"?, "height"?, "channels"?, "isDefault": false } ],
      "audioStreams": [ { "id": "…", "index": 1, "codec": "aac", "language"?, "channels"?, "layout"?, "isDefault": false, "commentary"?, "label": "Unknown Mono" } ],
      "videoStreams": [ { "id": "…", "index": 0, "codec": "h264", "language"?, "width"?, "height"?, "isDefault": false, "label": "180p" } ]
  } ] } ],
  "extras": [ { "id", "type", "path", "container"?, "durationMs"? } ],
  "artwork": [ { "role": "poster", "url": "/api/v1/titles/{id}/artwork/poster", "path": "…", "source": "local|fetched" } ],
  "artworkVersion"?,
  "subtitles": [ { "id", "source": "embedded|sidecar|fetched", "kind": "text|image", "language"?, "forced": false, "label": "English" } ],
  "overview"?, "tagline"?, "contentRating"?, "releaseDate"?, "runtimeMinutes"?, "studio"?, "genres"?,
  "cast"?: [ { "person", "role"?, "character"?, "kind"?, "personId"?, "photoVersion"? } ],
  "enrichmentStatus"?, "lockedFields"?, "identityKey"?, "displayTitle"?,
  "episode"?: { "showId", "showTitle", "showYear"?, "seasonId", "seasonNumber", "episodeNumber"?, "episodeLabel"? },
  "track"?: { "artistId", "artistName", "albumId", "albumTitle", "albumYear"?, "discNumber"?, "trackNumber"? }
}
```

`hidden: true` = every File Missing (excluded from browse lists but still fetchable). `episode`/`track` context appears only for those kinds. `audioStreams`/`videoStreams` are the client-selectable projections; `streams` is the raw FFmpeg-level list. Errors: `404` (unknown / ungranted / above ceiling).

#### TV & music hierarchy — all [Public], bearer-only, unpaginated

| Endpoint | Response |
| --- | --- |
| `GET /shows/{id}/seasons` | `{ "show": showSummaryJSON (fully decorated, incl. cast/lockedFields), "seasons": [ { "id", "showId", "seasonNumber", "specials"?, "episodeCount", "posterUrl"? } ], "resumePoint"?: { "id", "kind": "episode", "seasonId", "seasonNumber", "episodeNumber"?, "episodeLabel"?, "title", "overview"?, "resumePositionMs"?, "durationMs"?, "mode": "inProgress"\|"next", "enrichmentStatus"?, "stillUrl"? } }` — `resumePoint` is the Up Next anchor ([ADR-0028](./adr/0028-up-next-anchors-on-most-recently-played.md)); absent for not-started **and** fully-watched shows (disambiguate via `show.unwatchedEpisodeCount`). |
| `GET /seasons/{id}/episodes` | `{ "season": seasonJSON, "episodes": [ { "id", "kind": "episode", "title", "seasonNumber", "episodeNumber"?, "episodeLabel"?, "needsReview"?, "resumePositionMs"?, "watched"?, "addedAt"?, "overview"?, "enrichmentStatus"?, "stillUrl"?, "linked"?, "available"?, "linkedServer"? } ] }` — the Episode rows carry the mirror fields because nothing else in this document can: a Season has no Library of its own and the marked Show is a screen back. |
| `GET /artists/{id}/albums` | `{ "artist": artistSummaryJSON (decorated), "albums": [ { "id", "artistId", "title", "year"?, "hasArtwork"?, "artworkVersion"?, "releaseType"?, "genres"?, "enrichmentStatus"?, "trackCount", "linked"?, "available"?, "linkedServer"? } ] }` — `releaseType` is the normalized tag type (`"album"`, `"single"`, `"ep"`, …; absent when untagged); clients badge non-`album` types so same-titled releases (split per [ADR-0038](./adr/0038-album-identity-release-group-wins.md)) are tellable apart. |
| `GET /albums/{id}/tracks` | `{ "album": albumJSON (incl. "artistName"), "tracks": [ { "id", "kind": "track", "title", "discNumber"?, "trackNumber"?, "durationMs"?, "needsReview"?, "resumePositionMs"?, "watched"?, "overview"?, "enrichmentStatus"?, "linked"?, "available"?, "linkedServer"? } ] }` — disc/track order. |

`albumJSON` carries `linked`/`available`/`linkedServer` wherever it appears — the entries above, the `album` object here, and the `albums` group of `GET /search` (§3.5), which is the one place an Album row arrives with no Artist beside it to inherit the mark from. A Track row carries it too: a queue built from an Album list outlives the screen it was built on.

All: unknown/ungranted parent → `404`.

#### Artwork bytes — all [Public], bearer **or media cookie**

Raw image bytes (`http.ServeFile`: content type sniffed, Range-capable). Unknown role or no image → `404 "artwork not found"`.

| Endpoint | Roles |
| --- | --- |
| `GET /titles/{id}/artwork/{role}` | `poster`, `background` (an Episode still serves as `poster`) |
| `GET /shows/{id}/artwork/{role}` | `poster`, `background`, `logo` |
| `GET /seasons/{id}/artwork/{role}` | `poster` |
| `GET /artists/{id}/artwork/{role}` | `poster` (artist photo), `background`, `logo` |
| `GET /albums/{id}/artwork` | (no role — the local cover; `404` when none, client falls back) |
| `GET /people/{personRef}/artwork/{role}` | `profile` — `personRef` is provider-namespaced (`tmdb:3`); a person credited only in inaccessible Libraries is hidden `404` |

URLs advertised in parent JSON carry `?v={version}` cache-busters; title-detail artwork URLs don't. The query string is ignored server-side.

### 3.5 Home & search — [Public]

#### GET /home

```json
{ "continueWatching": [ homeTitleJSON ], "upNext": [ homeTitleJSON ], "recentlyAdded": [ homeTitleJSON ] }
```

`homeTitleJSON`: `{ "id", "kind", "title", "year"?, "tmdbId"?, "imdbId"?, "addedAt"?, "resumePositionMs"?, "durationMs"?, "episode"?, "track"?, "overview"?, "genres"?, "displayTitle"?, "linked"?, "available"?, "linkedServer"? }`. `linked`/`available`/`linkedServer` mean exactly what they mean on `libraryJSON` (§3.3) and `titleSummaryJSON` (§3.4), and are absent on a local Title — a Home row is where a viewer meets a mirrored Title with no Library screen around it, so the row itself both marks it and (via `linkedServer`, present only on a linked row) names the sharing server. Each row capped at 20, computed per-User, never stored. Continue Watching = 2–90% band, most recent first; Up Next = TV resume points; Recently Added = newest first. `resumePositionMs`/`durationMs` are populated on **Continue Watching only** — together they drive the card's progress bar, the same pairing `resumePoint` carries on the Show detail; Up Next / Recently Added omit both, and `durationMs` is also omitted when the duration is unknown.

#### GET /search?q=…

```json
{ "movies": [], "shows": [], "artists": [], "albums": [], "episodes": [], "tracks": [] }
```

Six always-present groups reusing the browse summary DTOs (search results carry no `?v=` artwork cache-buster), so every hit carries `linked`/`available` when it lives in a mirror — including the `albums` group, whose rows have no Artist beside them. `q` (fallback `query`); empty `q` → `200` with empty groups. `limit` caps each group (default 20, max 100). Case-insensitive substring on display names, access-filtered.

### 3.6 Playback

#### POST /titles/{id}/playback — [Public] (bearer only)

Negotiates a Capability profile → picks a tier ([ADR-0003](./adr/0003-three-tier-playback-with-capability-negotiation.md)) → creates a Playback session. All request fields optional (an empty profile lands on the transcode tier):

```json
{
  "deviceProfile": {
    "containers": ["mp4", "mkv"],
    "videoCodecs": [ { "codec": "h264", "maxLevel": "4.2", "maxResolution": "1080p", "hdr": ["hdr10"] } ],
    "audioCodecs": ["aac", "ac3"],
    "maxAudioChannels": 6,
    "textSubtitleFormats": ["vtt"],
    "hevcInMpegts": false
  },
  "constraints": { "maxBitrate": 8000000, "maxResolution": "1080p",
                   "preferredAudioLang": "en", "preferredSubtitleLang": "en" },
  "startPosition": 0,
  "editionId": "",
  "burnSubtitleId": "",
  "audioStreamId": "",
  "videoStreamId": "",
  "remuxSelectedOnly": false
}
```

`maxBitrate` (bits/sec) reflects current network and is the field that most often flips direct-play into transcode. `maxLevel`/`hdr`/`preferredSubtitleLang` are recorded, not yet enforced. `textSubtitleFormats` **is** enforced — it selects each text subtitle's delivery format (original vs WebVTT, [ADR-0033](./adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)); omit it (or declare only `vtt`) for WebVTT everywhere. Resolution tokens: `144p…4320p` plus `sd/hd/fhd/2k/4k/uhd/8k`. `audioStreamId`/`videoStreamId`/`burnSubtitleId` select non-default streams (may escalate the tier; a video pick restarts in-container, [ADR-0025](./adr/0025-selectable-video-streams-in-container-restart-switch.md)). `remuxSelectedOnly` (default `false`) forces a lean, **copy-only** `directStream` — one video + one audio Stream — on a File that would **otherwise directPlay**: the negotiated tier becomes `directStream` with the FFmpeg map pinned to the selected/negotiated video + audio (the `videoStreamId`/`audioStreamId` picks if sent, else the negotiated defaults), every other a/v Stream dropped, codecs copied (never re-encoded, so it does **not** count against the transcode cap). It is a **no-op** when the session is already `directStream`/`transcode` for another reason (container mismatch, a Quality cap, an AAC narrowing, a non-default pick) or when the resolved audio cannot ride the remux container — the returned `tier` tells the truth. Subtitles are unchanged (out-of-band / in-band renditions). Advertised via `features.remuxSelectedOnly`; a client hides the affordance when it is absent.

→ `200` decision (live-verified):

```json
{
  "sessionId": "…",
  "tier": "directPlay",
  "streamUrl": "/api/v1/sessions/{sessionId}/stream",
  "edition": { "id": "…", "name": "SD" },
  "videoStream"?: { "index": 0, "codec": "h264", "width": 320, "height": 180 },
  "audioStream"?: { "index": 1, "codec": "aac", "channels": 1 },
  "audioStreams": [ { "id": "…", "index": 1, "codec": "aac", "language"?, "channels"?, "layout"?, "isDefault": false, "commentary"?, "label": "Unknown Mono" } ],
  "videoStreams": [ { "id": "…", "index": 0, "codec": "h264", "width": 320, "height": 180, "isDefault": true, "label": "180p" } ],
  "subtitles": [ { "id", "source": "embedded|sidecar|fetched", "kind": "text|image",
                   "language"?, "forced": false, "label": "English",
                   "url"?: "/api/v1/titles/{id}/subtitles/{subId}.vtt",
                   "format"?: "vtt|srt|ass" } ],
  "estimatedBitrate": 112023,
  "streamToken"?: "opaque-256-bit-secret",
  "streamTokenExpiresAt"?: "2026-08-03T14:00:00Z"
}
```

- `tier` ∈ `directPlay | directStream | transcode`.
- `streamUrl`: **directPlay** → `/sessions/{id}/stream` (progressive byte-range); **directStream/transcode** → `/sessions/{id}/hls/master.m3u8` when the session has demuxed audio renditions or deliverable text subtitles, else `/sessions/{id}/hls/index.m3u8` ([ADR-0004](./adr/0004-hls-for-adaptive-progressive-for-direct-play.md)).
- `videoStream` **omitted for an audio-only Decision** — a music Track, or any File whose only video Stream is cover art ([ADR-0017](./adr/0017-audio-only-playback-path.md)). Test the field's **presence**, not its contents: it is absent, never a zero value. (It formerly marshalled as `{"index":0,"codec":""}` on every Track — present but empty, so `if (d.videoStream)` was true for audio-only and clients had to sniff the empty codec. That shape is gone; a client still carrying such a workaround can drop it once it no longer talks to an older server.) The `videoStreams` list is unaffected — it was already `[]` for a Track and stays a present empty list.
- `streamToken` / `streamTokenExpiresAt` — the session's **stream token** and its expiry ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)), minted with the session so handing a media URL to a receiver is one request rather than two. Gated by `features.streamToken`. Both are `omitempty` and must be treated as **optional**: they are absent on a server predating this slice, and absent on this one if the mint failed — the session is still playable over the bearer or the cookie, so a mint hiccup omits the fields rather than failing a negotiation the client can act on. The token substitutes into the path routes below (**not** into `streamUrl`, which stays the session-id form for the bearer/cookie paths): `/api/v1/stream/{streamToken}/stream` for a `directPlay` decision, `/api/v1/stream/{streamToken}/hls/{the same artifact streamUrl names}` for an HLS one. Scope, expiry, and revocation are in §Auth; treat the value as a secret — never display it, never log it.
- `audioStream` omitted only for a silent File. Subtitle `url` present only for text tracks ([ADR-0020](./adr/0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md)); image tracks burn in via `burnSubtitleId`. `format` names what the `url` serves ([ADR-0033](./adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)): when `deviceProfile.textSubtitleFormats` declares the track's **original** format (`srt`/`ass`, aliases `subrip`/`ssa` fold), the url points at the original bytes — ASS styling intact — else at the WebVTT conversion. Embedded `mov_text` is always WebVTT-only.

Errors:
- `404` `"title not found"` / `"subtitle not found"` / `"audio stream not found"` / `"video stream not found"`.
- `501 TRANSCODE_REQUIRED` — structurally unplayable for this client; `details: { "reason": "container|videoCodec|audioCodec|resolution|bitrate|audioChannels|noVideo|noFile", "detail": "…" }`.
- `503 SERVER_BUSY` — transcode cap full ([ADR-0009](./adr/0009-transcode-governance.md)); `details: { "retryable": true, "suggestedMaxBitrate": <half the estimate, floor 600000> }`. Direct play/remux never hit this.
- `429 STREAM_LIMIT` — the calling User is at their Playback ceiling's `maxStreams` (§3.2); `details: { "active", "limit" }`. Ending another session frees the slot. Not retryable on its own.
- `503 LINK_REVOKED` / `503 LINK_UNREACHABLE` — the Title lives in a **linked** Library and the sharing Server refused the credential, or did not answer at all (§3.11). Two sentences that are deliberately not interchangeable: one invites a retry, the other names the Admin's fix (paste a new invite).

**A Title in a linked Library negotiates the same way and comes back with a different session.** The decision is the *sharing* Server's — its tier, its estimate, its stream lists — with four things changed and everything else re-served untouched: `sessionId` is the local session's, `streamUrl` and each subtitle `url` are rewritten onto `/relay/{sessionId}/…`, and the sharer's stream token is dropped in favour of one minted here. Any other refusal the sharer gives — `SERVER_BUSY` with its `suggestedMaxBitrate`, `STREAM_LIMIT` with its counts, `TRANSCODE_REQUIRED` with its reason — **passes through verbatim**, because this Server knows nothing that would improve it. See §3.11.

#### Session lifecycle

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `GET /sessions/{id}/stream` — [Public] | bearer **or cookie** | Progressive bytes; `Range`/`If-Range`/HEAD supported; `200`/`206`. Foreign/ended session → `404`. Seek = byte range; no new decision needed. |
| `GET /sessions/{id}/hls/{file}` — [Public] | bearer **or cookie** | HLS artifacts: `master.m3u8`; `index.m3u8` + `NNN.ts`/`.m4s` + `init.mp4` (video); `audio_{streamId}.m3u8` + `audio_{streamId}_NNN.ts`/`.m4s` + `audio_{streamId}_init.mp4`; `subs_{subId}.m3u8` + `subs_{subId}_NNN.vtt` (4s cadence). Content types: `application/vnd.apple.mpegurl`, `video/mp2t`, `video/mp4`, `text/vtt`. Playlists are `Cache-Control: no-cache`. Errors: `404` `"session media unavailable"` / `"segment not available"`; `500 INTERNAL` `"failed to serve HLS media"` when FFmpeg cannot be launched at all — which is what a server advertising `features.transcode: false` answers here, immediately rather than by hanging ([ADR-0040](./adr/0040-transcode-tier-advertised-from-startup-resolved-ffmpeg-availability.md)). |
| `POST /sessions/{id}/progress` — [Public] | bearer only | `{ "positionMs": 123456, "state": "playing|paused|buffering", "audioStreamId"?, "videoStreamId"? }` every ~10–15s → `200` `{ "titleId", "resumePositionMs", "watched" }`. **Doubles as keepalive** — an idle session is reaped (FFmpeg killed, scratch deleted, cap slot freed). `audioStreamId` records an in-band audio pick as the Remembered audio ([ADR-0023](./adr/0023-per-user-audio-memory-two-level-language-keyed.md)); `videoStreamId` is its video mirror for players that switch video tracks in-container without re-negotiating (libmpv on direct play — records the Remembered video, ADR-0025). Unknown ids are ignored, best-effort. |
| `DELETE /sessions/{id}` — [Public] | bearer only | Clean stop → `204`. **Takes no body** — report the final position via a last `/progress` POST before deleting. **Also revokes the session's stream tokens**, in the same cascade the idle reaper fires. |
| `POST /sessions/{id}/stream-token` — [Public] | **bearer only** | Re-mints the session's stream token ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)). No body → `201` `{ "streamToken", "streamTokenExpiresAt", "expiresIn": 14400 }` — the same two field names the Decision uses, so a client has one name for the value however it obtained it, plus `expiresIn` in seconds so it can schedule a re-mint without trusting its clock against the server's. Gated by `features.streamToken`. For a client that decides to AirPlay long after negotiating: cheaper than re-negotiating a stream already playing. **Bearer only** — deliberately not the cookie and never a stream token, so a credential already handed to a television cannot extend its own life. Unknown, ended, or another User's session → `404` `"session not found"` (never `403`). |
| `GET /stream/{streamToken}/stream` — [Stream token] | **stream token in the path, no bearer, no cookie** | The same progressive bytes as `GET /sessions/{id}/stream`, for the same session — `Range`/`If-Range`, `200`/`206`, `Accept-Ranges: bytes`, content type sniffed from the File. **No session id in the URL: the token identifies the session.** |
| `GET /stream/{streamToken}/hls/{file}` — [Stream token] | **stream token in the path, no bearer, no cookie** | The same HLS artifacts, named identically to the session-id route: `master.m3u8`, `index.m3u8`, `NNN.ts`/`.m4s`, `init.mp4`, `audio_{streamId}*`, `subs_{subId}*`. Content types: `*.m3u8` → `application/vnd.apple.mpegurl` (+ `Cache-Control: no-cache`); `.ts` → `video/mp2t`; `.m4s`/`.mp4` → `video/mp4`; `.vtt` → `text/vtt; charset=utf-8`. Byte-identical to what the bearer path serves — no playlist is rewritten. |

**Both `/stream/…` routes are GET-only and answer every refusal identically.** A non-GET is `405 METHOD_NOT_ALLOWED` with `Allow: GET`, refused **before** the token is examined (otherwise the difference between `405` and `404` would be a token-validity oracle). Every credential or path failure — unknown token, expired token, a token whose session was revoked, an account bearer pasted into the path, a malformed path, an unknown artifact — is one byte-identical `404 { "error": { "code": "NOT_FOUND", "message": "session not found" } }`, with **no** `WWW-Authenticate` header. That is the same envelope a wrong-User fetch on `/sessions/{id}/stream` already produces, and it is deliberate: a refusal that told the cases apart would confirm which tokens are real.

**The token rides in the path, not a query string** ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)). Every playlist this server emits uses **bare relative URIs** (`000.ts`, `#EXT-X-MAP:URI="init.mp4"`, `audio_7.m3u8`), and a player resolves those against the playlist's URL **with the query string discarded** — so `?token=` would authenticate the manifest and fail every segment beneath it, while a path prefix carries down for free and not one playlist byte changes. Do not "tidy" a stream-token URL into a query parameter: it passes any test that only fetches a manifest and breaks on a real player.

A new decision is required only when constraints change (e.g. bandwidth drop → lower `maxBitrate`) or a different video Stream is selected.

#### PUT /titles/{id}/watchState — [Public]

`{ "watched": true|false }` → `200` `{ "titleId", "resumePositionMs", "watched" }`. Bypasses the threshold; marking either way clears resume. A manual mark does **not** count as "played" for the Up Next anchor ([ADR-0028](./adr/0028-up-next-anchors-on-most-recently-played.md)).

#### Subtitles

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `GET /titles/{id}/subtitles/{subId}.{vtt\|srt\|ass}` — [Public] | bearer **or cookie** | `.vtt` → the WebVTT conversion (`text/vtt`; embedded text extracts via FFmpeg on demand) — every text track has it. `.srt`/`.ass` → the **original bytes**, styling intact ([ADR-0033](./adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)): sidecar/fetched read raw off disk, embedded codec-copied by FFmpeg (`application/x-subrip` / `text/x-ssa`). Served only when the track's own format matches — a mismatch, an image track, or an unconvertible format → `404`. All variants `Cache-Control: private, max-age=86400`. |
| `POST /titles/{id}/subtitles/search` — [Public] | bearer only | `{ "language": "de" }` → `200` `{ "candidates": [ { "id", "language", "format", "release"?, "forced", "hearingImpaired", "matchedBy"?: "moviehash|imdb|query", "label" } ] }`. Any User, Members included ([ADR-0021](./adr/0021-external-subtitle-fetching-mirrors-enrichment.md)). Disabled/offline provider → `200` with `[]`, never an error. |
| `POST /titles/{id}/subtitles/fetch` — [Public] | bearer only | `{ "language", "candidate": { …echoed from search… } }` → `200` `{ "subtitle": { "id", "source": "fetched", "kind", "language", "forced", "label", "url" } }` — a decision-style track (`url` is the `.vtt` conversion; the download is cached in its **original** format, so the next negotiation offers the original to a capable client, ADR-0033). Errors: `400`, `404` `"subtitle no longer available"`, `503 SERVICE_UNAVAILABLE`. |

#### GET /files/{id}/download — [Public] (bearer **or `?token=`**)

Sessionless original bytes ("Open in VLC"), Range-capable, no Playback session. **Access-scoped like browse**: the caller's Scope is applied to the Title that owns the File, in both dimensions (Library grant *and* Rating ceiling), so a Member cannot fetch bytes they could not browse to. Unknown id, out-of-scope File, orphaned File, and Missing File all return the **identical** `404 "file not found"` — the refusal must not distinguish "not yours" from "does not exist" (§ "404, not 403"); on-disk unreadable → `404 "file unavailable"`.

### 3.7 Collections, Playlists, Watchlist

Members are decorated with the **same `titleSummaryJSON` a browse grid uses**; playlist/watchlist members add one field — `itemId` (the entry's row id, distinguishing duplicates). Missing Titles are omitted from resolved views while their membership rows persist.

#### Collections — writes [Admin], reads [Public]

| Endpoint | Notes |
| --- | --- |
| `POST /collections` — [Admin] | `{ "name", "description"? }` → `201` `{ "id", "name", "description"?, "createdAt"?, "updatedAt"? }`. |
| `GET /collections` — [Public] | → `200` `{ "collections": [ { …collectionJSON, "memberCount", "posterUrl"? } ] }` — count + poster computed over the **viewer's visible** members; a collection the viewer can see nothing of is absent entirely. Admin sees all, including empty ones. Newest-first. |
| `GET /collections/{id}` — [Public] | → `200` `{ …collectionJSON, "memberCount", "members": [ titleSummaryJSON ] }` in `sort_title` order, access-filtered. Zero-visible (non-admin) → `404`. |
| `PUT /collections/{id}` — [Admin] | `{ "name", "description"? }` → `200` collectionJSON (both fields replace). |
| `DELETE /collections/{id}` — [Admin] | → `204`. Membership rows cascade; Titles untouched. |
| `POST /collections/{id}/items` — [Admin] | `{ "titleIds": [ … ] }` → `204`. Set semantics: re-add is a no-op; may span media kinds. `422 UNKNOWN_TITLE` rejects the whole add atomically. |
| `DELETE /collections/{id}/items/{titleId}` — [Admin] | → `204`. Removing a non-member is a no-op `204`. |

#### Playlists — [Public], owner == caller, **no Admin override**

A foreign Playlist is `404` on every leaf (hide-existence), including for Admins.

| Endpoint | Notes |
| --- | --- |
| `POST /playlists` | `{ "name" }` → `201` `{ "id", "name", "createdAt"?, "updatedAt"? }` — untyped until the first append. |
| `GET /playlists` | → `200` `{ "playlists": [ { "id", "kind"?, "system"?, "name", "createdAt"?, "updatedAt"?, "itemCount" } ] }` — caller's own only; `itemCount` is the **raw row count** (incl. Missing/dupes), unlike detail's `memberCount` (visible). The Watchlist appears here with `system: "watchlist"`. |
| `GET /playlists/{id}` | → `200` `{ "id", "kind"?, "system"?, "name", "createdAt"?, "updatedAt"?, "memberCount", "members": [ { …titleSummaryJSON, "itemId" } ] }` in position order; duplicates preserved, each with its own `itemId`. |
| `PUT /playlists/{id}` | `{ "name" }` → `200` playlistJSON. `422 SYSTEM_PLAYLIST` for the Watchlist. |
| `DELETE /playlists/{id}` | → `204`. `422 SYSTEM_PLAYLIST` for the Watchlist. |
| `POST /playlists/{id}/items` | `{ "titleId" }` → `204` (append at end; new `itemId` not returned). First append fixes the kind (`movie`/`tv`/`music` from title kinds movie/episode/track); `422 KIND_MISMATCH` on cross-kind; `422 UNKNOWN_TITLE`. |
| `PUT /playlists/{id}/items` | `{ "itemIds": [ … ] }` — the **full permutation of the item ids `GET /playlists/{id}` just returned you** (i.e. the *visible* ones), rewritten transactionally → `204`. Any mismatch — wrong count, duplicate, unknown id, or an id you can't see — → `422 ITEM_SET_MISMATCH`, order unchanged. Members omitted from the resolved view (Missing/out-of-scope) keep their **index** in the sequence: they neither move nor need naming, so a Missing member does not freeze the order. |
| `DELETE /playlists/{id}/items/{itemId}` | → `204`. By **item id** (duplicates safe). Unknown item → `404 "playlist item not found"`. The kind persists even when the last item is removed. |

#### Watchlist — [Public], the per-User system Playlist, addressed by name

Lazily seeded on first touch (name "Watchlist", `system: "watchlist"`); no create endpoint.

| Endpoint | Notes |
| --- | --- |
| `GET /watchlist` | → `200` playlistDetailJSON (same shape as `GET /playlists/{id}`, always `system: "watchlist"`). |
| `POST /watchlist/items` | `{ "titleId" }` → `204`. Same `422 UNKNOWN_TITLE` / `422 KIND_MISMATCH` as playlists; never 404s (it self-seeds). |
| `DELETE /watchlist/items/{itemId}` | → `204`. Unknown item → `404 "watchlist item not found"`. |

### 3.8 Enrichment & editing (admin scope)

The Admin curation surface behind Edit item ([ADR-0019](./adr/0019-item-editing-preserves-local-identity.md)). All bearer + admin.

#### Library-level

| Endpoint | Notes |
| --- | --- |
| `POST /libraries/{id}/enrich` — [Admin] | **Starts** a pass and returns at once — it is NOT held open for it. `?mode=full` or `{ "mode": "full" }` (default `new` = pending only); `?mode=recheck` / `{ "mode": "recheck" }` is `new` PLUS the settled non-answers — `unmatched`, and `failed` with no scheduled retry ([ADR-0051](./adr/0051-a-settled-non-answer-is-re-asked-when-the-question-changes.md)), the mode to run after a matching improvement ships. An unrecognized mode falls back to the default rather than 400-ing. → `202` the pass state (below) with `"started": true`; `202` with `"started": false` when a pass was **already running** for that Library (reported, never duplicated — the per-Library lock would serialize them anyway). `503 ENRICH_UNAVAILABLE` when this server runs no background pass worker, `503 ENRICH_BUSY` when the queue is full — both used to be silent, which is how an operator ended up watching a button that could never do anything. Unknown Library → `404`, validated before anything is queued. Progress arrives on the `enrichProgress` SSE stream ([ADR-0016](./adr/0016-sse-for-realtime-updates.md)), whose terminal `complete` event carries the summary. Unconfigured enrichment still runs a pass and counts its candidates `disabled`. |
| `GET /libraries/{id}/enrich` — [Admin] | → `200` `{ "libraryId", "state": "idle"\|"running", "mode"?, "startedAt"?, "progress"?: { "total", "done", "matched", "unmatched", "failed", "disabled", "retrying" }, "lastPass"?: { "libraryId", "total", "matched", "unmatched", "failed", "disabled", "retrying", "mode", "finishedAt" } }`. Is a pass running over this Library, how far along, and what came of the last one — so a **reloaded page rejoins** a pass instead of showing an idle button. `retrying` counts leaves whose lookup failed transiently and are scheduled to be tried again rather than parked ([ADR-0048](./adr/0048-a-transient-enrichment-failure-is-retried-not-parked.md)); `failed` is only the permanent kind. The status is held **in memory**, so a Library reads `idle` with no `lastPass` after a server restart — deliberately, because a status that outlived the process would be claiming a pass had. Unknown Library → `404`. |
| `GET /libraries/{id}/enrichment-policy` — [Admin] | → `200` policy view ([ADR-0027](./adr/0027-per-library-enrichment-policy-sparse-override.md)): `{ "enrichEnabled": bool\|null, "inheritedEnrichEnabled", "effective": { "video", "music" }, "configured": { "video", "music" }, "consentState": "unset"\|"granted"\|"declined", "metadataLanguage": string\|null, "inheritedMetadataLanguage", "authoritativeProvider": string\|null, "inheritedAuthoritative": { "slug", "name" }, "effectiveAuthoritative": { "slug", "name" }, "authoritativeUnreachable": string\|null, "authoritativeCandidates": [ { "slug", "name" } ], "supplements": [ { "slug", "name", "override": bool\|null, "inheritedEnabled" } ] }` — `null` = inherit the global setting. `effective` and `inheritedEnrichEnabled` are **consent-gated** ([ADR-0032](./adr/0032-optional-maintainer-key-rotation-endpoint.md)): both read off while consent is declined or unanswered, whatever the policy and providers say. `configured` is the same resolution WITHOUT the gate, and `consentState` says why they differ — the pair lets the UI say "configured, waiting on consent" rather than claiming enrichment that will not happen. |
| `PUT /libraries/{id}/enrichment-policy` — [Admin] | Tri-state partial update: omit = unchanged, `null` = clear-to-inherit, value = override. Keys: `enrichEnabled`, `metadataLanguage`, `authoritativeProvider`, `providerOverrides` (`{ "slug": true\|false\|null }`). → `200` fresh policy view. `422 PROVIDER_NOT_AUTHORITATIVE` (validated before any write). Side effect: kicks a background re-enrich. |
| `POST /libraries/{id}/fix-match` — [Admin] | `{ "folderPath", "title"?, "year"?, "tmdbId"?, "imdbId"? }` (folder + ≥1 identity signal) → `200` `{ "id", "folderPath", "title", "year"?, "tmdbId"?, "imdbId"?, "identityKey", "orphaned"?, "createdAt"? }`. Takes effect on the next scan; persists across rescans. |
| `GET /libraries/{id}/overrides` — [Admin] | → `200` `{ "overrides": [ matchOverrideJSON ] }` incl. orphaned ones (`"orphaned": true`). |
| `DELETE /libraries/{id}/overrides/{overrideId}` — [Admin] | Discard one Match override → `204`; `404` unknown override. Offered on an **orphaned** correction (its anchor folder is gone, so it can never apply again); removing it restores that folder's convention-derived parse and touches no Title and no watch state ([ADR-0002](./adr/0002-naming-convention-is-identity-authority.md)/[ADR-0014](./adr/0014-watch-state-keyed-to-parsed-identity.md)). |
| `GET /libraries/{id}/enrichmentCandidates?q=&artist=&page=` — [Admin] | The per-item candidate search (below), anchored to the **Library** instead of an item — the searched kind comes from the Library's media kind (`movie` → movie, `tv` → show, `music` → album). It exists for an **Unmatched** file, which by definition has no Title to anchor the per-item route to. Same response shape, page size, and `503 SEARCH_UNAVAILABLE` contract. |
| `GET /libraries/{id}/externalPreview?ref=` — [Admin] | The paste-an-id counterpart of the above, same Library anchor and same error contract. |

#### The Needs-Fixing surface (admin)

The four reads behind the Admin **Needs Fixing** queue — everything wrong with a Library, in one place. Every row carries the same **fix context** flat on the wire, which is what lets a client name an item on sight rather than printing a bare title, and *check* it rather than just read it:

- **Which item** — `path` (a representative present file; absent when every File is Missing), `showTitle` / `seasonNumber` / `episodeNumber` / `episodeLabel` for an Episode, `artistName` / `albumTitle` / `discNumber` / `trackNumber` for a Track. `seasonNumber` is `omitempty`, and its absent value 0 **is** the Specials season.
- **Which artwork** — `showId` / `albumId` name the parent whose image represents the row, since an Episode has no poster of its own and a Track's cover belongs to its Album. Address `/shows/{id}/artwork/poster` and `/albums/{id}/artwork/cover`; a Movie or Show uses its own id.
- **What it matched to** — `enrichedTitle` and `releaseDate` are the record Enrichment settled on. They are what a *confirmation* is made against: a needs-review item is flagged for an uncertain parse (usually a **missing year**), so its own parsed name can never say whether the filing is right, and `releaseDate` supplies the very year the parse lacks. Trust them only when the row's `enrichmentStatus` is `matched`.

| Endpoint | Notes |
| --- | --- |
| `GET /libraries/{id}/needs-review` — [Admin] | → `200` `{ "items": [ { "id", "kind": "movie\|episode\|track\|show", "title", "year"?, "folderPath"?, "needsReview", "ambiguous"?, "collidingPaths"?, "reason"?, "enrichmentStatus"?, …fix context } ] }`. The **identity** attention list: Titles and Shows the scanner filed from an uncertain parse (`needsReview`), plus Titles whose Files collide (`ambiguous`). An item carries either flag or both. `enrichmentStatus` says whether there is a matched record to confirm the filing against (a never-enriched Show reports `pending`, matching a Title's column default, rather than an empty string). `folderPath` is the anchor a fix-match must use, present only for the kinds a folder override can fix (Movie, Show, Track) — an Episode has none. `reason` ∈ `no-year` \| `episode-numbering` \| `untagged`, read off the rule the scanner applied, so a client can state the problem instead of the word "needs review"; it explains the `needsReview` flag only, and is absent on an item flagged solely `ambiguous`. `ambiguous` is the naming convention's **collision rule** — two or more Files parsed to the same Edition identity and are not parts, so the convention refuses to guess which is the real one; `collidingPaths` names them in play order, and only the first of them plays until it is settled. It is **not** dismissible: `POST /titles/{id}/review` clears the uncertain parse and leaves the collision listed, because the files really do still collide. `needsReview` is always sent (never omitted when false) so a client can tell "not flagged" from "an older server that did not send it". |
| `GET /libraries/{id}/unmatched` — [Admin] | → `200` `{ "files": [ { "id", "path", "folderPath"?, "kind"?, "reason"?, "addedAt"? } ] }`. Recognized media that produced no Title, so these carry no fix context. `kind` says WHY and decides what may be offered ([ADR-0047](./adr/0047-an-unreadable-file-is-its-own-attention-kind.md)): `"unidentified"` — nothing named the work, fixed by naming it; `"unreadable"` — the name parsed and **ffprobe refused the bytes**, fixed only by replacing the file (or ignoring it in the file matcher), never by an identity correction. Absent `kind` means `unidentified`. `folderPath` is the fix-match anchor, derived from the **Library's kind** (movie → the movie folder or the loose file, tv → the Show folder above the Season folder, music → the album folder); a client must not derive it from the path, where the file's own directory is right only for a Movie. It is **withheld for an `unreadable` file**, because there is no identity correction to key there. `reason` carries ffprobe's own verdict for those. |
| `GET /libraries/{id}/enrichment-attention` — [Admin] | → `200` `{ "titles": [ { "id", "kind", "title", "year"?, "enrichmentStatus", …fix context } ] }`. Titles whose Enrichment could not settle on a record (`unmatched` / `failed`). |
| `POST /titles/{id}/review`, `POST /shows/{id}/review` — [Admin] | Dismiss a needs-review flag — the Admin confirms the uncertain parse is fine. `204`; sticky across rescans. |
| `GET /libraries/{id}/show-problems` — [Admin] | → `200` `{ "shows": [ { "showId", "title", "year"?, "path"?, "unassigned"?, "unidentified"?, "unmatchedPaths"?, "orphaned"?, "orphanedPath"?, "unreadablePaths"? } ] }`. The per-Show **unsettled File** counts behind the Needs-Fixing queue's one-row-per-Show collapse (ADR-0044, file-matcher/07). Two of the three sources are invisible to a client: which flat `unmatched` paths fall under a given Show's folders (`unmatchedPaths`, which the client then drops from its own list so a file is never both a count and a row), and Files the Admin explicitly left **unassigned**, which produce neither a Title nor an Unmatched row. `unassigned` is undecided and keeps the Show queued; ignoring or placing settles it. An **`unreadable`** path is deliberately NOT absorbed into `unmatchedPaths`: absorbing a path promises the matcher can settle it, and that screen shows an unreadable file as correctly placed, so folding it in would hide it (ADR-0047). It stays a flat `unmatched` row, and is reported here as `unreadablePaths` — attributed, never counted — so that row can name its Show and link to the matcher where ignoring it settles it. A Show whose ONLY entry is `unreadablePaths` is present with every count zero, and is not a queue row. `orphaned` counts Placements whose anchor file is gone — a broken correction, listed as its own row, never folded in with undecided Files. A Show with nothing unsettled is absent; a Library with no Shows answers `{ "shows": [] }`. Counted off the same arrangement `GET /shows/{id}/matcher` renders, so the row's count is exactly what that screen can clear. |
| `POST /shows/{id}/reviewEpisodes` — [Admin] | Dismiss the needs-review flag on **every** flagged Episode of a Show — the "Looks right" behind one collapsed Show row, which stands for the whole set the row counted. `204`, also when nothing was flagged. Unknown Show → `404`. |

#### The file matcher (admin)

One Show's whole working set, so an Admin can lay every **File** against every **Slot** and fix the arrangement ([ADR-0044](./adr/0044-file-anchored-placement-override-replayed-by-the-scanner.md)). It is the container-anchored counterpart of `GET /titles/{id}/episodeCandidates`, which is per-Title and per-season and therefore cannot express "these five files are one problem" — nor reach the files that are not Titles at all.

Everything on the wire is **kind-neutral** — `groups`, `slots`, `files`, addressed by container id, positions as `{ group, slot }` — so the Album matcher is the same contract at `/albums/{id}/matcher` rather than a second one that looks like it. Nothing here is named after a season or an episode.

Two properties are contractual:

- **`files` is complete.** It names every recognized media File under the Show's folders that is not an Extra, gathered from all three places ingestion can leave one: the Files of existing Episode Titles, the Library's `unmatched_files` rows under those folders, and the paths carrying an explicit decision — which by design produce neither a Title nor an Unmatched row. A File the list omits is a File the Admin cannot place, and they cannot tell an omission from an absence. (One gap, tracked separately: a file the Scanner classified as junk — a `sample`, or anything under 1 KiB — reaches none of the three and so is not listed.)
- **`slots` load per group.** The first response is complete for everything LOCAL and costs at most **one** provider call; a group's records arrive when it is expanded, and are cached for 5 minutes. Opening a ten-season Show therefore costs one round-trip, not ten.

| Endpoint | Notes |
| --- | --- |
| `GET /shows/{id}/matcher?group=` — [Admin] | → `200` `{ "containerId", "containerType": "show", "libraryId", "title", "year"?, "seriesExternalId"?, "slotsUnavailable"?, "groups": [ … ], "files": [ … ] }`. `group` (alias `season`) names the ONE group whose provider records are fetched; omit it for the cheap first load. Unknown Show → `404`. |
| `PUT /shows/{id}/matcher` — [Admin] | `{ "files": [ { "path", "state": "placed"\|"unassigned"\|"ignored", "placements"?: [ { "group", "slot", "ordinal"? } ] } ], "slots"?: [ { "group", "slot", "record": { "externalId"?, "group", "slot" } \| null } ] }` → `200` the **re-read** matcher document plus `applied`. `409 SCAN_RUNNING`, `409 SLOT_COLLISION`, `422 OUTSIDE_SHOW`, `422 EMPTY_SLOT`, `400` on a malformed state or a record naming no slot. Emits `libraryUpdated`. |
| `GET /shows/{id}/seriesSeasons?externalId=&group=` — [Admin] | → `200` `{ "externalId", "groups": [ { "number", "slotCount" } ], "group"?: { "number" }, "slots"?: [ slot ] }`. Another series' Slots, so a group can be filled from a foreign record (the *Batman* → *New Batman Adventures* case). The Slot's **position** stays local; only its **record** changes, and the change is written back through `PUT /shows/{id}/matcher`'s `slots` (the per-Title `PUT /titles/{id}/enrichmentOverride` with `season`+`episode` remains the single-Title route). Missing `externalId` → `400`; `503 SEARCH_UNAVAILABLE` when the provider cannot list — unlike the matcher, this route has nothing else to return. |

**`groups[]`** — `{ "number", "source": "provider"\|"local", "slotCount"?, "slotsLoaded", "slotsUnavailable"?, "fileCount", "placedCount", "unassignedCount", "ignoredCount", "slots": [ … ] }`. `source` says whether the provider knows this group or whether its Slots are only the positions local Files claim; `slotCount` is the provider's own count, which is what a *collapsed* group renders. Group **-1** is the Unsorted tray: Files that claim no group at all.

**`slots[]`** — `{ "group", "slot", "titleId"?, "name"?, "overview"?, "airDate"?, "stillUrl"?, "record"?: { "externalId", "group", "slot", "name"?, "overview"?, "airDate"?, "stillUrl"? } }`. The Slots of a group are the union of the provider's list, the positions local Files claim, and any position already placed. `stillUrl` is a same-origin `/providerImage` reference. `record` appears only where an **Episode pin** has repointed the Slot's record — at another position in the Show's own series (the common case: the provider counts a run of episodes in the next season) or in another series entirely (*The New Batman Adventures*). Either way a Slot's *position* stays the local library's numbering, never the borrowed record's.

The borrowed record's **own words ride inside `record`** rather than replacing the Slot's `name`/`overview`/`stillUrl`, and that split is contractual. The Slot's own fields stay whatever the container's series says at that position — *nothing at all* for a Season 4 the provider does not have — so a client can show the borrowed title, state where it came from, print the Slot's own local code, and fall straight back to the default record when the pin is cleared. They are filled only for the ONE group `?group=` expanded, and cost one extra provider call per distinct (series, group) borrowed from — one call for a whole borrowed run, cached like every other listing.

**`files[]`** — `{ "path", "state": "placed"\|"unassigned"\|"ignored", "titleId"?, "parsed"?: [ { "group", "slot" } ], "placements"?: [ { "group", "slot", "ordinal" } ], "decided", "orphaned"?, "unreadable"?, "reason"? }`. `parsed` is what the **filename** claims with every decision ignored; `placements` is where the File sits **now**. The screen compares them, because their disagreement *is* the correction being made. `decided` distinguishes a stored decision from the parse — "unassigned because the Admin said so" from "unassigned because nothing could number it" — and is what Revert needs. `orphaned` flags a Placement whose anchor file is gone. `unreadable` flags a file **ffprobe could not read**, and is the one field that arrives on a PLACED file with its `reason` intact: the filename numbered it, so it sits on its Slot looking finished while no Title was ever built from it, and a screen that stayed silent would show the library's one unplayable file as its most correct one (ADR-0047).

**The PUT body is the WHOLE arrangement, not a delta.** A File absent from `files` carries no decision at all, which is the meaningful third answer: *follow the filename*. Storage is sparse ([ADR-0027](./adr/0027-per-library-enrichment-policy-sparse-override.md)'s precedent), so taking a File off its Slot can only be said by sending it `unassigned`, and taking a correction *back* can only be said by omitting it.

**`slots` is the exception, and is sparse in the ordinary sense.** It repoints what *decorates* a Slot — the **Episode pin** — and a Slot it does not mention keeps the record it has. Absence has no second meaning to spend here (no record is ever derived from a filename), so a client that never sends the field cannot disturb a pin, while `"record": null` is the explicit *clear*: back to this series at this position, which for a Slot the container's series does not list means bare again. Each entry addresses a Slot by its **local** position; the record's `group`/`slot` are its position in *its own* series and stay there — a borrowed run numbered from 1 must never land on the container's real group 1.

**A pin rides in the Apply, and that is forced rather than chosen.** The pin is stored on a Title, and the Slots this exists to repoint are ones the Admin has just placed files onto, whose Titles do not exist until this call commits: there is nothing to pin at the moment of the gesture. It is also the only reading consistent with the screen's contract — nothing takes effect until Apply, Revert returns to the arrangement as opened, Cancel writes nothing — since a pin applied eagerly would be the one change Revert could not undo. The records are therefore written **inside the same transaction**, after the Titles exist, and reset only `enrichment_status`: `identity_key`, the Slot's position and every User's watch state are untouched ([ADR-0014](./adr/0014-watch-state-keyed-to-parsed-identity.md)). A record on a Slot no File fills is refused (`422 EMPTY_SLOT`) rather than silently stored nowhere; a record on a Slot whose File is `deferred` is skipped along with the Episode it would have decorated, and lands on the next Apply once that scan has built the Title.

**`applied`** — `{ "rearranged", "displaced": [path], "deferred": [path] }`. `displaced` are Files the server wrote a decision for on the Admin's behalf (a File that still *parsed* onto a Slot another File was placed on). `deferred` are placed Files the catalog has never probed: the decision is stored, but the Episode cannot be built without ffprobe — which Apply deliberately does not run — so it appears on the next scan. Without this the screen would look like it had silently dropped the correction.

**The degraded path is a state, not an error.** `slotsUnavailable` ∈ `no-series-match` (the Show never matched a provider record — the fix is to match it, not to reconfigure enrichment) | `enrichment-disabled` (Enrichment is off) | `provider-cannot-list` (the Authoritative provider does not implement episode listing; only TMDB does) | `provider-unreachable` (asked and failed). In every case the response is `200` with bare numbered Slots and the whole local half intact, because pure renumbering works offline and is most of what this screen is for. A per-group failure sets `groups[].slotsUnavailable` and leaves the rest of the response whole.

**`409 SLOT_COLLISION` is actionable by contract.** `details` carries `{ "slot": { "group", "slot" }, "paths": [ … ] }` — the contested Slot and every File claiming it — so the screen can offer the three real fixes (merge them onto that Slot as parts, move one, take one off its Slot). It is only ever a *parse-vs-parse* collision the matcher did not create: a Placement's own collision is settled by displacing the parsed File. `409 SCAN_RUNNING` means a scan holds the Library's lock ([ADR-0031](./adr/0031-targeted-scan-per-folder-soft-delete-and-shared-lock.md)) and **nothing was written**; retry when it finishes.

#### Per-entity editing — `{id}` a Title, or a Show/Artist/Album

Title routes return the full `titleDetailJSON`; Show/Artist/Album routes return `entityEnrichmentDetailJSON`: `{ "entityType": "show|artist|album", "entityId", "overview"?, "genres"?, "contentRating"?, "network"?, "enrichmentStatus"?, "lockedFields"?, "enrichmentOverride"?: { "externalId", "source"?, "status"?, "releaseId"? }, "cascade"?: { "updated", "attention" } }`. `releaseId` is the EDITION an Admin chose for an **Album** ([ADR-0052](./adr/0052-a-chosen-edition-is-part-of-the-record-and-it-licenses-position.md)), absent when nobody chose one and always absent for a Show/Artist — it is what lets the edition picker mark which pressing is the human's own choice rather than the system's guess.

| Endpoint (on `/titles/{id}/…` and `/{shows|artists|albums}/{id}/…`) | Notes |
| --- | --- |
| `GET …/enrichmentCandidates?q=&artist=&page=` — [Admin] | → `200` `{ "candidates": [ { "externalId", "title", "year"?, "thumbnailUrl"? (a same-origin `GET /providerImage` URL, never the provider's host), "disambiguation"?, "kind", "typeLabel"?, "tracklist"?: [ { "disc"?, "position", "title" } ], "releaseId"? } ], "hasMore"? }`. `releaseId` is the exact EDITION a pasted `/release/` URL named ([ADR-0052](./adr/0052-a-chosen-edition-is-part-of-the-record-and-it-licenses-position.md)); it appears only on an album `externalPreview` of such a URL, and is sent straight back as the override's `releaseId`. Page size 12. Blank `q` → empty list. `503 SEARCH_UNAVAILABLE` when the provider is unconfigured/unreachable. |
| `GET …/externalPreview?ref=` — [Admin] | Resolve a pasted TMDB/MusicBrainz id or URL → `200` single candidate. `400` invalid/kind-mismatch ref, `404` no record, `503`. |
| `GET /albums/{id}/editions` — [Admin] | The EDITIONS of an Album's matched release-group, so an Admin can choose the exact one their files are **without leaving Obelo** ([ADR-0052](./adr/0052-a-chosen-edition-is-part-of-the-record-and-it-licenses-position.md)) → `200` `{ "albumId", "releaseGroupId"?, "chosenReleaseId"?, "inUseReleaseId"?, "inUseSource"?, "localTrackCount", "editions": [ { "releaseId", "date"?, "country"?, "format"?, "trackCount", "disambiguation"? } ] }`. Album only — a Show/Artist has no such notion. `localTrackCount` is the album's OWN track count, sent rather than counted client-side so a fitting edition is identifiable without arithmetic. `inUseSource` is `chosen` \| `tagged` \| `fit` — which tier of the tracklist precedence answered, i.e. whether the edition in use is a human's choice, the files' tags, or the system's track-count guess. An album with **no matched release-group** is `200` with an empty `editions` list and no `releaseGroupId` (not an error: its own match is what to fix); unknown album → `404`; `503 SEARCH_UNAVAILABLE` when the provider cannot list, which clients degrade to the paste-a-URL hatch on rather than an error page. Reads only — **choosing** one is the existing `PUT /albums/{id}/enrichmentOverride` carrying `releaseId`, so there is exactly one apply path and one cascade. |
| `GET /titles/{id}/episodeCandidates?externalId=&season=` — [Admin] | List a picked series' episodes so an Admin can choose WHICH one decorates this file → `200` `{ "seasons"?: [ { "season", "episodeCount" } ], "season", "episodes": [ { "season", "episode", "name", "overview"?, "airDate"?, "stillUrl"? } ] }`. `seasons` is sent only when `season` is omitted, so a client fetches the list once. Omitting `season` opens on the file's own season, falling back to the series' first real season when it has no such season (common: the record lives in a re-numbered continuation). `stillUrl` is a same-origin `/providerImage` reference. `503 SEARCH_UNAVAILABLE` when the provider cannot list episodes; missing `externalId` → `400`. |
| `PUT …/enrichmentOverride` — [Admin] | `{ "externalId", "cascade"?, "releaseId"?, "season"?, "episode"? }` → `200` detail. `releaseId` pins WHICH EDITION of an **Album** decorates its tracks ([ADR-0052](./adr/0052-a-chosen-edition-is-part-of-the-record-and-it-licenses-position.md)) — a release picked from `GET /albums/{id}/editions`, or the one a pasted `/release/` URL names, carried back by `externalPreview` as the candidate's `releaseId` beside the release-group it resolved to. Album only; never identity (the release-group stays what an album IS, [ADR-0038](./adr/0038-album-identity-release-group-wins.md)), so pinning an edition re-keys nothing. **Omitting it CLEARS** a previously chosen edition — a picked search candidate or a pasted `/release-group/` URL is the Admin naming a less specific thing, and a stale edition under a new group would decorate the album from a stranger's tracklist. `season`+`episode` pin WHICH provider episode decorates an **Episode**, overriding the numbers parsed from its filename **for the lookup only** — the fix for a series the provider numbers differently from the files on disk. identity_key, the Title's own season/episode, its place in the library and every User's watch state are untouched ([ADR-0014](./adr/0014-watch-state-keyed-to-parsed-identity.md)); omit them to leave any existing pin alone. Durable record pin + immediate re-enrich; never touches identity/watch state; honors Locked fields. Cascade (Show→episodes, Album→tracks, Artist→albums→tracks) is best-effort, surfaced in `cascade` counts. |
| `GET /providerImage?ref=` — [Admin] | The metadata-provider image proxy behind the pickers: serves a candidate thumbnail's bytes from this origin so the browser never contacts TMDB / the Cover Art Archive ([ADR-0001](./adr/0001-fully-self-hosted-no-vendor-dependency.md)); it is why the CSP can say `img-src 'self'`. `ref` is an **opaque, HMAC-signed reference the server minted** when it emitted the candidate — never a caller-supplied URL, and a reference this process did not sign is `404` (it is not an open proxy). Bearer **or** media cookie (it is an `<img src>`). Any upstream failure — dead provider, non-image body, oversized body, refused redirect — is also `404`. References do not survive a server restart; refetch the candidate list. |
| `PUT /titles/{id}/enrichmentMatch` — [Admin] | `{ "tmdbId"? \| "imdbId"? \| "musicbrainzId"? }` (≥1) → `200` titleDetailJSON. The id-anchored variant of the override. |
| `PUT …/metadata` — [Admin] | Hand edits; every present field is written **and Locked** ([ADR-0019](./adr/0019-item-editing-preserves-local-identity.md)). Title fields: `overview, tagline, title, contentRating, releaseDate, runtimeMinutes, studio, genres, cast, lockArtwork`. Entity fields: `overview, contentRating, network, genres, title, lockArtwork`. `title` edits the display label only, never identity. → `200` detail with updated `lockedFields`. |
| `DELETE …/metadata/locks/{field}` — [Admin] | Release a Locked field back to auto → `200` detail. No-op if not locked. |
| `GET …/artworkCandidates?role=` — [Admin] | Role ∈ `poster|background|cover|logo` (required). → `200` `{ "role", "candidates": [ { "url", "thumbnailUrl"?, "width"?, "height"?, "source"? } ] }` — queried live, never persisted. `url` is the provider's own URL and is what a pick sends back; `thumbnailUrl` is the same-origin proxy URL to DISPLAY it with (see `GET /providerImage`). |
| `PUT …/artwork` — [Admin] | `{ "role", "url" }` (a candidate URL) → `200` detail. Downloads, caches, sets + Locks the role (stored Fetched-and-Locked; Local still wins at serve time). |
| `POST …/artworkUpload?role=` — [Admin] | **multipart/form-data**, file part `image` (JPEG/PNG/WebP, ≤16 MiB). Upload **is** select ([ADR-0026](./adr/0026-user-uploaded-artwork-upload-is-select-top-precedence.md)): fills + Locks the role, outranks every other source. Errors: `400`, `413 PAYLOAD_TOO_LARGE`, `415 UNSUPPORTED_MEDIA_TYPE`. |
| `PUT /titles/{id}/identityCorrection`, `PUT /shows/{id}/identityCorrection` — [Admin] | The **Wrong item** action: `{ "externalId", "title"?, "year"?, "cascade"? }` → `200` detail. Re-keys identity, **resets watch state**, clears Locked fields, pins + re-enriches. `422 WRONG_KIND` on a non-Movie leaf Title. Show cascade re-keys episodes best-effort. |

All identity/metadata mutations emit a `libraryUpdated` SSE event.

### 3.9 Server settings (admin scope)

All under `/settings/`, bearer + admin.

| Endpoint | Notes |
| --- | --- |
| `GET /settings/metadata-providers` — [Admin] | → `200` `{ "providers": [ { "slug", "name", "kinds": ["video"\|"music"…], "role": "authoritative"\|"supplement", "requiresKey", "enabled", "hasKey", "baseURL", "imageBaseURL"?, "description", "docsURL" } ], "metadataLanguage", "enablement": { "video", "music" }, "configuredEnablement": { "video", "music" }, "consentState": "unset"\|"granted"\|"declined", "autoEnrichAfterScan", "enrichIntervalSeconds", "musicBrainzRateLimitMs" }`. The key itself is **never returned** — only `hasKey`. `enablement` is the running server's **consent-gated** answer ([ADR-0032](./adr/0032-optional-maintainer-key-rotation-endpoint.md)) — what it will actually enrich, so it is off whenever consent is not granted; `configuredEnablement` is the ungated capability it becomes the instant consent is granted. Registry order: tmdb, omdb, thetvdb, anidb, musicbrainz, coverart, fanarttv, theaudiodb. |
| `PUT /settings/metadata-providers` — [Admin] | Partial update; per provider `{ "slug", "enabled"?, "apiKey"?, "baseURL"?, "imageBaseURL"? }` — pointer semantics: omitted = unchanged, `""` = clear/reset-to-default, value = set. Top-level `metadataLanguage`, `autoEnrichAfterScan`, `enrichIntervalSeconds`, `musicBrainzRateLimitMs` same tri-state. All-or-nothing validation before any write. → `200` (GET shape). Errors: `422 PROVIDER_UNKNOWN` / `PROVIDER_KEY_REQUIRED` / `PROVIDER_INVALID_BASE_URL` / `PROVIDER_INVALID_LANGUAGE` / `PROVIDER_INVALID_SETTING`. **Hot-reloads** the running provider — no restart. |
| `POST /settings/metadata-providers/{slug}/test` — [Admin] | Optional body `{ "apiKey"?, "baseURL"? }` (omitted = on-file values). Always `200` `{ "ok", "detail" }` on a completed probe (10s timeout; a down host is `ok:false`, not a 500). No persistence. |
| `GET /settings/subtitle-providers` — [Admin] | → `200` `{ "providers": [ { "slug", "name", "requiresKey", "enabled", "hasKey", "baseURL", "description", "docsURL" } ], "autoFetchLang" }` (`""` = auto-fetch off). |
| `PUT /settings/subtitle-providers` — [Admin] | Same pointer semantics; `autoFetchLang` normalized (`422 PROVIDER_INVALID_LANGUAGE` if unrecognized). Hot-reloads. |
| `POST /settings/subtitle-providers/{slug}/test` — [Admin] | As the metadata test: `200` `{ "ok", "detail" }`. |
| `GET /settings/enrichment-consent` — [Admin] | → `200` `{ "state": "unset"\|"granted"\|"declined", "grantedAt"? }`. |
| `PUT /settings/enrichment-consent` — [Admin] | `{ "granted": true\|false }` (required) → `200` (GET shape). Consent gates all outbound enrichment; hot-reloads. `422 PROVIDER_INVALID_SETTING` when absent. |
| `GET /settings/tailscale` — [Admin] | → `200` `{ "enabled", "hostname", "controlURL", "httpsEnabled", "status": { "state", "fqdn"?, "addresses"?, "keyExpiry", "loginURL"?, "lastError"?, "httpsBound", "httpsError"? } }`. Tailnet remote access ([ADR-0043](./adr/0043-tailnet-remote-access-via-embedded-tsnet.md)). See the state table below. |
| `PUT /settings/tailscale` — [Admin] | Partial update `{ "enabled"?, "hostname"?, "controlURL"?, "httpsEnabled"? }`, pointer semantics (omitted = unchanged). Validated whole before anything is written. → `200` (GET shape). Errors: `422 TAILNET_INVALID_HOSTNAME` (must be a single DNS label) / `TAILNET_INVALID_CONTROL_URL` (absolute http(s) URL, or `""` for Tailscale's own). **Applies with no restart**: flipping `enabled` connects or disconnects, and a changed hostname re-joins. |
| `POST /settings/tailscale/connect` — [Admin] | Brings the node up and persists the desire, so it comes back after a restart. → `200` (GET shape). |
| `POST /settings/tailscale/disconnect` — [Admin] | Stops the node and its listener, **keeping** the state directory — reconnecting needs no re-authorization. → `200` (GET shape). |
| `POST /settings/tailscale/forget` — [Admin] | Stops the node **and wipes** its state directory, so the next connect is a fresh join. → `200` (GET shape). It cannot finish the job: the now-dead node row stays in the operator's Tailscale console and only they can delete it. |

**Tailnet node states** (`status.state`), which a client branches on rather than parsing prose:

| State | Means | What the operator does |
| --- | --- | --- |
| `stopped` | Not running, nothing wrong. The resting state; remote access is off until it is turned on. | Connect |
| `starting` | Coming up, not settled. No address yet. | Wait |
| `needsLogin` | Interactive join waiting on a human — the **normal first run**, not an error. `loginURL` is the link to open. | Open the link |
| `keyExpired` | The node key **lapsed**: this worked and has stopped. `keyExpiry` is the date it lapsed and `loginURL` is a fresh link. Frame it as re-authorization, **not** as a fault. | Open the link; then disable key expiry in the Tailscale console |
| `running` | Up and reachable at `fqdn` over plain HTTP on the Tailnet. | Nothing |
| `error` | The node tried and failed — unreachable coordination server, rejected key, unusable state directory, or a build with no Tailnet support. `lastError` says which. Never a boot failure and never affects the LAN. | Read `lastError` |

`keyExpiry` is **nullable and never omitted**: `null` means the key does not expire (a tagged node, or expiry disabled in the console) — a genuinely different and better state than "expires soon", and one a missing field could not express. Otherwise it is RFC 3339 UTC. The server also logs the expiry at boot and warns under 14 days.

**`httpsEnabled` is the request; `status.httpsBound` is the outcome, and a client must not read one as the other.** `httpsEnabled` is a saved setting — it says the operator asked for HTTPS on the Tailnet, and a `PUT` that sets it succeeds whether or not it can work. `httpsBound` says whether tailnet `:443` **is accepting connections right now**, which additionally needs MagicDNS *and* HTTPS certificates enabled in the Tailscale console — prerequisites this server can neither set nor detect in advance. The two therefore disagree in precisely the misconfiguration this feature is most likely to be in, and **the scheme of the address a client displays or dials must come from `httpsBound`**: `enabled && !bound` means plain HTTP on the Tailnet is serving normally and `https://<fqdn>` would refuse the connection. `httpsBound` is **never omitted** — `false` is the case that matters, and a field that disappears when false cannot be told from one the server forgot to send.

`httpsError` accompanies `httpsBound: false` and is the server's own paragraph, identical to the one in the log: it names **both** console settings, states that the `http://` address is unaffected, and says the server retries on its own. Render it **verbatim** — it is the only actionable part, and a paraphrase drops the setting names. It is absent while HTTPS is off, and absent once it is bound. `httpsBound: true` means the listener came up (which proves both console prerequisites are met); it does **not** promise a certificate was issued — with `tsnet` the certificate is fetched inside the node on the first handshake, and a failure there is not observable from this server.

`fqdn` — the MagicDNS name — appears **here and nowhere else**: this is an authenticated admin surface, and an unauthenticated scanner hitting a port-forward must not learn that a Tailnet exists or what it is called. Publishing it to signed-in clients is a separate, later slice.

On a build without `-tags tailscale` (`features.tailscale: false`) all five routes are still served, and the verbs answer `200` with `status.state: "error"` and a `lastError` **naming the build** — never a `404`, which would be indistinguishable from a mistyped path.

### 3.10 Transcoding observability (admin scope)

#### GET /transcoding — [Admin]

Read-only snapshot ([ADR-0029](./adr/0029-transcoding-observability-admin-surface.md)):

```json
{
  "backend": { "requested": "nvenc", "active": "cpu", "degraded": true, "reason": "…" },
  "load": { "active": 1, "cap": 3, "atCapacity": false },
  "gpu": { "utilizationPct": 42, "vramUsedMb": 2048, "vramTotalMb": 8192,
           "encoderSessions": 1, "driverVersion": "550.54.14", "sampledAt": "…" }
}
```

`degraded` = requested hardware but running CPU. `cap: 0` = unlimited. `gpu` is `null` unless the active backend is NVENC and `nvidia-smi` answered; individual fields are `null` when a column is unavailable. Non-admin → `403`, not a filtered view.

### 3.11 Links — the receiving half of linking (admin scope)

Gated by the **`linkedLibraries`** feature flag; every route is bearer + admin. Branch on the flag, never on `linkProtocolVersion` — a server without these routes `404`s them, and there is no fallback, because there is nothing else that can hold a Link.

**A Link is this Server's standing relationship with another household's Server**, held on *this* side ([ADR-0055](./adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md), [ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)): the other Server's identity, the addresses it might be reached at, the token its `remote` User left behind, and a state. It is **one direction** — the Server that holds it browses and plays, the Server it points at shares — so two households sharing both ways hold two Links, one each. Linking is not a per-User act: a Link belongs to the household, and the Libraries it brings are then granted to Users like any other Library (§3.2).

`linkJSON`:

```json
{
  "id": "…",
  "serverId": "<the sharer's server id>",
  "serverName": "Kate's Obelo",
  "state": "connected",
  "activeOrigin": "https://media.example.org",
  "origins": [ "https://media.example.org", "http://obelo.tail1a2b.ts.net" ],
  "lastSyncedAt": "2026-09-03T09:14:02Z",
  "lastError": "",
  "libraries": [ { "id": "…", "name": "Cartoons", "kind": "tv" } ]
}
```

**There is no token field and there must never be one.** What the row holds is an outbound credential against somebody else's Server — the same posture as a metadata provider key, which this API reports only as a `hasKey` boolean. There is not even a `hasToken`, because a Link without a token cannot exist: `state` is what an operator actually wants to know.

- **`activeOrigin`** is the address that answered; **`origins`** is every address the invite carried, in the order it carried them. Both are shown so an operator can see *which* path is carrying their films — the tailnet one or the public one — which is otherwise invisible and is the first thing to look at when a Link is slow.
- **`lastSyncedAt`** is `null` until the mirror has pulled once — a pointer, not `""`, because `null` is this Server *saying* "never". A **relay** call moves the state but never stamps this: a play proves reachability, not freshness, and a month-old catalog must not report itself as just synced.
- **`libraries`** are the linked Libraries this Link brought. Never `null`; empty forever for a sharer who granted this Server nothing.

**The three states** ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §6), which a client branches on rather than parsing `lastError`:

| State | Means | What the Admin does |
| --- | --- | --- |
| `connected` | The last export or relay call succeeded. `available: true` on its Libraries and Titles. | Nothing |
| `unreachable` | A transport failure or a 5xx. Transient: retried with backoff (1m→30m) across every origin in the invite, and the event subscription is re-established on recovery. The mirror **stays**, badged `available: false`; a play answers `503 LINK_UNREACHABLE`; nothing is deleted and Continue Watching survives the friend's reboot. | Wait, or **Sync now** |
| `revoked` | The sharer answered `401` — the `remote` User or its Device was deleted over there. **No retries**: a dead credential cannot recover on its own, so a play answers `503 LINK_REVOKED` without spending a round trip. The mirror stays. | Ask for a fresh invite and **Re-key** |

**Every Link begins `unreachable` at startup**, until its first successful call — a Server that boots without a network says so rather than pretending.

| Endpoint | Notes |
| --- | --- |
| `GET /links` — [Admin] | → `200` a bare array of `linkJSON` (not an envelope). |
| `POST /links` — [Admin] | `{ "invite": "obelo-link:…" }` → **`201`** a new Link, **`200`** a re-key of one that already existed (the same `serverId` updates in place — created-vs-updated, exactly what those two statuses mean; a client that ignores the difference still gets the right Link). The whole flow runs inside the request: parse the string, probe the origins in order over the right dialer, check `features.serverLinking` and `linkProtocolVersion`, `POST /auth/link/redeem`, create the Link, create a linked Library per granted Library, and do the **first full pull** — so the answer already carries `libraries` and a `lastSyncedAt`. Errors: `400 BAD_INVITE`, `410 INVITE_EXPIRED`, `409 LINK_PROTOCOL` (`details: { theirs, ours, upgrade }`), `503 LINK_UNREACHABLE` (the message names the reason the last origin gave). |
| `POST /links/{id}/rekey` — [Admin] | `{ "invite": "obelo-link:…" }` → `200` linkJSON. The same flow, requiring a **matching `serverId`**: a Link is bound to one peer for its whole life, because the mirror is keyed by that Server's ids, so another household's invite is `409 LINK_SERVER_MISMATCH` and the row is left untouched. Replaces the credential and the addresses, keeps `createdAt`, and moves the state to `connected`. |
| `POST /links/{id}/sync` — [Admin] | No body → `200` the Link **as it stands afterwards**. Forces one sweep: reconcile the granted set, then pull every granted Library forward from its checkpoint. **Synchronous** — the operator pressed it because they had just plugged the other house's server back in, and "we will get to it" is not an answer to that. It is also the one sweep a `revoked` Link gets ("no retries" is about the automatic loop; a human asking is not a retry). A failed sweep is reported as one — `503 LINK_UNREACHABLE`, `409 LINK_REVOKED` — with the row already updated, so a client that merely refetches is correct too. Errors: `404` unknown Link, `405` on GET. |
| `DELETE /links/{id}` — [Admin] | → `204`, **whether or not the sharer could be reached** — a friend's server being off must not be able to keep this household linked to them. Best-effort first: this Server deletes its own Device over there (`DELETE /devices/{id}`, falling back to `POST /auth/logout`) so the sharer's Users page loses the row rather than keeping a ghost with a last-seen. Then it removes the Link, its linked Libraries, their mirrored rows **and this household's Watch state for them** — the same shape as deleting a local Library. This is the only thing that deletes what came over a Link. Errors: `404`. |

**Two dialers, no second identity** ([ADR-0055](./adr/0055-linking-is-a-one-time-invite-redeemed-server-to-server.md) §5). Per origin, this Server uses either the operating system's network (a port-forwarded host with an ACME certificate, a reverse proxy, a Tailscale Funnel address — all three are just an HTTPS origin) or **its own Tailnet node's dialer**, when the node is running and the host is a CGNAT literal or a name under the node's MagicDNS root. A machine a friend *shares in* keeps its owner's tailnet in its name, which is exactly the case the tailnet path exists for; a tailnet dial that fails falls back to the OS dialer, because a Funnel address wears the same name and this Server cannot tell the two apart in advance. Nothing is configured. An origin that answers as a **different** Server is skipped rather than fatal — the invite's addresses are addresses to try, not to believe — while a version mismatch stops the walk at once, since every origin leads to the same machine.

**Freshness.** After the first full pull the mirror is refreshed two ways: on the sharer's `libraryUpdated` nudge, over a `GET /events` subscription held under the Link's token (the `remote` User is audience-gated to its own grants, so it hears exactly the right Libraries), and on a timer as the poll fallback — `OBELO_LINK_SYNC_INTERVAL`, default `1h`. Setting it to `0` turns the background half off **entirely**: no timer, no backoff, and no subscription. `POST /links/{id}/sync` and the pull that follows a link or a re-key still work, and every Link still starts `unreachable`, because that is a statement about this boot and not about the schedule.

#### GET /relay/{sessionId}/{tail} — [Public] (bearer **or media cookie**), also reachable by [Stream token]

The transport of the one-hop relay ([ADR-0056](./adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md) §5). A play on a mirrored Title is negotiated by the **sharing** Server, under the `remote` User's bearer and that User's Playback ceiling, and transcoded there under its own governance; this Server opens a local Session wrapping the remote one, rewrites every media URL in the answer onto this route, and streams the bytes through. **Exactly one hop, always**: bytes never bypass this Server, and a linked Library is never relayed onward.

**The remote path tail is preserved**, which is the whole design:

```
sharer   /api/v1/sessions/{remoteId}/hls/index.m3u8
here     /api/v1/relay/{localId}/sessions/{remoteId}/hls/index.m3u8
```

Every playlist Obelo emits uses **bare relative URIs**, so a player resolves `000.ts` against the playlist's own URL — and because the tail's shape is preserved, that resolution lands on the matching relay path by construction, with not one playlist byte needing to change. It is the same argument that puts the stream token in the path ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md)), applied one hop further out. An **absolute** URI inside a playlist is rewritten line by line (`EXT-X-MAP`, `EXT-X-MEDIA`, `EXT-X-KEY`, and bare lines); a foreign URI is left alone.

**Credential rules**, which are the media routes' own and nothing new:

- **Bearer or the media cookie**, through the same middleware `GET /sessions/{id}/stream` uses, and **bound to the local Session's User**. Unknown, ended, another User's, or not-a-relay session → one `404 "session not found"`, the same existence-hiding answer the local media routes give; anonymous → `401`.
- **The stream token reaches the same bytes on its own existing route.** The token *is* the session identifier ([ADR-0039](./adr/0039-scoped-expiring-media-credential-for-delegated-fetches.md) — there is no id in that URL), so `/stream/{streamToken}/…` dispatches a relay session into the same serving code instead of a scratch directory that does not exist. Under a token, playlists map an absolute remote URI back to a **bare filename** rather than to a `/relay/{id}/…` path, so the secret is never written into a playlist and relative resolution still lands on the token prefix.
- **The tail is validated against the session, not forwarded as given.** Exactly three shapes are allowed: this session's own progressive stream (`sessions/{remoteId}/stream`), a single path element under its own `sessions/{remoteId}/hls/`, and the subtitle tracks of the Title it is playing (`titles/{remoteTitleId}/subtitles/{file}`). Anything else — another session's media, another Title's, a browse endpoint, a traversal — is `404`. Without that check the route would be an open proxy into a friend's Server under this household's credential.
- `GET`/`HEAD` only; anything else is `405` with `Allow: GET`.

Bytes are **streamed, never buffered to disk**: status, content type, `Content-Length` or a `206`'s `Content-Range`, and `Accept-Ranges` all pass through, so a seek behaves exactly as it does on a local file. A relayed fetch is also the session's keepalive, like a local manifest or segment request. Ending the local Session — `DELETE /sessions/{id}` or the idle reaper — ends the sharer's session too.

**Artwork needs no new route.** A `/artwork/…` request for a mirrored entity that has no local image is fetched from the sharer under the Link's token on first request and cached in the identity-keyed artwork cache under the linked Library; the second request never reaches the sharer. Nothing about the URL changes.

**What crosses the wire about the viewer: nothing.** The sharer sees one Session per concurrent stream under the one `remote` User, and `nowPlaying` shows "Brandon's server — Title X". No user id, username, display name, Device name or Device id from this household appears in any header, body, path or query of a relayed request; the person watching is known only here, and their Watch state is written here, against the mirrored row.

**This Server pays bandwidth, never CPU.** A relay Decision skips the local HLS runtime and the transcode meter entirely, so it holds no slot against the transcode cap ([ADR-0009](./adr/0009-transcode-governance.md)) — the encode is the sharer's, which is the point: they are the ones sharing with a stranger, so the budget that protects them must be theirs.
