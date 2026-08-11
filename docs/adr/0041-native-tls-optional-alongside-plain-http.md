# Native TLS is optional, and runs alongside the plain-HTTP listener

The server can terminate TLS itself. It is **opt-in and off by default**, and when it is on the HTTPS listener is an **addition**, not a replacement: the plain-HTTP listener keeps serving on `ListenAddr` exactly as before.

Two modes, selected by `OBELO_TLS_MODE`:

- **`files`** — an operator-supplied certificate and key, from any CA (including their own). Reloaded from disk without a restart, so a renewal does not silently start serving an expired chain.
- **`acme`** — a certificate obtained automatically from an ACME CA (Let's Encrypt by default) using the **TLS-ALPN-01** challenge, which completes on the TLS port itself.

This amends [ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md), which said the server speaks plain HTTP and assumes a reverse proxy terminates TLS, and named native termination "a planned later addition". The reverse-proxy deployment stays fully supported; it is no longer the only way to get TLS.

## Why

A reverse proxy was a reasonable v1 answer while remote access meant "the operator already runs nginx". It stopped being sufficient for the household that simply forwards a port: that deployment gets no TLS at all, so passwords, bearer tokens, and the `ms_media` cookie cross the internet in cleartext — and the cookie's `Secure` flag is deliberately off on plain HTTP (`internal/api/cookie.go`), so it is sent in the clear by design.

A proxy also *adds* one exposure it cannot remove. `GET /files/{id}/download` carries the account token in the query string ([ADR-0005](./0005-discovery-and-tls-via-reverse-proxy.md)'s accepted tradeoff, redacted by our own access log in `internal/api/logging.go`) — but nginx or Caddy in front will log the full request line by default, writing that credential to a file we do not control. Terminating TLS in-process removes that intermediary. It does not make the token-in-URL acceptable; that has its own fix (give the route the path-carried, session-scoped credential of [ADR-0039](./0039-scoped-expiring-media-credential-for-delegated-fetches.md)) and the two are independent.

**HTTPS does not replace the plain-HTTP listener, because a public CA cannot certify the LAN.** A publicly-trusted certificate is issued for a DNS name; LAN clients reach this server at a raw address or through the `_obelo._tcp` mDNS advertisement of [ADR-0034](./0034-server-identity-and-mdns-advertisement.md), which publishes A/AAAA records and a `.local` host name. No CA will issue for `192.168.1.50` or for `.local`. The alternatives are all worse than keeping the HTTP listener:

- **Split-horizon DNS** (an internal resolver pointing the public name at the LAN address) makes one certificate work everywhere and is the right answer for anyone who can run it — but it requires the household to operate DNS, which most cannot.
- **NAT hairpinning** works on many routers and fails silently on others; a product cannot rest on it.
- **A self-signed certificate or private CA** moves the cost onto every device. On Apple platforms the profile must be installed *and* separately enabled under Settings → General → About → Certificate Trust Settings, and Apple caps leaf validity at 398 days, so it is a recurring chore on every phone, tablet, and Apple TV in the house. That is a permanent support burden traded for an encryption the LAN threat model does not ask for.

So the LAN keeps the transport it has, discovery keeps working untouched, and TLS is applied to the hop that actually crosses hostile ground.

**TLS-ALPN-01 rather than HTTP-01** because HTTP-01 requires port 80 to be reachable, which means a second listener and a second port-forward for a household that only wanted one. TLS-ALPN-01 completes on the TLS port. DNS-01 is not implemented: it needs a per-provider API credential, which is a vendor dependency of exactly the kind [ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md) resists, for a case split-horizon DNS already serves.

**ACME is compatible with [ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md), and that is a deliberate reading rather than an oversight.** Let's Encrypt is a third party, but it is not a relay, it holds no account or content, `files` mode reaches the same result with any CA the operator chooses, and the server runs fully without either. It is a degradeable enhancement in the same sense metadata enrichment is. It must therefore never be the default, and a failure to obtain a certificate must not stop the server from booting and serving the LAN.

## Consequences

- **Two listeners, one handler.** Both serve the same `http.Handler`; nothing in the request path learns which one it arrived on except through `r.TLS`. Graceful shutdown covers both.
- **`X-Forwarded-Proto` must stop being trusted unconditionally.** `internal/api/cookie.go` reads it to decide the cookie's `Secure` flag. That was safe only while a proxy was assumed; once clients reach the server directly, the header is attacker-supplied. It must prefer `r.TLS` and consult the header only where an operator has declared a trusted proxy — the same setting that would let `clientIP` (`internal/api/middleware.go`) stop collapsing the login limiter and the device-code quota into one global budget behind a proxy.
- **HSTS stays off**, for the reason given when the security headers landed: it is sticky per hostname, and the plain-HTTP LAN path still exists. It becomes correct only for a deployment that is HTTPS-only, which this ADR does not create. `upgrade-insecure-requests` stays out of the CSP for the same reason.
- **Discovery must say which scheme it is advertising**, or a client that finds the server by mDNS cannot know whether to speak TLS to the advertised port. This is an [ADR-0034](./0034-server-identity-and-mdns-advertisement.md) amendment, not a change here.
- **HTTP/2 arrives for free** on the TLS listener, which suits HLS: many small segment fetches multiplex over one connection instead of queueing against the per-origin limit. The plain-HTTP listener stays HTTP/1.1.
- **A certificate is now a thing that can expire in production.** `files` mode must reload from disk — a renewal rewrites the files and a server that read them once at boot will serve a dead chain until somebody restarts it. `acme` mode owns its own renewal.
- **The dependency cost is near zero**: `golang.org/x/crypto` is already a direct dependency, so `acme/autocert` adds its `acme` package plus `x/net` (already indirect) and `x/text`.
- This says nothing about Tailscale. A tailnet would supply a name and a certificate that work identically on the LAN and remotely, which is a strictly better answer to the problem above — but it is an optional feature and cannot be the only path to TLS.
