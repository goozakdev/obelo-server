# Tailnet remote access: an optional third listener, embedded, behind a build tag

The server can join the operator's **tailnet** as a node of its own and serve the same API and web
app there, reached at a stable name — `<hostname>.<tailnet>.ts.net` — from any device that is also
on the tailnet. It is **opt-in, off by default**, and the tailnet listener is an **addition**: the
plain-HTTP LAN listener and the optional ADR-0041 HTTPS listener are untouched. Three listeners,
one `http.Handler`.

It is configured and controlled **from the admin web UI** — connect, disconnect, forget, node
state, login link, FQDN, key expiry — and it is the first piece of this server's *network*
configuration that lives in the database rather than in the environment.

This is the feature [ADR-0041](./0041-native-tls-optional-alongside-plain-http.md) named and
deferred: *"A tailnet would supply a name and a certificate that work identically on the LAN and
remotely, which is a strictly better answer to the problem above — but it is an optional feature
and cannot be the only path to TLS."* That sentence still holds in both directions. This ADR builds
the better answer; it does not retire the other one.

## Why

ADR-0041 gave the port-forwarding household TLS. It did not remove the port-forward, the dynamic
DNS, the domain purchase, the CGNAT problem, or the router page — and it could not, because a
public CA will only certify a name that resolves on the public internet, which is the whole reason
that machinery exists.

A tailnet deletes all of it at once. There is no port to forward, no address to remember, no DNS
record to keep current, no inbound exposure to the internet at all, and the name works identically
from the sofa and from a hotel. The certificate comes with it. For the household that would
otherwise be reading a runbook about CGNAT, that is not an incremental improvement.

**The cost is honest and it is real: every client device must also join the tailnet.** Tailscale
must be installed and signed in on the iPhone, the iPad, the Apple TV, and any laptop whose browser
is used. This is a worse first-run than "type an address" and a better everything-after. It is also
the reason this cannot replace ADR-0041: a household that wants to hand a URL to someone who will
not install a VPN client still needs the port-forward path.

## The ADR-0001 reading

[ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md) says remote access is reached "via the
operator's own networking (domain / dynamic DNS / **VPN** / reverse proxy)" and that "we provide no
NAT-punching relay". A VPN is therefore already blessed. Two things still need saying out loud
rather than being quietly assumed:

- **Tailscale's coordination server is a third party**, and this feature does not work without one.
  It qualifies under the same exemption ACME got: optional, off by default, the server runs fully
  without it, and nothing about the catalog, playback, or accounts depends on it. It must never
  become the default and must never be able to stop the server booting.
- **DERP is a relay, and it may carry media bytes.** When NAT traversal fails, WireGuard traffic
  falls back to Tailscale-operated DERP servers. That is precisely the shape ADR-0001 says we will
  not *provide* — and we still do not provide one; the operator chooses to use somebody else's. The
  distinction is real but thin, and it is documented rather than hidden. It is also throttled, so
  the practical symptom is a 4K stream that stutters for a reason nothing in this server can see or
  report.

- **There was a third vendor flow, and it is turned off.** `tsnet` uploads its own diagnostic logs
  to `log.tailscale.com` by default. Nobody asked for that, nothing here needs it, and it would
  have applied to the Headscale operator too — someone who has gone to the trouble of running their
  own coordination server precisely so that Tailscale is not in the loop, and whose node would then
  have been posting logs to Tailscale anyway. The adapter calls `envknob.SetNoLogsNoSupport()`, so
  the flows in this feature are exactly the two named above and no others. The named cost is real
  and accepted: it forfeits Tailscale's ability to diagnose a problem on this node, which matters
  to somebody filing a support ticket and not to a household media server.

So the escape hatch ships with the feature: **a Control URL setting**, empty by default, pointing
at Tailscale's coordination server unless the operator names their own (Headscale). One text field
is a cheap price for keeping ADR-0001 defensible, and both the coordination server and the node
support it natively.

**A custom DERP map is deliberately not offered.** It is the only remaining Tailscale-operated
component in the path, and closing it would make the story airtight — but self-hosting DERP is a
real ops project, and a knob nobody turns is a knob that rots untested. Reach for it if somebody
actually asks.

## Embedded (`tsnet`), not a daemon

