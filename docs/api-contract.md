# API Contract

The single HTTP/JSON API with public and admin scopes ([ADR-0010](./adr/0010-unified-two-scope-api.md)), consumed by the web app and all clients. Treated as a versioned product because clients and server update independently.

> **Generated from source at commit `c57f537` (2026-07-14)** — which committed the ADR-0033 original-format subtitle delivery and the progress `videoStreamId` that an earlier revision of this doc described as an uncommitted working tree — plus the ADR-0034 Server identity (`GET /server`'s `id`/`name`) and mDNS advertisement, currently uncommitted. Extracted from handler/DTO structs in `internal/api` and verified against a live instance. Every JSON field name below is a verbatim struct tag; examples are captured or derived from real responses. If code and this doc disagree, the code wins — regenerate by re-running the extraction against `internal/api/*.go` and `internal/events/broker.go`.

**Reading this catalog:**
- Every path lives under **`/api/v1`** (`APIPrefix`, `internal/api/api.go`). The prefix is stripped before dispatch; unknown paths under it return the enveloped `404 NOT_FOUND`, never a plain-text 404.
- Each endpoint is tagged **[Public]** (any authenticated User), **[Admin]** (requires `role: "admin"`), or **[Unauthenticated]**.
- **An Apple TV / native client needs only the [Public] endpoints** plus `/server`, `/setup`, `/auth/*`, `/devices`. The [Admin] scope is used only by the management web app (ADR-0010).

---

## Part 1 — Invariants (the stable promises)

### Style & versioning

- **REST-ish JSON over HTTP**, resource-oriented, **camelCase** field names.
- **Versioning via URL path** — `/api/v1/…`. One integer major version. Additive changes (new fields/endpoints) never bump it; only breaking changes mint `/api/v2`, and the server may serve both during a transition.
- **Handshake** — `GET /server` returns server version, supported API versions, and a **feature-flags** map. Clients branch on feature flags, not version strings. A flag means "this server serves these routes"; `TestFeaturesMatchRoutes` holds the map to that meaning by probing the routes. The one exception is **`transcode`**, which advertises the transcode *delivery tier* rather than a route, and currently reads `false` pending resolution from the ffmpeg backend.
- **Success content type**: `application/json; charset=utf-8`, except `204 No Content` (empty body) and the media byte endpoints (images, video, HLS artifacts, WebVTT).

### Error envelope

Every error — including the catch-all 404/405 — returns:

```json
{ "error": { "code": "STRING_ENUM", "message": "human readable", "details": { } } }
```

`details` is omitted when empty. A wrong method on a known path returns `405 METHOD_NOT_ALLOWED` with an `Allow` header.

Complete `code` enum (`internal/api/errors.go`): `NOT_FOUND`, `METHOD_NOT_ALLOWED`, `INTERNAL`, `BAD_REQUEST`, `UNAUTHORIZED`, `FORBIDDEN`, `FOLDER_OVERLAP`, `NO_FILES`, `SETUP_CLOSED`, `INVALID_CLAIM_TOKEN`, `INVALID_CREDENTIALS`, `USERNAME_TAKEN`, `LAST_ADMIN`, `ADMIN_GRANT`, `UNKNOWN_LIBRARY`, `ADMIN_CEILING`, `UNKNOWN_RATING`, `UNKNOWN_TITLE`, `KIND_MISMATCH`, `ITEM_SET_MISMATCH`, `SYSTEM_PLAYLIST`, `TRANSCODE_REQUIRED`, `SERVER_BUSY`, `SERVICE_UNAVAILABLE`, `PROVIDER_UNKNOWN`, `PROVIDER_KEY_REQUIRED`, `PROVIDER_INVALID_BASE_URL`, `PROVIDER_INVALID_LANGUAGE`, `PROVIDER_INVALID_SETTING`, `PROVIDER_NOT_AUTHORITATIVE`, `SEARCH_UNAVAILABLE`, `WRONG_KIND`, `UNSUPPORTED_MEDIA_TYPE`, `PAYLOAD_TOO_LARGE`.

### Request bodies

Every JSON body is capped at **1 MiB** and decoded with **unknown fields rejected** — an extra key, malformed JSON, or an oversized body is `400 BAD_REQUEST` `"invalid JSON body"`. Do not send fields the endpoint doesn't define. (Exceptions: the scan/enrich trigger bodies are best-effort — a malformed body falls back to the default mode.)

### Authentication

Three credential transports, each honored only where stated:

1. **Bearer token** (the universal credential): `Authorization: Bearer <token>`. The token is **opaque and DB-backed** — validated on every request, so revocation (logout, device delete) is immediate ([ADR-0015](./adr/0015-opaque-db-backed-tokens.md)). Every endpoint accepts it. When bearer and another credential are both present, bearer wins. Auth failures set `WWW-Authenticate: Bearer` and return `401 UNAUTHORIZED`.
2. **`ms_media` cookie** (media/browser credential): carries the *same* opaque token. Set by `POST /auth/login` (`HttpOnly`, `SameSite=Lax`, `Path=/api/v1`, 30-day MaxAge, `Secure` only when the request arrived over TLS), cleared by `POST /auth/logout`. Honored **only** by the read-only media/stream GETs and the SSE stream — exactly: `GET /titles/{id}/artwork/{role}`, `GET /titles/{id}/subtitles/{subId}.vtt`, `GET /shows/{id}/artwork/{role}`, `GET /seasons/{id}/artwork/{role}`, `GET /artists/{id}/artwork/{role}`, `GET /albums/{id}/artwork`, `GET /people/{personRef}/artwork/{role}`, `GET /sessions/{id}/stream`, `GET /sessions/{id}/hls/*`, `GET /events`. No JSON/mutation endpoint honors it.
3. **`?token=` query param**: honored by **exactly one** endpoint — `GET /files/{id}/download` (external players fed a `.xspf` can send neither header nor cookie). The URL-borne token is an accepted tradeoff there; it is still DB-validated and revocable.

**Native clients (Apple TV / libmpv):** the tvOS client plays through **libmpv**, whose HTTP layer (ffmpeg) can attach arbitrary headers to every media request — so a native client simply sends **`Authorization: Bearer <token>` on media requests too** (mpv: `http-header-fields`), and needs neither the cookie nor `?token=`. The `ms_media` cookie exists for players that *cannot* set headers — the browser's `<video>`/`<img>`/`EventSource` (and it would equally serve an AVPlayer-based client via cookie injection). Do not put `?token=` on stream/HLS URLs — the server only accepts it on `/files/{id}/download`.

**Bootstrap** ([ADR-0013](./adr/0013-first-admin-claim-token-bootstrap.md)): `GET /server` reports `setupRequired: true` while zero Users exist; the server logs a **claim token** at boot; `POST /setup` with `{claimToken, username, password}` creates the first Admin. Setup does *not* log you in — call `/auth/login` after.

### Access control

- Enforced **server-side on every endpoint**. A Member sees only granted Libraries and only Titles at or below their Rating ceiling; listings are filtered in SQL so counts and pagination stay correct; an Admin sees everything.
- **404-not-403 (hide-existence)**: an entity outside the caller's access — an ungranted Library's Title, another User's playback session, another User's Playlist (no Admin override), a Collection with zero visible members — returns `404`, indistinguishable from not-existing.
- **Un-rated content stays visible.** A Title with no Content rating, or a label outside the known ladder (MPAA + US TV), is never hidden by a ceiling — a capped Member's un-enriched Library does not look empty.

### Pagination — the actual state

Cursor pagination (opaque `cursor` param, keyset seek — never offset) is implemented **only on the three top-level grids**: `GET /libraries/{id}/titles` and its TV/music delegates (shows, artists). Params `limit` (default 20, max 100) and `cursor`; the response carries `nextCursor` (absent on the last page). **Not paginated**: seasons/episodes, albums/tracks (full listings), `/home` (each row capped at 20), `/search` (`limit` caps each group independently, default 20, max 100).

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
- The `transcode` flag is hardcoded `false` rather than computed from the resolved ffmpeg backend, so a server that can transcode still does not advertise it. Unlike the other flags it is not route-existence — `/transcoding` (the ADR-0029 admin snapshot) is served either way.

---

## Part 2 — Real-time events (SSE)

### GET /events — [Public]

Single server→client SSE stream ([ADR-0016](./adr/0016-sse-for-realtime-updates.md)). Auth: bearer **or** `ms_media` cookie (a browser `EventSource` cannot set headers).

Wire format: status 200 with `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`, `X-Accel-Buffering: no`. First bytes are the comment `: connected`. Each event:

```
event: <type>
data: <json>
```

No `id:`, no `retry:`, no heartbeat. The subscriber's identity (user, admin flag, accessible-Library set) is resolved **once at subscribe time**; per-subscriber buffers hold 32 events and a full buffer **drops** events (publish never blocks) — SSE is an optimization; every event maps to a pollable resource.

**Audience gating** happens before enqueue: *broadcast* → everyone; *admin-only* → Admins only; *library-scoped* → subscribers whose accessible-Library set contains the event's Library (Admins always).

| Event | Audience | Payload | Poll fallback |
| --- | --- | --- | --- |
| `enrichProgress` | broadcast | `{ "libraryId", "total", "done", "matched", "unmatched", "failed", "disabled", "complete" }` | `/libraries`, `/libraries/{id}/titles` |
| `scanProgress` | library-scoped | `{ "libraryId", "titlesFound", "filesFound", "complete", "scope"?, "added"?, "removed"? }` — `scope` is the Targeted-scan entity label (absent for full scans); `added`/`removed` only on the terminal targeted event | `GET /libraries/{id}/scan` |
| `libraryUpdated` | library-scoped | `{ "libraryId" }` — a refetch nudge, not a diff | `/libraries`, `/libraries/{id}/titles` |
| `sessionStarted` | admin-only | `{ "sessionId", "userId", "titleId" }` | — (no session list yet) |
| `nowPlaying` | admin-only | `{ "sessionId", "userId", "titleId", "positionMs" }` | — |
| `sessionEnded` | admin-only | `{ "sessionId", "userId", "titleId" }` | — |

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
    "playlists": true, "realtimeEvents": true, "transcode": false
  },
  "setupRequired": false
}
```

`id` and `name` are the **Server identity** ([ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md)). Both are `omitempty` and both are **additive** — a server predating ADR-0034 omits them, so treat them as optional rather than as an error.

- **`id`** — an opaque UUID, minted once into the server's data dir and stable for its lifetime. It is *not* derived from a key or from hardware, so it survives both. **Its purpose is to make an address change survivable:** a client that stored the id can rediscover the server at a new address (DHCP lease change) and keep its token, because the token is bound to a Device row on this server ([ADR-0015](./adr/0015-opaque-db-backed-tokens.md)), never to an address. It is also the only way to answer "is this the same server I logged into?". Wiping the data dir mints a new id — correct, since that server has no Users, Devices, or tokens to honor.
- **`name`** — the operator's display name (`JUICEBOX_SERVER_NAME`, defaulting to the host's name). Cosmetic; nothing keys on it, and renaming never invalidates a token. That is exactly why it is a separate field from `id`.

Neither is a secret: this endpoint is unauthenticated, the id grants nothing, and the name is operator-chosen.

Errors: `500 INTERNAL`.

#### LAN discovery — mDNS/Bonjour

Not an HTTP endpoint, but part of how a native client reaches this contract ([ADR-0005](./adr/0005-discovery-and-tls-via-reverse-proxy.md), implemented by [ADR-0034](./adr/0034-server-identity-and-mdns-advertisement.md)). The server advertises on the local link:

```
service:  _juicebox._tcp        (in the local domain, on the listen port)
TXT:      txtvers=1  id=<uuid>  name=<display name>  path=/api/v1
```

Verify with `dns-sd -B _juicebox._tcp local` / `dns-sd -L "<name>" _juicebox._tcp local`.

- **TXT is a hint, not a contract** (RFC 6763). Confirm everything against `GET /server` once connected; `id`/`name` appear in both, and the handshake is authoritative.
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

Side effect: sets the `ms_media` cookie with the same token.
Errors: `400 BAD_REQUEST` (`"device.clientId is required"` / bad body), `401 INVALID_CREDENTIALS`, `500`.

#### POST /auth/logout — [Public] (bearer only)

No body → `204`. Revokes exactly the calling token and clears the `ms_media` cookie.

#### GET /devices — [Public]

→ `200` `{ "devices": [ { "id", "name", "platform", "clientId", "createdAt"?, "lastSeenAt"? } ] }` — the **caller's** Devices only.

#### DELETE /devices/{id} — [Public] (self) / [Admin] (any)

→ `204`. Revokes that Device's token immediately. A foreign Device (non-admin caller) is `404 NOT_FOUND` `"device not found"` — forbidden is deliberately indistinguishable from missing.

### 3.2 Users (admin scope)

All bearer + admin. Non-admin → `403 FORBIDDEN`.

| Endpoint | Body → Response |
| --- | --- |
| `POST /users` — [Admin] | `{ "username", "password", "role": "admin"\|"member" }` → `201` bare `{ "id", "username", "role" }`. Errors: `400`, `409 USERNAME_TAKEN`. |
| `GET /users` — [Admin] | → `200` `{ "users": [ { "id", "username", "role" } ] }` |
| `GET /users/{id}` — [Admin] | → `200` `{ "id", "username", "role", "libraryIds": [], "ratingCeiling": "" }` — `libraryIds` never null (`[]` for an admin), `ratingCeiling` `""` = uncapped. Errors: `404`. |
| `DELETE /users/{id}` — [Admin] | → `204`. Errors: `404`, `409 LAST_ADMIN`. |
| `PUT /users/{id}/password` — [Admin] | `{ "password" }` → `204`. Errors: `400`, `404`. |
| `PUT /users/{id}/libraryAccess` — [Admin] | `{ "libraryIds": [ … ] }` (full replace-set) → `204`. Errors: `404`, `422 ADMIN_GRANT`, `422 UNKNOWN_LIBRARY`. On 422 the prior grant set is unchanged. |
| `PUT /users/{id}/ratingCeiling` — [Admin] | `{ "rating": "PG-13" }` (`""` clears) → `204`. Errors: `404`, `422 ADMIN_CEILING`, `422 UNKNOWN_RATING`. |

### 3.3 Libraries & scanning

`libraryJSON`: `{ "id", "name", "kind", "createdAt"?, "rootFolders": [ { "id", "path" } ] }`.

| Endpoint | Notes |
| --- | --- |
| `POST /libraries` — [Admin] | `{ "name", "kind": "movie"\|"tv"\|"music", "rootFolders": ["/abs/path"] }` → `201` libraryJSON. Errors: `400`, `409 FOLDER_OVERLAP`. |
| `GET /libraries` — [Public] | → `200` `{ "libraries": [ … ] }`, filtered to the caller's grants. |
| `GET /libraries/{id}` — [Public] | → `200` libraryJSON. Ungranted/unknown → `404`. |
| `PATCH /libraries/{id}` — [Admin] | `{ "name"?, "addRootFolders"?: [ … ] }` (partial; `kind` immutable) → `200` libraryJSON. Errors: `400`, `404`, `409 FOLDER_OVERLAP`. |
| `DELETE /libraries/{id}` — [Admin] | → `204`. |

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
  "runtimeMinutes"?, "studio"?, "genres"?: [], "enrichmentStatus"?, "artworkVersion"? }
```

