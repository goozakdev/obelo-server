# An Online source is browsed and played live and the Server keeps none of it

[ADR-0057](./0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
decision 1 says the set of Extension points grows by ADR. This ADR adds one: the **Online
source provider**, a Plugin that lets a User browse and watch something outside the household
— YouTube is the example, PeerTube and the Internet Archive are the first two real ones —
without any of it becoming part of the catalog. It decides how that squares with
[ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md), how the bytes reach a
client, what the Server is and is not allowed to keep, who may see a source, and what an
Online source shares with the rest of the product (almost nothing). The terms are in
`CONTEXT.md`: **Online source**, **Online source provider**, **Online row**, **Online item**.

## Decisions

**1. An Online source does not violate ADR-0001.** It is optional and off by default, and the
Server runs fully — catalog, playback, accounts, LAN — with none enabled or reachable. This is
the same exemption ACME took ([ADR-0041](./0041-native-tls-optional-alongside-plain-http.md))
and the tailnet path took ([ADR-0043](./0043-tailnet-remote-access-via-embedded-tsnet.md)).

**2. The Plugin resolves; the Server plays.** The contract is `rows()` returning
`[{id, label, items[]}]`, `row(id, cursor)` returning the next page (by opaque cursor, a deliberate departure from
[ADR-0057](./0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
decision 2's offset paging, because remote sources page by tokens), `search(query)`
returning items, and `resolve(itemId, capability hints)` returning playable variants. The
Plugin only turns an Online item into URL(s). The Server plays it through its normal
negotiation: where the client can play the format the bytes are relayed untouched, otherwise
ffmpeg transcodes. The Playback ceiling, the transcode cap
([ADR-0009](./0009-transcode-governance.md)) and stream tokens apply to every client
exactly as they do to a Title. A client never fetches from the source directly.

**3. The contract's shapes.** An **Online item** is `{id, title, thumbnail URL, duration}`
plus an optional description and an optional publish date. The Server caps the row count, the
item count and label length, drops malformed entries (including a row or item id outside a URL-safe character set), and
accepts only https
thumbnail and media URLs. The only item kind in v1 is a single video: channels and playlists
are rows, there are no series and no audio-only items.

**4. `resolve()` returns a list of variants.** Each is one of: *muxed*
`{url, container, codecs, resolution}`, *split* `{videoUrl, audioUrl, codecs, resolution}`, or
*manifest* `{HLS/DASH url}`; each carries the request headers it requires (referer, user
agent). The Server chooses using existing negotiation and the Playback ceiling: a muxed
variant the client can play is relayed; a split variant, a manifest that needs transcoding, or
anything over the ceiling goes to ffmpeg. The Plugin may receive capability hints (for
example a maximum height).

**5. Nothing is persisted.** Bytes in flight — transcode segments in the existing cache — are
allowed and cleaned at session end. Catalog data is never stored: answers come from live
calls, with at most a short in-memory cache that a restart clears, and thumbnails are proxied
and never written to disk. There is **no per-User state** in v1: no watch state, no resume
position, no history, and an Online item never appears in Continue Watching.

**6. Access is granted per User, and never to a rated User.** An Admin enables a source
server-wide, then grants it to each User the way a Library is granted; Admins always see it.
A User with **any** Rating ceiling cannot be granted a source. Setting a ceiling on a User who
holds source grants is allowed and removes those grants automatically; the confirmation names
the sources removed, and removing the ceiling later does not restore them. Losing a grant
either way ends that User's live Online sessions for the source at once. Linked servers (the Remote
role) never see sources and the Export never carries them.

**7. A source is independent of the catalog.** No global search, no Collections, no
Playlists, no Continue Watching, Up Next or Recently Added; an Online item can never be added
to a stored Playlist or Collection and never travels over a Link. Search exists only on the
source's own page. The home screen shows **one tile per granted source**; selecting it opens
the source page with its rows and its search. Loading the home screen never calls a Plugin.

**8. Settings are server-wide only.** An Admin enters them once (an API key, an instance, a
region). There are no per-User third-party accounts and no per-User OAuth in v1.

**9. The media fetch posture is first-URL-only.** The Server checks the *first* resolved URL:
https, a host matching the manifest's network allowlist, and not resolving to a private or
loopback address. The allowlist gains a **domain-suffix form** (for example
`.googlevideo.com`) usable for media hosts. ffmpeg then fetches directly. Redirects and
HLS/DASH segment hops are **not** re-checked. Every ffmpeg run for an Online item is given
`-protocol_whitelist https,tls,tcp,crypto`, which allows only https, tls, tcp and crypto and
blocks every other protocol (`file:`, plain http, `data:`, rtmp, udp, concat, pipe and the rest).

A **relayed** play is different: there the Server fetches the upstream bytes itself through
`safefetch`, so every redirect gets the same https and private-address check. Only the ffmpeg
path is first-URL-only.

*Residual risk, accepted and documented:* on the ffmpeg path, after the first URL, a redirect
or a manifest hop may point at an https address on the LAN, and ffmpeg will fetch it. The
first-URL check itself is made on the Server's own DNS lookup and ffmpeg resolves again, so a
rebinding DNS answer can pass the check and still send ffmpeg to the LAN.

> **Amended (settings URL, 2026-10-08):** the host of the URL an Admin enters in an Online source's settings is also allowed — for the Plugin's own fetches, as ADR-0058 already allows an operator-typed host, and as an exact media host for the first-URL check, still https-only and never private or loopback. This lets a Plugin such as a PeerTube source target the instance the Admin chose without listing every instance in its manifest.

**10. A resolved URL is re-resolved once, on failure.** `resolve()` runs once per playback
session. On a 403 or 410 from the media host the Server re-resolves once and continues from
the current position: a relay makes a new upstream request, and ffmpeg restarts at the
position, as stream switching does ([ADR-0025](./0025-selectable-video-streams-in-container-restart-switch.md)).
If re-resolving fails the session ends with "This video is no longer available from
{source}". There is no timed refresh.

**11. Plays are visible to the Admin and silent to sinks.** An Online item play appears on the
Admin sessions and transcoding page ([ADR-0029](./0029-transcoding-observability-admin-surface.md)),
labelled with the source and the item title. It emits **no** Event sink events in v1.

**12. The tile icon is a package member.** A Plugin package may carry an optional `icon.png`.
The signature covers it when present. It must be PNG, square and at most 64 KiB, checked at
install, and is served from the Server's own origin. With no icon the tile is generic and
shows the source's name.

**13. Playback is in the shell-owned player.** An Online item plays in the persistent player
([ADR-0018](./0018-persistent-shell-owned-player.md)). Playing one replaces the Queue with
that single item. Online items and Titles are never mixed in a Queue; there is no
play-row-from-here and no autoplay in v1; and Online items have no Markers, lyrics or
subtitles in v1.

**14. Failure is visible and local.** The tile is always shown while a source is enabled and
granted. If the source page fails to load it shows "{source} isn't responding" and a retry; a
failed search shows the same message in place of results. A disabled Plugin (crashed or
turned off by an Admin) hides the tile from Users and shows the reason on the Admin plugin
screen. `rows()` and `row()` answers are cached in memory for 5 minutes per source, shared
across Users; `search` is never cached; a restart clears everything.

**15. Names.** Online source, Online source provider (the Extension point), Online row, Online
item. Channel, Remote source, Streaming service, Virtual library and Section are avoided.

**16. v1 is the server API plus the web app.** Android (`../obelo-android`), iOS and tvOS
(`../obelo-apple`) each get their own issue, written now as ready-for-human and blocked by the
server and web slices being tested. A client that does not request
sources simply shows no tiles. There is **no Bundled online source**: PeerTube and Internet
Archive are two reference plugins, each in its own sibling repository (`../obelo-plugin-peertube`,
`../obelo-plugin-internetarchive`, modelled on `../obelo-plugin-discord`), and YouTube is left to
third parties.

## Why

ADR-0001's objection is to a dependency the Server cannot run without. An Online source is
the opposite: nothing starts, scans, plays or signs in because of it, and an operator who
never enables one never talks to anyone.

The Rating ceiling exclusion follows from the existing rule that an unrated thing is visible
(`internal/access/rating.go:54-62`). An open catalog is unrated by construction, and nobody
in the household curated it, so granting it to a rated User would expose everything the
ceiling exists to hide. Plugin-declared ratings would hand the Server's judgment to the
Plugin, against ADR-0057 decision 3.

There is no per-User state because watch state is keyed to a parsed identity
([ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md)) and an Online item has none:
asking for it again is asking the source again.

The Server stays in the byte path so that the Playback ceiling, transcode governance and
stream tokens hold for every client with no per-client exception. The host owns judgment
(ADR-0057 decision 3): the Plugin supplies URLs and data, and the Server decides whether to
fetch, how to play and who may see.

### Considered

- **Direct client fetch.** The client would fetch the resolved URL itself. Rejected: it
  bypasses the Playback ceiling, the transcode cap and stream tokens, and leaves format
  support to each client.
- **Resume position only.** Keep a position per Online item and nothing else. Rejected in
  favour of no per-User state at all in v1.
- **Plugin-declared per-item ratings.** Rejected for the reason above; a rated User is simply
  never granted a source.
- **Per-User third-party accounts.** Rejected for v1; settings are server-wide.
- **Host-side fetching with every hop checked.** The orchestrator's recommendation: the
  Server would fetch the media itself and apply the https, allowlist and private-address
  checks to every redirect and every HLS/DASH segment, closing the residual risk in decision
  9. The maintainer chose against it and for first-URL-only, accepting that a later hop can
  reach an https address on the LAN.
- **A manifest-embedded icon.** The orchestrator's recommendation. The maintainer chose an
  `icon.png` package member covered by the signature instead.

## Consequences

- The Plugin package layout in [ADR-0058](./0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
  gains an optional `icon.png`, covered by the signature when present.
- The manifest network allowlist gains a domain-suffix form for media hosts.
- The Server, not the guest, fetches media, and ffmpeg fetches directly under the protocol
  whitelist; the first-URL-only residual risk is documented, not closed.
- ADR-0057's set of Extension points grows by one, and ADR-0001 gains an amendment.
- `CONTEXT.md` gains Online source, Online source provider, Online row and Online item, and
  lists Online source provider under Plugin and Extension point.

## Back-pointers

[ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md) (amended: the Online source
exemption), [ADR-0057](./0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
(the set of Extension points grows; cursor paging) and
[ADR-0058](./0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
(`icon.png`, the domain-suffix allowlist, media fetching) each carry a pointer back here.