The node runs **inside the server process** via `tailscale.com/tsnet`: userspace WireGuard over a
gVisor network stack, no `tailscaled`, no `/dev/net/tun`, no `NET_ADMIN`. The Docker image runs as
the unprivileged `USER obelo` ([ADR-0006](./0006-docker-first-modular-monolith.md)) and continues
to; nothing about the container's privileges changes.

The alternative — drive an external `tailscaled` over its LocalAPI unix socket — is genuinely
attractive on paper. It costs **zero** new Go dependencies (the LocalAPI is JSON over a socket, a
couple hundred lines to speak), it uses the kernel's WireGuard so throughput is not a question, and
`tailscale cert` output would drop straight into the existing `files`-mode reloader. It was
rejected because of what "configure it from the web interface" then means: the operator must
already have installed and run a daemon we do not ship, mounted its socket into the container, and
solved the permissions (the socket is root-owned `0600` by default) — all before the web UI has
anything to control. The setup burden lands entirely *outside* the surface this feature exists to
provide, and what remains for the UI is status and a stop button. Embedding means the web UI is the
whole setup: click Connect, click the link, done.

Two costs come with that and neither is small:

- **Media traverses a userspace network stack.** gVisor's netstack is fast enough for HLS on any
  modern host, but it is not the kernel, and this is the streaming path. Measure it on real
  hardware when the adapter lands rather than assuming.
- **The dependency graph explodes.** Measured, on this machine:

  | | today | `tsnet` hello-world alone | **this server, `-tags tailscale`** |
  |---|---|---|---|
  | modules in graph | 40 | 547 | **570** |
  | packages compiled | 273 | 544 | **610** |
  | binary | 21 MB | 26 MB | **40 MB** |
  | cold build | 1.3 s | 13.4 s | **14.9 s** |

  (`tailscale.com v1.102.2`, pulling in `gvisor.dev/gvisor` and `wireguard-go`.) The third column
  is **measured**, on the same machine, once the adapter existed (issue 02) — it replaces the
  40–45 MB projection this table used to carry, and the projection was close: 21 MB → 40 MB, a
  build an order of magnitude slower.

  Three things the numbers say that the projection did not:

  - **The module graph grows for everyone**, tag or no tag: `go.mod` is one file, so a tag-less
    `go list -m all` also reports 570. What the tag buys is packages COMPILED (274 without it
    against 610 with it) and therefore binary size and build time — not a smaller dependency
    graph to audit. The CVE feed arrives either way; only the linked code does not.
  - **The tag-less binary is unchanged**: 21 MB, exactly as before, because none of those
    packages is reachable without the tag.
  - Cold build here means `go clean -cache` first, so the whole graph including the standard
    library is rebuilt. Under that method today's tag-less build measures **5.8 s**, not the
    1.3 s in the first column, which was taken by an unstated method — so **5.8 s → 14.9 s** is
    the like-for-like pair, and the first column's number is kept only because it is what was
    recorded at the time.

  - **The Go toolchain moved for everyone too**, which is the sharpest version of the point above.
    `tailscale.com v1.102.2` requires Go 1.26.5, so `go.mod`'s directive went 1.25.0 → 1.26.5 and
    the Dockerfile's builder went `golang:1.25-alpine` → `golang:1.26-alpine`. Nine transitive
    modules moved with it, `golang.org/x/crypto` among them — the dependency the ACME path
    (ADR-0041) is built on. **A tag-less build compiles under a toolchain chosen by a dependency it
    does not link.** That is the honest limit of what the build tag isolates, and it is worth
    knowing before the next large dependency is weighed against this precedent.

  `tailscale.com` is also large, fast-moving, and security-sensitive. **Pin the version and upgrade
  deliberately.** Letting it drift is how a media server acquires a CVE feed it never asked for.

## The build tag, and the guard it obliges

`tsnet` is linked only under **`-tags tailscale`**. The shipped Docker image and the release
binaries carry the tag; a plain `go build` stays lean, which keeps day-to-day development on a
1.3-second loop and keeps `go.mod`'s four-direct-dependency character intact for anyone who wants
a media server and not a VPN.

`GET /server`'s `features` map gains a `tailscale` key so the SPA knows whether to render the panel
at all, and **a tag-less build registers the same routes** — answering with a specific error that
names the build, not a bare 404 that reads identically to a typo'd path.

