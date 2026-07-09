# API Contract

The single HTTP/JSON API with public and admin scopes ([ADR-0010](./adr/0010-unified-two-scope-api.md)), consumed by the web app and all clients. Treated as a versioned product because clients and server update independently.

## Style & versioning

- **REST-ish JSON over HTTP**, resource-oriented, **camelCase** field names.
- **Versioning via URL path** — `/api/v1/…`. One integer major version. Additive changes (new fields/endpoints) never bump it; only breaking changes mint `/api/v2`, and the server may serve both during a transition.
- **Handshake** — `GET /api/v1/server` returns server version, supported API versions, and a **feature-flags** map. Clients branch on feature flags, not on version strings, so mismatched client/server pairs degrade gracefully instead of breaking.
- **Error envelope** — every error returns:
  ```json
  { "error": { "code": "STRING_ENUM", "message": "human readable", "details": { } } }
  ```
  with a machine-readable `code` and correct HTTP status.

## Authentication

- **Login:** `POST /api/v1/auth/login` with `{ username, password, device: { name, platform, clientId } }` → `{ token, user, device }`. The client persists a stable `clientId` (UUID); re-login from the same `clientId` reuses/refreshes the existing Device instead of duplicating it.
- **Transport:** `Authorization: Bearer <token>`.
- **Token format:** opaque, DB-backed, validated per request — not a JWT, so revocation is immediate ([ADR-0015](./adr/0015-opaque-db-backed-tokens.md)).
- **Revocation:** `POST /api/v1/auth/logout` kills the current token; `DELETE /api/v1/devices/{id}` (self or Admin) kills that Device's token.
- **Bootstrap:** `GET /api/v1/server` reports `setupRequired: true` when zero users; `POST /api/v1/setup` with `{ claimToken, username, password }` creates the first Admin ([ADR-0013](./adr/0013-first-admin-claim-token-bootstrap.md)).

## Browse & read surface

- **Resources** mirror the domain: `/libraries`, `/libraries/{id}/titles`, `/titles/{id}` (Editions/Files/Streams nested), `/shows/{id}/seasons` → `/seasons/{id}/episodes`, `/artists/{id}/albums` → `/albums/{id}/tracks`, `/collections`, `/playlists`, `/search`, `/home` (computed Continue Watching / Up Next / Recently Added rows).
- **Cursor pagination** — `?limit=&cursor=`, response carries `nextCursor`. Not offset (which double-shows/skips when the catalog mutates and scales poorly to 100k-track libraries).
- **Filtering/sorting** via a bounded, defined query-param set (`sort=`, `filter[unwatched]=`, `filter[genre]=`), not an arbitrary query language.
- **Access enforced server-side on every endpoint.** A Member sees only the Libraries granted to them and only Titles at or below their Rating ceiling; an entity outside that access returns **`404`, not `403`** — hiding existence so restricted content can't be enumerated. `/libraries` and every browse/search/home listing are filtered to the caller's grants in SQL (so pagination and counts stay correct); an Admin sees everything (all Libraries, no cap).
- **Un-rated content stays visible.** A Title with no **Content rating** (Enrichment is optional, so many Titles have none) — or a rating label outside the known maturity ladder (MPAA + US TV) — is treated as "no rating information" and is **never** hidden by a ceiling. A ceiling only caps Titles whose rating is known and above it, so a capped Member's freshly-scanned, un-enriched Library does not look empty. (A strict "hide what we can't classify" mode is a possible future per-ceiling toggle.)

## Collections & Playlists

The two **Organization** groupings (CONTEXT.md). Both resolve their member **Titles** decorated like a browse grid and access-filtered per viewer; they differ in ownership and ordering.

- **Shared member shape.** A Collection/Playlist member carries the **same Title-summary fields a browse list does** (id, kind, title, year, watched/`resumePositionMs`, `genres`, `contentRating`, `artworkVersion`, …) — one decoration code path, so a grouping grid is byte-for-byte consistent with a browse grid. A Playlist member adds an **`itemId`** (the only extra field) so duplicate entries are distinguishable.

