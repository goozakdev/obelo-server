# LAN discovery via mDNS; remote access and TLS via operator's reverse proxy

> **Amended by [ADR-0034](./0034-server-identity-and-mdns-advertisement.md)**, which implements
> the discovery half below — it went unbuilt for a long time while this ADR read as though it
> had shipped. ADR-0034 adds the **Server identity** the advertisement needed and the
> `_juicebox._tcp` responder. The remote-access and TLS postures below stand unchanged.
>
> **Retired 2026-07-15 — *"The server provides a configurable external URL"*.** Never built, and
> the design does not need it: **the server emits no absolute self-referential URL anywhere.** HLS
> playlists reference bare session-relative filenames (`index.m3u8`, `subs_003.vtt`), and every
> URL-shaped API field — `streamUrl`, `subtitles[].url`, the artwork URLs — is root-relative
> `/api/v1/...`. A relative URI resolves against the origin the client already used, so a proxied
> client stays on the proxy's origin with nothing for the server to know or configure. Nothing
> reads the server's own host: no `r.Host` on any URL path, no `url.URL` construction, no
> redirects. `X-Forwarded-Proto` is read in exactly one place — the cookie `Secure` flag
> (`internal/api/cookie.go`). The claim is struck from the remote-access posture below, along with
> the consequence that rested on it.
>
> This assumes the server owns the **origin root**, which the embedded-SPA design
> ([ADR-0012](./0012-react-spa-embedded-in-binary.md)) already requires independently:
> `web/vite.config.ts` sets no `base`, the built bundle references `/assets/...`, and
> `internal/webui/webui.go` registers `/api/v1` and `/` as literal mux patterns with no
> `StripPrefix`. Mounting at a subpath (`https://example.com/juicebox/`) is therefore unsupported
> at all three layers — build, SPA runtime, and routing. Subdomains are unaffected and are the
> expected deployment. Supporting a subpath would be a real feature (a vite `base`, a rebuilt
> bundle, prefix-aware routing, a prefix threaded through every DTO builder), not the config knob
> the retired claim implied — which is the other reason not to revive it as one.

LAN discovery: the server advertises itself via mDNS/Bonjour for zero-config pairing on Apple clients, with manual address entry as the fallback.

Remote access: no built-in tunneling or relay (consistent with [ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md)). The operator exposes the server themselves, on the root of an origin it owns. The server behaves correctly behind a reverse proxy by emitting only relative URLs — notably in HLS playlists — so it never needs to know the origin clients reach it on. It trusts `X-Forwarded-Proto` for the one decision that does turn on the original scheme: the session cookie's `Secure` flag.

TLS: in v1 the server speaks plain HTTP and assumes a reverse proxy terminates TLS. Native TLS termination (managing its own certificates) is a planned later addition, not built now.

## Consequences
- Relative URLs make remote HLS playback correct for free — there is no absolute-URL generation to get wrong. This is why the external-URL claim was retired unbuilt rather than implemented.
- The server must be mounted at the root of its origin; a subpath deployment is unsupported.
- We ship reverse-proxy / dynamic-DNS setup docs rather than networking code.
- Adding native TLS later is additive and should not disturb the proxy path.