**CI must run the suite both ways, and this is not optional advice.** `CLAUDE.md` records what
happens to this repo when two guards want opposite things and only one side is automated: the
committed `index.html` was wrong for a month while `check-bundle` reported success the whole time.
A build tag is that shape again — a default build that lacks a feature and a release build that has
it. If only one variant is exercised, the untested variant is the one users run.

The seam below is what makes "both ways" cheap rather than a doubled test matrix.

## The seam

A small interface — start, status, FQDN, listen, listen-TLS, close, and a channel of state changes
— is the only thing the rest of the server knows about. **The `tsnet`-backed implementation is the
only file behind the build tag.** Settings persistence, the API handlers, the
connect/disconnect/forget state machine, the expiry warnings, and the events all compile and test
in *both* builds against a fake injected through `internal/testharness` — the pattern
`WithGPUProbe` and `WithMetadataProvider` already establish.

This is deliberate scope control on the untestable surface. You cannot join a real tailnet from a
unit test, so the question is only how much code is stranded on the far side of that fact. With the
seam here it is a few hundred adapter lines, verified by hand against a real tailnet.

Running Tailscale's own `tstest/integration/testcontrol` to cover the adapter too was considered and
rejected: it is an internal test package with no stability promise, and trading a hand-verified
adapter for a flaky CI job is a bad deal on a solo project.

## Decisions

- **Listeners.** Tailnet `:80` is bound whenever the node is up — it works immediately, with no
  prerequisites, and WireGuard already encrypts it. Tailnet `:443` with a real Let's Encrypt
  certificate for the MagicDNS name is **opt-in**, because it requires MagicDNS and HTTPS
  certificates to be enabled in the Tailscale console — a prerequisite outside this web UI, so
  turning it on by default would produce a failure the operator cannot act on from here. A
  certificate failure costs HTTPS on the tailnet and nothing else; ACME's posture, restated.
- **No new cookie work.** ADR-0041 discovered the hard way that plain HTTP and HTTPS on one
  hostname share a cookie jar. Tailnet `:80` and `:443` are exactly that pair. The existing split
  is keyed on `r.TLS` rather than on which listener accepted the connection
  (`internal/api/cookie.go`), so `__Secure-ms_media` / `ms_media_plain` already covers this. Worth
  stating because the reflex is to add a third name.
- **DB-authoritative, live.** `OBELO_TAILSCALE_*` seeds the settings on first boot; thereafter the
  admin UI is the source of truth and connect/disconnect take effect with no restart — the
  metadata-providers model, not the TLS model. This is a deliberate departure: TLS is boot-time
  because a listener that appears mid-flight is a surprise, whereas a VPN that cannot be turned on
  without shell access to the box is a feature nobody can use. An enabled node **connects at boot**,
  best-effort; otherwise a restart strands the operator outside a house they cannot get back into.
- **Joining is interactive by default.** The node starts with no key and emits a
  `login.tailscale.com` URL, which the UI shows; the admin authenticates in their own browser. **No
  long-lived tailnet credential is ever stored in Obelo's database.** `OBELO_TAILSCALE_AUTHKEY`
  remains for headless installs and the test harness, consumed at join and never persisted.
- **Two off switches, because they mean different things.** *Disconnect* stops the node and its
  listeners but keeps the state directory, so reconnecting is instant and needs no re-auth.
  *Forget* additionally wipes it, so the next connect is a fresh join. The UI must say plainly that
  Forget leaves a dead node row in the Tailscale console that only the operator can delete —
  nothing here can reach across and remove it.
- **State lives at `<OBELO_DATA_DIR>/tailscale`, mode 0700**, alongside the ACME cache and the
  server identity, and for the same reason: it is durable state whose loss costs a re-auth. Wiping
  the data dir mints a new node exactly as it mints a new Server identity (ADR-0034).
- **The MagicDNS name is its own setting**, seeded from `ServerName` sanitized to a DNS label and
  falling back to `obelo` — the same shape ADR-0034's amendment uses for the `.local` name. It is
  *not* derived live from `ServerName`, because `ServerName` is documented as purely cosmetic and
  making a rename silently break every roaming client's stored URL is precisely the coupling
  ADR-0034 refused when it split `id` from `name`.
- **The FQDN is published in authenticated responses only.** A signed-in client can learn
  `https://obelo.tail1a2b.ts.net` and keep it as its away-from-home address; an unauthenticated
  scanner hitting the port-forward learns nothing about the tailnet's existence or its name. (If
  tailnet HTTPS is on, the name is in Certificate Transparency logs regardless — but that is the
  operator's choice to make, not ours to make for them by putting it on an open endpoint.)
