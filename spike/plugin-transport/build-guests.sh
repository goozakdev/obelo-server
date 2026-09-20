#!/usr/bin/env bash
# Build every guest with every toolchain that is on this machine, into build/.
#
# Stock Go is assumed. TinyGo is used when `tinygo` is on PATH or TINYGO points at
# it; it is NOT required to run the spike, only to reproduce the small-binary
# column of the ADR-0058 table. See README.md.
set -euo pipefail
cd "$(dirname "$0")"

# GOWORK=off: this repository has a go.work at its root (ADR-0059 decision 9) and
# none of the spike's modules are in it, so a build run from in here would be in
# workspace mode and fail with "main module does not contain package". Off is also
# what these guests are meant to be built under — each is a standalone module,
# like a plugin author's.
export GOWORK=off

OUT=build
mkdir -p "$OUT"

TINYGO=${TINYGO:-$(command -v tinygo || true)}

build_stock() { # dir name
  ( cd "guests/$1" && CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
      -o "../../$OUT/$2.go.wasm" . )
}

# -scheduler=none is not a micro-optimization: with TinyGo's default scheduler
# every exported call is dispatched through a goroutine scheduler loop, and with
# wazero's WithCloseOnContextDone that loop is where the per-call cost goes. It
# costs a factor of several on every call and ~40% of the binary. A plugin that
# is a request-response function needs no goroutines.
build_tiny() { # dir name
  [ -n "$TINYGO" ] || return 0
  ( cd "guests/$1" && "$TINYGO" build -target=wasip1 -buildmode=c-shared -no-debug \
      -scheduler=none -o "../../$OUT/$2.tinygo.wasm" . )
}

build_stock bare bare
build_stock extism extism
build_stock probe probe

build_tiny bare bare
build_tiny extism extism
# The probe guest is stock-Go only: TinyGo has no os/exec, so the guest that tries
# to spawn a process cannot be compiled by it at all. That is a toolchain fact,
# not a sandbox one, and the sandbox claim is proved with the stock-Go build.

ls -l "$OUT"
