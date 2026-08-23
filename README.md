# Obelo

**A fully self-hosted media server for your household's movies, TV, and music.**

Obelo scans the video and music files you already have on disk, organizes
them into browsable libraries, decorates them with artwork and descriptions from
public metadata sources, and streams them to your devices through a clean web
app — all from a single binary you run on your own hardware. No accounts on
someone else's servers, no phoning home, no vendor lock-in.

> Point it at a folder of media, open it in a browser, and press play.

---

## Highlights

- **Movies, TV, and Music** in one server — each library holds one kind, backed
  by one or more folders on disk (multiple folders merge into one library).
- **Identity from your file names, not a cloud database.** The scanner derives
  what each file *is* from its path and embedded tags, so your library is yours
  and stays correct offline. External metadata only ever *decorates* — it never
  decides identity.
- **Automatic artwork & metadata (enrichment).** Posters, backgrounds, logos,
  cast, descriptions, artist photos, and biographies are fetched from public
  providers (TMDB, MusicBrainz, fanart.tv, TheAudioDB, and more). Fully
  optional and off until you turn it on.
- **Adaptive streaming with three playback tiers.** Direct play when the client
  can handle the file as-is, direct stream (remux) when only the container needs
  changing, and full FFmpeg transcode when it doesn't — chosen automatically
  from what each client reports it can play.
- **Hardware-accelerated transcoding.** CPU (libx264) always works; NVENC,
  VAAPI, Quick Sync, and VideoToolbox backends are selectable, with a live
  admin view of transcode load and (on NVIDIA) GPU telemetry.
- **Multi-user with real access control.** Per-user libraries, content-rating
  ceilings, private watch state, resume/Continue-Watching, TV Up Next, and
  named per-device tokens you can revoke individually.
- **Subtitles that just work.** Embedded, sidecar (`Movie.en.srt`), and
  on-demand fetched subtitles — delivered as selectable tracks or burned in when
  the format requires it.
- **Incremental, resilient scanning.** Runs on a schedule or on demand; missing
  files are soft-deleted (watch state survives their return); a per-entity
  **targeted scan** re-checks just one movie/show/album's folders.
- **Admin niceties.** Hand-edit and lock fields, correct a wrong match, upload
  your own artwork, and curate Collections — all without the next scan undoing
  your work.
- **One process, one binary.** A Go server with the React web app embedded
  inside it (`go:embed`). SQLite for state, the filesystem for caches. Nothing
  else to stand up.

See [`CONTEXT.md`](./CONTEXT.md) for the full domain glossary and
[`docs/adr/`](./docs/adr/) for the architectural decisions behind these choices.

---

## Requirements

- **`ffmpeg`** (with `ffprobe`) on the server's `PATH` — used for probing files
  and for transcoding. This is the only runtime dependency.
- A folder (or folders) of media, and a browser to reach the web app.

That's it for running the prebuilt Docker image. To **build from source** you
additionally need **Go 1.25+** and **Node.js 22+**.

---

## Quick start with Docker (recommended)

Build the image (the build context is the repository root, not the `docker/`
directory):

```sh
docker build --platform linux/amd64 -f docker/Dockerfile -t obelo .
```

Run it, mounting a writable data directory and your media (read-only):

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

- **`:8080`** — the web app and API (same origin). Open <http://localhost:8080>.
- **`/data`** — the one writable directory: SQLite database, artwork/subtitle
  caches, transcode scratch. Mount a volume so it survives restarts.
- **Your media** — mount anywhere read-only; you'll point libraries at these
  paths from the web UI.