`resumePositionMs`/`watched` are the **calling User's** watch state. `enrichmentStatus` ∈ `pending|matched|unmatched|failed|disabled`. `artworkVersion` is an opaque cache-bust token.

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
| `GET /seasons/{id}/episodes` | `{ "season": seasonJSON, "episodes": [ { "id", "kind": "episode", "title", "seasonNumber", "episodeNumber"?, "episodeLabel"?, "needsReview"?, "resumePositionMs"?, "watched"?, "addedAt"?, "overview"?, "enrichmentStatus"?, "stillUrl"? } ] }` |
| `GET /artists/{id}/albums` | `{ "artist": artistSummaryJSON (decorated), "albums": [ { "id", "artistId", "title", "year"?, "hasArtwork"?, "artworkVersion"?, "genres"?, "enrichmentStatus"?, "trackCount" } ] }` |
| `GET /albums/{id}/tracks` | `{ "album": albumJSON (incl. "artistName"), "tracks": [ { "id", "kind": "track", "title", "discNumber"?, "trackNumber"?, "durationMs"?, "needsReview"?, "resumePositionMs"?, "watched"?, "overview"?, "enrichmentStatus"? } ] }` — disc/track order. |

All: unknown/ungranted parent → `404`.

#### Artwork bytes — all [Public], bearer **or `ms_media` cookie**

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

