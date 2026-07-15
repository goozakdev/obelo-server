# Apple TV client — integration playbook (libmpv)

How the tvOS client *uses* the Juice Box API: the sequences, state machines, and recovery rules. Endpoint shapes live in `api-contract.md` (bundled alongside); this doc covers the choreography. Everything here uses only the **[Public]** scope plus the auth spine.

**The player is libmpv** (embedded via `MPVKit` or similar, rendering into a Metal layer). That choice shapes this whole doc: mpv's network stack is ffmpeg, so it sends real HTTP headers on media requests (no cookie tricks), demuxes MKV directly, renders ASS subtitles natively via libass, and switches audio/video/subtitle tracks in-container without server round-trips.

Server base URL: discovered on the LAN (below) or user-entered (`http://<host>:8080`, or an HTTPS reverse-proxy URL). All API paths below are relative to `<base>/api/v1`.

## 0. Finding the server

The server advertises `_juicebox._tcp` on the local link with TXT `txtvers=1 id=<uuid> name=<display> path=/api/v1` ([ADR-0034](../../adr/0034-server-identity-and-mdns-advertisement.md); `api-contract.md` §3.1). Browse with `NWBrowser`; declare `NSBonjourServices: [_juicebox._tcp]` in Info.plist or you will see nothing.

- **Still build manual entry.** mDNS is link-local, so a reverse-proxied or VPN-reachable server can never be discovered. Manual entry is the permanent path, not a stopgap.
- **Store the `id` alongside the token.** This is the payoff: when the server's DHCP lease changes, rediscover the service whose `id` matches, update the base URL, and **keep the token** — it is bound to a Device row, not to an address. The user never sees a re-login.
- TXT is a hint (RFC 6763). Confirm against `GET /server`, which carries the same `id`/`name`.
- Both fields are additive: a server predating ADR-0034 omits them. Absent means "old server", never "error".

## 1. Cold start

```
GET /server                      (unauthenticated)
 ├─ setupRequired: true  → show "finish setup in the web app" screen; poll or retry.
 └─ setupRequired: false → have stored token?
     ├─ yes → validate it with any cheap call (GET /devices).
     │        401 → drop token, go to login. 200 → straight to Home.
     └─ no  → login screen.
```

- Persist a **stable `clientId` UUID** on first launch (Keychain). Re-login with the same `clientId` reuses the server-side Device instead of creating "Living Room (7)".
- Branch on `features` flags, not `version`. Treat absent keys as `false`. **Known caveat** (contract, Part 1): `search`, `collections`, `playlists`, `realtimeEvents` currently read `false` on a server where the routes work — until the server flips them, gate UI on a minimum server `version` instead of those four flags.

## 2. Login & credential handling

```
POST /auth/login { username, password, device: { name, platform: "tvos", clientId } }
→ { token, user, device }
```

Store `token` in the Keychain. It is opaque, DB-backed, and **revocable at any moment** — so *every* 401 anywhere means "token is dead": drop it and return to login. No refresh flow, no expiry to track.

**One token, one transport.** With libmpv there is no cookie dance: every request — JSON calls *and* media fetches — carries `Authorization: Bearer <token>`:

```swift
// mpv property, set once per player (and again after re-login):
mpv.setString("http-header-fields", "Authorization: Bearer \(token)")
```

The `ms_media` cookie and `?token=` exist for header-less players (browsers); this client ignores them entirely.

Logout: `POST /auth/logout`, then clear the Keychain and the mpv header property.

## 3. Browse layer

