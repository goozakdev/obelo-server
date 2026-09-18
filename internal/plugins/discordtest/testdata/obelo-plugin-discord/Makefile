# The Obelo Discord Event sink.
#
# Two files make a Plugin: manifest.json and plugin.wasm. `make` produces the
# second; the first is checked in beside it.

MODULE := plugin.wasm

.PHONY: all
all: $(MODULE)

# The one build command, and the one an author needs to know. Stock Go, no
# dependencies, no toolchain to install.
#
# -buildmode=c-shared is what makes the module export `_initialize` rather than
# `_start`, which is what the host instantiates. Building without it produces a
# module that traps on its first call inside an uninitialised runtime.
#
# GOFLAGS is cleared because a -mod=vendor or a -tags list inherited from
# whatever shell this runs in has nothing to do with a module that imports only
# the standard library, and inheriting one is how this build breaks on somebody
# else's machine.
$(MODULE): main.go go.mod
	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= \
		go build -buildmode=c-shared -o $(MODULE) .

# TinyGo produces a module about 13x smaller and compiles it about 14x faster
# (Obelo ADR-0058 measured both). It is NOT required — Obelo's own acceptance
# suite builds this plugin with stock Go — and it is worth having once the module
# is something you ship rather than something you are debugging:
#
#	tinygo build -target=wasip1 -buildmode=c-shared -o plugin.wasm .
.PHONY: tinygo
tinygo:
	tinygo build -target=wasip1 -buildmode=c-shared -o $(MODULE) .

# `go vet` needs the target explicitly: main.go is behind //go:build wasm,
# because //go:wasmexport will not compile anywhere else. Without these two
# variables vet reports "build constraints exclude all Go files", which is true
# and unhelpful.
.PHONY: vet
vet:
	GOOS=wasip1 GOARCH=wasm GOFLAGS= go vet ./...

.PHONY: fmt
fmt:
	gofmt -l .

.PHONY: clean
clean:
	rm -f $(MODULE)
