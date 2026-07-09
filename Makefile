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

.PHONY: all build web go-build run test test-go test-e2e check-bundle fmt clean

all: build

## build: frontend bundle first, then the Go binary that embeds it.
build: web go-build

## web: install deps (if needed) and produce the SPA bundle into the embed dir.
web:
	cd $(WEB_DIR) && npm install && npm run build

## go-build: compile the binary (assumes the bundle is already built into $(EMBED_DIR)).
go-build:
	go build -o $(BIN) ./cmd/juicebox

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

## fmt: gofmt the Go tree.
fmt:
	gofmt -w .

## clean: remove build outputs (keeps the committed placeholder index.html).
clean:
	rm -rf $(BIN) $(WEB_DIR)/node_modules $(WEB_DIR)/dist
	git checkout -- $(EMBED_DIR) 2>/dev/null || true
