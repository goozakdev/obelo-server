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

# The linux/amd64 container gate (.scratch/plugin-system issue 18). ADR-0058
# decision 1 buys wazero because its optimizing compiler serves amd64 and arm64
# from one `.wasm`; ADR-0006 ships amd64 (and arm64). Those are TWO wazero
# backends, and until this target existed every Installed plugin call in the suite
# had run on exactly one of them — the development machine's arm64 — while the
# production image executes the other. `CGO_ENABLED=0 GOARCH=amd64 go build ./...`
# was all seven Phase 2 issues could do, and compiling for an architecture is not
# running on it.
#
# AMD64_PKGS is the package list. Widening it is one edit, deliberately: start with
# what touches a guest and grow when the emulated run time says it can.
#
# ./internal/bundled/... and the seven ./plugins/<id>/... joined it in
# .scratch/bundled-plugins issue 08, and they are the reason this target now
# matters more than it did. Until then the only guest compiled and called under
# the amd64 backend was a TEST guest; now the seven modules the server SHIPS are
# built inside the container by the `plugins` recipe below and driven through
# wazero by ./internal/api and ./internal/bundled — which is the proof that the
# image an operator pulls enriches, on the architecture it runs on. `plugins/`
# cannot be spelled `./plugins/...`: a pattern crosses into a workspace module
# only when it names that module's own directory (see GOPKGS), so this is the
# same per-directory wildcard.
AMD64_PKGS ?= ./pluginapi/... ./pluginsdk/... ./internal/plugins/... ./internal/bundled/... \
              ./internal/eventsink/... ./internal/subfetch/... ./internal/enrich/... \
              ./internal/api/ $(PLUGIN_PKGS)
AMD64_IMAGE ?= golang:1.26

# ffmpeg, installed into the container before the run. This is not incidental and it
# is not skippable: internal/api SYNTHESISES its media fixtures with
# `ffmpeg -f lavfi -i testsrc`, and a machine without ffmpeg does not skip those tests
# — it FAILS them. Measured on the first attempt at this target: 364 failures, every
# one an empty fixture library (the scan found 0 titles), not one of them an
# architecture difference. The tests that DO skip politely are a different set; these
# assert on a library the fixture was supposed to have filled.
#
# golang:1.26 ships no ffmpeg. The one apt installs is the same Debian trixie package
# the runtime image installs (ADR-0042), so the gate encodes with the ffmpeg an
# operator actually gets. It costs ~40 s and one apt fetch per run, which is why this
# is a step inside the single `docker run` rather than a second image to maintain.
AMD64_SETUP ?= apt-get update -qq >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends ffmpeg >/dev/null 2>&1 && ffmpeg -version | head -1

# Go's default is 10 minutes PER PACKAGE, and internal/api does not fit in it under
# emulation — it panicked at 10m00s mid-test with nothing wrong, which reads like a
# hang and is not one. Raised rather than narrowed, because the package IS the
# end-to-end plugin coverage: install, uninstall, catalog, signing, the declared
# settings form and the Discord fixture all reach a guest through real HTTP.
#
# RAISED TO 60m for the Bundled plugins (.scratch/bundled-plugins: issue 08).
# internal/api is 462 s natively now that every server it boots installs and
# compiles seven real modules, and MEASURED 2026-09-18 it is 1054 s in the
# container — 2.3x, and 17.6 minutes. That is more than half of the old 30, which
# is close enough that a slower host or a busy one would hit the wall and report a
# hang rather than a failure.
AMD64_TIMEOUT ?= 60m

# One named test, run with -v BEFORE the quiet run, so the output carries proof
# rather than an inference: it compiles the wasm guest from source with the same
# `GOOS=wasip1 GOARCH=wasm go build` a Plugin author runs, instantiates it under
# wazero, and calls into it. A bare list of `ok` lines would not tell you a guest
# had executed at all.
#
# The recipe GREPS for its `--- PASS:` line rather than trusting the exit code, and
# that is not belt-and-braces: `go test -run` whose pattern matches nothing prints a
# warning and EXITS 0. Rename this test, or fumble the shell quoting so the regex
# arrives mangled, and the proof step would go on reporting success while proving
# nothing — which is the exact failure CLAUDE.md records twice and this target exists
# to close a third instance of.
AMD64_PROOF_TEST ?= TestAGuestDeliversASignedDocument

