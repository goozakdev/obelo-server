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

# Static amd64 binary. -trimpath + -ldflags "-s -w" shrink it; no CGO needed.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/juicebox ./cmd/juicebox

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