- **Home screen**: `GET /home` → `continueWatching` / `upNext` / `recentlyAdded` rows (each ≤20, computed per-user). Refetch on foreground and after any playback session ends.
- **Library grids**: `GET /libraries` → per-library `GET /libraries/{id}/titles`. Only the three top-level grids paginate (`limit`/`cursor` → `nextCursor`); drive collection-view prefetch off `nextCursor`. Seasons/episodes/albums/tracks come back whole.
- **Detail → play**: `GET /titles/{id}` gives Editions/Files/Streams for the pre-play UI. For TV, `GET /shows/{id}/seasons` includes `resumePoint` — the server-computed Up Next episode with `mode` `inProgress` (Continue + Restart) or `next` (Play). Don't compute next-episode logic client-side.
- **Artwork**: fetch the JSON-advertised URLs with the bearer header (plain `URLSession`). URLs may carry `?v=` cache-busters — treat the full URL string as the cache key and invalidation is free.
- **404 means "doesn't exist for this user"** everywhere (access-hiding). Render not-found/empty, never "forbidden".

## 4. Playback state machine

```
            ┌──────────────────────────────────────────────────────┐
            ▼                                                      │
  [Negotiate] ── 200 decision ──► [Playing] ── user caps quality ─►[Re-negotiate]
      │  │                          │    ▲                         (new constraints,
      │  └─ 503 SERVER_BUSY ─► retry with suggestedMaxBitrate       new session)
      │  └─ 501 TRANSCODE_REQUIRED ─► show "can't play" w/ reason
      ▼                             │
   [Error UI]                       └── user stops / item ends ──► [Stop]
```

**Negotiate** — `POST /titles/{id}/playback` with the libmpv profile (see `capability-profile.md`) and current `constraints`. With that profile nearly everything comes back `tier: "directPlay"` with `streamUrl: /sessions/{id}/stream`. Set `startPosition` from the title's `resumePositionMs`; hand the absolute `streamUrl` to mpv (`loadfile`), then seek mpv to `startPosition` (the progressive stream is byte-range seekable; mpv handles it).

**Playing** — start a 10–15 s timer:

```
POST /sessions/{id}/progress { positionMs, state: "playing"|"paused"|"buffering",
                               audioStreamId?, videoStreamId? }
```

This is **both** watch-state reporting and the session keepalive. The server reaps a session after **90 s without a report** (default `JUICEBOX_SESSION_IDLE_TIMEOUT`) — keep reporting while **paused** too. Report raw position only; the server applies the watched threshold (≥90% marks watched, <2% stores nothing). The two optional ids are the **track-memory write-back** — see §5.

**Stop** — final `POST /progress` with the last position, then `DELETE /sessions/{id}` (no body). Fire both in a background task on app suspension; if missed, the reaper cleans up within 90 s.

**Seek** — mpv seeks via byte-range (direct play) or within the HLS manifest (transcode). Never needs a new session.

**Re-negotiate** (new decision → new session → `loadfile` the new URL at the current position, then `DELETE` the old session) only when `constraints` change — in practice: the user picks a quality cap that forces a transcode, or you drop the cap back to direct play.

## 5. Track selection — mostly local, report the picks

This is where libmpv pays off. On **direct play the whole container is streaming to the player**, so:

- **Audio tracks**: enumerate from the decision's `audioStreams[]` (matches mpv's track list by container index — `index` in both). Switch locally: `mpv.setString("aid", ...)`. **Report the pick** on the next progress tick as `audioStreamId` (the decision entry's `id`) so the server records the **Remembered audio** — next negotiation of this title/show re-applies it via the decision's resolved `audioStream`, which you then apply to mpv at start.
- **Video tracks** (multi-cut files, e.g. B&W vs colour): same pattern — decision's `videoStreams[]`, switch locally with `vid`, report `videoStreamId` on the next progress tick for the **Remembered video**. No session restart (that's an HLS-only constraint; you're not on HLS).
- **Embedded subtitles — text and image (PGS/VOBSUB)**: already in the container; mpv lists and renders them natively, libass styling and all. Select with `sid`. **No server involvement, no burn-in, ever, on direct play.** Ignore the decision's embedded-track `url`s in this case.
- **Sidecar / fetched subtitles**: *not* in the container — load each from the decision's `subtitles[]` via `sub-add <base+url>` (mpv sends the auth header on these too). With the profile declaring `ass`/`srt`, the `url`/`format` fields point at **original-format bytes** ([ADR-0033](../../adr/0033-original-format-subtitle-delivery-negotiated-by-capability.md)) — ASS renders with full styling. Key parsing/labeling off `format`, not byte-sniffing.
- **Fetching missing subtitles**: `POST /titles/{id}/subtitles/search` `{ language }` → candidates → `POST .../subtitles/fetch` (any user; quota-bearing). The response's track serves `.vtt`; `sub-add` it immediately, and on the *next* negotiation it arrives with its original format like any fetched track.
- **On the transcode tier** (HLS): mpv plays the master playlist natively — in-band audio renditions and WebVTT subtitle renditions appear as ordinary mpv tracks. Image subs are the one case that still needs server burn-in: re-negotiate with `burnSubtitleId` *only when already transcoding*.

Subtitle preference has **no server memory** in v1 (audio and video do) — persist the user's subtitle language/on-off locally.

## 6. Real-time events (SSE)

`GET /events` with the bearer header (`URLSession` streaming). First bytes are `: connected`; then `event:`/`data:` pairs.

- **No heartbeat, no `id:`, no `retry:`** — detect death via read timeout, reconnect with backoff + jitter, and refetch the current screen's data on reconnect (no resume; events are refetch nudges, never diffs).
- Useful here: `libraryUpdated` (invalidate grids), `scanProgress`/`enrichProgress` (optional "library updating" affordance). `session*` events are admin-only.
- SSE is an optimization — everything is pollable; on-foreground refetch is the fallback.

## 7. Error-recovery matrix

| Response | Meaning | Client action |
| --- | --- | --- |
| `401 UNAUTHORIZED` (any endpoint) | Token revoked/invalid | Drop token, clear mpv header property, return to login. No retry. |
| `404` on a title/session/playlist | Doesn't exist *for this user* (or reaped session) | Not-found/empty UI. Mid-playback session 404 → offer resume (re-negotiate at last position). |
| `503 SERVER_BUSY` + `details.suggestedMaxBitrate` | Transcode cap full | Offer "retry at lower quality" with the suggested bitrate. Rare with the mpv profile (few transcodes). |
| `501 TRANSCODE_REQUIRED` + `details.reason` | Structurally unplayable | Show "can't play" with the reason. Not retryable. |
| `422` (`KIND_MISMATCH`, `ITEM_SET_MISMATCH`, `UNKNOWN_TITLE`, `SYSTEM_PLAYLIST`) | Domain rule violation | Surface inline; server state unchanged (all 422s are no-ops). |
| `400 BAD_REQUEST` `"invalid JSON body"` | Client bug: unknown field, >1 MiB, malformed | Fix the payload — the decoder rejects unknown fields. |
| Network unreachable | Server down / off-LAN | Cached UI + backoff; `GET /server` is the cheapest liveness probe. |
| mpv `end-file` with error mid-stream | Stream died (reaped session, network) | Check the session with a progress POST: 404 → re-negotiate at last position; else `loadfile` the same URL and seek. |

## 8. Things the server owns (don't reimplement)

- **Watched threshold** (90%/2%) — report raw positions only.
- **Up Next / resume point** — read `resumePoint` from `/shows/{id}/seasons`.
- **Remembered audio/video** — report picks via progress; apply the decision's resolved streams at start.
- **Access filtering** — everything arrives pre-filtered.
- **Edition choice** — omit `editionId` and the server picks; send it only on explicit user choice.

## 9. libmpv housekeeping (not API, but will bite)

- **Licensing**: build libmpv **LGPL** (`-Dgpl=false`, and mind ffmpeg's own flags) for App Store distribution, and provide relinking compliance per LGPL. The GPL default build is not App-Store-compatible.
- Ship mpv's own ICC/HDR tone-mapping config for the Apple TV's output mode; declare `hdr` in the profile but verify Dolby Vision output behavior on-device (mpv outputs HDR10 from DV profiles it can't fully handle).
- Set a distinct `User-Agent` (e.g. `JuiceBox-tvOS/<version>`) via mpv's `user-agent` property — useful in server logs next to the Device row.
