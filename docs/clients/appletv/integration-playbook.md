# Apple TV client — integration playbook (libmpv)

How the tvOS client *uses* the Obelo API: the sequences, state machines, and recovery rules. Endpoint shapes live in `api-contract.md` (bundled alongside); this doc covers the choreography. Everything here uses only the **[Public]** scope plus the auth spine.

**The player is libmpv** (embedded via `MPVKit` or similar, rendering into a Metal layer). That choice shapes this whole doc: mpv's network stack is ffmpeg, so it sends real HTTP headers on media requests (no cookie tricks), demuxes MKV directly, renders ASS subtitles natively via libass, and switches audio/video/subtitle tracks in-container without server round-trips.

Server base URL: discovered on the LAN (below) or user-entered (`http://<host>:8080`, or an HTTPS reverse-proxy URL). All API paths below are relative to `<base>/api/v1`.

## 0. Finding the server

The server advertises `_obelo._tcp` on the local link with TXT `txtvers=1 id=<uuid> name=<display> path=/api/v1` ([ADR-0034](../../adr/0034-server-identity-and-mdns-advertisement.md); `api-contract.md` §3.1). Browse with `NWBrowser`; declare `NSBonjourServices: [_obelo._tcp]` in Info.plist or you will see nothing.

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
- Branch on `features` flags, not `version`. Treat absent keys as `false` — that is what makes a flag safe to read against a server that predates one.
- Historical note, because an earlier revision of this playbook told you to do the wrong thing: `search`, `collections`, `playlists`, and `realtimeEvents` once advertised `false` while serving those routes, and this document advised gating UI on "a minimum server `version`" until the server flipped them. That advice was never implementable — `version` was `0.1.0` both before and after the fix, so no version string ever separated a lying server from an honest one. The flags are now pinned to their routes by `TestFeaturesMatchRoutes`. Gate on the flag; do not add a version check.

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
- **`linked` / `available`** may appear on a library and on grid rows — a library mirrored from another household's server. See §9; treat them as ordinary libraries everywhere else.

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

This is **both** watch-state reporting and the session keepalive. The server reaps a session after **90 s without a report** (default `OBELO_SESSION_IDLE_TIMEOUT`) — keep reporting while **paused** too. Report raw position only; the server applies the watched threshold (≥90% marks watched, <2% stores nothing). The two optional ids are the **track-memory write-back** — see §5.

**Stop** — final `POST /progress` with the last position, then `DELETE /sessions/{id}` (no body). Fire both in a background task on app suspension; if missed, the reaper cleans up within 90 s.

**Seek** — mpv seeks via byte-range (direct play) or within the HLS manifest (transcode). Never needs a new session.

**Re-negotiate** (new decision → new session → `loadfile` the new URL at the current position, then `DELETE` the old session) only when `constraints` change — in practice: the user picks a quality cap that forces a transcode, or you drop the cap back to direct play.

## 5. Track selection — mostly local, report the picks

This is where libmpv pays off. On **direct play the whole container is streaming to the player**, so:

- **Audio tracks**: enumerate from the decision's `audioStreams[]` (matches mpv's track list by container index — `index` in both). Switch locally: `mpv.setString("aid", ...)`. **Report the pick** on the next progress tick as `audioStreamId` (the decision entry's `id`) so the server records the **Remembered audio** — next negotiation of this title/show re-applies it via the decision's resolved `audioStream`, which you then apply to mpv at start.
- **Video tracks** (multi-cut files, e.g. B&W vs colour): same pattern — decision's `videoStreams[]`, switch locally with `vid`, report `videoStreamId` on the next progress tick for the **Remembered video**. No session restart (that's an HLS-only constraint; you're not on HLS).
- **Force Remux on Server** (`remuxSelectedOnly: true`, bandwidth trim): on a **direct-play** file the whole container ships — every audio dub, every co-packaged cut. On a constrained link, set this on the negotiate request to force a **copy-only** `directStream` carrying just the selected/negotiated video + audio (pass `videoStreamId`/`audioStreamId` to pin them, else the server's defaults); other a/v Streams are dropped, nothing is re-encoded, and it does **not** hit the transcode cap. It is a **no-op** once the session is already `directStream`/`transcode` for another reason — so grey the checkbox unless the decision came back `directPlay`. Gate the whole affordance on `features.remuxSelectedOnly`. Note this switches you onto **HLS** (the master playlist), so track selection follows the transcode-tier rules below, not the local-mpv ones.
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
| `429 STREAM_LIMIT` + `details.{active,limit}` | The user is at their concurrent-stream cap | "You're already watching on N devices." Not retryable until one ends. On a linked title this may be the *other* household's cap — the sentence is the same. |
| `503 LINK_UNREACHABLE` | A title from a linked library; that server isn't answering | "<Library>'s server can't be reached right now." Offer retry. Do **not** clear anything cached. |
| `503 LINK_REVOKED` | A linked library whose credential was revoked | "Access to this shared library has been revoked." Do **not** offer retry — an admin must paste a new invite on the server. |
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

## 9. Linked servers — someone else's libraries on this server

Gate everything in this section on **`features.linkedLibraries`**. Absent and `false` are indistinguishable, which is what makes the flag safe against an older server. (`features.serverLinking` is the *other* half — it says this server can be linked *to* — and no client reads it.)

A household's Obelo can hold a **Link** to a friend's Obelo, and the libraries the friend granted appear here as ordinary libraries whose contents live on the other machine ([ADR-0056](../../adr/0056-a-linked-library-is-a-read-only-mirror-played-through-a-one-hop-relay.md)). **There is no new playback path**: you negotiate `POST /titles/{id}/playback` exactly as always, and the server relays. What changes for a client is two fields and three error codes.

### 9.1 Rendering: `linked` and `available`

**Every browse row carries the pair**, so you never have to remember which screen the viewer came through. Both are **absent on anything local** — a household that has never linked sends exactly the wire it always did, so absent means "local", never "error".

| Shape | Where you meet it |
| --- | --- |
| `libraryJSON` | `GET /libraries` |
| `titleSummaryJSON` | the movie grid, and the `movies` / `episodes` / `tracks` groups of `GET /search`, collection members, playlist members, the Watchlist |
| `showSummaryJSON` | the TV grid, the `shows` search group, and the `show` object of `GET /shows/{id}/seasons` |
| `artistSummaryJSON` | the music grid, the `artists` search group, and the `artist` object of `GET /artists/{id}/albums` |
| `homeTitleJSON` | **Continue Watching / Up Next / Recently Added** (`GET /home`) |
| `albumJSON` | the `albums` of `GET /artists/{id}/albums`, the `album` of `GET /albums/{id}/tracks`, and the `albums` search group |
| the Track rows | `GET /albums/{id}/tracks` |
| the Episode rows | `GET /seasons/{id}/episodes` |

Three shapes deliberately carry neither, and each is reachable only from a document that does: `seasonJSON` (it has no library of its own — read the Show beside it, or the Episode rows under it), the `resumePoint` block on the Show detail (read the Show), and `titleDetailJSON` on `GET /titles/{id}` (read the row you opened; and the play itself answers `LINK_UNREACHABLE` / `LINK_REVOKED` in its own right, §9.3).

- **`linked: true`** — this library/row is a mirror of another household's. Show a small, quiet badge. Do not offer anything that writes: there is no scan, no edit, no artwork upload on the tvOS client anyway, but if you ever add one, this is the flag that hides it.
- **`available`** — sent **only** for a linked library/row, and then always (it is a real `false`, not an omission). `false` means the sharing server is not answering right now.
  - **Grey it; never hide it.** The shelf, its titles, and their place in Continue Watching all stay. Hiding them would make a friend's reboot look like a library that vanished, and it would strand the user's resume positions.
  - A greyed row must **stay selectable** and open its detail. The failure sentence belongs at play time, where it can name the cause.

### 9.2 Scanning an invite (iPhone / iPad only — **not** the Apple TV)

The Apple TV has no camera and no role here. On the phone and tablet, in an **Admin** session:

**Settings → Link a server** → camera → decode a QR whose payload starts with `obelo-link:` → `POST /links` on **the user's own server**, with that user's Admin bearer.

```
obelo-link:<base64url(JSON)>

{ "v": 1, "id": "<sharer's server id>", "name": "<sharer's server name>",
  "origins": [ "https://media.example.org", "https://obelo.tail1a2b.ts.net" ],
  "code": "…", "exp": "<RFC3339>" }
```

```
POST /links   [Admin]
{ "invite": "obelo-link:…" }
→ 201 (new) | 200 (re-key) linkJSON  — see api-contract.md §3.11
```

Five rules, and each of them is a way this goes wrong:

1. **Post the string verbatim.** Do not decode, re-encode, normalise, trim inside, or "fix" it. Your only job is to confirm the `obelo-link:` prefix so the camera does not fire on every QR code in the room; the server does the parsing and owns every refusal. Surrounding whitespace and base64 padding are tolerated server-side.
2. **The bearer is the user's own Admin token on their own server.** Nothing in the invite is a credential you hold, and nothing in it names a server you should talk to. The one call you make is to the server this app is already signed into.
3. **The phone stores nothing from the QR** — not the code, not the origins, not the sharer's id or name. It is a courier: decode, post, forget. The code is single-use and dies on redemption anyway, so a copy on the phone can only ever be a liability.
4. **Also accept a pasted string.** Not every invite arrives as a picture, and a text field is the fallback when a camera cannot focus.
5. **Do not build a Links management UI.** Listing, re-keying, syncing and unlinking live in the web app. Scanning is the one thing a phone does better.

The call can take a few seconds — the server probes the addresses, redeems the code, creates the libraries and pulls the whole catalog before it answers — so show progress and do not time out early. On success, the response names the sharer and the libraries received; say so, and refetch `GET /libraries`.

### 9.3 The refusals, and the sentence for each

Every one of these is a different next move for the person holding the phone. Do not collapse them.

| Code | Status | Say |
| --- | --- | --- |
| `BAD_INVITE` | 400 | "That isn't a valid invite — ask them to send it again." (Also what a spent code returns.) |
| `INVITE_EXPIRED` | 410 | "This invite has expired. Ask them for a fresh one." An invite lasts 24 hours. |
| `LINK_PROTOCOL` | 409 | `details.upgrade` is `"theirs"` or `"ours"` — **use it**: "Their server needs an update" or "This server needs an update." Never make the user compare two version numbers. |
| `LINK_UNREACHABLE` | 503 | "Couldn't reach their server at any of the addresses in the invite." Offer retry. If you decoded the invite for its `origins`, listing them helps — read-only, never dialled. |
| `LINK_SERVER_MISMATCH` | 409 | Only on a re-key: "This invite is for a different server." |
| `403` | — | The signed-in user is not an Admin. Hide the entry point for non-Admins rather than letting them find this. |

Anything else: show the server's own `message`. It is written to be read.

## 10. libmpv housekeeping (not API, but will bite)

- **Licensing**: build libmpv **LGPL** (`-Dgpl=false`, and mind ffmpeg's own flags) for App Store distribution, and provide relinking compliance per LGPL. The GPL default build is not App-Store-compatible.
- Ship mpv's own ICC/HDR tone-mapping config for the Apple TV's output mode; declare `hdr` in the profile but verify Dolby Vision output behavior on-device (mpv outputs HDR10 from DV profiles it can't fully handle).
- Set a distinct `User-Agent` (e.g. `Obelo-tvOS/<version>`) via mpv's `user-agent` property — useful in server logs next to the Device row.
