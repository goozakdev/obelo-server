# syntax=docker/dockerfile:1

# Multi-stage build for the self-hosted media server, targeting linux/amd64.
#
# Build context is the REPO ROOT (not this directory):
#     docker build -f docker/Dockerfile -t obelo .
#
# Critical build order (ADR-0012): the Vite/React SPA bundle must be produced
# BEFORE `go build`, because the Go binary embeds internal/webui/dist via
# `go:embed`. The stages below enforce that: `web` builds the bundle, `build`
# copies it in and then compiles the binary that embeds it.
#
# The server is pure Go (modernc.org/sqlite — no CGO), so it cross-compiles to a
# static amd64 binary from any build host and runs on a minimal Alpine runtime.
# ffmpeg/ffprobe are shelled out at runtime (ADR-0002) and are installed into
# the final image.

# ---------------------------------------------------------------------------
# Stage 1 — web: build the SPA bundle into internal/webui/dist.
# Runs on the native build platform (JS output is architecture-independent).
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web

# Install deps first (cached until the lockfile changes).
COPY web/package.json web/package-lock.json ./
RUN npm ci

# Build the bundle. Vite's outDir is ../internal/webui/dist (see web/vite.config.ts),
# so this emits into /src/internal/webui/dist, the go:embed target.
COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2 — build: compile the Go binary for linux/amd64 with the embedded SPA.
# Runs on the native build platform and cross-compiles (pure Go, CGO disabled).
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Module deps first (cached until go.mod/go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Source tree. `internal` carries the committed placeholder dist/index.html,
# which the next COPY overwrites with the real bundle from the web stage.
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=web /src/internal/webui/dist/ ./internal/webui/dist/

# Default metadata credentials (ADR-0032), injected only for OFFICIAL image builds
# from CI secrets — EMPTY by default so a plain `docker build` bundles no keys and
# stays credential-free. Pass them as build args:
#   --build-arg OBELO_BOOTSTRAP_TMDB_KEY=<plaintext tmdb key>
#   --build-arg OBELO_BOOTSTRAP_FANART_KEY=<plaintext fanart.tv key>
#   --build-arg OBELO_APP_ENC_KEY=<base64 AES-256-GCM key>
#   --build-arg OBELO_ROTATION_URL=<https://host/v1/keys>
# The two provider keys are base64-obfuscated below (so `strings` on the binary
# yields no bare key); kAppEncKey and the rotation URL are injected verbatim. The URL
# is not a secret (ciphertext-only, public in the binary) but is injected so the
# maintainer host stays out of the open-source repo. The RUNTIME override is the
# separate OBELO_KEY_ROTATION_URL env var.
ARG OBELO_BOOTSTRAP_TMDB_KEY=""
ARG OBELO_BOOTSTRAP_FANART_KEY=""
ARG OBELO_APP_ENC_KEY=""
ARG OBELO_ROTATION_URL=""

# Static amd64 binary. -trimpath + -ldflags "-s -w" shrink it; no CGO needed. The
# -X flags inject the resolved default credentials (empty unless the args are set).
#
# `-tags tailscale` (ADR-0043) links the embedded Tailnet node, so the SHIPPED
# image can join the operator's Tailnet; a plain `go build` cannot, and that is
# deliberate. Nothing else about this image changes for it, and that is the point
# of choosing `tsnet` over an external daemon:
#
#   NO new capabilities. NO /dev/net/tun. NO NET_ADMIN. `USER obelo` stays.
#
# The node runs userspace WireGuard over a gVisor network stack inside the server
# process, which needs none of those. If a future change appears to need any of
# them, the approach is wrong — not this Dockerfile.
RUN CONFIG_PKG=github.com/marioquake/obelo-server/internal/config; \
    TMDB_OBF=$(printf %s "$OBELO_BOOTSTRAP_TMDB_KEY" | base64 | tr -d '\n'); \
    FANART_OBF=$(printf %s "$OBELO_BOOTSTRAP_FANART_KEY" | base64 | tr -d '\n'); \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -tags tailscale -ldflags="-s -w \
      -X ${CONFIG_PKG}.bootstrapTMDBKey=${TMDB_OBF} \
      -X ${CONFIG_PKG}.bootstrapFanartKey=${FANART_OBF} \
      -X ${CONFIG_PKG}.kAppEncKey=${OBELO_APP_ENC_KEY} \
      -X ${CONFIG_PKG}.DefaultKeyRotationURL=${OBELO_ROTATION_URL}" \
      -o /out/obelo ./cmd/obelo

# ---------------------------------------------------------------------------
# Stage 3 — runtime: amd64 image with ffmpeg + the GPU userspace + the binary.
# The platform is pinned to a constant on purpose: this guarantees an amd64
# image even when built on an arm64 host (matching the amd64 binary compiled
# above). BuildKit emits a cosmetic "const platform" warning for this — that is
# expected and the build is correct.
#
# WHY DEBIAN AND NOT ALPINE (ADR-0042): the runtime base is glibc because NVENC
# requires it. Alpine's ffmpeg is built with NO h264_nvenc at all, so ADR-0009's
# setup-time detection could never validate step 1 (encoder compiled in) and
# OBELO_HARDWARE_ACCEL=nvenc always warned and fell back to CPU. Rebuilding
# ffmpeg would not have been enough: the NVIDIA Container Toolkit injects the
# HOST's glibc-linked driver libraries (libnvidia-encode.so, libcuda.so,
# nvidia-smi) into the container, and those cannot load into a musl process. A
# glibc base is the only way an NVIDIA card works here.
# ---------------------------------------------------------------------------
FROM --platform=linux/amd64 debian:trixie-slim AS runtime

# ffmpeg bundles ffprobe; ca-certificates for outbound HTTPS (TMDB/MusicBrainz);
# tzdata so scheduled scans honor the configured timezone.
#
# The rest is the GPU userspace for the backends ADR-0009 names. NVENC needs
# nothing installed — Debian's ffmpeg carries h264_nvenc and dlopen()s the
# driver the container toolkit injects at run time. VAAPI and QSV do: libva
# finds nothing without a VA DRIVER, and the previous Alpine image shipped
# libva with an EMPTY /usr/lib/dri, so those two backends failed their
# test-encode on every host regardless of the card fitted.
#   mesa-va-drivers       — AMD (radeonsi) + the Intel gallium fallback
#   intel-media-va-driver — Intel iHD, Gen8+ (the QSV/VAAPI path on modern Intel)
#   libvpl2               — the oneVPL dispatcher ffmpeg 7's h264_qsv links
#   libmfx-gen1.2         — the oneVPL GPU runtime the dispatcher then loads
#
# Pre-Gen8 Intel (the legacy i965 driver) is deliberately NOT installed: those
# parts are old enough that CPU libx264 is the realistic path, and iHD dropped
# them. Such a host validates nothing and falls back to CPU, as designed.
RUN set -eux; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ffmpeg \
      ca-certificates \
      tzdata \
      mesa-va-drivers \
      intel-media-va-driver \
      libvpl2 \
      libmfx-gen1.2 \
      procps \
    ; \
    rm -rf /var/lib/apt/lists/*

# /dev/dri/renderD* is root:render 0660, so the unprivileged runtime user needs
# both groups to open a VAAPI/QSV device. `render` is not guaranteed to exist in
# a slim base, hence the create-if-missing. This only fixes the CONTAINER side:
# the host's render GID often differs, so the run docs still tell operators to
# pass --group-add with the host value.
RUN set -eux; \
    useradd --system --no-create-home --shell /usr/sbin/nologin --uid 10001 obelo; \
    getent group render >/dev/null || groupadd --system render; \
    usermod -aG video,render obelo

# NVIDIA Container Toolkit capability request (NVENC + nvidia-smi telemetry).
# `--gpus all` selects which DEVICES are visible but does NOT widen the driver
# capabilities, and the default set omits `video` — without which
# libnvidia-encode is never injected and h264_nvenc fails its test-encode even
# on a correctly configured host. `utility` is what puts nvidia-smi on PATH for
# the ADR-0029 GPU telemetry block. Harmless on non-NVIDIA hosts: with no NVIDIA
# runtime in play these are inert environment variables.
ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility

COPY --from=build /out/obelo /usr/local/bin/obelo

# The single writable data dir (SQLite DB, artwork cache). Mount a volume here.
ENV OBELO_DATA_DIR=/data \
    OBELO_LISTEN_ADDR=:8080
RUN mkdir -p /data && chown obelo:obelo /data
VOLUME /data

USER obelo
EXPOSE 8080

ENTRYPOINT ["obelo"]
