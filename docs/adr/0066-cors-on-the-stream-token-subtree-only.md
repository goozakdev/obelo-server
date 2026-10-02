# CORS on the stream-token subtree, and nowhere else

Every response under `/api/v1/stream/…` — the routes of [ADR-0039](./0039-scoped-expiring-media-credential-for-delegated-fetches.md) —
carries CORS headers, and answers `OPTIONS` and `HEAD`. No other route in this API sends any
`Access-Control-*` header.

## The gap

A Google Cast receiver (the Default Media Receiver is the motivating client) is a web page on a
foreign origin. It fetches the playlist, the segments and the progressive bytes itself, with
`fetch`/XHR or a CORS-mode media element, so the browser withholds every response that does not
grant that origin a read — including the refusals, which is how a receiver's script would tell a dead
token from a network error. Before this decision the subtree sent no CORS header at all and answered
`OPTIONS` with the same `405` as `POST`, so every cross-origin read failed even though the requests
reached the server and were answered `200`/`206`. ADR-0039 already named a Chromecast-style receiver
as a member of the class of player that hands a URL to somebody else; this is what that receiver needs
on top of the token.

## Decision

1. **Scope is exactly the subtree `handleStreamTokenSubtree` owns**, including its relay branch
   (ADR-0056 §5). Not `/sessions/…`, not `/relay/…`, not `/titles/…`, not artwork, not `/events`, not
   any JSON route.
2. **Headers on every response of the subtree**, set before the method gate and the token are
   examined, byte-identical whatever the token: `Access-Control-Allow-Origin` = the request's `Origin`
   **echoed verbatim** (omitted when there is no `Origin`), `Vary: Origin`, and
   `Access-Control-Expose-Headers: Content-Length, Content-Range, Accept-Ranges`.
3. **The `Origin` is echoed, never `*`, and credentials are never allowed.** Google documents that
   `*` cannot be used for Cast, and for a non-credentialed response an echo grants exactly what `*`
   would. `Access-Control-Allow-Credentials` is never sent: these routes honour no cookie and no
   bearer, so a credentialed cross-origin read is impossible by construction and stays so.
4. **The preflight is answered in the method gate's position**, before the token is examined:
   `OPTIONS` on any path under `/stream/` is `204 No Content`, empty body, the headers above plus
   `Access-Control-Allow-Methods: GET, HEAD`, `Access-Control-Allow-Headers: Content-Type,
   Accept-Encoding, Range` and `Access-Control-Max-Age: 3600` — identically for a live, dead or
   malformed token. This is ADR-0039's validity-oracle argument applied to a new method: an answer
   that differs by token confirms which tokens are real, so `OPTIONS` must not become the oracle
   `POST` was prevented from being.
5. **Every other non-GET/HEAD method stays `405`**, with `Allow: GET, HEAD`, refused before the token,
   and now carrying the headers so a receiver's script can read the status. Refusals stay the one
   byte-identical `404` envelope with no `WWW-Authenticate`; CORS is added to it and nothing else
   changes.
6. **`HEAD` is admitted in the same gate position (S2)** and then runs exactly as `GET` — token check,
   dispatch, then `http.ServeContent` / the HLS handlers answer headers with no body. A dead token's
   `HEAD` gets its `GET`'s `404`. On the relay branch the `HEAD` is fetched from the sharer as a `GET`
   with the body dropped, because the sharer's media routes are GET-only; so a relay `HEAD` on
   `/relay/{id}/…` now answers `200` via an upstream `GET` (D39-2), where it was a `405` before. Whether the Default Media
   Receiver ever sends `HEAD` is unverified (it needs a real receiver); it costs nothing to answer.
7. **Not taken: S3 and S4** of the Cast server-change list — a token-reachable WebVTT route, and a
   `CODECS` attribute on MPEG-TS master variants. Neither is needed to play; HLS masters already carry
   `subs_*` WebVTT renditions reachable under the token. They can be decided separately if a real
   receiver shows it needs them.
8. **No feature flag.** The headers are additive and harmless to every existing client.

## Why bearer routes gain nothing and cookie routes must never get it

A browser attaches the `ms_media` cookie to a media request **ambiently**: any page the user visits
can make the request and the browser adds the cookie. Today the same-origin policy is what stops that
page reading the response. An echoed `Origin` together with `Access-Control-Allow-Credentials: true`
on a cookie route would lift that protection, and any web page the user visits could read their media
and their `/events` stream cross-site. So the cookie routes (`/sessions/{id}/hls/…`, artwork,
`/events`, …) must never answer CORS, and a future "tidy-up" that puts CORS in a shared middleware is
exactly the change this ADR forbids. Bearer routes gain nothing: no browser attaches an
`Authorization` header on its own, so a cross-origin page has no bearer to send, and the clients that
do hold one are not browsers on a foreign origin. The stream-token subtree is different because the
credential is **in the URL**: whoever holds the URL already holds the grant, an echoed `Origin` adds no
authority to it, and that is the whole reason a receiver can be handed it.

## Conflicts with earlier ADRs

None. ADR-0039's "GET only" posture is widened to GET and HEAD (neither mutates anything); its
"method then credential" order is kept and now covers `OPTIONS`.

## Consequences

- One small helper (`setStreamTokenCORS`) is the only place in the server that writes
  `Access-Control-*`; a test asserts that the bearer, cookie, JSON and artwork routes send none.
- `mux.HandleFunc("/stream/", …)` must stay method-less: a `"GET /stream/"` pattern makes Go's
  `ServeMux` answer `OPTIONS` with its own `405` before the gate runs.
