# A scoped, expiring media credential for players that delegate the fetch

A Playback session can mint a **stream token**: a 256-bit secret that authorises the media artifacts
of that one session, expires in four hours, dies with its session, and rides in the URL **path** —

```
GET /api/v1/stream/{streamToken}/stream        progressive direct play
GET /api/v1/stream/{streamToken}/hls/{file}    master/media playlists, segments, WebVTT
```

There is no session id in either URL; the token *is* the session identifier. This is the third media
credential here, after the bearer header and the `ms_media` cookie, and the server's second expiring
credential kind, after the Device authorization grant ([ADR-0036](./0036-device-authorization-grant-for-tv-sign-in.md)).

## The gap

Media bytes were reachable by exactly two credentials, and both assume the same thing:

- **`Authorization: Bearer`** — the native path. libmpv sets real HTTP headers, so the Apple TV / iOS
  client authenticates media exactly as it authenticates JSON.
- **The `ms_media` cookie** — ambient auth for a browser's `<video src>` / `<img src>` /
  `EventSource`, honoured only by the read-only media GETs (`requireAuthAllowCookie`).

The assumption both share is that **the party that wants the bytes is the party holding the
credential**. A whole class of player breaks it: it does not fetch the media, it hands the *URL* to
somebody else and lets them fetch.

**AirPlay video is the motivating case, not the only member of the class.** An iOS client that
AirPlays does not send pixels to the receiver; AVFoundation gives the receiver the URL and the
receiver fetches for itself. That receiver is an Apple TV running somebody else's software. It cannot
be made to send a bearer header, and whether it forwards cookies attached to the asset is a property
of AVFoundation and the receiver firmware — not something this server or its clients decide. The same
shape reappears for a Chromecast-style receiver, a smart-TV app, or any share-a-link feature.

Two non-answers were considered and rejected before this one:

- **Recording it as a client limitation.** It had been, in the iOS repo's checklist and ADR-0001
  there. But no client can solve it — the receiver's HTTP behaviour is not ours — so filing it on the
  client side files it somewhere it cannot be fixed.
- **Hoping the cookie survives the handoff.** It might. It is unverifiable from this repo, it varies
  by OS version and by receiver, and it leaves a shipped feature whose auth works by accident. A
  server-side answer turns the client's probe into a nice-to-know instead of a gate.

## Why not widen `?token=`

The cheapest fix — let the media routes accept the `?token=` query parameter that
`GET /files/{id}/download` already accepts — is the wrong one, and it is worth writing down why,
because `requireAuthAllowQueryToken` has already argued the case and this ADR is what keeps its
argument true rather than making it a lie by extension. Its own comment:

> Tradeoff (accepted for the self-hosted LAN posture, ADR-0005): a URL-borne token can land in server
> access logs, browser history, and the .xspf file on disk. **It is scoped here to a single read-only
> GET**, the token still validates against the DB on every request (revokes immediately on
> logout/device-delete, ADR-0015), and **NO other endpoint honors a query token**.

The token in that parameter is the opaque **account** token: the same durable credential that
authorises every read and every mutation in this API, and one that **never expires** — there is no
`expires_at` on `auth_tokens` and nothing reads its `created_at`. The comment accepts URL exposure
*because* the exposure is confined to one read-only download. Handing that same permanent,
full-authority credential to a third-party television — which will log it, and may cache it —
is a different proposition, and every clause of that comment would have to be deleted to allow it.

So the answer is not a wider `?token=`. It is a **different credential**, on the other side of the
line: scoped to one session's bytes, read-only by construction, expiring, revoked with its session.
Both live in URLs; only one of them can afford to. `requireAuthAllowQueryToken` is untouched by this
slice and its comment remains exactly as true as it was.

## Why the path, not the query

This is the shape of the whole feature, not a preference, and it is decided by how HLS resolves URIs.

**Every playlist this server emits uses bare relative URIs** — `000.ts`, `#EXT-X-MAP:URI="init.mp4"`,
`audio_7.m3u8`, `subs_3.m3u8`. A player resolves those against the playlist's URL **with the query
string discarded**. A `?token=` on a manifest therefore authenticates the manifest and nothing under
it: the playlist loads, every segment 401s, and the failure appears only on a real player — a test
that fetches a manifest passes.