Want native clients to **find the server** instead of being handed an address?
Run it with `--network host` instead of `-p` — mDNS is link-local multicast and
does not cross a Docker bridge. See
[`docker/README.md`](./docker/README.md#lan-discovery-bonjour-_obelo_tcp).

Building on Apple Silicon produces a `linux/amd64` image (the Go binary is
pure-Go and cross-compiles) — but **keep the `--platform linux/amd64` flag**. The
image content is amd64 either way; without the flag the manifest gets *labelled*
`linux/arm64`, which local `docker run` ignores but `docker push`/`docker pull`
honour. Check the `└─` line before trusting a build:

```sh
docker image ls --tree obelo      # must read: └─ linux/amd64
```

If your Docker can't run amd64 images locally you can still build them:

```sh
docker buildx build --platform linux/amd64 -f docker/Dockerfile -t obelo . --load
```

Full details, plus the maintainer publish checklist, are in
[`docker/README.md`](./docker/README.md#build).

### GPU-accelerated transcoding

The image ships the full GPU userspace for every backend Obelo supports on
Linux, so `nvenc`, `vaapi`, and `qsv` all work — the card just has to be reachable
from inside the container.

**NVIDIA (NVENC)** — needs the
[NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
on the host:

```sh
docker run --rm --gpus all -p 8080:8080 \
  -e OBELO_HARDWARE_ACCEL=nvenc \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

The admin **Transcoding** tab then shows GPU telemetry (utilization, VRAM,
encoder sessions, driver version). Without `--gpus all`, or on any non-NVENC
backend, the GPU block reads "unavailable" — that's expected; the rest of the
tab still works.

**Intel / AMD (VAAPI or QSV)** — pass the render node through instead, and add
the host's `render` group so the unprivileged container user can open it:

```sh
docker run --rm -p 8080:8080 \
  --device /dev/dri:/dev/dri \
  --group-add "$(getent group render | cut -d: -f3)" \
  -e OBELO_HARDWARE_ACCEL=vaapi \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

Use `OBELO_HARDWARE_ACCEL=auto` to let the server pick the best backend the host
actually validates. Whichever you choose, the boot log states what happened —
including a loud warning if an explicitly requested backend fell back to CPU.

More detail lives in [`docker/README.md`](./docker/README.md).

---

## Build & run from source

Prerequisites: **Go 1.25+**, **Node.js 22+**, and **`ffmpeg`** on `PATH`.

```sh
git clone https://github.com/goozakdev/obelo-server.git
cd obelo
make build      # builds the web bundle first, then the Go binary that embeds it
./bin/obelo
```

`make build` enforces the required order: the React/Vite SPA is bundled into
`internal/webui/dist` *before* `go build`, because the binary embeds it. Other
handy targets:

| Target        | What it does                                             |
| ------------- | -------------------------------------------------------- |
| `make build`  | Full build (web bundle → Go binary).                     |
| `make run`    | Build, then run the server.                              |
| `make test`   | Go unit/integration tests, then the Playwright E2E suite.|
| `make fmt`    | `gofmt` the tree.                                        |
| `make clean`  | Remove build outputs.                                    |

By default the server listens on `:8080` and stores state in `./data`.

---

## First run: create the first admin

Everything in Obelo is authenticated, so the very first admin is
bootstrapped with a **one-time claim token** printed to the server's logs on
first start (when the database has no users yet):

```
obelo: no users yet — first-Admin claim token: <token>
obelo: complete setup via POST /api/v1/setup with this claimToken
```

Open the web app, and the setup wizard will ask for that token to create your
admin account. Once the first admin exists, setup permanently closes. With
Docker, read the token from `docker logs <container>`.

After logging in: create your libraries (point them at your mounted media
folders), pick a media kind for each, trigger a scan, and — if you want artwork
and descriptions — turn on enrichment and grant consent.

---

## Configuration

All configuration is via `OBELO_*` environment variables. Common ones:

| Variable                             | Default   | Purpose                                                        |
| ------------------------------------ | --------- | -------------------------------------------------------------- |
| `OBELO_LISTEN_ADDR`               | `:8080`   | `host:port` the server binds to.                               |
| `OBELO_TLS_MODE`                  | `off`     | `off` / `files` / `acme`. Either **adds** an HTTPS listener.   |
| `OBELO_TLS_CERT`                  | —         | Absolute path to the PEM certificate chain (`files` mode).     |
| `OBELO_TLS_KEY`                   | —         | Absolute path to the PEM private key (`files` mode).           |
| `OBELO_TLS_LISTEN_ADDR`           | `:8443`   | `host:port` for HTTPS; must differ from `OBELO_LISTEN_ADDR`.   |
| `OBELO_TLS_DOMAINS`               | —         | Comma-separated DNS names. **Required** in `acme` mode.        |
| `OBELO_ACME_EMAIL`                | —         | Optional contact for the CA's expiry notices.                  |
| `OBELO_ACME_DIRECTORY`            | Let's Encrypt | ACME directory URL; point at staging while setting up.     |
| `OBELO_TRUSTED_PROXIES`           | —         | CIDRs whose `X-Forwarded-*` headers are believed. See below.   |
| `OBELO_DATA_DIR`                  | `./data`  | Writable data directory (DB + caches).                         |
| `OBELO_SCAN_INTERVAL`             | `1h`      | Scheduled incremental scan cadence (`0` disables).             |
| `OBELO_HARDWARE_ACCEL`            | `off`     | `off` / `auto` / `nvenc` / `vaapi` / `qsv` / `videotoolbox`.   |
| `OBELO_MAX_CONCURRENT_TRANSCODES` | `3`       | Cap on simultaneous transcodes (`0` = unlimited).              |
| `OBELO_TMDB_API_KEY`              | —         | Enables Movie/TV enrichment via TMDB.                          |
| `OBELO_MUSICBRAINZ_ENABLED`       | `false`   | Turns on Music enrichment (needs no key).                      |
| `OBELO_FANART_TV_API_KEY`         | —         | Adds artist imagery from fanart.tv.                            |
| `OBELO_THEAUDIODB_API_KEY`        | —         | Adds artist images + biographies from TheAudioDB.              |
| `OBELO_OPENSUBTITLES_API_KEY`     | —         | Enables on-demand subtitle fetching.                           |
| `OBELO_METADATA_LANGUAGE`         | `en-US`   | Preferred language/region for fetched metadata.               |
| `OBELO_ADVERTISE_IP`              | auto      | IPs to publish in the mDNS records (comma-separated).          |
| `OBELO_MDNS_INTERFACE`            | auto      | Interface the mDNS responder listens on (e.g. `eth0`).         |
| `OBELO_TAILSCALE_ENABLED`         | `false`   | Join a Tailnet at boot. **First boot only** — see below.       |
| `OBELO_TAILSCALE_HOSTNAME`        | server name | MagicDNS name to join under (a single DNS label).            |
| `OBELO_TAILSCALE_CONTROL_URL`     | Tailscale | Coordination server; set it to your own Headscale.             |
| `OBELO_TAILSCALE_HTTPS`           | `false`   | Also serve HTTPS on the Tailnet (needs console prerequisites). |
| `OBELO_TAILSCALE_AUTHKEY`         | —         | Pre-authorized join key for a headless first join. Never stored. |

### HTTPS

Obelo can terminate TLS itself ([ADR-0041](./docs/adr/0041-native-tls-optional-alongside-plain-http.md)) — useful if you reach your server from outside the house by forwarding a port, with no reverse proxy in front. There are two ways to get a certificate.

**`acme` — Obelo gets one for you, automatically.** This is the one most households want. You need a domain name whose public DNS points at your house, and **one forwarded port**: the router's port 443 to `OBELO_TLS_LISTEN_ADDR`.

```
OBELO_TLS_MODE=acme
OBELO_TLS_DOMAINS=media.example.com
OBELO_ACME_EMAIL=you@example.com          # optional; the CA warns you before expiry
```

The settings are only half of it — the DNS record, the port-forward, and the
staging-first order of operations are the part that actually goes wrong. For a
first-time setup end to end, follow
[docs/runbooks/https-with-lets-encrypt.md](./docs/runbooks/https-with-lets-encrypt.md).

**`files` — you supply the certificate.** Any CA, including your own:

```
OBELO_TLS_MODE=files
OBELO_TLS_CERT=/etc/letsencrypt/live/example.com/fullchain.pem
OBELO_TLS_KEY=/etc/letsencrypt/live/example.com/privkey.pem
```

**HTTPS is an addition, never a replacement.** The plain-HTTP listener keeps serving `OBELO_LISTEN_ADDR` exactly as before, because no public CA will issue a certificate for a LAN address or a `.local` name — so the LAN, and the mDNS discovery that depends on it, goes on working unchanged while TLS covers the hop that leaves the house. Both listeners serve the same API, and a session created on one works on the other.

Things worth knowing about `acme` mode:

- **Only one port has to be forwarded.** Obelo proves it controls your domain with the TLS-ALPN-01 challenge, which completes on the HTTPS port itself. Nothing needs port 80, and there is no second listener to set up.
- **`OBELO_TLS_DOMAINS` is required, and it is an exact list.** Every name you want a certificate for goes in it, `www.` included; wildcards are not possible with this challenge type. There is no "any name" setting on purpose — without the list, the server would ask the CA for a certificate for whatever name any stranger's connection asked about, which spends your rate limit on other people's domains.
- **Set it up against the staging directory first.** Let's Encrypt's production endpoint allows only five failed attempts per name per hour, so one wrong port-forward can lock you out for the rest of the afternoon. Point `OBELO_ACME_DIRECTORY=https://acme-staging-v02.api.letsencrypt.org/directory` while you get DNS and the router right, then remove it. Staging certificates come from an untrusted root, so your browser will warn — that warning *is* the success signal — and switching to production issues a fresh, real certificate.
- **A certificate that cannot be obtained does NOT stop the server.** If the CA is unreachable, DNS has not propagated, the port-forward is not set up yet, or you are rate-limited, Obelo boots anyway, keeps serving plain HTTP on the LAN, logs the reason, and keeps trying. A CA outage is not your mistake, and it should not cost you your media server.
- **Keep the data directory.** The ACME account key and every issued private key live in `OBELO_DATA_DIR/acme` (mode `0700`). If that directory is wiped on each restart — a container without a volume, or a tmpfs — Obelo re-issues every time and will hit the CA's duplicate-certificate limit within days.
- **Renewal is automatic** and needs nothing from you.

And about `files` mode:

- **Renewals are picked up without a restart.** The certificate files are re-read when they change, so a certbot renewal takes effect on the next connection. If a renewal leaves a file half-written, the previous certificate keeps serving and the problem is logged rather than taking the listener down.
- **A broken certificate stops the boot.** If you turn `files` mode on and the certificate is missing, unreadable, or does not match the key, the server refuses to start and says which path is wrong. That is deliberate, and it is the opposite of the `acme` behaviour above for a reason: a path that is wrong is a typo you can fix in ten seconds, and starting anyway would leave you on plain HTTP believing you had TLS. A CA being down is nobody's typo.

### Remote access over a Tailnet

Obelo can join your **Tailnet** as a node of its own and serve the same API and web app there, reached at a stable name like `obelo.tail1a2b.ts.net` from any device that is also on the Tailnet ([ADR-0043](./docs/adr/0043-tailnet-remote-access-via-embedded-tsnet.md)). There is no port to forward, no DNS record to keep current, no domain to buy, and nothing exposed to the open internet.

The honest cost, up front: **every client device must also join the Tailnet** — Tailscale installed and signed in on the iPhone, the iPad, the Apple TV, and any laptop whose browser you use. That is a worse first run than typing an address and a better everything after. It is why this does not replace the HTTPS setup above: a household that wants to hand a URL to someone who will not install a VPN client still needs the port-forward.

**It is configured from the web UI**, not from these variables: Settings → Remote access has Connect, the login link, the address, the node's state, and Disconnect / Forget. The `OBELO_TAILSCALE_*` variables above **seed the settings on first boot only** — after that the database is authoritative and they are ignored, so a value you change in the UI is not undone by a restart. Connect and disconnect take effect immediately, with no restart.

**It needs the release build.** The embedded Tailscale node is compiled in only under `-tags tailscale`, which the Docker image and the release binaries carry and a plain `go build` does not — that keeps ~550 dependencies out of a development build. `GET /api/v1/server` reports `features.tailscale`, and a build without it answers the remote-access endpoints with an error that names the build rather than a confusing `404`. Nothing about the container changes for this: no extra capabilities, no `/dev/net/tun`, and it still runs as the unprivileged `obelo` user, because the node runs in userspace inside the server process.

**Node keys expire — by default after 180 days**, and the symptom is "remote access stopped working" six months after you last touched the box. The expiry appears in the settings panel and in the boot log, and the log warns under 14 days. The real fix is to disable key expiry for this node in the Tailscale console; nothing in Obelo can extend it.

For a first-time setup end to end, follow [docs/runbooks/remote-access-with-tailscale.md](./docs/runbooks/remote-access-with-tailscale.md).

### Running behind a reverse proxy

If something else terminates TLS in front of Obelo — nginx, Caddy, a container
ingress — tell Obelo which address that proxy connects from:

```
OBELO_TRUSTED_PROXIES=127.0.0.1/32          # a proxy on the same machine
OBELO_TRUSTED_PROXIES=10.4.0.9/32,10.4.0.10/32   # two proxies, by address
```

**It is empty by default, and empty means no `X-Forwarded-For` or
`X-Forwarded-Proto` header is believed from anybody.** That default is not
caution for its own sake. Obelo can now terminate HTTPS itself, so clients reach
it directly, and a forwarded header on a direct connection is written by whoever
dialled the port. Obelo therefore reads one only when the machine on the other
end of the socket is an address you listed. Without the setting it uses the
connection's real peer address and the connection's real scheme, which is always
true and sometimes less useful.

Setting it turns two things back on:

- **Per-client rate limits.** The failed-login limiter and the TV sign-in code
  quota are counted per client address. Behind an undeclared proxy every request
  looks like it came from the proxy, so the whole household shares one budget —
  safe, but blunt. Declared, each device gets its own again.
- **The session cookie's `Secure` flag on the proxy's HTTPS.** Undeclared, Obelo
  sees a plain-HTTP request and marks the cookie accordingly. Nothing breaks — the
  cookie still travels inside your proxy's TLS — but it is not flagged.

**Only list a proxy you control.** Every host inside a range you name is allowed
to tell Obelo what address a request came from, which means it can also *lie*
about it, and a host that can name its own address has no rate limit at all. List
the proxy itself — `10.4.0.9/32` — not the network it happens to sit on:
`10.0.0.0/8` because the proxy is somewhere in there also trusts every laptop,
television, and compromised smart plug on your LAN. A bare address means that one
host (`127.0.0.1` is read as `127.0.0.1/32`), and an entry that is not an address
or a CIDR stops the server at boot and says which one it was.

Obelo reads the **left-most entry in `X-Forwarded-For` that is not itself a
trusted proxy**, working from the right. A client can put anything it likes at the
front of that header; whatever your proxy appends comes after it, so the forged
part is never reached.

Provider keys and language seed the database only on **first boot** — afterward
you manage providers from the admin settings UI (no restart needed). Obelo
is **offline-first**: with no keys configured it makes zero outbound calls and
every title simply shows as un-enriched. The full list of knobs is documented in
[`internal/config/config.go`](./internal/config/config.go).

---

## Attribution

Obelo's *metadata enrichment* is optional and, when enabled, decorates your
library using these public sources. Obelo is not endorsed by or affiliated
with any of them.

<table>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/tmdb.svg" alt="TMDB" height="26"></td>
    <td>Movie &amp; TV metadata and artwork. <b>This product uses the TMDB API but is not endorsed or certified by <a href="https://www.themoviedb.org/">TMDB</a>.</b></td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/musicbrainz.svg" alt="MusicBrainz" height="52"></td>
    <td>Music metadata (artists, albums, tracks) from <a href="https://musicbrainz.org/">MusicBrainz</a>, the open music encyclopedia.</td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/fanarttv.png" alt="fanart.tv" height="44"></td>
    <td>Additional artist and fan artwork courtesy of <a href="https://fanart.tv/">fanart.tv</a>.</td>
  </tr>
  <tr>
    <td width="180" align="center"><img src="assets/attribution/theaudiodb.png" alt="TheAudioDB" height="26"></td>
    <td>Artist images and biographies courtesy of <a href="https://www.theaudiodb.com/">TheAudioDB</a>.</td>
  </tr>
</table>

Album cover art is additionally sourced from the
[Cover Art Archive](https://coverartarchive.org/). Logos and trademarks are the
property of their respective owners and are used here solely to credit the data
sources.

---

## Project layout

```
cmd/obelo/        server entry point
internal/            the modular monolith (scanner, enrichment, playback, api, …)
web/                 React + TypeScript SPA (embedded into the binary)
docker/              multi-stage Dockerfile + docker notes
docs/adr/            architectural decision records
CONTEXT.md           domain glossary (the project's ubiquitous language)
```