### Collections (Admin-curated, shared)

- **Endpoints & scopes.** Writes are **Admin**: `POST /collections` (`{ name, description? }`), `PUT /collections/{id}` (rename / edit description), `DELETE /collections/{id}`, `POST /collections/{id}/items` (`{ titleIds: [...] }`, add), `DELETE /collections/{id}/items/{titleId}` (remove). Reads are **any authenticated User**: `GET /collections` (list — each card carries a per-viewer member count + representative poster) and `GET /collections/{id}` (detail + decorated members).
- **Per-viewer access-filtering.** Members are filtered by the viewer's library grants + Rating ceiling — the **same access rules as the rest of the catalog** (see *Access enforced server-side* above): a member in an ungranted Library or above the viewer's ceiling is silently absent. The count and poster on the list card are computed over the viewer's visible members.
- **Zero-visible hiding.** A non-Admin who can see **no** members of a Collection doesn't see it exist at all — it is absent from `GET /collections` and **`404`** on detail (the 404-not-403 posture). An **Admin always sees every Collection** and its full membership, including an empty one.
- **Semantics.** Membership is an idempotent **set** (re-adding a Title is a no-op); a Collection may span media kinds (a Movie + an Episode is fine); a **Missing** Title is omitted from the resolved view while its membership row persists (it reappears when its Files return). Deleting a Collection (or the underlying Title/Library) drops the membership rows; the Titles are untouched.

### Playlists (User-owned, private, ordered)

- **Endpoints (all public scope, owner == caller).** `POST /playlists` (`{ name }`, creates empty/untyped), `GET /playlists` (lists the caller's own only), `GET /playlists/{id}` (detail + ordered, decorated members), `PUT /playlists/{id}` (rename), `DELETE /playlists/{id}` (delete), `POST /playlists/{id}/items` (`{ titleId }`, append at end), `PUT /playlists/{id}/items` (`{ itemIds: [...] }`, reorder — the **full** item-id permutation, rewritten transactionally; a set that isn't exactly the current item ids is a `422 ITEM_SET_MISMATCH` no-op), `DELETE /playlists/{id}/items/{itemId}` (remove one entry **by item id**, since duplicates exist).
- **Owner-private (hide-existence).** Every read/write resolves caller ownership; a non-owner — **including an Admin** (no override) — gets **`404`**, exactly like another User's playback session. A foreign Playlist is also absent from `GET /playlists`.
- **Single media kind.** A Playlist is created untyped; the **first** appended Title fixes its kind (movie / tv / music — Title kinds `movie` / `episode` / `track` respectively). A later append whose kind doesn't match is a **`422 KIND_MISMATCH`** that leaves the kind and membership unchanged.
- **Ordered, duplicates allowed.** Members are returned in explicit position order. The same Title may appear more than once (a sequence, not a set); each occurrence is its own `itemId`.
- **Resolved view is access-filtered + Missing-aware.** Even though the owner only added Titles they could see, the resolved view applies the owner's **current** access Scope and omits now-out-of-scope or Missing members — while the `playlist_items` row survives so the member returns if access/Files return. Deleting the owner cascades their Playlists away.

## Capability profile

Input to the tier decision ([ADR-0003](./adr/0003-three-tier-playback-with-capability-negotiation.md)). Split into a static device profile and dynamic per-request constraints.

- **Device profile** (static; registered on the Device, referenced by `clientId`):
  ```json
  {
    "containers": ["mp4", "mkv", "fmp4"],
    "videoCodecs": [
      { "codec": "h264", "maxLevel": "4.2", "maxResolution": "1080p" },
      { "codec": "hevc", "maxResolution": "2160p", "hdr": ["hdr10", "dolbyvision"] }
    ],
    "audioCodecs": ["aac", "ac3", "eac3", "flac"],
    "maxAudioChannels": 6,
    "textSubtitleFormats": ["webvtt"]
  }
  ```
  Codecs carry per-codec resolution/HDR limits, not a flat list.
