# Docker build

Multi-stage image that builds the media server for **linux/amd64** from source:
the React/Vite SPA is bundled first, embedded into the Go binary (`go:embed`,
ADR-0012), and the result runs on a slim Debian image with `ffmpeg` and the GPU
userspace for every hardware backend Obelo supports
([ADR-0042](../docs/adr/0042-glibc-runtime-base-for-hardware-transcoding.md)).

## Build

The build context is the **repository root**, not this directory:

```sh
docker build --platform linux/amd64 -f docker/Dockerfile -t obelo .
```

**Pass `--platform linux/amd64`.** It is not optional on an arm64 host, and
omitting it fails in a way that looks like success. The Dockerfile's
`FROM --platform=linux/amd64` pins the runtime stage's *base image*, so the
content is always amd64 (amd64 Debian, and a `GOARCH=amd64` Go binary that
cross-compiles because it is pure Go, no CGO). But it does **not** set the
platform BuildKit stamps on the output manifest descriptor — that comes from the
build request, which defaults to the build host:

| build command on an arm64 Mac | manifest descriptor | image config |
| ----------------------------- | ------------------- | ------------ |
| `docker build`                | **`linux/arm64`**   | `amd64`      |
| `docker build --platform linux/amd64` | `linux/amd64` | `amd64`   |

Both produce the *identical* manifest — only the label differs. `docker run`
reads the **config**, so an unlabelled build runs fine locally and even reports
itself as amd64; `docker push` and `docker pull` select on the **descriptor**, so
the registry ends up serving an "arm64 image" that is really amd64 bytes, and
`amd64` hosts are told no image is available for their platform.

Verify before you trust it — read the `└─` line, not `docker inspect`:

```sh
docker image ls --tree obelo
# obelo   <id>   223MB   223MB
# └─ linux/amd64   <-- must say amd64; `docker inspect .Architecture` says amd64 either way
```

If your Docker can't run amd64 images locally, that is fine — you can still build
and push them. Use buildx to build and push in one step without loading the image
into your local daemon:

```sh
docker buildx build --platform linux/amd64 -f docker/Dockerfile -t obelo . --load
```

Either command produces a **credential-free** image: no default TMDB/fanart.tv keys
are bundled, and the server uses BYOK (bring-your-own-key) exactly as a
`make build` binary does. That is the correct result for everyone who is not the
maintainer cutting an official image.

### Official builds (maintainer only)

Official images bundle default metadata credentials (ADR-0032). They are passed as
**BuildKit secrets**, never `--build-arg`:

```sh
export OBELO_BOOTSTRAP_TMDB_KEY=<tmdb v3 api key>
export OBELO_BOOTSTRAP_FANART_KEY=<fanart.tv project key>
export OBELO_APP_ENC_KEY=<base64 AES-256-GCM key>

docker build --platform linux/amd64 -f docker/Dockerfile \
  --secret id=tmdb_key,env=OBELO_BOOTSTRAP_TMDB_KEY \
  --secret id=fanart_key,env=OBELO_BOOTSTRAP_FANART_KEY \
  --secret id=app_enc_key,env=OBELO_APP_ENC_KEY \
  --build-arg OBELO_ROTATION_URL=https://<rotation-host>/v1/keys \
  --build-arg CACHE_EPOCH=$(date +%Y-%m-%d) \
  -t ghcr.io/goozakdev/obelo-server:latest .
```

A `--mount=type=secret` file exists only while that one `RUN` executes: it never
becomes a layer, never lands in image metadata, and never enters the build cache.
`--build-arg` does none of that — the value is written into the build stage's
metadata (`docker history` on the build stage shows it) and would be **pushed** by
any `--cache-to type=registry`, besides sitting in `ps` output and your shell
history at invocation time. Secrets are optional, which is why an ordinary build
just works and produces the credential-free image above.