Making a query token reach the segments means rewriting URIs in **three playlist producers**:

1. the **master playlist** builder, which names the rendition playlists (`internal/audio/hls.go`,
   `subtitle.RenditionLines`);
2. the **synthesized media playlist**, which the server writes itself for the runtimes that own their
   playlist — the re-encoding transcode, a video copy with probed segment boundaries, and the demuxed
   audio renditions (`internal/playback/remux.go`);
3. **ffmpeg's own playlist**, which the server does *not* author. For the runtimes that do not
   synthesize one — a remux/copy with no probed boundaries, and the audio-only path — the server waits
   for ffmpeg's playlist and serves it **verbatim**, deliberately, so the segment/playlist
   correspondence stays byte-exact ([ADR-0024](./0024-direct-stream-video-transcode-audio-fmp4.md)).
   Post-processing that on the hot path is precisely the thing the transcode work fought to avoid.

(The flag that selects between 2 and 3 is `ownsPlaylist`, and it reads the way it is spelled: `true`
means *the server owns and synthesizes* the playlist, `false` means ffmpeg's own bytes go out
untouched. An earlier draft of this design had that backwards; the conclusion survives either way,
but a reader chasing it in the code should chase the right branch.)

A **path prefix** costs none of that. `000.ts` relative to `/api/v1/stream/{tok}/hls/index.m3u8` is
`/api/v1/stream/{tok}/hls/000.ts` by plain URI resolution. **Zero playlist bytes change**, which is
what makes this cheap rather than a fourth playlist rewriter. Progressive direct play is served under
the same prefix for the same reason it would otherwise be missed: an AVPlayer client AirPlaying an
mp4/h264 Title lands on `directPlay`, not HLS, and would hit the identical wall one route over.

## The posture, stated rather than assumed

This is a self-hosted LAN product ([ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md)), and a
security posture here should be argued rather than inherited.

**A stream token in a URL will be logged somewhere we do not control.** By a proxy, by the receiver,
by whatever sits between them. This is accepted, in the same spirit as
`requireAuthAllowQueryToken`'s comment — and it is *why* the credential is scoped and expiring rather
than durable. The blast radius of a leaked stream token is **one film, for one session's lifetime, on
a LAN**: it reaches only that session's media GETs, no metadata, no mutation, no other session, no
other User, and it is not an account token anywhere.

What we do control, we redact. The token never appears in an error envelope (the resolver returns no
error, so there is nothing that *could* be rendered), the request handed to the handlers carries a
redacted `URL.Path`, and `api.RedactPath` exists for any log outside the API. On the secret-handling
line [ADR-0036](./0036-device-authorization-grant-for-tv-sign-in.md) drew between its two codes, a
stream token sits firmly on the `deviceCode` side and inherits that handling rather than reinventing
it: 256 bits of CSPRNG from the same generator, stored only as a SHA-256 hash, shown to nobody,
logged nowhere. There is deliberately **no** short display sibling — nothing here is read by a human,
so a `userCode` analogue would only be a second thing to confuse with the secret.

A bad, expired, or revoked token is a `404` with the byte-identical envelope a wrong-User session
fetch already produces, never a `401` or `403`, and with no `WWW-Authenticate` to invite a retry. The
method gate runs **before** the token is examined, so a non-GET cannot become a validity oracle.

### The TTL is four hours, and expiry is not the control

**Revocation is.** The session-ended cascade deletes a session's tokens, and it hangs off the Playback
Manager's session-ended observer, which both `DELETE /sessions/{id}` and the idle reaper already
fire — one revocation path rather than two that drift. Worst case is
`SessionIdleTimeout + SessionReapInterval` = 90s + 30s = **two minutes** from the last byte fetched.
An abandoned session cannot leave a live credential behind.

The TTL only binds where that cascade cannot run at all: **after a restart**, when the in-memory
session map is gone but the SQLite rows survive. That is the case four hours is sized for, and it is
bounded from both sides:

- **It must not die before its session.** An actively-watched session lives as long as the sitting
  does — every progress report and every manifest/segment fetch Touches it, and only 90 seconds of
  *total silence* lets the reaper end it, which says nothing about total length. Crucially, **the
  party holding the token cannot re-mint**: minting requires a bearer, and the receiver was handed a
  URL and will not come back for another. A token that expires mid-film is a television going black
  for no reason the viewer can act on.
- **It must not outlive its usefulness.** Since it cannot be shortened by leaning on re-minting, the
  ceiling is "one sitting, generously": long enough for a feature-length Title with pauses, short
  enough that a token logged on somebody's television is dead by the next day.

Four hours is the smallest round span that clears both, and against the reaper it reads honestly:
it is **120 of the reaper's two-minute worst-case windows**, so the cascade gets 120 chances to
revoke before the expiry ever matters.

A client that wants a shorter-lived credential already has one: `POST /sessions/{id}/stream-token`
mints a fresh token on demand (bearer only), which is also how a client that decides to AirPlay long
after negotiating gets one without re-negotiating a stream that is already playing.

## What this deliberately does not fix

This ADR introduces the server's **second** expiring credential kind, and is a decent argument that
the **first** one should expire too. It does not do that work, and a reader should not leave assuming
it did:

- **`auth_tokens` still never expires.** No `expires_at`, nothing reading `created_at`; an account
  token lives until logout or `DELETE /devices/{id}`. A live finding, tracked in the client repo's
  checklist §4, and a separate slice. Deliberately not entangled with this one — the machinery here
  (a TTL, an expiry predicate in the `WHERE` clause, a sweep on a request-time trigger) is the second
  worked example of how that slice would look, which is the most this one owes it.
- **There is still no rate limit on `/auth/login`.** The only rate limit in this server is the
  per-User one on device-auth approve (ADR-0036). Same finding, same separation.
- **`?token=` is unchanged**, still honoured by exactly one route, and `requireAuthAllowQueryToken` is
  untouched. **The `ms_media` cookie is unchanged** — it serves the browser well and this takes
  nothing from it.

## Consequences

- **A feature flag, `streamToken`.** Clients branch on it, never on a version. It is load-bearing in a
  way most flags are not: a client offering an AirPlay button against a server without these routes
  fails at the moment the user's television goes black, which is not an error the app can catch and
  apologise for. An older server 404s the paths and omits the Decision fields, so an absent flag means
  "hide the affordance" and costs nothing else.
- **A second credential table, and the two namespaces never meet.** `stream_tokens` holds only a hash,
  like `device_auth` and like `auth_tokens` (ADR-0015). Nothing in the stream-token path reads
  `auth_tokens`, and nothing in `Authenticate` reads `stream_tokens` — so neither credential works on
  the other's surface, by construction rather than by check. `session_id` is not a foreign key
  (Playback sessions live in memory and are never persisted); `user_id` cascades, so deleting a User
  revokes their outstanding tokens.
- **The Decision carries the token.** `streamToken` + `streamTokenExpiresAt`, both `omitempty`, minted
  with the session so an AirPlay handoff is one request rather than two. A mint failure omits the
  fields rather than failing the negotiation — the session is still playable over the bearer or the
  cookie, and a client that can act on a Decision should get one.
- **An identity with a User and nothing else.** A stream-token request runs as its User: no Device
  (the fetching party never signed in) and no account token (that field is the raw bearer, and a
  stream token is not one). The invariant this rests on is that everything reachable from these routes
  keys on `User.ID` alone — anything needing a role, a Device, or the account token must not be
  reachable from here at all.
- **An access log that redacts by construction.** There was no request logging in this repo, so
  nothing leaked today and the whole risk was the logger somebody adds tomorrow. `api.LogRequests`
  prints a redacted path and never a query string — because the *other* URL-borne credential is the
  permanent `?token=` on the direct-file download. `api.RedactPath` is exported so that any future
  logger, proxy shim, or error reporter has one thing to call.
- **The routes are additive.** `/sessions/{id}/stream` and `/sessions/{id}/hls/{file}` are untouched
  and serve byte-identical bytes; the artifact dispatch is shared by both entry points rather than
  copied, so a new rendition rule cannot land on one URL shape and not the other.
