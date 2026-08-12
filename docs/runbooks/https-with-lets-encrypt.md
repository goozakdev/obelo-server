# Runbook: get an HTTPS certificate from Let's Encrypt

**When:** you reach your server from outside the house by forwarding a port, and
you want that hop encrypted without running a reverse proxy. Governing decision:
[ADR-0041](../adr/0041-native-tls-optional-alongside-plain-http.md).

**Precondition:** a domain name you control the DNS for, a router you can
configure, and a real public IPv4 address (step 0 checks the last one).

**What this does not change:** the plain-HTTP listener on `OBELO_LISTEN_ADDR`
keeps serving the LAN exactly as before, and mDNS discovery goes on working. No
public CA will certify `192.168.1.50` or a `.local` name, so HTTPS is an
*addition* covering the hop that leaves the house — never a replacement. Both
listeners serve the same API and a session created on one works on the other.

The mechanism, because it determines every step below: Obelo proves it controls
your domain with the **TLS-ALPN-01** challenge, which the CA validates by
connecting *back* to this server on **port 443**. Nothing uses port 80, so a
household forwards exactly one port.

## 0. Check you are not behind CGNAT

If the ISP does not hand out a real public IPv4, no port-forward can work and
nothing below will help.

```sh
curl -4 ifconfig.me          # your address as the internet sees it
```

Compare that with the WAN address on the router's status page. Different values,
or a WAN address in `100.64.0.0/10` (`100.64.` – `100.127.`), means carrier-grade
NAT. Ask the ISP for a public address before going on.

## 1. Pin the server's LAN address

Add a **DHCP reservation** for the machine running Obelo, so it always gets the
same LAN address (say `192.168.1.50`). A port-forward aimed at an address that
moves is the standard way this setup breaks a month after it was working.

## 2. Create the DNS record

A subdomain keeps the record clear of the apex's website and mail records:

```
Type: A    Name: media    Value: <public IPv4>    TTL: 300
```

Verify it before touching anything else — most first-attempt failures are just
this:

```sh
dig +short media.example.com @1.1.1.1     # must print your public address
```

**Wildcards are impossible here.** TLS-ALPN-01 cannot validate one (RFC 8737 §3
allows wildcards only over DNS-01, which ADR-0041 rules out), and every name you
want must be listed in full in step 4. `internal/config/config.go`
(`validateACMEDomain`) refuses a `*` entry at startup rather than letting
autocert silently ignore it.

**If your DNS is on Cloudflare, the record must be DNS-only (grey cloud).** With
proxying on, Cloudflare terminates TLS itself, the challenge never reaches this
server, and issuance fails permanently with nothing in the log pointing at the
cause.

## 3. Deal with a changing address

Residential addresses usually rotate. When yours does, the A record goes stale
and both access *and* automatic renewal stop — quietly, since renewal happens
weeks before expiry with nobody watching. Either turn on the router's DDNS client
or run a cron job that PATCHes the record through the registrar's API. Skip this
only with a static address.

## 4. Forward the port and start against **staging**

In the router: **external TCP 443 → `192.168.1.50:8443`**.

- The **external** port must be exactly 443. Let's Encrypt validates TLS-ALPN-01
  there and nowhere else; it is not configurable.
- The **internal** port is `OBELO_TLS_LISTEN_ADDR` — arbitrary, and above 1024 so
  Obelo needs no privileges.
- TCP only. Nothing listens on 80.
- If the host runs its own firewall, open the internal port there too
  (`ufw allow 8443/tcp`).

Then configure Obelo — **pointed at staging**, not production:

```sh
OBELO_TLS_MODE=acme
OBELO_TLS_DOMAINS=media.example.com
OBELO_ACME_EMAIL=you@example.com
OBELO_ACME_DIRECTORY=https://acme-staging-v02.api.letsencrypt.org/directory
OBELO_TLS_LISTEN_ADDR=:8443          # the default; shown because step 4 depends on it
OBELO_DATA_DIR=/data                 # must be durable — see "keep the cache" below
```

Production allows only **five failed authorizations per hostname per hour**, so
one wrong port-forward costs you the rest of the afternoon. Staging is generous,
and its certificates chain to an untrusted root, which is exactly what makes the
next step's browser warning a useful signal.

