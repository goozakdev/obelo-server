# Juice Box build orchestration.
#
# Critical build order (ADR-0012): the frontend bundle must be built into
# internal/webui/dist BEFORE `go build`, because the Go binary embeds it via
# go:embed. `make build` enforces that order. A missing embed dir is a Go
# COMPILE error (the desired loud failure); a committed placeholder keeps plain
# `go build ./...` working without a Node toolchain, and `make check-bundle`
# fails loudly if a real bundle was never built in.

WEB_DIR := web
EMBED_DIR := internal/webui/dist
BIN := bin/juicebox

# Build-time default metadata credentials (ADR-0032). EMPTY here and in source: a
# plain `make build` bundles NO keys, so a build-from-source binary is credential-
# free and uses BYOK. OFFICIAL builds export these from CI secrets:
#   JUICEBOX_BOOTSTRAP_TMDB_KEY / _FANART_KEY  — the plaintext provider keys
#   JUICEBOX_APP_ENC_KEY                        — the base64 AES-256-GCM rotation key
#   JUICEBOX_ROTATION_URL                       — the maintainer rotation endpoint
# The two provider keys are base64-OBFUSCATED here (a speed bump so `strings` on
# the binary yields no bare key); kAppEncKey and the URL are injected as-is (the URL
# is not a secret — it's ciphertext-only and public in the binary — but is injected
# so the maintainer host stays out of the open-source repo, not GitHub-searchable).
# Note JUICEBOX_ROTATION_URL is the BUILD-time default; the RUNTIME override is the
# separate JUICEBOX_KEY_ROTATION_URL env var read by config.FromEnv.
# `printf %s "" | base64` is empty, so an unset key injects an empty string.
CONFIG_PKG := github.com/marioquake/juicebox/internal/config
BOOTSTRAP_TMDB_OBF   := $(shell printf %s "$(JUICEBOX_BOOTSTRAP_TMDB_KEY)" | base64 | tr -d '\n')
BOOTSTRAP_FANART_OBF := $(shell printf %s "$(JUICEBOX_BOOTSTRAP_FANART_KEY)" | base64 | tr -d '\n')
LDFLAGS := -X $(CONFIG_PKG).bootstrapTMDBKey=$(BOOTSTRAP_TMDB_OBF) \
           -X $(CONFIG_PKG).bootstrapFanartKey=$(BOOTSTRAP_FANART_OBF) \
           -X $(CONFIG_PKG).kAppEncKey=$(JUICEBOX_APP_ENC_KEY) \
           -X $(CONFIG_PKG).DefaultKeyRotationURL=$(JUICEBOX_ROTATION_URL)

.PHONY: all build web go-build keytool run test test-go test-e2e check-bundle check-credentials-free fmt clean

all: build

## build: frontend bundle first, then the Go binary that embeds it.
build: web go-build

## web: install deps (if needed) and produce the SPA bundle into the embed dir.
web:
	cd $(WEB_DIR) && npm install && npm run build

## go-build: compile the binary (assumes the bundle is already built into $(EMBED_DIR)).
## Injects the default metadata credentials (ADR-0032) via -ldflags -X — empty
## unless the JUICEBOX_BOOTSTRAP_* / JUICEBOX_APP_ENC_KEY env vars are set (CI).
go-build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/juicebox

## keytool: build the offline maintainer key-rotation CLI (ADR-0032). Seals default
## provider keys into the rotation envelope for the runbook — never bundles a secret,
## needs no ldflags. See docs/runbooks/metadata-key-rotation.md.
keytool:
	go build -o bin/keytool ./cmd/keytool

## run: build everything, then run the server.
run: build
	$(BIN)

## test: the whole suite — Go tests then the Playwright E2E (which builds+boots the binary).
test: test-go test-e2e

## test-go: Go unit/integration tests (uses the committed placeholder bundle).
test-go:
	go test ./...

## test-e2e: Playwright browser smoke (builds the frontend + real binary, boots it).
test-e2e:
	cd $(WEB_DIR) && npm run test:e2e

## check-bundle: fail loudly if the embedded bundle is the placeholder, not a real build.
check-bundle:
	@go run ./internal/webui/cmd/checkbundle

## check-credentials-free: fail if a bundled-credential var carries a non-empty
## literal in source (ADR-0032) — the repo must be credential-free against
## scrapers. The authoritative gate is TestBootstrapVarsEmptyInSource (an AST
## check run by `make test-go`); this grep is the fast standalone CI guard.
check-credentials-free:
	@if grep -nE '(bootstrapTMDBKey|bootstrapFanartKey|kAppEncKey)[[:space:]]+string[[:space:]]*=[[:space:]]*"[^"]' internal/config/bootstrap.go; then \
	  echo "ERROR: a bundled-credential var has a non-empty literal in source (ADR-0032)"; exit 1; \
	else echo "ok: bundled-credential vars are empty in source"; fi

## fmt: gofmt the Go tree.
fmt:
	gofmt -w .

## clean: remove build outputs (keeps the committed placeholder index.html).
clean:
	rm -rf $(BIN) $(WEB_DIR)/node_modules $(WEB_DIR)/dist
	git checkout -- $(EMBED_DIR) 2>/dev/null || true