- **No trust flows from being on the tailnet.** Same authentication, same authorization, same rate
  limits, same handler. `100.64.0.0/10` must never appear in `OBELO_TRUSTED_PROXIES`; a tailnet
  peer is a client, not a proxy, and listing the range would let any device on the tailnet forge
  the client address that the login limiter and device-code quota are keyed on.

## Node key expiry is the failure mode to design against

Tailscale node keys expire after **180 days** by default. Nodes joined with a *tagged* auth key are
exempt; a node joined interactively — the flow chosen above — is not.

This is ADR-0041's *"a certificate is now a thing that can expire in production"* with the serial
numbers filed off, and it is worse in one respect: it lands six months after the last time anybody
touched the box, and the symptom is "remote access stopped working" with nothing local broken.

Nothing here can prevent it. What it can do is make being surprised by it impossible: the expiry
date appears in the settings panel *and* in the boot log, a warning fires under ~14 days, and when
the key does lapse the fresh login URL is surfaced in the admin UI so re-authenticating is one
click from the LAN. The runbook tells the operator to disable key expiry for this node in the
Tailscale console, which is the actual fix and lives somewhere we cannot reach.

## Consequences

- **A third listener.** Graceful shutdown covers it, as it already covers two. Nothing in the
  request path learns which listener it arrived on except through `r.TLS`.
- **Tailnet `:443` gets no HTTP/2, and ADR-0041's "HTTP/2 arrives for free" does NOT generalize to
  it.** That claim was true of a listener we build: `http.Server.ServeTLS` negotiates `h2` when we
  leave `TLSNextProto` alone. Here `tsnet` owns the `tls.Config` and its `NextProtos` is empty, so
  ALPN agrees on nothing and every connection is HTTP/1.1. It is not fixable from above the seam —
  only by reimplementing `ListenTLS` against unexported internals, which is not worth owning.

  **The consequence points the wrong way, which is why it is written down rather than filed as a
  detail.** ADR-0041 wanted HTTP/2 specifically for HLS: many small segment fetches multiplexing
  over one connection instead of queueing against the browser's per-origin limit. The path that
  needs that most is the *remote* one, where latency is highest — and the remote path is precisely
  the one that now cannot have it. So the tailnet's headline advantage (a real certificate, no
  port-forward) comes with the worse transport for the traffic that matters, while the
  port-forwarded deployment ADR-0041 built keeps the better one. Anyone measuring remote HLS
  performance across the two paths should expect this and not go looking for a bug.
- **A shutdown hook registered on one listener can make another listener's missing hook invisible,
  and this has now cost three separate debugging sessions.** `RegisterOnShutdown(Events.Close)`
  exists because an SSE handler at `GET /events` only returns when the broker closes; `Broker.Close`
  is idempotent and the broker is process-wide. So a test that drains all listeners together passes
  whether or not the listener under test has a hook of its own — the *other* listener's hook
  released the subscriber. It was found in ADR-0041's two-listener work, again when the tailnet
  listener landed, and a third time on the tailnet TLS listener, each time by a test that passed
  when it should have failed.

  **The rule, for whoever adds a fourth listener: drain the listener under test ON ITS OWN.** That
  is the only shape that proves its hook exists. This is recorded here rather than only in a ticket
  because `.scratch/` is gitignored, and the first two records of it are in files that will not
  survive a fresh clone.
