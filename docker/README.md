# Docker build

Multi-stage image that builds the media server for **linux/amd64** from source:
the React/Vite SPA is bundled first, embedded into the Go binary (`go:embed`,
ADR-0012), and the result runs on a slim Debian image with `ffmpeg` and the GPU
userspace for every hardware backend Obelo supports
([ADR-0042](../docs/adr/0042-glibc-runtime-base-for-hardware-transcoding.md)).

## Build

The build context is the **repository root**, not this directory:

```sh
docker build -f docker/Dockerfile -t obelo .
```

Building on an Apple Silicon / arm64 host still produces an amd64 image — the Go
binary cross-compiles (pure Go, no CGO) and the runtime stage is pinned to
`linux/amd64`. If your Docker can't run amd64 images locally, build and push
with buildx instead:

```sh
docker buildx build --platform linux/amd64 -f docker/Dockerfile -t obelo . --load
```

## Run

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

- `:8080` — HTTP API + web UI (same origin).
- `/data` — the single writable data dir (SQLite DB, artwork cache); mount a
  volume so it survives container restarts.
- Mount your media libraries read-only (any path); point libraries at those
  paths from the web UI.

## Configuration

All config is via `OBELO_*` environment variables (see
`internal/config/config.go`). The image sets sensible defaults:

| Variable                   | Default   | Purpose                          |
| -------------------------- | --------- | -------------------------------- |
| `OBELO_LISTEN_ADDR` | `:8080`   | host:port to bind                |
| `OBELO_DATA_DIR`    | `/data`   | writable data directory          |

Pass others (e.g. `OBELO_TMDB_API_KEY`, `OBELO_HARDWARE_ACCEL`,
`OBELO_SCAN_INTERVAL`) with `-e` as needed.

## LAN discovery (Bonjour / `_obelo._tcp`)

Native clients can find the server without anyone typing an IP
([ADR-0034](../docs/adr/0034-server-identity-and-mdns-advertisement.md)). In
Docker that needs one thing you do not need for anything else:

```sh
docker run --rm --network host \
  -e OBELO_LISTEN_ADDR=:8088 \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

**`--network host` is required.** mDNS is link-local multicast; it does not cross
a Docker bridge, so a published port (`-p 8080:8080`) gives you a perfectly
reachable server that no client will ever *discover*. Manual address entry still
works in that setup — discovery is a convenience, never the only path.

The boot log says what was advertised:

```
obelo: advertising "Living Room" as nuc.local. on _obelo._tcp port 8088 at 192.168.1.50 (id: …)
```

If a client finds nothing, read that line first — it is the only visible symptom
discovery has. Two things it can tell you:

- **The line is missing, replaced by `mDNS advertisement unavailable (…)`.** The
  server could not work out an address to publish. Set
  `OBELO_ADVERTISE_IP=192.168.1.50` (this host's LAN address; comma-separate
  several).
- **The line is there but the address is wrong** — a `172.17.x.x` or other bridge
  address instead of your LAN address. Same fix.

If the address is right and clients still find nothing, the responder is probably
listening on the wrong interface: the kernel picks the "default multicast
interface" from the routing table, and on a Docker host that is often `docker0`
rather than your NIC, so the query never reaches the server. Pin it:

```sh
-e OBELO_MDNS_INTERFACE=eth0
```

Verify from a Mac on the same LAN with `dns-sd -B _obelo._tcp local`. Note that
this does **not** work against a server on the *same* Mac — macOS's own
`mDNSResponder` owns port 5353 and a second responder on that host is invisible to
it. That is a same-host artifact, not a server fault.

## Hardware transcoding

The image ships the userspace for all three Linux backends ADR-0009 names —
NVENC, VAAPI, and QSV. Installing it is only half the job: the container also
has to be able to reach the card, and that differs per vendor.

Whatever you pass, `OBELO_HARDWARE_ACCEL` is only a *preference*. At startup the
server validates it against the host in two steps (the encoder must be compiled
into ffmpeg **and** a one-frame test-encode must succeed) and then logs the
outcome. Read that line first when a GPU seems idle:

```
obelo: hardware acceleration: using configured hardware backend nvenc (h264_nvenc validated)
obelo: hardware acceleration: WARNING — configured backend nvenc did not validate
       (encoder missing or no working device); falling back to CPU libx264
```

The warning is never fatal — playback keeps working on CPU libx264.

### NVIDIA (NVENC)

Requires the [NVIDIA Container
Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
on the host. Nothing else: the toolkit injects the driver libraries, and the
image already requests the `video` capability that carries `libnvidia-encode`.

```sh
docker run --rm --gpus all -p 8080:8080 \
  -e OBELO_HARDWARE_ACCEL=nvenc \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

### Intel / AMD (VAAPI, QSV)

Pass the DRM render node through and add the **host's** `render` GID. The image
puts its runtime user in a `render` group, but GIDs are host-specific, so the
in-image group alone will usually not match the device's owner:

```sh
docker run --rm -p 8080:8080 \
  --device /dev/dri:/dev/dri \
  --group-add "$(getent group render | cut -d: -f3)" \
  -e OBELO_HARDWARE_ACCEL=vaapi \
  -v "$PWD/data:/data" \
  -v /path/to/your/media:/media:ro \
  obelo
```

Swap `vaapi` for `qsv` on Intel if you want Quick Sync specifically; `auto`
probes NVENC → VAAPI → QSV and takes the first that validates.

A backend that validates when run as root but not as the normal user is the
group-permission problem above, not a driver problem — check `--group-add`
before anything else.

Intel's `intel-media-va-driver-non-free` decodes a few extra formats, but it
lives in Debian's non-free component and is deliberately not baked in; add it
yourself in a derived image if you need it.

## GPU telemetry (NVENC)

The admin **Transcoding** tab shows best-effort GPU telemetry (utilization, VRAM,
encoder sessions, driver version) when `OBELO_HARDWARE_ACCEL=nvenc` resolves to
an active NVENC backend. It shells out to `nvidia-smi`, which the container
toolkit injects along with the driver.

Without `--gpus all` (or on any non-NVENC backend), the GPU block reads
"unavailable" — that is expected, not a defect. The rest of the Transcoding tab
(resolved backend + live load) works regardless.
