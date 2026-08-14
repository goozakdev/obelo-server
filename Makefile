# Obelo build orchestration.
#
# Critical build order (ADR-0012): the frontend bundle must be built into
# internal/webui/dist BEFORE `go build`, because the Go binary embeds it via
# go:embed. `make build` enforces that order. A missing embed dir is a Go
# COMPILE error (the desired loud failure); a committed placeholder keeps plain
# `go build ./...` working without a Node toolchain, and `make check-bundle`
# fails loudly if a real bundle was never built in.

WEB_DIR := web
EMBED_DIR := internal/webui/dist
BIN := bin/obelo

# Build-time default metadata credentials (ADR-0032). EMPTY here and in source: a
# plain `make build` bundles NO keys, so a build-from-source binary is credential-
# free and uses BYOK. OFFICIAL builds export these from CI secrets:
#   OBELO_BOOTSTRAP_TMDB_KEY / _FANART_KEY  — the plaintext provider keys
#   OBELO_APP_ENC_KEY                        — the base64 AES-256-GCM rotation key
#   OBELO_ROTATION_URL                       — the maintainer rotation endpoint
# The two provider keys are base64-OBFUSCATED here (a speed bump so `strings` on
# the binary yields no bare key); kAppEncKey and the URL are injected as-is (the URL
# is not a secret — it's ciphertext-only and public in the binary — but is injected
# so the maintainer host stays out of the open-source repo, not GitHub-searchable).
# Note OBELO_ROTATION_URL is the BUILD-time default; the RUNTIME override is the
# separate OBELO_KEY_ROTATION_URL env var read by config.FromEnv.
# `printf %s "" | base64` is empty, so an unset key injects an empty string.
# The `tailscale` build tag (ADR-0043). RELEASE artifacts carry it — the Docker
# image and the release binaries — so the shipped server can join the operator's
# Tailnet; the default `go-build` target does NOT, so day-to-day development stays
# on a fast loop and go.mod's small-dependency character stays visible to anyone
# who wanted a media server and not a VPN.
#
# TAGS is overridable per invocation (`make test-go TAGS=tailscale`), and the
# `check` target below deliberately runs the Go suite BOTH ways rather than once.
TAGS ?=
GOTAGS := $(if $(TAGS),-tags $(TAGS),)
RELEASE_TAGS := tailscale

CONFIG_PKG := github.com/marioquake/obelo-server/internal/config
BOOTSTRAP_TMDB_OBF   := $(shell printf %s "$(OBELO_BOOTSTRAP_TMDB_KEY)" | base64 | tr -d '\n')
BOOTSTRAP_FANART_OBF := $(shell printf %s "$(OBELO_BOOTSTRAP_FANART_KEY)" | base64 | tr -d '\n')
LDFLAGS := -X $(CONFIG_PKG).bootstrapTMDBKey=$(BOOTSTRAP_TMDB_OBF) \
           -X $(CONFIG_PKG).bootstrapFanartKey=$(BOOTSTRAP_FANART_OBF) \
           -X $(CONFIG_PKG).kAppEncKey=$(OBELO_APP_ENC_KEY) \
           -X $(CONFIG_PKG).DefaultKeyRotationURL=$(OBELO_ROTATION_URL)

.PHONY: all build build-release web go-build go-build-release keytool run test test-go test-go-tailscale test-e2e check check-fmt vet vet-tailscale check-placeholder check-bundle check-credentials-free fmt clean

all: build

## build: frontend bundle first, then the Go binary that embeds it.
build: web go-build

## web: install deps (if needed) and produce the SPA bundle into the embed dir.
web:
	cd $(WEB_DIR) && npm install && npm run build

