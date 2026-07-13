# syntax=docker/dockerfile:1

# Multi-stage build for the self-hosted media server, targeting linux/amd64.
#
# Build context is the REPO ROOT (not this directory):
#     docker build -f docker/Dockerfile -t juicebox .
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
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
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
#   --build-arg JUICEBOX_BOOTSTRAP_TMDB_KEY=<plaintext tmdb key>
#   --build-arg JUICEBOX_BOOTSTRAP_FANART_KEY=<plaintext fanart.tv key>
#   --build-arg JUICEBOX_APP_ENC_KEY=<base64 AES-256-GCM key>
#   --build-arg JUICEBOX_ROTATION_URL=<https://host/v1/keys>
# The two provider keys are base64-obfuscated below (so `strings` on the binary
# yields no bare key); kAppEncKey and the rotation URL are injected verbatim. The URL
# is not a secret (ciphertext-only, public in the binary) but is injected so the
# maintainer host stays out of the open-source repo. The RUNTIME override is the
# separate JUICEBOX_KEY_ROTATION_URL env var.
ARG JUICEBOX_BOOTSTRAP_TMDB_KEY=""
ARG JUICEBOX_BOOTSTRAP_FANART_KEY=""
ARG JUICEBOX_APP_ENC_KEY=""
ARG JUICEBOX_ROTATION_URL=""

# Static amd64 binary. -trimpath + -ldflags "-s -w" shrink it; no CGO needed. The
# -X flags inject the resolved default credentials (empty unless the args are set).
RUN CONFIG_PKG=github.com/marioquake/juicebox/internal/config; \
    TMDB_OBF=$(printf %s "$JUICEBOX_BOOTSTRAP_TMDB_KEY" | base64 | tr -d '\n'); \
    FANART_OBF=$(printf %s "$JUICEBOX_BOOTSTRAP_FANART_KEY" | base64 | tr -d '\n'); \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w \
      -X ${CONFIG_PKG}.bootstrapTMDBKey=${TMDB_OBF} \
      -X ${CONFIG_PKG}.bootstrapFanartKey=${FANART_OBF} \
      -X ${CONFIG_PKG}.kAppEncKey=${JUICEBOX_APP_ENC_KEY} \
      -X ${CONFIG_PKG}.DefaultKeyRotationURL=${JUICEBOX_ROTATION_URL}" \
      -o /out/juicebox ./cmd/juicebox

# ---------------------------------------------------------------------------
# Stage 3 — runtime: minimal amd64 image with ffmpeg + the binary.
# The platform is pinned to a constant on purpose: this guarantees an amd64
# image even when built on an arm64 host (matching the amd64 binary compiled
# above). BuildKit emits a cosmetic "const platform" warning for this — that is
# expected and the build is correct.
# ---------------------------------------------------------------------------
FROM --platform=linux/amd64 alpine:3.20 AS runtime

# ffmpeg bundles ffprobe; ca-certificates for outbound HTTPS (TMDB/MusicBrainz);
# tzdata so scheduled scans honor the configured timezone.
RUN apk add --no-cache ffmpeg ca-certificates tzdata \
    && adduser -D -H -u 10001 juicebox

COPY --from=build /out/juicebox /usr/local/bin/juicebox

# The single writable data dir (SQLite DB, artwork cache). Mount a volume here.
ENV JUICEBOX_DATA_DIR=/data \
    JUICEBOX_LISTEN_ADDR=:8080
RUN mkdir -p /data && chown juicebox:juicebox /data
VOLUME /data

USER juicebox
EXPOSE 8080

ENTRYPOINT ["juicebox"]
