# syntax=docker/dockerfile:1
# check=skip=FromPlatformFlagConstDisallowed
#
# The skip above is for the runtime stage's `FROM --platform=linux/amd64`, which is
# a deliberate pin (see stage 3). The OTHER check this file used to trip —
# SecretsUsedInArgOrEnv — is NOT skipped: the credential ARGs became `--mount=type=secret`
# in stage 2, so the warning is fixed rather than silenced. If it ever fires again,
# something regressed; do not add it here.

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
# from CI secrets — ABSENT by default so a plain `docker build` bundles no keys and
# stays credential-free.
#
# The three CREDENTIALS are BuildKit secrets, not build args, and that distinction is
# load-bearing. As `ARG`s they tripped SecretsUsedInArgOrEnv, and while the leak that
# check warns about did not actually reach the final image (these live in the `build`
# stage; `runtime` only copies the binary out, so `docker history`/`docker inspect` on
# the shipped image showed nothing), two real exposures remained: the resolved values
# were written into BuildKit's build cache in plaintext — and would have been PUSHED by
# any `--cache-to type=registry` — and `--build-arg KEY=<plaintext>` puts the key in
# `ps` output, shell history, and one `set -x` away from a CI log.
#
# A `--mount=type=secret` file exists only for the duration of the RUN below: never a
# layer, never image metadata, never the cache. Secrets are `required=false` by default,
# so an unset one yields an absent file and an empty key — the credential-free default.
#
#   docker build -f docker/Dockerfile \
#     --secret id=tmdb_key,env=OBELO_BOOTSTRAP_TMDB_KEY \
#     --secret id=fanart_key,env=OBELO_BOOTSTRAP_FANART_KEY \
#     --secret id=app_enc_key,env=OBELO_APP_ENC_KEY \
#     --build-arg OBELO_ROTATION_URL=<https://host/v1/keys> \
#     -t obelo .
#
# The rotation URL stays an ARG on purpose: it is not a secret (the envelope it serves
# is ciphertext-only and the URL is public in the binary), it is injected only to keep
# the maintainer host out of the open-source repo. The RUNTIME override is the separate
# OBELO_KEY_ROTATION_URL env var.
ARG OBELO_ROTATION_URL=""

# WHAT BUSTS THIS LAYER'S CACHE — read before a key rotation.
#
# Secret VALUES are deliberately excluded from BuildKit's cache key (that is the point
# of a secret). So rotating a key and rebuilding with no other change REUSES the cached
# `go build` layer and silently ships the OLD key, reporting success throughout. That is
# the same shape as the two failures CLAUDE.md records: a guard that passes because it
# did not run.
#
# CACHE_EPOCH is the fix and is referenced in the RUN body purely so it participates in
# the cache key. Bump it whenever an injected credential changes (a date is fine:
# --build-arg CACHE_EPOCH=2026-08-22). It is named for the MECHANISM, not for keys, and
# deliberately so: as `KEY_EPOCH` it tripped SecretsUsedInArgOrEnv on the substring
# "KEY" alone — a false positive, but one that would have re-taught everyone to ignore
# this file's warnings, which is how the real one hid. docs/runbooks/metadata-key-rotation.md instructs
# this at the point of rotation; `--no-cache-filter build` is the belt-and-suspenders
# alternative if you would rather not think about it.
ARG CACHE_EPOCH=""

# Static amd64 binary. -trimpath + -ldflags "-s -w" shrink it; no CGO needed. The
# -X flags inject the resolved default credentials (empty unless the secrets are set).
# The two provider keys are base64-obfuscated (so `strings` on the binary yields no
# bare key); kAppEncKey and the rotation URL are injected verbatim.
#
# The obfuscation is a SPEED BUMP, not secrecy: anyone holding the image can base64-
# decode these back out. That is ADR-0032's accepted tradeoff and the reason the key-
# rotation Worker exists at all. Nothing in this file changes that — the secret mounts
# above protect the BUILD HOST and the build cache, not the shipped artifact.
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
RUN --mount=type=secret,id=tmdb_key \
    --mount=type=secret,id=fanart_key \
    --mount=type=secret,id=app_enc_key \
    set -eu; \
    : "cache-epoch ${CACHE_EPOCH}"; \
    read_secret() { if [ -f "$1" ]; then cat "$1"; fi; }; \
    CONFIG_PKG=github.com/goozakdev/obelo-server/internal/config; \
    TMDB_OBF=$(read_secret /run/secrets/tmdb_key | base64 | tr -d '\n'); \
    FANART_OBF=$(read_secret /run/secrets/fanart_key | base64 | tr -d '\n'); \
    APP_ENC_KEY=$(read_secret /run/secrets/app_enc_key); \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -tags tailscale -ldflags="-s -w \
      -X ${CONFIG_PKG}.bootstrapTMDBKey=${TMDB_OBF} \
      -X ${CONFIG_PKG}.bootstrapFanartKey=${FANART_OBF} \
      -X ${CONFIG_PKG}.kAppEncKey=${APP_ENC_KEY} \
      -X ${CONFIG_PKG}.DefaultKeyRotationURL=${OBELO_ROTATION_URL}" \
      -o /out/obelo ./cmd/obelo

# ---------------------------------------------------------------------------
# Stage 3 — runtime: amd64 image with ffmpeg + the GPU userspace + the binary.
# The platform is pinned to a constant on purpose: it guarantees amd64 CONTENT —
# an amd64 Debian base to match the amd64 binary compiled above — even when built
# on an arm64 host. BuildKit's FromPlatformFlagConstDisallowed check flags this;
# the `# check=skip=` directive at the top of the file silences that ONE check,
# since the pin is the intended behaviour.
#
# THE PIN DOES NOT LABEL THE OUTPUT, AND YOU MUST STILL PASS `--platform` TO BUILD.
#
# `FROM --platform=` chooses this stage's BASE IMAGE. It does not set the platform
# BuildKit stamps on the resulting manifest DESCRIPTOR — that comes from the build
# request, which defaults to the build host. So on an arm64 machine:
#
#   docker build                          -> descriptor linux/arm64, config amd64
#   docker build --platform linux/amd64   -> descriptor linux/amd64, config amd64
#
# Both produce the IDENTICAL manifest; only the label differs. That split is why
# this hid for so long: `docker run` reads the CONFIG (amd64, so it works locally
# and even warns that the image is amd64), while `docker push` and `docker pull`
# select on the DESCRIPTOR (arm64, so the registry serves an "arm image" that is
# really amd64 bytes). Every local test passes while the published artifact is
# wrong — the same shape as the two failures in CLAUDE.md, for the third time.
#
# There is no Dockerfile-side fix; the output platform belongs to the build
# request. Release builds MUST pass `--platform linux/amd64`, and the check that
# catches a regression is `docker image ls --tree <tag>` — read the `└─` line, not
# `docker inspect .Architecture`, which is the field that lies. See docker/README.md.
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