Under Docker add `-p 8443:8443`, or use `--network host` (which you already need
for mDNS discovery, and which makes the published-port flag unnecessary).

Restart. The boot log states the whole configuration in one line:

```
obelo: ACME enabled for [media.example.com] via https://acme-staging-v02.api.letsencrypt.org/directory
(cache: /data/acme) — certificates are obtained on the first HTTPS handshake for a listed name,
over TLS-ALPN-01 on :8443; port 80 is not used and does not need forwarding
```

## 5. Trigger issuance from outside the house

**Nothing is fetched at boot.** Issuance is driven by the first HTTPS handshake
for a listed name (`acmeCertificates.GetCertificate` in `cmd/obelo/tls.go`
explains why: a boot-time attempt races the listener the CA has to dial back
into, and burns one of the five failed authorizations on every restart of a box
whose DNS is not pointed yet).

So make that handshake happen, from **cellular rather than house wifi** — plenty
of routers will not hairpin a connection from inside back to your own public
address:

```
https://media.example.com
```

**The browser will warn that the certificate is untrusted. That warning is the
success signal** — it is the staging root. Confirm in the log:

```
obelo: HTTPS certificate ready for "media.example.com" from https://acme-staging-v02… (…)
```

A failure instead logs `WARNING: no HTTPS certificate for …` with the reason,
once a minute with a suppressed count (an internet-facing port takes continuous
host-policy rejections from scanners, and a line each would bury the one failure
that is about your domain). In order of likelihood: DNS not propagated, the
port-forward aimed at a stale LAN address, or the name missing from
`OBELO_TLS_DOMAINS`. From any machine outside the network, this shows how far a
connection actually gets:

```sh
openssl s_client -connect media.example.com:443 -servername media.example.com </dev/null
```

**A certificate that cannot be obtained never stops the server.** Obelo boots,
keeps serving plain HTTP on the LAN, logs the reason, and retries on the next
handshake. A CA outage is not the operator's typo, and ADR-0041 declines to trade
the LAN for one.

## 6. Switch to production

1. Stop Obelo.
2. **Empty the cache**: `rm -rf /data/acme/*`. This step is not optional and it is
   the one people skip. autocert keys its cache by domain name alone — see
   `certKey.String()` in `x/crypto/acme/autocert`, which records nothing about
   which CA issued what — so a leftover staging certificate for
   `media.example.com` keeps being served against production and it looks as
   though the switch did nothing. Discarding the account key with it is harmless.
3. Remove `OBELO_ACME_DIRECTORY` entirely; the default is Let's Encrypt
   production.
4. Start, and load the URL from outside once more.

## Verifying it took

A padlock with no warning, and a log line naming the production directory:

```
obelo: HTTPS certificate ready for "media.example.com" from https://acme-v02.api.letsencrypt.org/directory (…)
```

The line is logged **once per name**, on the first successful handshake — if you
are looking for it after the fact, grep the boot logs rather than expecting it to
repeat. To read the served chain directly:

```sh
openssl s_client -connect media.example.com:443 -servername media.example.com </dev/null 2>/dev/null \
  | openssl x509 -noout -issuer -subject -dates
```

## Afterwards

- **Keep the cache.** `OBELO_DATA_DIR/acme` (mode `0700`) holds the ACME account
  key and every issued private key. A container without a volume, or a tmpfs,
  re-issues on every restart and hits the CA's duplicate-certificate limit within
  days. It is also the most sensitive directory the server owns: anyone who can
  read it can impersonate this server to every client in the house. Obelo warns
  at boot if its mode is looser than `0700`.
- **Renewal is automatic** and needs nothing scheduled. `OBELO_ACME_EMAIL` is what
  gets you an expiry warning from the CA if it ever silently stops working —
  which, per step 3, is most often a changed public address.
- **The LAN still uses plain HTTP** (`http://192.168.1.50:8080`), by design.
- **Do not put a reverse proxy in front of the TLS port in `acme` mode.** It would
  terminate TLS and the ALPN challenge would never arrive. A proxy deployment
  wants `OBELO_TLS_MODE=files` or `off`, plus `OBELO_TRUSTED_PROXIES`.
- **Adding a name later** (a `www.`, a second household name) means appending it
  to `OBELO_TLS_DOMAINS` and restarting. It gets its own certificate on its first
  handshake; the existing one is untouched.
