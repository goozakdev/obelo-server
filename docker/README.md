# Docker build

Multi-stage image that builds the media server for **linux/amd64** from source:
the React/Vite SPA is bundled first, embedded into the Go binary (`go:embed`,
ADR-0012), and the result runs on a minimal Alpine image with `ffmpeg`.

## Build

The build context is the **repository root**, not this directory:

```sh
docker build -f docker/Dockerfile -t juicebox .
```

Building on an Apple Silicon / arm64 host still produces an amd64 image — the Go
binary cross-compiles (pure Go, no CGO) and the runtime stage is pinned to
`linux/amd64`. If your Docker can't run amd64 images locally, build and push
with buildx instead:

```sh
docker buildx build --platform linux/amd64 -f docker/Dockerfile -t juicebox . --load
```

## Run

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  juicebox
```

- `:8080` — HTTP API + web UI (same origin).
- `/data` — the single writable data dir (SQLite DB, artwork cache); mount a
  volume so it survives container restarts.
- Mount your media libraries read-only (any path); point libraries at those
  paths from the web UI.

## Configuration

All config is via `JUICEBOX_*` environment variables (see
`internal/config/config.go`). The image sets sensible defaults:

| Variable                   | Default   | Purpose                          |
| -------------------------- | --------- | -------------------------------- |
| `JUICEBOX_LISTEN_ADDR` | `:8080`   | host:port to bind                |
| `JUICEBOX_DATA_DIR`    | `/data`   | writable data directory          |

Pass others (e.g. `JUICEBOX_TMDB_API_KEY`, `JUICEBOX_HARDWARE_ACCEL`,
`JUICEBOX_SCAN_INTERVAL`) with `-e` as needed.

## LAN discovery (Bonjour / `_juicebox._tcp`)

Native clients can find the server without anyone typing an IP
([ADR-0034](../docs/adr/0034-server-identity-and-mdns-advertisement.md)). In
Docker that needs one thing you do not need for anything else:

```sh
docker run --rm --network host \
  -e JUICEBOX_LISTEN_ADDR=:8088 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  juicebox
```

**`--network host` is required.** mDNS is link-local multicast; it does not cross
a Docker bridge, so a published port (`-p 8080:8080`) gives you a perfectly
reachable server that no client will ever *discover*. Manual address entry still
works in that setup — discovery is a convenience, never the only path.

The boot log says what was advertised:

```
juicebox: advertising "Living Room" as nuc.local. on _juicebox._tcp port 8088 at 192.168.1.50 (id: …)
```

If a client finds nothing, read that line first — it is the only visible symptom
discovery has. Two things it can tell you:

- **The line is missing, replaced by `mDNS advertisement unavailable (…)`.** The
  server could not work out an address to publish. Set
  `JUICEBOX_ADVERTISE_IP=192.168.1.50` (this host's LAN address; comma-separate
  several).
- **The line is there but the address is wrong** — a `172.17.x.x` or other bridge
  address instead of your LAN address. Same fix.

If the address is right and clients still find nothing, the responder is probably
listening on the wrong interface: the kernel picks the "default multicast
interface" from the routing table, and on a Docker host that is often `docker0`
rather than your NIC, so the query never reaches the server. Pin it:

```sh
-e JUICEBOX_MDNS_INTERFACE=eth0
```

Verify from a Mac on the same LAN with `dns-sd -B _juicebox._tcp local`. Note that
this does **not** work against a server on the *same* Mac — macOS's own
`mDNSResponder` owns port 5353 and a second responder on that host is invisible to
it. That is a same-host artifact, not a server fault.

## GPU telemetry (NVENC)

The admin **Transcoding** tab shows best-effort GPU telemetry (utilization, VRAM,
encoder sessions, driver version) when `JUICEBOX_HARDWARE_ACCEL=nvenc` resolves to
an active NVENC backend. It is read by shelling out to `nvidia-smi`, so the
container needs both the NVIDIA container runtime and the binary on `PATH`:

```sh
docker run --rm --gpus all -p 8080:8080 \
  -e JUICEBOX_HARDWARE_ACCEL=nvenc \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  juicebox
```

Without `--gpus all` (or on any non-NVENC backend), the GPU block reads
"unavailable" — that is expected, not a defect. The rest of the Transcoding tab
(resolved backend + live load) works regardless.
