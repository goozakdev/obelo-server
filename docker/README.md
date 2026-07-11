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
