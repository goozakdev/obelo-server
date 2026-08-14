# Runbook: reach your server from anywhere with Tailscale

> **STATUS: NOT BUILT YET.** This describes the feature decided in
> [ADR-0043](../adr/0043-tailnet-remote-access-via-embedded-tsnet.md) and specified in
> `.scratch/tailscale/PRD.md`, written alongside the decision so the operator-facing shape was
> argued before the code. **Nothing below works today.** Delete this banner when phases 1–4 land,
> and check every step against what actually shipped rather than trusting this file — ADR-0034
> records exactly how much damage a doc that reads like a kept promise can do.

**When:** you want to watch your library from outside the house and you would rather not forward a
port, buy a domain, run dynamic DNS, or expose the server to the internet at all. Governing
decision: [ADR-0043](../adr/0043-tailnet-remote-access-via-embedded-tsnet.md).

**Precondition:** a Tailscale account (the free tier is ample for a household), an Obelo build with
the feature compiled in, and **the willingness to install Tailscale on every device that will play
media** — the phone, the tablet, the Apple TV, the laptop. That last one is the whole cost of this
path. If it is unacceptable, use [Let's Encrypt and a port-forward](./https-with-lets-encrypt.md)
instead; both paths are supported and neither replaces the other.

**What this does not change:** the plain-HTTP listener on `OBELO_LISTEN_ADDR` keeps serving the LAN
exactly as before, mDNS discovery goes on working, and any HTTPS you configured under
[ADR-0041](../adr/0041-native-tls-optional-alongside-plain-http.md) is untouched. This is a third
listener, never a replacement. A session created on one works on the others.

**No port-forward is involved at any point.** If a step below has you opening the router's admin
page, you are on the wrong runbook.

## 0. Check your build has it

```sh
curl -s http://192.168.1.50:8080/api/v1/server | grep -o '"tailscale":[a-z]*'
```

`"tailscale":true` means the feature is compiled in. `false` — or nothing — means you are running a
build without it (see ADR-0043: the official Docker image and releases carry it; a plain
`go build` from source does not, unless you pass `-tags tailscale`).

## 1. Create the tailnet

Sign up at [tailscale.com](https://tailscale.com) if you have not. The account *is* the tailnet;
there is nothing else to create. Note the tailnet's name in the admin console — it looks like
`tail1a2b.ts.net` and it forms half of your server's future address.

## 2. Connect the server

In the Obelo web UI: **Settings → Remote access → Connect**.

A login link appears within a few seconds. Open it, approve the machine in your Tailscale account,
and the panel switches to connected and shows the server's address:

```
http://obelo.tail1a2b.ts.net
```

**`http`, not `https`, and that is correct here** — the tailnet is already encrypted end to end by
WireGuard, and the certificate that gets you the padlock is opt-in in step 5. The name is the part
that matters now.

That address is now permanent. It does not change when your ISP changes your IP, when the router
reboots, or when the server's DHCP lease moves.

**Headless / compose-first installs** can skip the click by minting an auth key in the Tailscale
console (Settings → Keys) and passing it in the environment:

```yaml
environment:
  OBELO_TAILSCALE_ENABLED: "true"
  OBELO_TAILSCALE_AUTHKEY: "tskey-auth-..."   # consumed at join, never stored
```

These variables **seed the settings on first boot only**. Afterwards the web UI is authoritative
and editing them changes nothing — the same handoff the metadata provider keys use.

## 3. Turn off key expiry for this node — do not skip this

Tailscale expires a machine's membership after **180 days** by default. When it lapses, remote
access stops. Nothing local breaks, nothing logs an error on the client, and it happens roughly six
months after the last time you thought about any of this.

In the Tailscale admin console: **Machines → your Obelo server → ⋯ → Disable key expiry.**

Obelo will warn you in the settings panel and in its log as the date approaches, and will show you
a fresh login link if it lapses — but the fix lives in the console, not here.

## 4. Install Tailscale on the devices

Install the Tailscale app on every device that will reach the server, and sign each one into the
**same account**. iOS, iPadOS, tvOS, macOS, Windows, Android and Linux are all covered.

There is no per-device configuration beyond signing in. A device on the tailnet reaches
`obelo.tail1a2b.ts.net`; a device not on it does not, which is the point.

## 5. Turn on HTTPS for the tailnet — recommended, not optional

This step used to be described as optional cosmetics. It is not. **Do it.**

The tailnet is already encrypted end to end — that is what WireGuard does — so this is not about
confidentiality. Three other things hang off it:

- **HTTP/2, which is a real speed difference.** HTTP/2 is negotiated during the TLS handshake, so
  **plain HTTP can never have it** — there is no cleartext path to it that browsers or the Apple
  clients speak. With HTTPS on, the tailnet multiplexes many small requests over one connection;
  without it, they queue against the browser's per-origin connection limit. A poster grid or an HLS
  playlist is dozens of small fetches, and the remote path is where latency is highest, so this is
  precisely where it hurts most.
- **Apple apps cannot use the plain-HTTP address at all.** App Transport Security refuses cleartext
  to a globally-resolvable name like a `.ts.net` one — the request never leaves the app. Safari is
  exempt, which is why the web app can work while the Obelo app cannot. HTTPS sidesteps it.
- **No browser warning, and no exception to click.**

The address Obelo publishes to clients follows what actually bound. **Leave this off and every
client gets the `http://` address and stays on HTTP/1.1 permanently, with nothing telling you what
you left behind.**

It requires two things in the **Tailscale admin console**, both outside Obelo:

1. **DNS → MagicDNS**: enabled.
2. **DNS → HTTPS Certificates**: enabled.

Then in Obelo: **Settings → Remote access → Enable HTTPS**.

Two things to know before you do it:

- Your server's name is published in public **Certificate Transparency logs** the moment a
  certificate is issued. That reveals that `obelo.tail1a2b.ts.net` exists. It reveals nothing about
  what is on it and grants no access — but it is public and permanent, so decide deliberately.
- If certificate provisioning fails, **plain HTTP on the tailnet keeps working**, and the address
  shown to you stays `http://` rather than promising an `https://` that would refuse connections.
  The reason appears on this settings page and in the server log, in the same words. It is not an
  outage, and it retries on its own once you fix the console settings — no restart.

If you skip this step, everything still works — you simply keep the slower transport and the Obelo
app cannot use the tailnet address. It is reversible either way: untick it and the listener goes.

## 6. Point the clients at it

Use the address from step 2 as the server address in each Obelo client. On the LAN you may keep
using the local address; both work, and the tailnet address works in both places, which is the
argument for just using it everywhere.

**In a browser, you will be asked to sign in again** the first time you use the tailnet address.
That is not a bug: `https://obelo.tail1a2b.ts.net` and `http://192.168.1.50:8080` are different
origins as far as any browser is concerned, and sessions do not cross origins. Native clients carry
their own token and are unaffected.

## Running your own coordination server (Headscale)

If you would rather not depend on Tailscale's coordination server at all, point Obelo at your own:

**Settings → Remote access → Control URL**, or `OBELO_TAILSCALE_CONTROL_URL` on first boot.

Empty — the default — means Tailscale's. Everything else in this runbook works the same way; the
console steps happen in your Headscale instead. See
[ADR-0001](../adr/0001-fully-self-hosted-no-vendor-dependency.md) for why this knob exists.

## Two things not to do

**Never put `100.64.0.0/10` (or any tailnet range) in `OBELO_TRUSTED_PROXIES`.** A tailnet peer is
a client, not a proxy. Listing the range would let any device on the tailnet assert whatever client
address it likes — and the login failure limiter and the device-code quota are keyed on that
address, so a device that can name its own has no limit at all.

**Do not turn off the plain-HTTP listener.** LAN clients and mDNS discovery use it, and no
certificate can cover a `.local` name or a raw LAN address. See ADR-0041.

## Troubleshooting

**The login link never appears.** The server cannot reach the coordination server. Check the
server's own internet access; the log line names what it tried.

**Connected, but a client cannot reach it.** Confirm the client device is signed into the *same*
tailnet — this is the overwhelmingly common cause. Check the machine list in the console: both
should be there and both should be online.

**It worked for months and then stopped.** Step 3. Check the machine list for an expired node and
re-authorize with the link in Obelo's settings panel.

**Playback stutters remotely but the LAN is fine.** Your two devices could not connect directly and
fell back to a **DERP relay**, which is throttled and shared. Tailscale's console shows the
connection type per machine. Direct connections usually establish given a moment; a network that
blocks UDP outright will never get one, and this path will stay slow. ADR-0001 records this as the
known cost of the tailnet path.

**HTTPS says no certificate.** Both console toggles in step 5, then read the reason on the Remote
access settings page — it is the same paragraph the log carries, and it names which prerequisite is
missing. Plain HTTP on the tailnet is unaffected meanwhile, and the address shown to you stays
`http://` for exactly as long as that is the truth.

**I disconnected and the machine is still in the console.** Expected. *Disconnect* stops the node
and keeps its identity so reconnecting is instant. *Forget* wipes the identity here, but the row in
the Tailscale console is yours to delete — nothing Obelo does can remove it.

**I want to move the server to a different tailnet.** Forget, then Connect, then sign in to the
other account. Delete the stale machine row in the old console.
