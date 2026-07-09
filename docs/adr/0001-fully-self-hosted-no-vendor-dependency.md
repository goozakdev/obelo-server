# Fully self-hosted, no vendor account or relay

The server depends on no third-party service the operator does not control. Accounts and authentication live entirely in the server's own database — there is no cloud login. Remote access is reached directly via the operator's own networking (domain / dynamic DNS / VPN / reverse proxy); there is no vendor-operated relay.

The server must function **fully offline**. External metadata enrichment (cover art, descriptions, cast) from public sources is **optional** and read-only: if the server has no internet access, everything still works, just with sparser metadata.

## Consequences
- We own identity, session management, and authorization — no delegating to an external IdP.
- Remote access is the operator's responsibility; we provide no NAT-punching relay. We may help with reverse-proxy/TLS guidance but do not host infrastructure.
- The metadata subsystem must treat external sources as a degradeable enhancement, never a hard dependency.