`OBELO_ROTATION_URL` stays a plain build arg on purpose: it is not a secret (the
envelope it serves is ciphertext-only and the URL is readable in the binary), it is
injected only to keep the maintainer host out of the open-source repo. Operators
override it at runtime with `OBELO_KEY_ROTATION_URL`.

> **`CACHE_EPOCH` is not optional when a key changes.** Secret values are excluded
> from BuildKit's cache key by design, so rotating a key and rebuilding with no
> other source change reuses the cached `go build` layer and ships the **old** key
> while reporting success. Bump `CACHE_EPOCH` (any changed string; a date is fine)
> whenever an injected credential changes, or pass `--no-cache-filter build`. See
> [the rotation runbook](../docs/runbooks/metadata-key-rotation.md).

What lands in the binary is unchanged by any of this: the two provider keys are
base64-obfuscated so `strings` yields no bare key, and `kAppEncKey` is verbatim.
That is a speed bump, not secrecy — anyone holding an official image can decode
them back out, which is precisely why the rotation Worker exists. The secret mounts
protect the **build host and its cache**, not the shipped artifact.

### Publishing an official image

Run these three checks **before** `docker push`. Each one covers a failure that
looks like success locally.

```sh
TAG=ghcr.io/goozakdev/obelo-server:latest

# 1. PLATFORM — the descriptor must say amd64, not just the config.
docker image ls --tree "$TAG"        # read the `└─` line
#   └─ linux/amd64        <- correct
#   └─ linux/arm64        <- WRONG: rebuild with --platform linux/amd64
# `docker inspect --format '{{.Architecture}}'` prints amd64 in BOTH cases. It is
# not a check; it is the field that hid this bug.

# 2. CREDENTIALS — the key you think you shipped is the key in the binary.
docker create --name obelo-verify "$TAG" >/dev/null
docker cp obelo-verify:/usr/local/bin/obelo ./obelo-check >/dev/null
docker rm obelo-verify >/dev/null
strings ./obelo-check | grep -c "$(printf %s "$OBELO_BOOTSTRAP_TMDB_KEY" | base64)"   # >= 1
strings ./obelo-check | grep -c "$OBELO_BOOTSTRAP_TMDB_KEY"                            # 0 — no BARE key
# A stale cached layer ships the OLD key silently; see CACHE_EPOCH above.

# 3. BUNDLE — the image must embed a REAL SPA build, not the placeholder.
strings ./obelo-check | grep -c 'obelo-spa-placeholder'    # must be 0
# Check the IMAGE, not the repo: `go run ./internal/webui/cmd/checkbundle` inspects
# your working tree, where the placeholder is the CORRECT committed state (CLAUDE.md),
# so it exits 1 on a clean checkout and tells you nothing about what you are pushing.
# The Docker build produces its own bundle in stage 1; this is the only check that
# sees it.

rm ./obelo-check
```

Then push. **No `--platform` flag on the push** — if step 1 passed, the image is
already a correctly-labelled single-platform amd64 manifest, and `docker push
--platform` only strips the attestation manifest for no benefit:

```sh
docker push "$TAG"
```

`Mounted from <other-repo>` lines in the push output are normal and harmless —
that is cross-repository blob mounting, a registry-side dedup for layers GHCR
already holds in another repo you can read. The blobs are re-referenced under the
new repo and served from it; there is no dependency on the source repo, and it
only means the push was faster.

Finally, confirm the package is actually reachable. **GHCR packages are private on
first push**, and visibility does *not* carry over when you push to a new
namespace — so a repo rename silently un-publishes your image:

```sh
TOK=$(curl -s "https://ghcr.io/token?scope=repository:goozakdev/obelo-server:pull&service=ghcr.io" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOK" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  https://ghcr.io/v2/goozakdev/obelo-server/manifests/latest
# 200 = public. 403 = private: flip it at
#   https://github.com/users/goozakdev/packages/container/obelo-server/settings
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