- **Session constraints** (dynamic; sent per playback request):
  ```json
  { "maxBitrate": 8000000, "maxResolution": "1080p",
    "preferredAudioLang": "en", "preferredSubtitleLang": "en" }
  ```
  `maxBitrate` reflects the current network (cellular vs wifi) and user quality cap — the field that most often flips a direct-play-capable file into a transcode.

The server merges device profile + constraints to choose a tier.

## Playback negotiation & session lifecycle

- **Request:** `POST /api/v1/titles/{id}/playback` with `{ clientId, constraints, startPosition, editionId? }`. The server picks the best Edition by constraints unless `editionId` is given.
- **Decision response:**
  ```json
  { "sessionId": "...", "tier": "directPlay|directStream|transcode",
    "streamUrl": "...",
    "edition": {}, "videoStream": {}, "audioStream": {},
    "subtitles": [
      { "id": "...", "source": "embedded|sidecar|fetched", "kind": "text|image",
        "language": "en", "forced": false, "label": "English",
        "url": ".../titles/{id}/subtitles/{subId}.vtt" }
    ],
    "estimatedBitrate": 6000000 }
  ```
  `streamUrl` is a progressive byte-range URL for directPlay, or an HLS manifest URL otherwise ([ADR-0004](./adr/0004-hls-for-adaptive-progressive-for-direct-play.md)).
- **Subtitles** ([ADR-0020](./adr/0020-subtitle-delivery-in-band-hls-out-of-band-track-image-burn-in.md)): `subtitles` is every selectable Subtitle track for the played File — the union of embedded Streams and Sidecar/Fetched tracks — replacing the old thin `subtitle: {mode,url}`. A **text** track carries an out-of-band WebVTT `url` (identity-scoped, cacheable, media-cookie-or-bearer) the client drops into a `<track>`; selection is client-side only (no round-trip). An **image** track carries no `url` (it burns in on the transcode tier — a later slice). The list is non-empty-safe (`[]` when the File has none).
- **Fetching a subtitle online** ([ADR-0021](./adr/0021-external-subtitle-fetching-mirrors-enrichment.md)): when a Title lacks a subtitle in a wanted language, **any User (Members included)** can fetch one from an external provider. `POST /api/v1/titles/{id}/subtitles/search` with `{ "language": "de" }` returns `{ "candidates": [ { id, language, format, release, forced, hearingImpaired, matchedBy, label } ] }` (matched moviehash → imdb_id → filename query; a disabled/offline provider degrades to `[]`, never an error). `POST /api/v1/titles/{id}/subtitles/fetch` with `{ "language", "candidate": {…} }` (the chosen candidate echoed back) downloads it, converts a text sub to WebVTT, caches it identity-keyed under the data dir, records a `source:"fetched"` row (the pick **locks** — re-fetch is explicit), and returns `{ "subtitle": {id, source, kind, language, forced, label, url} }` — a decision-style track the client enables like any other. A fetched track survives rescans and follows a Match override.
- **Subtitle provider settings** (Admin): `GET/PUT /api/v1/settings/subtitle-providers` (registry joined with settings; the key is never returned, only `hasKey`; `autoFetchLang` defaults `""` = off) and `POST /api/v1/settings/subtitle-providers/{slug}/test` (a live `{ ok, detail }` connectivity probe) — the exact shape of the metadata-provider settings surface. A save rebuilds + hot-swaps the running provider with no restart.
- **Server busy:** a transcode that would exceed the cap ([ADR-0009](./adr/0009-transcode-governance.md)) returns **HTTP 503**, `code: "SERVER_BUSY"`, `details: { retryable: true, suggestedMaxBitrate }`. Direct play / remux never hit this.
- **Session lifecycle:** the decision creates a Playback session. Periodic progress reports (below) double as keepalive. A session with **no progress for N seconds is reaped** — transcode scratch deleted, FFmpeg job killed, cap slot freed. `DELETE /api/v1/sessions/{id}` is the clean stop.
- **Seeking needs no new decision:** HLS seeks move within the manifest (server realigns the transcode); direct play uses byte-range. A new decision is required only if constraints change (e.g. network drop → lower `maxBitrate`).