`homeTitleJSON`: `{ "id", "kind", "title", "year"?, "tmdbId"?, "imdbId"?, "addedAt"?, "resumePositionMs"?, "durationMs"?, "episode"?, "track"?, "overview"?, "genres"?, "displayTitle"? }`. Each row capped at 20, computed per-User, never stored. Continue Watching = 2–90% band, most recent first; Up Next = TV resume points; Recently Added = newest first. `resumePositionMs`/`durationMs` are populated on **Continue Watching only** — together they drive the card's progress bar, the same pairing `resumePoint` carries on the Show detail; Up Next / Recently Added omit both, and `durationMs` is also omitted when the duration is unknown.

#### GET /search?q=…

```json
{ "movies": [], "shows": [], "artists": [], "albums": [], "episodes": [], "tracks": [] }
```

Six always-present groups reusing the browse summary DTOs (search results carry no `?v=` artwork cache-buster). `q` (fallback `query`); empty `q` → `200` with empty groups. `limit` caps each group (default 20, max 100). Case-insensitive substring on display names, access-filtered.

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
  "videoStreamId": ""
}
```

`maxBitrate` (bits/sec) reflects current network and is the field that most often flips direct-play into transcode. `maxLevel`/`hdr`/`preferredSubtitleLang` are recorded, not yet enforced. `textSubtitleFormats` **is** enforced — it selects each text subtitle's delivery format (original vs WebVTT, [ADR-0033](./adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)); omit it (or declare only `vtt`) for WebVTT everywhere. Resolution tokens: `144p…4320p` plus `sd/hd/fhd/2k/4k/uhd/8k`. `audioStreamId`/`videoStreamId`/`burnSubtitleId` select non-default streams (may escalate the tier; a video pick restarts in-container, [ADR-0025](./adr/0025-selectable-video-streams-in-container-restart-switch.md)).

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
  "estimatedBitrate": 112023
}
```

