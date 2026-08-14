# Fully self-hosted, no vendor account or relay

> **Amended by [ADR-0043](./0043-tailnet-remote-access-via-embedded-tsnet.md)** — the optional
> tailnet path. It does not weaken the rule; it writes down the reading that lets a tailnet sit
> inside it, because the sentence below already lists **VPN** as one of the operator's own
> networking options and a reader could reasonably conclude either way.
>
> **The coordination server is a third party**, and the tailnet path does not work without one. It
> qualifies under the same exemption ACME took in
> [ADR-0041](./0041-native-tls-optional-alongside-plain-http.md): optional, off by default, holds
> no account and no content, and the server runs fully — catalog, playback, accounts, LAN — with
> it unreachable or never configured. It must never become the default and must never be able to
> stop a boot.
>
> **DERP is the sharp edge, and it is a relay.** When NAT traversal fails, WireGuard traffic falls
> back to Tailscale-operated DERP servers, so media bytes can cross a third party's infrastructure
> — the shape the sentence below refuses. The line held is narrow and worth stating precisely:
> *we* provide no relay, and the operator may choose to use somebody else's. It is documented
> rather than hidden, including the practical symptom (DERP is throttled, so the failure looks
> like a stuttering 4K stream and nothing in this server can see or report why).
>
> **The escape hatch is a Control URL setting**, empty by default. An operator who will not accept
> a vendor coordination server points it at their own (Headscale). One text field is what keeps
> this ADR defensible rather than merely argued around. A custom DERP map — the remaining
> Tailscale-operated component — was considered and deliberately not shipped; see ADR-0043.

The server depends on no third-party service the operator does not control. Accounts and authentication live entirely in the server's own database — there is no cloud login. Remote access is reached directly via the operator's own networking (domain / dynamic DNS / VPN / reverse proxy); there is no vendor-operated relay.

The server must function **fully offline**. External metadata enrichment (cover art, descriptions, cast) from public sources is **optional** and read-only: if the server has no internet access, everything still works, just with sparser metadata.

## Consequences
- We own identity, session management, and authorization — no delegating to an external IdP.
- Remote access is the operator's responsibility; we provide no NAT-punching relay. We may help with reverse-proxy/TLS guidance but do not host infrastructure.
- The metadata subsystem must treat external sources as a degradeable enhancement, never a hard dependency.
