# The Obelo Discord Event sink.
#
# A Plugin is installed as one Plugin package: a zip of manifest.json, plugin.wasm
# and, optionally, plugin.sig.json. `make` produces the module; the manifest is
# checked in beside it; `make package` zips them.

MODULE := plugin.wasm
PACKAGE := obelo-plugin-discord.zip

# The pack tool ships in the Obelo server repository. It is built there by
# `make pluginsign` (bin/pluginsign); put it on your PATH or point this at it:
#
#	make package PLUGINSIGN=/path/to/obelo-server/bin/pluginsign
PLUGINSIGN ?= pluginsign

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

# The file an Admin uploads, or pastes the URL of. `pack` refuses a manifest or a
# layout the server would refuse. A plugin.sig.json beside the manifest (made by
# `pluginsign sign`) is included when there is one.
.PHONY: package
package: $(MODULE)
	$(PLUGINSIGN) pack -manifest manifest.json -module $(MODULE) \
		$(if $(wildcard plugin.sig.json),-signature plugin.sig.json) -out $(PACKAGE)

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
	rm -f $(MODULE) $(PACKAGE)