- `tier` ∈ `directPlay | directStream | transcode`.
- `streamUrl`: **directPlay** → `/sessions/{id}/stream` (progressive byte-range); **directStream/transcode** → `/sessions/{id}/hls/master.m3u8` when the session has demuxed audio renditions or deliverable text subtitles, else `/sessions/{id}/hls/index.m3u8` ([ADR-0004](./adr/0004-hls-for-adaptive-progressive-for-direct-play.md)).
- `videoStream` **omitted for an audio-only Decision** — a music Track, or any File whose only video Stream is cover art ([ADR-0017](./adr/0017-audio-only-playback-path.md)). Test the field's **presence**, not its contents: it is absent, never a zero value. (It formerly marshalled as `{"index":0,"codec":""}` on every Track — present but empty, so `if (d.videoStream)` was true for audio-only and clients had to sniff the empty codec. That shape is gone; a client still carrying such a workaround can drop it once it no longer talks to an older server.) The `videoStreams` list is unaffected — it was already `[]` for a Track and stays a present empty list.
- `audioStream` omitted only for a silent File. Subtitle `url` present only for text tracks ([ADR-0020](./adr/0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md)); image tracks burn in via `burnSubtitleId`. `format` names what the `url` serves ([ADR-0033](./adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)): when `deviceProfile.textSubtitleFormats` declares the track's **original** format (`srt`/`ass`, aliases `subrip`/`ssa` fold), the url points at the original bytes — ASS styling intact — else at the WebVTT conversion. Embedded `mov_text` is always WebVTT-only.