- **`GET /server` gains a `features.tailscale` flag**; additive, no `/api/v2`.
- **A setting is a request; a listener is an outcome; the UI must render the second.** The
  `httpsEnabled` toggle says what the operator asked for. Whether tailnet `:443` actually came up
  depends on two switches in the Tailscale console that this server can neither set nor detect in
  advance. The admin screen shipped reading the first as though it were the second, and told
  operators to use an `https://` address that refused connections — while everything underneath
  behaved correctly, kept `:80` serving, logged the reason, and armed a retry. **The status object
  therefore carries `httpsBound` and `httpsError`, sourced from the supervisor's observation of the
  listener and never from the setting.**

  The general rule, worth more than the two fields: **the UI must not assert a capability the
  server has not confirmed.** The failure is quiet, it points the operator at their own client or
  network, and it is worse than a plain error for exactly that reason.

  **This is a pattern here, not an anecdote — it was found twice in one day.** The other instance
  is older and shipped: the enrichment consent gate (ADR-0032) is applied on every runtime path, so
  a declined consent really does make zero outbound calls, but *not* on the paths that exist only
  to feed the admin UI — which report "Enrichment on" while the consent control a screen-height
  above says enrichment is off. Same shape, with the consent decision playing the part the
  Tailscale console plays here. See
  `.scratch/distributed-metadata-keys/issues/05-consent-is-not-applied-to-the-display-paths.md`.

  **The shape to watch for:** a fact computed a second time for display, by a path that skips a
  gate the runtime path applies. A third independent derivation of the same value is the smell, and
  in both cases it was one letter or one field away from being right — which is why neither was
  caught by reading. Note that `acme` mode has the identical exposure today with no `tlsBound`-style
  signal; it is out of reach only because `OBELO_TLS_MODE` is environment-only with no UI, so a TLS
  admin surface would need this treatment on day one.
- **Publishing the FQDN creates a client obligation.** ADR-0034's own rule applies: *"it must land
  with the client that reads it, or it is churn advertising a fact nobody consumes."* The iPad and
  Apple TV clients must store the tailnet address and fall back to it when the LAN address fails,
  or this field is decoration. That work is outside this repo and should land close behind.

  **When it lands, the address is gated on the FIELD and not the ROUTE** — `GET /server` stays
  unauthenticated and becomes bearer-*aware*, answering 200 with the field null to an absent,
  invalid, expired or revoked bearer, and **never 401**. Wrapping the route in `requireAuth` is the
  obvious implementation and it is a bug: the Apple client drives token-drop from
  `ObeloError.unauthorized`, so a handshake that started 401-ing would present as a revoked token
  and log people out. The cost of that choice is real and lands entirely on the client — "no bearer"
  and "valid bearer, no tailnet" are the same null on the wire, so a client cannot clear its stored
  address on a null without first knowing its own request carried a *known-good* token. Any second
  client will meet the same trap, and it fails intermittently and only after roaming, which is the
  worst way to find it. See `.scratch/tailscale/issues/05-…` for the settled contract.
- **The Server identity is the SAME on all three listeners, and a client's failover depends on it.**
  There is one `http.Handler` and one `server.Metadata` in the process; the tailnet listener is
  handed the same handler as the LAN and TLS ones, and `handleServerInfo` reads neither the request
  nor the listener. So `GET /server` reports one `id` (ADR-0034) no matter which listener answered,
  by construction rather than by convention.

  Stated explicitly because a client cannot verify it and its whole failover rests on it: the Apple
  clients refuse to store an address or attach a token when the handshake reports an id different
  from the one they paired with. **If the tailnet node ever grew an identity of its own, every
  failover would fail closed as "that is a different server", inexplicably and silently.** Anything
  that makes a listener's handler or metadata listener-specific breaks a client in another
  repository, and this bullet is the only warning it will get.
- **The web app does not roam.** `https://obelo.tail1a2b.ts.net` and `http://192.168.1.50:8080` are
  different web origins, so a browser user signs in once per origin. Native clients carry bearer
  tokens bound to a Device row and are unaffected. This is inherent to origins, not a bug to fix.
- **mDNS is unchanged and still advertises one port.** ADR-0034's 2026-08-11 amendment already
  settled that the record describes the LAN, and `internal/discovery/mdns.go` already excludes
  `tailscale*` interfaces from advertisement. A tailnet is not a local link and never will be
  discoverable; the FQDN is how a client finds the server remotely.
- **Two build variants exist forever.** Every bug report now has a "which build?" dimension, and CI
  pays for both.

## Non-goals

- **No Funnel.** Publishing the server to the public internet through Tailscale's edge would put a
  vendor relay directly in the media path — the one thing ADR-0001 prohibits outright — behind a
  toggle in an admin UI. If a household wants public exposure, ADR-0041 is that path and it is
  theirs to configure.
- **No exit node, no subnet router, no Tailscale SSH.** A userspace `tsnet` node cannot be the
  first two anyway, and none of them is about reaching this media server.
- **No custom DERP map.** See above.
- **Not a replacement for ADR-0041.** Both paths stay. They solve the same problem for different
  households.
