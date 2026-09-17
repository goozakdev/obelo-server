# An Installed plugin is a WebAssembly guest, called through a hand-rolled ABI on wazero

[ADR-0057](./0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
decided what a Plugin *is* — a closed set of three Extension points behind one wire-shaped
contract — and deliberately left how an **Installed plugin** is loaded and sandboxed to a
second ADR. This is that ADR. It decides the transport, the sandbox boundary, the host
functions a guest may call, the deadline model and whether an instance is reused, and it
decides them against measurements rather than taste: the spike that produced every number in
this document is in [`spike/plugin-transport/`](../../spike/plugin-transport/), which is its
own Go module, ships nothing, and is throwaway. See
[PRD](../../.scratch/plugin-system/PRD.md) ("Phase 2 leaning") and
`.scratch/plugin-system/issues/08-spike-the-transport-and-write-adr-0058.md`.

## Decisions

**1. An Installed plugin is a WebAssembly module, run by `github.com/tetratelabs/wazero`.**
The PRD's leaning holds and is now a decision. wazero is pure Go with no cgo, it uses its
optimizing compiler (not the interpreter) on both amd64 and arm64, one `.wasm` artifact serves
both architectures, and a module can be loaded and dropped while the server runs — which is
every constraint ADR-0006 and ADR-0001 place on this, met by one dependency. The alternatives
die for the reasons the PRD's table already gave, and the spike changes none of them: stdlib
`plugin` needs cgo and an identical toolchain and dependency graph; compile-time registration
*is* Phase 1 and remains available to an operator who wants zero runtime risk; go-plugin over
gRPC needs a native binary per architecture, lets the guest inherit the container's network,
and pulls gRPC into a tree that has none. go-plugin stays named as the fallback if an Extension
point ever needs bytes-heavy or process-spawning work, and this ADR does not retire it.

**2. The ABI is hand-rolled, not Extism.** Three fixed exports plus one per contract call —
five in all for a Subtitle provider — JSON in and JSON out through the
guest's own linear memory (decision 3). This is the decision the spike existed to make, and it
was close: **Extism runs on wazero**, so choosing it buys an ABI, a manifest convention and
cross-language PDKs — *not* a different or better sandbox — and the spike measured the two
transports as a wash on the call that matters (103 µs vs 120 µs for a search on a stock-Go
guest). Three things broke the tie:

- **The dependency delta.** wazero adds **one** module to `go.mod`: itself. (`golang.org/x/sys`
  is already there via tailscale.) `github.com/extism/go-sdk` adds **seven** compiled-in
  modules — `observe-sdk`, `gobwas/glob`, `ianlancetaylor/demangle`, `tetratelabs/wabin`,
  `go.opentelemetry.io/proto/otlp`, `google.golang.org/protobuf` and itself — and with them
  OpenTelemetry and protobuf, of which this repo currently has none. A trivial binary that only
  constructs a runtime is **3.8 MB with wazero and 17.4 MB with the Extism SDK**: 13.6 MB of
  observability tree on a server whose whole point is one small image on a Pi.
- **The part of Extism we would have to refuse is the part that is free.** Extism's manifest
  carries `AllowedHosts` and its SDK ships an `http_request` host function that honours it —
  but ADR-0001 says every outbound fetch goes through `safefetch`, and ADR-0049 says the
  throttle is keyed on **host** and shared across Plugins. Our `http_fetch` (decision 5) has to
  be ours. What is left of Extism is an ABI worth ~33 guest lines and ~75 host lines.
- **Extism pins its own wazero** (v1.9.0 in its `go.mod` against v1.12.0 current). Taking it
  means a second opinion about which runtime version this server executes untrusted code on.

**3. The ABI, in full.** A guest exports `obelo_alloc(size) -> ptr`, `obelo_free(ptr)`, one
function per contract call taking `(ptr, len)` of the request JSON and returning a single
`i64` packed as `ptr<<32 | len` of the response JSON, and `last_error() -> i64` for the case a
call could not produce a response at all. **The guest owns every buffer on both sides**: the
host asks for memory, writes into it, and gives it back; the host never fabricates a guest
pointer, and a guest that is handed a pointer it did not allocate refuses. Request and response
are the `pluginapi/v1` wire types, exactly as issue 07 froze and schema'd them, which is what
makes an author in another language possible without a PDK: the ABI is five function signatures
and a JSON schema.

**4. The sandbox is "WASI instantiated, nothing granted", and it holds.** wazero's
`wasi_snapshot_preview1` **is** instantiated, because a stock-Go guest imports it for its clock
and its random source and will not load without it (the spike confirmed: `module[wasi_snapshot_preview1]
not instantiated`). Every capability inside it is then simply never configured — no preopened
directory, no `WithFS`/`WithFSConfig`, no environment, no stdio, and wazero has no host sockets
to withhold. A module that imports anything other than `wasi_snapshot_preview1` and this
server's own host-function module is **refused at load**, before it is instantiated. The probe
guest, compiled by the ordinary command with no special flags, was told this (§Sandbox probe).

**5. Four host functions, and the set is closed the way the Extension points are.**

- **`http_fetch`** — the ONLY way out. The host performs the request with `safefetch.Client`,
  so the redirect policy, the bounded chain and the refusal of loopback/RFC1918/link-local
  targets apply unchanged (ADR-0001). The URL's host must match the manifest's `network.hosts`
  allowlist, **checked host-side against the manifest on disk and never against anything the
  guest says**, and the throttle is the ADR-0049 limiter keyed on **host**, so two Plugins
  pointed at one source share one budget. A refused host is an audit line, not a silent empty
  answer.
- **`log`** — a line, prefixed with the Plugin id. It is how a Plugin author debugs, and it is
  why the guest needs no stdout.
- **`kv_get` / `kv_set`** — a Plugin-scoped namespace in SQLite (ADR-0007: state in SQLite,
  blobs on disk). Scoped by Plugin id, so one Plugin cannot read another's.
- **`settings_get`** — the fixed `Settings` shape of ADR-0057, with **secrets handed over only
  at call time** and never persisted into the guest.

No filesystem, no spawn, no raw sockets, no clock beyond WASI's, no host function that returns
a handle. Every one of these is request-response and JSON-shaped, like the contract itself.

**6. Every call carries a deadline, and the deadline is enforced by the runtime.** The runtime
is built with `WithCloseOnContextDone(true)` and each call gets a `context` with a deadline.
A guest that never returns is unwound: the spike's spinning guest, given 200 ms, was stopped
after **201 ms** with `module closed with context deadline exceeded`. This is not free — it is
the difference between 24.4 µs and 102.7 µs on a search call, roughly 78 µs — and it is bought
anyway, because the call it guards exists to make an HTTP request that takes 100–500 ms, and
because without it there is no way to stop a loop inside a single guest function at all.
**A killed instance is dead**, not resumable, which decision 7 has to handle.

**7. One instance per Plugin, reused, serialized, and recycled.** Not instance-per-call: a
fresh instance per search costs **2811 µs against 103 µs pooled**, a factor of **27**, which is
nearly 3 ms of CPU burned per call for a property — no state carried between calls —
that the contract does not need and a Built-in has never had. Not a naive pool either, because
**a guest's linear memory only ever grows** (wasm cannot return a page), so one pooled instance
answering 1 MiB downloads traps with `out of bounds memory access` after **560 calls**
(stock Go; 331 with TinyGo). So: one instance per Plugin, one call at a time (the contract is
request-response and the ADR-0049 limiter already serializes per host), **discarded and rebuilt
on any trap, any deadline kill, and after a configured budget of bytes returned**. Rebuilding
is cheap — compilation is the expensive half and is kept: 0.03–1.74 ms to instantiate against
43–606 ms to compile.

**8. `apiVersion` is checked at install, not at call.** A manifest naming an `apiVersion` this
server does not speak is refused when it is installed, with a message naming **which side to
upgrade** — the ADR-0055 posture, applied to a Plugin instead of a Link. A Plugin is never
half-loaded and never discovers the mismatch mid-enrichment.

**9. The dependency is committed by the first loader issue, not by this ADR.** `go.mod` is
unchanged by this document; `.scratch/plugin-system/issues/09-…` adds
`github.com/tetratelabs/wazero` when it loads the first Installed Event sink. Nothing in
`internal/` or `cmd/` imports a wasm runtime today.

## The numbers

Measured on darwin/arm64 (Apple silicon), Go 1.26.5, TinyGo 0.42.0, wazero 1.12.0, Extism
go-sdk 1.7.1 / go-pdk 1.1.3, wazero's compiler backend. Per-call figures are the mean of 1000
calls (200 for the mebibyte). Reproduce with `./build-guests.sh && go run ./cmd/spike` in
[`spike/plugin-transport/`](../../spike/plugin-transport/).

| Variant | Guest size | Compile | Instantiate | search, pooled | search, instance-per-call | 64 KiB download | 1 MiB download | 1 MiB calls one instance survives |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| bare wazero / stock Go | 3.2 MiB | 606 ms | 1.74 ms | 103 µs | 2811 µs | 1212 µs | 16.0 ms | 560 |
| bare wazero / TinyGo | 243 KiB | 43 ms | 0.03 ms | 109 µs | 840 µs | 2702 µs | 35.1 ms | 331 |
| Extism / stock Go | 3.2 MiB | 603 ms | 1.12 ms | 120 µs | 2884 µs | 2255 µs | 31.0 ms | 683 |
| Extism / TinyGo | 243 KiB | 45 ms | 0.07 ms | 95 µs | 1083 µs | 3302 µs | 47.5 ms | 404 |

**Portability, as verified rather than assumed.** The harness ran natively on darwin/arm64, and
the spike module cross-compiles clean with `CGO_ENABLED=0` for `linux/amd64`, `linux/arm64` and
`darwin/arm64` — the two architectures ADR-0006 ships plus the development machine. It was **not**
executed on linux/amd64: Docker was not running on this machine, and that is a gap issue 09's CI
closes rather than something this ADR may claim. wazero's `NewRuntimeConfig` selects the
optimizing compiler on amd64 and arm64 and falls back to the interpreter elsewhere; it exports
no way to ask which one it chose, so the spike carries `OBELO_SPIKE_INTERPRETER=1` to force the
interpreter, and the difference is the answer: with the compiler, preparing the stock-Go guest
costs 606 ms and each call then costs ~100 µs; with the interpreter, preparation is effectively
free and a handful of calls take an order of magnitude longer.

Compilation is the one genuinely expensive thing and it happens once per installed Plugin, at
boot or at install. Everything else is noise beside the HTTP request the call exists to make.
Note the split verdict on toolchains: **TinyGo wins size and compile time by more than an order
of magnitude, stock Go wins byte-payload throughput by a factor of two** — its `encoding/json`
and allocator are simply better at a mebibyte. Neither difference changes the transport choice,
which is why the authoring guide can leave it to the author.

The deadline mechanism, priced separately because it is a runtime-wide switch:

| Variant | search, pooled, deadline ON | search, pooled, deadline OFF | overhead |
| --- | ---: | ---: | ---: |
| bare wazero / stock Go | 102.7 µs | 24.4 µs | +321.6% |
| bare wazero / TinyGo | 109.2 µs | 24.6 µs | +344.2% |
| Extism / stock Go | 120.2 µs | 40.1 µs | +199.7% |
| Extism / TinyGo | 95.1 µs | 40.2 µs | +136.5% |

The same Subtitle provider was written twice. `answers.go` and `wire.go` are **byte-identical**
between the two guests, so only the ABI glue differs:

| | Bare wazero | Extism | Bare costs |
| --- | ---: | ---: | ---: |
| Guest lines the author writes (non-blank, non-comment) | 73 | 40 | +33 |
| Host lines this server writes | 146 | 62 | +75 |
| New modules in the server's `go.mod` | 1 | 7 | −6 |
| Trivial host binary | 3.8 MB | 17.4 MB | −13.6 MB |

(Nine of `barehost.go`'s 146 lines are the runtime and sandbox configuration the Extism path
reuses, hence +75 rather than +84.)

## Sandbox probe

A guest that tries to be hostile, compiled by the ordinary command, granted no host function,
run under the configuration decision 4 describes:

| The guest tried | It was told |
| --- | --- |
| `net.Dial("tcp", "93.184.216.34:80")` | `dial tcp 93.184.216.34:80: address 93.184.216.34:80: connect: Connection refused` |
| `net.Listen("tcp", "127.0.0.1:34517")` | *no error* — `Addr()=127.0.0.1:34517` (see below) |
| `net.Dial` to the guest's own listener | *no error* (see below) |
| `os.ReadFile("/etc/passwd")` | `open /etc/passwd: Bad file number` |
| `os.ReadDir("/")` | `open /: Bad file number` |
| `exec.Command("/bin/sh", "-c", "echo pwned").Run()` | `open /dev/null: Bad file number` |
| — with no WASI module instantiated at all — | `module[wasi_snapshot_preview1] not instantiated` (it does not load) |

Two of those want reading carefully, because a naive glance says the sandbox leaked:

- **`net.Listen` succeeds and the guest can dial its own listener.** Go's `wasip1` port ships an
  **in-process fake network stack** (`net/net_fake.go`, build tag `js || wasip1`). The guest is
  talking to itself inside its own linear memory. The proof is in the test: the host then dials
  the address the guest claims to have bound and gets `connection refused`, because nothing on
  the machine is listening. `net.Dial` to a real public address gets the same refusal from the
  same fake stack — no packet is ever offered to the host.
- **`exec.Command` fails on `/dev/null`, not on `fork`.** The guest never reaches the spawn: the
  standard library cannot open the process's standard streams, because there is no filesystem.
  The effect is what matters and the errno is what was observed, so it is recorded as observed.

The module import check is the load-time half of the same rule: `bare.go.wasm` imports from
`[wasi_snapshot_preview1]` and nothing else, and an Extism-built guest additionally imports
`extism:host/env`. A guest asking for a module the host did not offer is refused before it runs.

## What a Plugin author installs

Nothing, on the server. The server needs no toolchain at all — it runs `.wasm`.

An **author** needs one of:

- **Stock Go 1.24+**: `CGO_ENABLED=0 GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared` with
  `//go:wasmexport`. Nothing to install beyond Go. Costs a 3.2 MiB module and 606 ms of
  compile-on-load.
- **TinyGo 0.36+** (0.42.0 measured), plus `binaryen` for `wasm-opt`: **243 KiB and 43 ms**, a
  13× and 14× improvement, for a slightly smaller standard library.

Both were measured; both work; the authoring guide (issue 14) will recommend TinyGo and accept
stock Go. An author in another language writes the same five exports against the JSON schema.

Three traps the spike hit, recorded so the loader does not rediscover them:

1. A `-buildmode=c-shared` guest exports **`_initialize`**, not `_start`, and wazero runs
   `_start` by default. Without `ModuleConfig.WithStartFunctions("_initialize")` the first call
   traps in `runtime.notInitialized`.
2. TinyGo's `c-shared` output already exports `malloc`/`free`, so a guest exporting its own
   `free` fails to link with `duplicate export name`. Hence `obelo_alloc`/`obelo_free`.
3. TinyGo's **default scheduler** dispatches every exported call through a goroutine scheduler
   loop; `-scheduler=none` was worth a factor of ~20 per call and ~40% of the binary for a
   request-response guest.

## Why

The sandbox is the whole reason a Plugin may exist at all. ADR-0001 promises a self-hosted
server with no vendor dependency and no phoning home, and ADR-0057 promises that a Plugin may
be a Library's **Authoritative** provider — code the maintainer did not write, leading the
enrichment of somebody's whole library. That promise is only defensible if the code physically
cannot do anything but answer the question it was asked. WebAssembly is the only option on the
table where "cannot" is a property of the execution environment rather than of the author's
good behaviour, and wazero is the only implementation of it that costs this project nothing in
cgo, in architectures, or in image size.

The ABI is hand-rolled for the same reason the contract is wire-shaped rather than borrowed:
the thing that would have been convenient to borrow — Extism's manifest-driven network
allowlist — is precisely the thing `safefetch` and ADR-0049's host-keyed throttle must own.
Paying 33 lines in a guest and 75 in the host to keep the server's dependency tree at one new
module is the same trade this project has made every previous time.

## Consequences

- Issue 09 adds `github.com/tetratelabs/wazero` to `go.mod` — one new module — and nothing else.
- The loader owns an instance lifecycle: build on install and at boot, one call at a time,
  discard on trap/deadline/byte-budget, rebuild from the retained `CompiledModule`. A Plugin
  that traps is recorded and disabled and **never stops a boot** (the ADR-0043 rule).
- Byte payloads cost what base64-in-JSON costs: ~1 ms for a realistic 64 KiB subtitle, ~16 ms
  for a stress-test mebibyte. Issue 12's host-side byte cap matters more for the guest's linear
  memory than for the host's.
- `pluginapi/v1`'s JSON schema (issue 07) becomes load-bearing: it is the contract for an author
  who is not writing Go, since there is no PDK to hand them.
- Extism is not foreclosed. A guest is a wasm module either way; if a cross-language PDK ever
  earns its 13.6 MB, the host can grow an Extism-compatible calling convention without the
  contract changing.
- The spike module is excluded from the root module by construction (its own `go.mod`), so
  `go build ./...`, `go vet ./...` and `go test ./...` at the root never see it.

## Non-goals

- A wasm guest for **Built-ins**. They are this server's own code and are called in-process,
  exactly as ADR-0057 decision 5 has them.
- Fuel metering or a memory cap per Plugin beyond what the deadline and the recycle budget
  give. wazero can limit memory pages; whether a Plugin needs a tighter cap than "it dies and is
  rebuilt" is a question for the first Plugin that misbehaves.
- Concurrency inside a guest. One call at a time, per Plugin. A Plugin that wants parallelism is
  asking the host for a second instance, which is a pool-size decision and not an ABI one.
- Plugin-supplied UI code, in either phase (ADR-0057, unchanged).