Errors:
- `404` `"title not found"` / `"subtitle not found"` / `"audio stream not found"` / `"video stream not found"`.
- `501 TRANSCODE_REQUIRED` — structurally unplayable for this client; `details: { "reason": "container|videoCodec|audioCodec|resolution|bitrate|audioChannels|noVideo|noFile", "detail": "…" }`.
- `503 SERVER_BUSY` — transcode cap full ([ADR-0009](./adr/0009-transcode-governance.md)); `details: { "retryable": true, "suggestedMaxBitrate": <half the estimate, floor 600000> }`. Direct play/remux never hit this.

#### Session lifecycle

| Endpoint | Auth | Notes |
| --- | --- | --- |
| `GET /sessions/{id}/stream` — [Public] | bearer **or cookie** | Progressive bytes; `Range`/`If-Range`/HEAD supported; `200`/`206`. Foreign/ended session → `404`. Seek = byte range; no new decision needed. |
| `GET /sessions/{id}/hls/{file}` — [Public] | bearer **or cookie** | HLS artifacts: `master.m3u8`; `index.m3u8` + `NNN.ts`/`.m4s` + `init.mp4` (video); `audio_{streamId}.m3u8` + `audio_{streamId}_NNN.ts`/`.m4s` + `audio_{streamId}_init.mp4`; `subs_{subId}.m3u8` + `subs_{subId}_NNN.vtt` (4s cadence). Content types: `application/vnd.apple.mpegurl`, `video/mp2t`, `video/mp4`, `text/vtt`. Playlists are `Cache-Control: no-cache`. Errors: `404` `"session media unavailable"` / `"segment not available"`. |
| `POST /sessions/{id}/progress` — [Public] | bearer only | `{ "positionMs": 123456, "state": "playing|paused|buffering", "audioStreamId"?, "videoStreamId"? }` every ~10–15s → `200` `{ "titleId", "resumePositionMs", "watched" }`. **Doubles as keepalive** — an idle session is reaped (FFmpeg killed, scratch deleted, cap slot freed). `audioStreamId` records an in-band audio pick as the Remembered audio ([ADR-0023](./adr/0023-per-user-audio-memory-two-level-language-keyed.md)); `videoStreamId` is its video mirror for players that switch video tracks in-container without re-negotiating (libmpv on direct play — records the Remembered video, ADR-0025). Unknown ids are ignored, best-effort. |
| `DELETE /sessions/{id}` — [Public] | bearer only | Clean stop → `204`. **Takes no body** — report the final position via a last `/progress` POST before deleting. |

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

