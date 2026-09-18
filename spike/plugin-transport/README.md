# Plugin transport spike — THROWAWAY

This directory is **not part of the server**. It is its own Go module, so the root
`go.mod` knows nothing about it and `go build ./...` / `go test ./...` at the repo
root never see it. Nothing here ships, nothing here is imported by a production
package, and it may be deleted the day nobody wants to re-run the numbers.

It exists to answer one question with measurements:

> For an Installed plugin (PRD Phase 2), is the transport **bare wazero with a
> hand-rolled ABI**, or **Extism** (its PDK in the guest, its SDK in the host)?

The answer, the numbers and the reasoning are in
[`docs/adr/0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md`](../../docs/adr/0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md).
Issue: `.scratch/plugin-system/issues/08-spike-the-transport-and-write-adr-0058.md`.

## What is here

| Path | What it is |
| --- | --- |
| `wire/` | A **copy** of the Subtitle provider wire structs from `internal/pluginapi/v1`. A separate module cannot import `internal/`, and the real contract is moving to `pluginapi/v1` under issue 07 in parallel. |
| `guests/bare/` | Guest variant A: stock-Go/TinyGo wasm, hand-rolled JSON-over-guest-memory ABI. Own module, no dependencies. |
| `guests/extism/` | Guest variant B: the same behaviour through `github.com/extism/go-pdk`. Own module. |
| `guests/probe/` | The hostile guest: it tries to open a socket, read a file, list a directory and spawn a process. |
| `barehost.go` | Host side of variant A. |
| `extismhost.go` | Host side of variant B. |
| `probe.go` | Runs the hostile guest under the sandbox configuration a real loader would use. |
| `cmd/spike/` | The harness. Prints every number the ADR quotes, as markdown. |
| `transport_test.go` | The acceptance tests: both guests answer both calls, a deadline stops a spinning guest, the sandbox holds. |

`guests/bare/answers.go` and `guests/bare/wire.go` are **byte-identical** to their
`guests/extism/` copies. Only `main.go` differs between the two guests, which is
what makes "how many lines does the plugin author write" a fair comparison.

## Running it

```sh
./build-guests.sh          # compiles the guests into build/ (gitignored)
go test ./...              # acceptance, sandbox and deadline proofs
go run ./cmd/spike         # the measurements, as a markdown table
```

Cross-compilation proof (ADR-0006: one image, two architectures, no cgo):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
```

## Toolchains

Stock Go is enough to build every guest; `GOOS=wasip1 GOARCH=wasm go build
-buildmode=c-shared` with `//go:wasmexport` has been a supported combination
since Go 1.24.

**TinyGo is optional and is not installed on this machine by default.** The
measurements that name it were taken with TinyGo 0.42.0 unpacked from its GitHub
release tarball plus `binaryen` (TinyGo shells out to `wasm-opt`). `build-guests.sh`
uses `tinygo` from `PATH` or from `$TINYGO`, and simply skips the TinyGo rows when
neither is there:

```sh
TINYGO=/path/to/tinygo/bin/tinygo ./build-guests.sh
```

The server never needs either toolchain. They are what a **plugin author** installs.

## Gotchas this spike hit, for whoever writes the loader

1. A `-buildmode=c-shared` guest exports `_initialize`, not `_start`, and wazero
   runs `_start` by default. Without
   `ModuleConfig.WithStartFunctions("_initialize")` the first export call traps in
   `runtime.notInitialized` (stock Go) or `runtime.wasmExportCheckRun` (TinyGo).
   The Extism SDK does this itself.
2. TinyGo's `-buildmode=c-shared` already exports `malloc`/`free`, so a guest that
   exports its own `free` fails to link with `duplicate export name` out of
   `wasm-opt`. The spike's allocator is `obelo_alloc`/`obelo_free`.
3. TinyGo's default scheduler dispatches every exported call through a goroutine
   scheduler loop. `-scheduler=none` is worth a factor of several per call and
   ~40% of the binary for a request-response guest.
4. A guest's linear memory only ever grows — wasm has no way to return a page —
   so a **pooled instance dies** after enough large payloads. The harness measures
   how many.
