# LAN discovery via mDNS; remote access and TLS via operator's reverse proxy

> **Amended by [ADR-0034](./0034-server-identity-and-mdns-advertisement.md)**, which implements
> the discovery half below — it went unbuilt for a long time while this ADR read as though it
> had shipped. ADR-0034 adds the **Server identity** the advertisement needed and the
> `_juicebox._tcp` responder. The remote-access and TLS postures below stand unchanged.
>
> **Still unbuilt:** *"The server provides a configurable external URL"* — there is no
> `JUICEBOX_EXTERNAL_URL`; absolute-URL correctness rests entirely on forwarded headers. That
> may well be sufficient, in which case the claim should be **retired** rather than implemented.
> Do not read it as a description of the server.

LAN discovery: the server advertises itself via mDNS/Bonjour for zero-config pairing on Apple clients, with manual address entry as the fallback.

Remote access: no built-in tunneling or relay (consistent with [ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md)). The operator exposes the server themselves. The server provides a configurable external URL and behaves correctly behind a reverse proxy: it trusts forwarded headers and emits correct absolute URLs (notably in HLS playlists).

TLS: in v1 the server speaks plain HTTP and assumes a reverse proxy terminates TLS. Native TLS termination (managing its own certificates) is a planned later addition, not built now.

## Consequences
- v1 must get forwarded-header handling and absolute-URL generation right, or remote HLS playback breaks.
- We ship reverse-proxy / dynamic-DNS setup docs rather than networking code.
- Adding native TLS later is additive and should not disturb the proxy path.