Sessionless original bytes ("Open in VLC"), Range-capable, no Playback session. Missing File → `404 "file not found"`; on-disk unreadable → `404 "file unavailable"`.

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
| `POST /libraries/{id}/enrich` — [Admin] | `?mode=full` or `{ "mode": "full" }` (default `new` = pending only). **Synchronous** — → `200` `{ "libraryId", "total", "matched", "unmatched", "failed", "disabled" }` (the completed pass's summary; `enrichProgress` SSE streams during it). Unconfigured enrichment no-ops with candidates counted `disabled`. |
| `GET /libraries/{id}/enrichment-policy` — [Admin] | → `200` policy view ([ADR-0027](./adr/0027-per-library-enrichment-policy-sparse-override.md)): `{ "enrichEnabled": bool\|null, "inheritedEnrichEnabled", "effective": { "video", "music" }, "metadataLanguage": string\|null, "inheritedMetadataLanguage", "authoritativeProvider": string\|null, "inheritedAuthoritative": { "slug", "name" }, "effectiveAuthoritative": { "slug", "name" }, "authoritativeUnreachable": string\|null, "authoritativeCandidates": [ { "slug", "name" } ], "supplements": [ { "slug", "name", "override": bool\|null, "inheritedEnabled" } ] }` — `null` = inherit the global setting. |
| `PUT /libraries/{id}/enrichment-policy` — [Admin] | Tri-state partial update: omit = unchanged, `null` = clear-to-inherit, value = override. Keys: `enrichEnabled`, `metadataLanguage`, `authoritativeProvider`, `providerOverrides` (`{ "slug": true\|false\|null }`). → `200` fresh policy view. `422 PROVIDER_NOT_AUTHORITATIVE` (validated before any write). Side effect: kicks a background re-enrich. |
| `POST /libraries/{id}/fix-match` — [Admin] | `{ "folderPath", "title"?, "year"?, "tmdbId"?, "imdbId"? }` (folder + ≥1 identity signal) → `200` `{ "id", "folderPath", "title", "year"?, "tmdbId"?, "imdbId"?, "identityKey", "orphaned"?, "createdAt"? }`. Takes effect on the next scan; persists across rescans. |
| `GET /libraries/{id}/overrides` — [Admin] | → `200` `{ "overrides": [ matchOverrideJSON ] }` incl. orphaned ones (`"orphaned": true`). |

#### Per-entity editing — `{id}` a Title, or a Show/Artist/Album

Title routes return the full `titleDetailJSON`; Show/Artist/Album routes return `entityEnrichmentDetailJSON`: `{ "entityType": "show|artist|album", "entityId", "overview"?, "genres"?, "contentRating"?, "network"?, "enrichmentStatus"?, "lockedFields"?, "enrichmentOverride"?: { "externalId", "source"?, "status"? }, "cascade"?: { "updated", "attention" } }`.

| Endpoint (on `/titles/{id}/…` and `/{shows|artists|albums}/{id}/…`) | Notes |
| --- | --- |
| `GET …/enrichmentCandidates?q=&artist=&page=` — [Admin] | → `200` `{ "candidates": [ { "externalId", "title", "year"?, "thumbnailUrl"?, "disambiguation"?, "kind", "typeLabel"?, "tracklist"?: [ { "disc"?, "position", "title" } ] } ], "hasMore"? }`. Page size 12. Blank `q` → empty list. `503 SEARCH_UNAVAILABLE` when the provider is unconfigured/unreachable. |
| `GET …/externalPreview?ref=` — [Admin] | Resolve a pasted TMDB/MusicBrainz id or URL → `200` single candidate. `400` invalid/kind-mismatch ref, `404` no record, `503`. |
| `PUT …/enrichmentOverride` — [Admin] | `{ "externalId", "cascade"? }` → `200` detail. Durable record pin + immediate re-enrich; never touches identity/watch state; honors Locked fields. Cascade (Show→episodes, Album→tracks, Artist→albums→tracks) is best-effort, surfaced in `cascade` counts. |
| `PUT /titles/{id}/enrichmentMatch` — [Admin] | `{ "tmdbId"? \| "imdbId"? \| "musicbrainzId"? }` (≥1) → `200` titleDetailJSON. The id-anchored variant of the override. |
| `PUT …/metadata` — [Admin] | Hand edits; every present field is written **and Locked** ([ADR-0019](./adr/0019-item-editing-preserves-local-identity.md)). Title fields: `overview, tagline, title, contentRating, releaseDate, runtimeMinutes, studio, genres, cast, lockArtwork`. Entity fields: `overview, contentRating, network, genres, title, lockArtwork`. `title` edits the display label only, never identity. → `200` detail with updated `lockedFields`. |
| `DELETE …/metadata/locks/{field}` — [Admin] | Release a Locked field back to auto → `200` detail. No-op if not locked. |
| `GET …/artworkCandidates?role=` — [Admin] | Role ∈ `poster|background|cover|logo` (required). → `200` `{ "role", "candidates": [ { "url", "width"?, "height"?, "source"? } ] }` — queried live, never persisted. |
| `PUT …/artwork` — [Admin] | `{ "role", "url" }` (a candidate URL) → `200` detail. Downloads, caches, sets + Locks the role (stored Fetched-and-Locked; Local still wins at serve time). |
| `POST …/artworkUpload?role=` — [Admin] | **multipart/form-data**, file part `image` (JPEG/PNG/WebP, ≤16 MiB). Upload **is** select ([ADR-0026](./adr/0026-user-uploaded-artwork-upload-is-select-top-precedence.md)): fills + Locks the role, outranks every other source. Errors: `400`, `413 PAYLOAD_TOO_LARGE`, `415 UNSUPPORTED_MEDIA_TYPE`. |
| `PUT /titles/{id}/identityCorrection`, `PUT /shows/{id}/identityCorrection` — [Admin] | The **Wrong item** action: `{ "externalId", "title"?, "year"?, "cascade"? }` → `200` detail. Re-keys identity, **resets watch state**, clears Locked fields, pins + re-enriches. `422 WRONG_KIND` on a non-Movie leaf Title. Show cascade re-keys episodes best-effort. |

All identity/metadata mutations emit a `libraryUpdated` SSE event.

### 3.9 Server settings (admin scope)

All under `/settings/`, bearer + admin.

| Endpoint | Notes |
| --- | --- |
| `GET /settings/metadata-providers` — [Admin] | → `200` `{ "providers": [ { "slug", "name", "kinds": ["video"\|"music"…], "role": "authoritative"\|"supplement", "requiresKey", "enabled", "hasKey", "baseURL", "imageBaseURL"?, "description", "docsURL" } ], "metadataLanguage", "enablement": { "video", "music" }, "autoEnrichAfterScan", "enrichIntervalSeconds", "musicBrainzRateLimitMs" }`. The key itself is **never returned** — only `hasKey`. Registry order: tmdb, omdb, thetvdb, anidb, musicbrainz, coverart, fanarttv, theaudiodb. |
| `PUT /settings/metadata-providers` — [Admin] | Partial update; per provider `{ "slug", "enabled"?, "apiKey"?, "baseURL"?, "imageBaseURL"? }` — pointer semantics: omitted = unchanged, `""` = clear/reset-to-default, value = set. Top-level `metadataLanguage`, `autoEnrichAfterScan`, `enrichIntervalSeconds`, `musicBrainzRateLimitMs` same tri-state. All-or-nothing validation before any write. → `200` (GET shape). Errors: `422 PROVIDER_UNKNOWN` / `PROVIDER_KEY_REQUIRED` / `PROVIDER_INVALID_BASE_URL` / `PROVIDER_INVALID_LANGUAGE` / `PROVIDER_INVALID_SETTING`. **Hot-reloads** the running provider — no restart. |
| `POST /settings/metadata-providers/{slug}/test` — [Admin] | Optional body `{ "apiKey"?, "baseURL"? }` (omitted = on-file values). Always `200` `{ "ok", "detail" }` on a completed probe (10s timeout; a down host is `ok:false`, not a 500). No persistence. |
| `GET /settings/subtitle-providers` — [Admin] | → `200` `{ "providers": [ { "slug", "name", "requiresKey", "enabled", "hasKey", "baseURL", "description", "docsURL" } ], "autoFetchLang" }` (`""` = auto-fetch off). |
| `PUT /settings/subtitle-providers` — [Admin] | Same pointer semantics; `autoFetchLang` normalized (`422 PROVIDER_INVALID_LANGUAGE` if unrecognized). Hot-reloads. |
| `POST /settings/subtitle-providers/{slug}/test` — [Admin] | As the metadata test: `200` `{ "ok", "detail" }`. |
| `GET /settings/enrichment-consent` — [Admin] | → `200` `{ "state": "unset"\|"granted"\|"declined", "grantedAt"? }`. |
| `PUT /settings/enrichment-consent` — [Admin] | `{ "granted": true\|false }` (required) → `200` (GET shape). Consent gates all outbound enrichment; hot-reloads. `422 PROVIDER_INVALID_SETTING` when absent. |

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