## Progress & watch state

- **Report against the session:** `POST /api/v1/sessions/{id}/progress` with `{ positionMs, state: "playing|paused|buffering" }` every ~10–15s while playing. The server resolves the Title from the session; this is also the keepalive.
- **Server applies the Watched threshold, not the client:** server writes resume position to the (User, Title) watch state; crossing **90%** marks watched + clears resume + advances TV Up Next; a stop below the **2%** floor saves no resume. Clients report raw position only — they cannot invent "watched" semantics.
- **Stop:** `DELETE /api/v1/sessions/{id}` carries a final position; completion past 90% flips watched.
- **Manual override:** `PUT /api/v1/titles/{id}/watchState` toggles watched/unwatched, bypassing the threshold.
- **Concurrency:** per-(User, Title), **last-write-wins** on position — no locking or merge.

## Real-time updates

Server→client push over a single SSE stream ([ADR-0016](./adr/0016-sse-for-realtime-updates.md)):

- `GET /api/v1/events` — SSE stream scoped to the authenticated user. Cookie-capable auth (a browser `EventSource` cannot set `Authorization`). Each event carries an **audience**, gated inside the Broker against the subscriber's identity *before* enqueue — so a stream never even buffers an event it isn't entitled to.
- Event types, with audience and pollable-resource fallback:
  | Event | Audience | Poll fallback |
  | --- | --- | --- |
  | `enrichProgress` | broadcast (carries no per-user data) | `/libraries`, `/libraries/{id}/titles` |
  | `scanProgress` | library-scoped | `/libraries/{id}/scan` |
  | `libraryUpdated` | library-scoped | `/libraries`, `/libraries/{id}/titles` |
  | `sessionStarted` | admin-only | `/sessions/{id}/*` |
  | `nowPlaying` | admin-only | `/sessions/{id}/*` |
  | `sessionEnded` | admin-only | `/sessions/{id}/*` |
- **Audience semantics.** *broadcast* → every subscriber; *admin-only* → Admins only (a Member's stream never receives session events); *library-scoped* → subscribers whose accessible-Library set contains the event's Library (Admins see all Libraries). The accessible-Library set is resolved from the subscriber's per-User grants at subscribe time, so a Member's stream carries `scanProgress` / `libraryUpdated` only for the Libraries granted to them; an Admin receives every library-scoped event.
- Plain HTTP (reverse-proxy friendly), browser/native auto-reconnect. Every event maps to a pollable resource (above) as a fallback — SSE is an optimization, not the only path to state. **Note:** there is no `GET /sessions` collection endpoint today; only the per-session sub-resources (`/sessions/{id}/stream`, `/sessions/{id}/hls/...`, `/sessions/{id}/progress`, `DELETE /sessions/{id}`) exist. An Admin session-list endpoint is a known follow-up; the session *events* are deliverable without it.
- **Scan trigger is asynchronous.** `POST /libraries/{id}/scan` (Admin) accepts the request and returns **202 Accepted** with the scan status (`state: "running"`); the scan then runs in the background, server-side, decoupled from the request connection — a client that navigates away (or otherwise disconnects) does **not** cancel it. The pollable `GET /libraries/{id}/scan` (and the `scanProgress` SSE above) is the source of truth for progress and completion. An unknown Library is `404` (validated before the 202); a non-Admin is `403`.