## go-build: compile the binary (assumes the bundle is already built into $(EMBED_DIR)).
## Injects the default metadata credentials (ADR-0032) via -ldflags -X — empty
## unless the OBELO_BOOTSTRAP_* / OBELO_APP_ENC_KEY env vars are set (CI).
##
## NO BUILD TAGS by default (ADR-0043): this is the development build, and it links
## no `tailscale.com`. Use `make build-release` (or TAGS=tailscale) for the shipped
## artifact. The two differ in exactly one advertised capability —
## GET /server's features.tailscale — and in nothing else.
go-build:
	go build $(GOTAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/obelo

## build-release: the artifact that ships — the SPA bundle plus the Go binary WITH
## the `tailscale` tag (ADR-0043), matching what docker/Dockerfile produces.
build-release: web go-build-release

## go-build-release: go-build with the release tags.
go-build-release:
	$(MAKE) go-build TAGS="$(RELEASE_TAGS)"

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
	go test $(GOTAGS) ./...

## test-go-tailscale: the same suite with the `tailscale` build tag — the OTHER
## half of the matrix, and the variant that actually ships (ADR-0043).
test-go-tailscale:
	$(MAKE) test-go TAGS="$(RELEASE_TAGS)"

## test-e2e: Playwright browser smoke (builds the frontend + real binary, boots it).
test-e2e:
	cd $(WEB_DIR) && npm run test:e2e

## check: the pre-commit gate — run this before every commit.
##
## Cheap guards first so a failure arrives in seconds rather than after the Go
## suite. Deliberately NOT including check-bundle or test-e2e: both want a REAL
## frontend bundle built in, which is the opposite of what check-placeholder
## requires, and both are release-time concerns rather than commit-time ones.
##
## IT RUNS THE GO SUITE TWICE, WITH AND WITHOUT `-tags tailscale`, AND THAT IS NOT
## NEGOTIABLE (ADR-0043). Two variants of this binary exist forever now: a default
## build with no Tailnet support and a release build with it. This is the same
## shape as the index.html guard described in CLAUDE.md — two guards wanting
## opposite things, one side automated — which was wrong for a month while the
## automated side reported success throughout. If only one variant is exercised
## here, it will be the one users do not run: the shipped Docker image carries the
## tag, and a plain `go test ./...` does not.
check: check-fmt vet vet-tailscale check-placeholder check-credentials-free test-go test-go-tailscale

## check-fmt: fail if anything is not gofmt-clean. Run `make fmt` to fix.
## This exists because nothing enforced formatting and it silently drifted to
## eleven files across six packages before anyone noticed.
check-fmt:
	@files=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$files" ]; then \
	  echo "ERROR: not gofmt-clean (run 'make fmt'):"; echo "$$files"; exit 1; \
	else echo "ok: gofmt clean"; fi

## vet: go vet over the whole module.
vet:
	@go vet $(GOTAGS) ./... && echo "ok: go vet clean"

## vet-tailscale: go vet over the tagged variant. A file behind a build tag is
## invisible to the default vet, which is the whole problem with build tags.
vet-tailscale:
	@$(MAKE) --no-print-directory vet TAGS="$(RELEASE_TAGS)"

## check-placeholder: fail if the bundle that WOULD BE COMMITTED is a real build
## rather than the placeholder (CLAUDE.md "Build artifacts").
##
## This is the exact OPPOSITE of check-bundle below, and both are correct: a
## RELEASE must not ship the placeholder, and a COMMIT must not ship a real
## build. Only the release side was ever automated, and the commit side rotted
## once already — between 2026-07 and 2026-08-08 the committed index.html
## referenced /assets/index-ijKWYbjt.js, an asset .gitignore had never let anyone
## commit, so a fresh clone served a blank page requesting two 404s while
## check-bundle reported success throughout. This target is the missing half.
##
## It inspects git's INDEX (`git show :path`), not the working tree, and that is
## deliberate: a developer running the app locally MUST have a real bundle on
## disk, and a check that failed for them would be disabled within a week. What
## matters is only what gets committed.
check-placeholder:
	@git rev-parse --git-dir >/dev/null 2>&1 || { \
	  echo "ERROR: not a git repository — cannot inspect the committed bundle"; exit 1; }
	@if git show :$(EMBED_DIR)/index.html 2>/dev/null | grep -q 'obelo-spa-placeholder'; then \
	  echo "ok: the committed $(EMBED_DIR)/index.html is the placeholder"; \
	else \
	  echo "ERROR: $(EMBED_DIR)/index.html would be committed as a REAL build."; \
	  echo "       A fresh clone would then serve a blank page (the hashed assets"; \
	  echo "       are gitignored and never committed). Restore it with:"; \
	  echo "         git checkout $(EMBED_DIR)/index.html"; \
	  exit 1; fi

## check-bundle: fail loudly if the embedded bundle is the placeholder, not a real build.
## RELEASE-time guard; the commit-time guard is check-placeholder above, which wants
## the opposite. Do not "reconcile" them.
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