# Named volumes, NOT host cache directories: the container's cache is amd64 object
# code and the host's is arm64. Sharing a directory between them means every
# `make test-go` on the host evicts what the container just built, and vice versa.
AMD64_MOD_CACHE   ?= obelo-go-mod-amd64
AMD64_BUILD_CACHE ?= obelo-go-build-amd64

CONFIG_PKG := github.com/goozakdev/obelo-server/internal/config
BOOTSTRAP_TMDB_OBF   := $(shell printf %s "$(OBELO_BOOTSTRAP_TMDB_KEY)" | base64 | tr -d '\n')
BOOTSTRAP_FANART_OBF := $(shell printf %s "$(OBELO_BOOTSTRAP_FANART_KEY)" | base64 | tr -d '\n')
LDFLAGS := -X $(CONFIG_PKG).bootstrapTMDBKey=$(BOOTSTRAP_TMDB_OBF) \
           -X $(CONFIG_PKG).bootstrapFanartKey=$(BOOTSTRAP_FANART_OBF) \
           -X $(CONFIG_PKG).kAppEncKey=$(OBELO_APP_ENC_KEY) \
           -X $(CONFIG_PKG).DefaultKeyRotationURL=$(OBELO_ROTATION_URL)

# GOPKGS is what `go test` and `go vet` walk, and it is three patterns rather than
# one because this repository is three Go modules joined by go.work (ADR-0059
# decision 9): the server, `pluginapi` and `pluginsdk`.
#
# `./...` DOES NOT COVER A WORKSPACE. Inside a workspace it still expands only to
# the packages of the module the working directory belongs to, so after the module
# split a plain `go test ./...` silently stopped running the contract's own
# round-trip and schema-staleness suites while reporting success — the shape
# CLAUDE.md records. An explicit subdirectory pattern DOES cross into a workspace
# module, which is why these three are spelled out. A module added to go.work
# belongs here too.
#
# A module added to go.work belongs here too — and `./plugins/...` DOES NOT WORK
# for the Bundled plugins: a pattern crosses into a workspace module only when it
# names that module's own directory, so `./plugins/tmdb/...` is covered and
# `./plugins/...` silently matches nothing ("matched no packages", exit 0). The
# wildcard below spells one pattern per plugin directory, so adding a plugin is a
# directory and not a Makefile edit — which matters, because three agents adding
# three plugins in parallel would otherwise conflict here every time.
# The Bundled plugins (ADR-0059, .scratch/bundled-plugins). Each plugins/<id>/ is
# a Go module with a manifest.json beside it; `make plugins` compiles each to
# WebAssembly and writes the module and the manifest into BUNDLED_DIR, which
# internal/bundled embeds. DISCOVERY IS THE FILESYSTEM: a new plugin is a new
# directory with a manifest in it, and nothing here names one.
BUNDLED_DIR := internal/bundled/modules
PLUGIN_DIRS := $(patsubst %/manifest.json,%,$(wildcard plugins/*/manifest.json))
PLUGIN_PKGS := $(patsubst %,./%/...,$(PLUGIN_DIRS))
# Where the uncompressed module lands on its way to being gzipped. NOT inside
# BUNDLED_DIR: that whole directory is embedded, so a stray .wasm left beside the
# .wasm.gz would be compiled into the binary twice over.
PLUGIN_BUILD_DIR := bin/plugins

GOPKGS := ./... ./pluginapi/... ./pluginsdk/... $(PLUGIN_PKGS)

.PHONY: all build build-release web go-build go-build-release plugins keytool pluginsign run test test-go test-go-tailscale test-go-amd64 test-go-amd64-tailscale amd64-pkgs test-web test-e2e check check-amd64 check-release check-fmt vet vet-tailscale check-placeholder check-bundle check-no-bundled-modules-tracked check-credentials-free check-web fmt clean

all: build

## build: the Bundled plugin modules and the frontend bundle first, then the Go
## binary that embeds both.
build: plugins web go-build

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

## build-release: the artifact that ships — the Bundled plugin modules, the SPA
## bundle, and the Go binary WITH the `tailscale` tag (ADR-0043), matching what
## docker/Dockerfile produces.
build-release: plugins web go-build-release

## go-build-release: go-build with the release tags.
go-build-release:
	$(MAKE) go-build TAGS="$(RELEASE_TAGS)"

## plugins: build every Bundled plugin under plugins/*/ into internal/bundled/modules
## (ADR-0059 decision 10, .scratch/bundled-plugins).
##
## THE RECIPE IS A SCRIPT AND NOT A LOOP HERE, deliberately. docker/Dockerfile has
## to build the same seven modules before `go build` — internal/bundled/modules/ is
## gitignored, so an image built without that step compiles, boots, scans and
## silently enriches NOTHING — and for one release that loop existed twice, in this
## file and in that one, with a comment asking the next person to keep them in
## step. scripts/build-bundled-plugins.sh is now the single definition, and the
## Dockerfile's `plugins` stage runs the same file. Read it for why the build
## command is spelled the way it is (.scratch/bundled-plugins: issue 08).
plugins:
	@./scripts/build-bundled-plugins.sh $(BUNDLED_DIR) $(PLUGIN_BUILD_DIR)

## keytool: build the offline maintainer key-rotation CLI (ADR-0032). Seals default
## provider keys into the rotation envelope for the runbook — never bundles a secret,
## needs no ldflags. See docs/runbooks/metadata-key-rotation.md.
keytool:
	go build -o bin/keytool ./cmd/keytool

## pluginsign: build the plugin-signing CLI (.scratch/plugin-system issue 15).
## Makes a publisher key pair, signs a plugin's manifest + module with it, and
## verifies the result through the SAME package the server verifies with
## (internal/plugins/signing) — which is what makes "the tool's output installs"
## a property rather than a hope.
##
## It is NOT keytool. keytool seals the maintainer's provider keys into an
## AES-GCM envelope for rotation (ADR-0032): symmetric, one channel, one
## recipient. This is asymmetric authentication of a public artifact by an author
## this project has never met. Two commands, deliberately.
pluginsign:
	go build -o bin/pluginsign ./cmd/pluginsign

## run: build everything, then run the server.
run: build
	$(BIN)

## test: the whole suite — Go tests, the web component suite, then the Playwright
## E2E (which builds+boots the binary).
test: test-go test-web test-e2e

## test-go: Go unit/integration tests (uses the committed placeholder bundle).
##
## THE -timeout IS NOT DECORATION (.scratch/bundled-plugins: issue 08). Go's
## default is ten minutes PER PACKAGE and internal/api had quietly grown to within
## a minute of it: one module made it 325 s, seven make it ~530 s, and every server
## that package boots — several hundred of them — installs all seven Bundled
## plugins and compiles them under wazero. A suite that panics at 10m00s mid-test
## reads like a hang and is not one; it is the same failure the amd64 target
## already hit and raised for, one architecture over. The number is deliberately
## generous rather than snug, because the machine that will hit it first is a
## slower one than this or a busy one, and a gate that fails on a laptop under load
## teaches people to pass -timeout themselves.
TEST_TIMEOUT ?= 30m

test-go:
	go test $(GOTAGS) -timeout $(TEST_TIMEOUT) $(GOPKGS)

## test-go-tailscale: the same suite with the `tailscale` build tag — the OTHER
## half of the matrix, and the variant that actually ships (ADR-0043).
test-go-tailscale:
	$(MAKE) test-go TAGS="$(RELEASE_TAGS)"

## test-go-amd64: run the plugin-facing packages INSIDE a linux/amd64 container, so
## wazero's amd64 compiler backend actually executes a guest (.scratch/plugin-system
## issue 18). Needs a running Docker daemon and nothing else — no amd64 toolchain on
## the host, no TinyGo, no ffmpeg.
##
## THIS IS THE THIRD INSTANCE OF THE SHAPE CLAUDE.md RECORDS, and it is why this is a
## target rather than a note in an ADR. Issues 08, 09, 11, 12, 13, 15 and 16 each ended
## by saying execution on amd64 was unverified and "should be a CI job", and each of
## them was green throughout: `CGO_ENABLED=0 GOARCH=amd64 go build ./...` passed every
## time, reporting success about a property it does not test. A Plugin is the one thing
## in this server COMPILED AT RUN TIME, by a backend wazero picks per architecture
## (ADR-0058 decision 1), so "it cross-compiles" says nothing whatever about the
## machine the image ships to — which is the amd64 one (ADR-0006).
##
## It is NOT part of `make check`, on purpose. `check` is the pre-commit gate and must
## keep running on a laptop with Docker closed; this needs a daemon and takes minutes
## under emulation. `check-amd64` below is the release-side gate instead, and
## docker/README.md's publish checklist calls it — beside the image build, which is the
## only reason the architecture matters at all.
##
## Override AMD64_PKGS to widen or narrow it; override TAGS for the shipped variant (or
## use test-go-amd64-tailscale).
##
## IT BUILDS THE SEVEN BUNDLED PLUGINS INSIDE THE CONTAINER, with the same script
## `make plugins` runs, before any test (.scratch/bundled-plugins: issue 08). That
## is the whole reason this target is worth more than it was: until now the only
## guest ever compiled and called under the amd64 backend was a TEST guest, and the
## modules the server actually SHIPS ran on exactly one architecture — the
## development machine's — while the image runs the other. The build writes into
## the mounted tree, so a run leaves the host's internal/bundled/modules/ holding
## container-built (linux/amd64 toolchain) modules; they are gitignored build
## output and valid either way, and `make plugins` puts the host's back.
##
## MEASURED 2026-09-17 on an arm64 Mac, Docker 29: 14 min 47 s cold, of which
## internal/api is 835 s (315 s native, so emulation costs 2.7x) and the ffmpeg install
## ~40 s. A SECOND run with no source change is 47 s, because Go's test cache answers
## for every package — the proof test above is `-count=1` precisely so that something
## still really runs. Everything passes, with no behavioural difference between
## wazero's two backends; the full result is the ADR-0058 "Carried out" note.
## RE-MEASURED 2026-09-18, with the seven real modules built inside the container and
## in the package list: `make check-amd64` (both halves) is 37 min 51 s, of which
## internal/api is 1054 s against 462 s natively. Everything still passes and nothing
## behaves differently between the two backends — which is now a statement about the
## providers an operator depends on, not only about a test guest.
test-go-amd64:
	@docker info >/dev/null 2>&1 || { \
	  echo "ERROR: test-go-amd64 needs a RUNNING DOCKER DAEMON."; \
	  echo "       It runs the suite inside a --platform linux/amd64 $(AMD64_IMAGE)"; \
	  echo "       container, because there is no other way to execute amd64 code on an"; \
	  echo "       arm64 host — and executing it is the entire point of this target."; \
	  echo "       Start Docker and run it again. 'make check' does NOT need Docker."; \
	  exit 1; }
	docker run --rm --platform linux/amd64 \
	  -v "$(CURDIR)":/src -w /src \
	  -v $(AMD64_MOD_CACHE):/go/pkg/mod \
	  -v $(AMD64_BUILD_CACHE):/root/.cache/go-build \
	  -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
	  $(AMD64_IMAGE) sh -c 'set -e; \
	    echo "container kernel arch: $$(uname -m)"; \
	    go version; \
	    $(AMD64_SETUP); \
	    ./scripts/build-bundled-plugins.sh $(BUNDLED_DIR) $(PLUGIN_BUILD_DIR); \
	    go test $(GOTAGS) -count=1 -v -run "^$(AMD64_PROOF_TEST)\$$" ./internal/plugins/ | tee /proof.log; \
	    grep -q "^--- PASS: $(AMD64_PROOF_TEST)" /proof.log || { \
	      echo "ERROR: $(AMD64_PROOF_TEST) did not PASS, so no guest was compiled and"; \
	      echo "       called under the amd64 backend. A -run that matches nothing exits 0."; \
	      exit 1; }; \
	    go test $(GOTAGS) -timeout $(AMD64_TIMEOUT) $(AMD64_PKGS)'

## test-go-amd64-tailscale: test-go-amd64 with the release build tag (ADR-0043), so the
## amd64 gate covers the variant the Docker image actually carries. Same argument
## `check` makes for running the host suite twice, one architecture over.
##
## It turned out to be nearly free — 15:56 against 14:47, about 70 s — because the
## tailscale tree compiles once into the amd64 build-cache volume and no plugin code
## sits behind the tag. That is why check-amd64 runs both rather than arguing about it.
test-go-amd64-tailscale:
	$(MAKE) test-go-amd64 TAGS="$(RELEASE_TAGS)"

## amd64-pkgs: print AMD64_PKGS and nothing else. It exists for a caller that is
## ALREADY running on amd64 — the GitHub runner in .github/workflows/check.yml — where
## a container would add emulation to a machine that needs none. One list, printed
## rather than copied, so the workflow cannot drift from the target.
amd64-pkgs:
	@echo '$(AMD64_PKGS)'

## test-web: the vitest component suite. An alias for check-web (below), which is
## the single implementation, so the gate and the developer-facing name cannot drift.
test-web: check-web

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
##
## IT ALSO RUNS THE WEB SUITE (check-web), as of 2026-08-14. It did not before, and
## that was the same mistake in a third place: 775 vitest tests that ran only when a
## human remembered to. See check-web below for why it is placed where it is.
##
## IT RUNS ON ONE ARCHITECTURE — whichever one you are sitting at — and it deliberately
## stays that way: it must work with Docker closed. The second architecture is
## check-amd64, which is a release-time gate for the same reason check-bundle is.
##
## IT BUILDS THE BUNDLED PLUGINS FIRST (.scratch/bundled-plugins: issue 08), which
## is why `plugins` leads the list. internal/bundled embeds build output, and the
## Go suite drives the REAL seven modules through the REAL sandbox; without this a
## fresh clone would either fail `go test ./internal/bundled` with "run make
## plugins" or spend two seconds per module compiling them from source inside every
## test binary that boots a server. Note the consequence, because it reads as a
## contradiction and is not one: this target therefore never FAILS with "run make
## plugins" — it builds them. That sentence is what a developer running
## `go test ./internal/bundled` by hand gets, and it is the guard that stops a
## release shipping a server with no providers; check-no-bundled-modules-tracked
## below is the other half, and wants the opposite thing on purpose.
check: plugins check-fmt vet vet-tailscale check-placeholder check-no-bundled-modules-tracked check-credentials-free check-web test-go test-go-tailscale

## check-amd64: the RELEASE-side gate, beside `check` rather than inside it
## (.scratch/plugin-system issue 18). Run it before building an image to push.
##
## `check` proves the tree is correct on the machine you are sitting at. This proves
## it is correct on the machine it ships to — which for an Installed plugin is a
## different question, not a stricter one: ADR-0058 runs a guest under wazero's
## optimizing compiler, and wazero chooses a DIFFERENT compiler backend for amd64 than
## for arm64. Nothing else in this repo has a per-architecture code path, which is
## exactly why nothing else needs this.
##
## It is not in `check` because `check` may not require Docker, and it is not in the
## Dockerfile because a build is not a test run. docker/README.md's publish checklist
## is where it is called from.
check-amd64: test-go-amd64 test-go-amd64-tailscale

## check-release: the full release-time gate (.claude/scratch/issue-08-followups
## D003) — a real frontend bundle, check-bundle, the Playwright E2E suite
## (test-e2e), then check-amd64, run together in that order. Needs a running
## Docker daemon and about 45 minutes. It ALWAYS restores the committed
## placeholder embed (CLAUDE.md "Build artifacts") and rebuilds the host's own
## plugin modules afterward — even when an earlier step failed — so it leaves
## the tree exactly as it found it, and exits non-zero if any step failed.
##
## Stops at the first failing step (a real bundle or providers a later step
## silently ran without are not worth reporting on) but the restore-and-rebuild
## cleanup below still runs unconditionally, INCLUDING when the run is killed
## (TERM/ALRM to make's pid, or HUP/INT/QUIT to the whole process group — see
## scripts/check-release.sh for why make's pid alone can't take HUP/INT/QUIT)
## mid-step — see scripts/check-release.sh for how backgrounding a step lets
## the cleanup trap actually reach the whole tree (npm, playwright, chromium,
## the booted obelo binary, vite) instead of leaving it running on :8099 with
## a real bundle stuck in the tracked placeholder. SIGKILL cannot be caught,
## so it still skips all of this and leaves the tree exactly as the kill
## found it.
##
## The recipe below is one script call and deliberately never contains a
## literal $(MAKE) reference: GNU make always executes (not just prints) any
## recipe line that does, even under `make -n`, which used to make `-n
## check-release` build the real thing instead of describing it.
##
## CHECK_RELEASE_SKIP_AMD64=1 skips check-amd64 (its own ~40-minute Docker
## cost), so the rest of this target can be proven without waiting on it.
## Unset (the default) runs everything.
CHECK_RELEASE_SKIP_AMD64 ?=
check-release:
	@CHECK_RELEASE_SKIP_AMD64="$(CHECK_RELEASE_SKIP_AMD64)" EMBED_DIR="$(EMBED_DIR)" ./scripts/check-release.sh

## check-fmt: fail if anything is not gofmt-clean. Run `make fmt` to fix.
## This exists because nothing enforced formatting and it silently drifted to
## eleven files across six packages before anyone noticed.
check-fmt:
	@files=$$(gofmt -l . 2>/dev/null); \
	if [ -n "$$files" ]; then \
	  echo "ERROR: not gofmt-clean (run 'make fmt'):"; echo "$$files"; exit 1; \
	else echo "ok: gofmt clean"; fi

## vet: go vet over every module in the workspace (see GOPKGS).
vet:
	@go vet $(GOTAGS) $(GOPKGS) && echo "ok: go vet clean"

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

## check-no-bundled-modules-tracked: fail if a built plugin module or manifest was
## ever committed (ADR-0059 decision 10).
##
## It is THE OTHER HALF of the pair `go test ./internal/bundled` makes, and the two
## want opposite things on purpose — the same shape CLAUDE.md records for the SPA
## bundle, and the reason both halves are automated here rather than one of them.
## The test fails when the modules are MISSING, so a clone that has not run
## `make plugins` cannot quietly ship a server with no providers; this fails when
## they are PRESENT in git, so nobody ever "fixes" that by committing 7 MB of
## build output that no reviewer can read and that goes stale the moment a plugin
## changes.
##
## `git ls-files` inspects what is TRACKED, not what is on disk: a developer who
## has run `make plugins` has the modules sitting right there, and a check that
## failed for them would be disabled within a week.
check-no-bundled-modules-tracked:
	@git rev-parse --git-dir >/dev/null 2>&1 || { \
	  echo "ERROR: not a git repository — cannot inspect the tracked plugin modules"; exit 1; }
	@tracked=$$(git ls-files $(BUNDLED_DIR) | grep -v '^$(BUNDLED_DIR)/\.keep$$' || true); \
	if [ -n "$$tracked" ]; then \
	  echo "ERROR: built plugin modules are tracked in git:"; echo "$$tracked"; \
	  echo "       $(BUNDLED_DIR) holds BUILD OUTPUT. Remove them with:"; \
	  echo "         git rm --cached $$tracked"; \
	  exit 1; \
	else echo "ok: no built plugin modules are tracked"; fi

## check-credentials-free: fail if a bundled-credential var carries a non-empty
## literal in source (ADR-0032) — the repo must be credential-free against
## scrapers. The authoritative gate is TestBootstrapVarsEmptyInSource (an AST
## check run by `make test-go`); this grep is the fast standalone CI guard.
check-credentials-free:
	@if grep -nE '(bootstrapTMDBKey|bootstrapFanartKey|kAppEncKey)[[:space:]]+string[[:space:]]*=[[:space:]]*"[^"]' internal/config/bootstrap.go; then \
	  echo "ERROR: a bundled-credential var has a non-empty literal in source (ADR-0032)"; exit 1; \
	else echo "ok: bundled-credential vars are empty in source"; fi

## check-web: run the web component suite (vitest, ~775 tests) as part of the gate.
##
## Until 2026-08-14 NOTHING automated ran this suite: there is no CI in this repo,
## and `check` ran the Go suite twice while the TypeScript half was guarded not at
## all. A test there executed only when a human typed `npm test`, which is how a
## flake failed twice and left no evidence of WHICH test failed
## (.scratch/web-app/issues/08-...).
##
## It sits AFTER the static guards and BEFORE the Go suite on purpose: ~7s of
## typecheck+vitest fails faster than ~4min of `go test` run twice, so a broken web
## test arrives while you are still looking at the terminal. Same "cheap guards
## first" rule the rest of this target list follows. Measured: `make check-web` is
## 7.2s, and a cold `make check` went 4:17 -> ~4:20 by adding it.
##
## It runs `npm run typecheck` as well, and that is a SECOND unwired guard found
## while fixing the first — drop this one line if you disagree, nothing else
## depends on it. The script was defined in package.json and invoked by nothing:
## vitest strips types with esbuild and never typechecks, so a type error passes
## all 775 tests and surfaces only at `make web`/`make build` — release time. That
## is once again "two guards, only the release side automated", which is the exact
## shape CLAUDE.md records. It costs ~4s and is green today.
##
## A failing run names the failing test twice over — once on stdout after the
## summary, once in web/test-results/vitest-last-run.txt, which survives the
## terminal. See web/vitest-failure-log.ts. There is deliberately no vitest
## `retry`: a retried flake would make THIS TARGET REPORT SUCCESS, which is the
## precise failure this repo already paid for once (CLAUDE.md, "Build artifacts").
##
## The missing-node_modules case FAILS rather than skipping, for the same reason:
## a guard that passes because it did not run is worse than no guard, since it
## reports green. `npm ci` is not run automatically — it is a network operation
## and a pre-commit gate should not silently mutate the toolchain.
check-web:
	@[ -d $(WEB_DIR)/node_modules ] || { \
	  echo "ERROR: $(WEB_DIR)/node_modules is missing, so the web suite cannot run."; \
	  echo "       This FAILS rather than skipping — a guard that passes without"; \
	  echo "       running is exactly the shape CLAUDE.md records as having rotted."; \
	  echo "       Install the toolchain once with:  cd $(WEB_DIR) && npm ci"; \
	  exit 1; }
	cd $(WEB_DIR) && npm run typecheck
	cd $(WEB_DIR) && npm test

## fmt: gofmt the Go tree.
fmt:
	gofmt -w .

## clean: remove build outputs (keeps the committed placeholder index.html).
clean:
	rm -rf $(BIN) $(WEB_DIR)/node_modules $(WEB_DIR)/dist
	git checkout -- $(EMBED_DIR) 2>/dev/null || true
