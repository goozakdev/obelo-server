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

> **Carried out (plugin-system issue 18, 2026-09-17):** "both amd64 and arm64" was, until
> this issue, half true. Every Installed plugin in this repo's history had been compiled and
> called on **arm64 only** — the development machine — because Docker was not running here when
> issues 08 through 16 were written, and each of them closed by saying so and asking for a CI
> job. What they could do was `CGO_ENABLED=0 GOARCH=amd64 go build ./...`, and it passed every
> time, which is the thing worth naming: a Plugin is compiled **at run time**, by a backend
> wazero chooses per architecture, so cross-compiling the host says nothing whatever about the
> backend that executes the guest. The one measurement this ADR's own "Portability" section
> refused to claim was exactly that one.
>
> It is now claimed, and measured. `make test-go-amd64` runs `./pluginapi/...`,
> `./internal/plugins/...`, `./internal/eventsink/...`, `./internal/subfetch/...`,
> `./internal/enrich/...` and `./internal/api/` inside a `--platform linux/amd64`
> `golang:1.26` container, where the wasm test guest is **compiled from source by the ordinary
> `GOOS=wasip1 GOARCH=wasm go build` and called**, on the machine ADR-0006 ships to. Everything
> passes, with **no behavioural difference of any kind** between the two backends: not one
> wazero trap, not one timing assumption broken — the deadline test of decision 6 and the
> byte-budget recycling of decision 7 both hold unchanged — and not one test that had to be
> skipped or loosened for the architecture. That is the useful result, and it was not a
> foregone one.
>
> **The cost, on an arm64 Mac under Docker 29's qemu emulation (2026-09-17):** 14 min 47 s cold
> for the untagged run and 15 min 56 s for the `-tags tailscale` one. The same packages run
> natively in about 6.5 min, so emulation costs roughly **2.3× wall**; per package the honest
> figure is `internal/api` at **835 s against 315 s native, 2.7×**, and `internal/plugins` —
> the one that compiles and calls the guest — at **60 s against 45.5 s**. A second run with no
> source change is **47 s**, because Go's test cache answers for every package; the one named
> proof test is `-count=1` so it executes regardless, which is the point of it. `make
> check-amd64` runs both variants and is called from docker/README.md's publish checklist.
> `make check` is unchanged and still needs no Docker.
>
> Two things the container taught us that are worth writing down. **ffmpeg has to be installed
> into it**: `internal/api` synthesises its media fixtures with `ffmpeg -f lavfi -i testsrc`, and
> without ffmpeg those tests do not skip politely — 364 of them fail on an empty fixture library,
> which looks exactly like a catastrophic architecture regression and is nothing of the kind.
> And **Go's default 10-minute per-package timeout is not enough** for `internal/api` under
> emulation; it panics mid-test at 10m00s, which reads like a hang. Both are properties of the
> harness, not of amd64, and both cost an afternoon to tell apart from the thing being measured.
>
> `.github/workflows/check.yml` was added in the same commit and is **dormant** — this repo has
> no remote — so that the day one exists, `make check` starts running per commit on an amd64
> runner and this container stops being the only place the gap is closed.

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

> **Carried out in part (plugin-system issue 09, 2026-09-17):** the first loader ships **two** of
> the four — `http_fetch` and `log` — under the module name `obelo`. `kv_get`/`kv_set` and
> `settings_get` arrive with the Extension points that need them (issues 11 and 12); an Event
> sink needs neither, because its Settings ride **with** the call in `SinkDeliverRequest`, which
> is the strongest form of "secrets at call time only" this decision asked for: a rebuilt
> instance starts with nobody's credential.
>
> One rule this decision left implicit had to be made explicit, because a sink cannot work
> without it. `http_fetch` permits **the manifest's `network.hosts`, plus the host of the URL the
> Admin configured for that Plugin** — an author cannot know the address of somebody else's
> receiver, so a sink restricted to manifest hosts could never post anywhere. The two are then
> treated differently, and deliberately: a target the **Plugin** chose is also refused when it
> resolves into loopback/RFC1918/link-local space (so `169.254.169.254` is refused even when the
> manifest allowlists it), while a target the **operator** typed is not, because a receiver on
> their own LAN is the point of this product. That is exactly the asymmetry `safefetch` documents
> for every other fetch in this server, applied at the only granularity where a Plugin has one.
> Redirects off either are the safe fetcher's, unchanged. Every refusal is an audit line —
> `plugin=<id> host=<h> reason=<allowlist|private-address|fetch-policy|oversize|bad-url>` — and a
> run of them disables the Plugin.

> **Carried out further (plugin-system issue 12, 2026-09-17):** the **Subtitle provider** seam
> does not need `settings_get` either, and does not get it. Its two calls travel in
> `SubtitleSearchCall` and `SubtitleDownloadCall`, each carrying the resolved `Settings` the way
> `SinkDeliverRequest` does, so this decision's "secrets handed over only at call time" holds for
> a second Extension point without a third host function existing. Two of the four named here are
> now the whole set for two of the three seams; `kv_get`/`kv_set` arrive with issue 11, which has
> state to keep rather than a credential to borrow.
>
> The same slice makes the **byte cap** decision 3's "byte payloads come back whole and
> size-capped" implies concrete for a payload coming OUT of a guest rather than into one. The host
> narrows the caller's `maxBytes` to `Options.MaxFetchBytes` on the way in, restates it to the
> guest, and checks the length of what comes back against it regardless — because a cap the guest
> enforces is not one. An oversize answer is **discarded whole**, never truncated to fit: a
> truncated subtitle is one the host would cache, record as fetched and serve, and a viewer would
> watch a subtitle that simply stops. It is counted like an allowlist violation rather than like a
> call failure, for the reason issue 09 gave — the call itself succeeded, so the failure counter
> would be cleared by the very call that earned it.

> **Carried out in full (plugin-system issue 11, 2026-09-17):** the Metadata provider Extension
> point needed the other two, and they landed with it. The set is now **five**, not four, because
> `kv_delete` was added beside `kv_get`/`kv_set`: a Plugin that may only ever grow its namespace
> has no way to evict a cache entry it knows is stale, and the one-line store method costs less
> than the workaround an author would otherwise write. The namespace is
> `plugin_kv(plugin_id, key, value BLOB, updated_at)` with `PRIMARY KEY (plugin_id, key)` —
> migration `0068_plugin_kv` — and the `plugin_id` is prefixed **by the host**, from the manifest
> on disk, so there is no spelling of a key that reaches another Plugin's value. Uninstall is one
> `DELETE` (`store.DeletePluginNamespace`). Keys and values are size-capped (256 B / 64 KiB by
> default) and exceeding a cap is a **refusal**, never a truncation, for the reason an oversize
> fetch is.
>
> `settings_get` takes no request and answers the `Settings` the host resolved for the call the
> guest is **currently inside**; outside a call it answers a zero `Settings`, so a secret is never
> readable beyond the call it belongs to. That is why a Metadata provider needs the function
> where a sink did not: a sink has one call and its Settings ride with it, while a provider has
> eight, and eight per-call envelopes carrying the same document would be eight places for a
> secret to be forgotten. No new envelope type was added for the metadata calls at all — their
> requests and responses are the `pluginapi/v1` types issue 07 froze, unchanged.

> **Withdrawn in part, and amended (bundled-plugins, 2026-09-18;
> [ADR-0059](./0059-the-shipped-metadata-providers-are-bundled-plugins.md) decisions 5 and 7):**
> the sentence "the throttle is the ADR-0049 limiter keyed on **host**, so two Plugins pointed at
> one source share one budget" was never carried out — `http_fetch` makes no throttle call — and
> is now **withdrawn**. A guest paces itself. One instance per Plugin, serialized (decision 7),
> already makes that pacing process-wide for the source, which is the property ADR-0049 wanted;
> what is given up is a shared budget between two *different* Plugins on one host. The operator's
> rate-limit setting reaches every Metadata provider through the fixed `Settings` shape, and the
> authoring guide says "pace yourself".
>
> The User-Agent is the **host's**: the same identity the Built-ins sent
> (`obelo/<server.Version> ( <project contact> )`), with the plugin id and version appended as a
> comment. A guest-supplied `User-Agent` is dropped rather than appended, which retires the
> hardcoded `obelo/1.0` and the two-header result the previous `Add` produced.

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

> **Amended (bundled-plugins, 2026-09-18;
> [ADR-0059](./0059-the-shipped-metadata-providers-are-bundled-plugins.md) decision 6):** a fetch
> ran under the call's own context, so a slow upstream killed the guest at the call deadline,
> the kill counted toward the failure threshold, and three slow lookups disabled a whole
> provider — ADR-0048's "a transient failure is retried, not parked", violated for a source
> instead of an item. Two rules now hold. **A Metadata provider call has a 30-second default
> budget**, which a manifest may raise to a host cap, because with guest-side pacing a lookup
> that makes several fetches needs room to wait. **A fetch always returns before the call
> deadline**: `http_fetch` bounds each request by the remaining call budget minus a grace margin,
> so a slow upstream comes back to the guest as a fetch error, the guest answers `unavailable`,
> the item takes ADR-0048's backoff, and no failure is counted. A deadline kill therefore means
> exactly one thing — the guest itself spun — which is what decision 6's threshold was written
> for. The instance lifecycle of decision 7 is unchanged.

**8. `apiVersion` is checked at install, not at call.** A manifest naming an `apiVersion` this
server does not speak is refused when it is installed, with a message naming **which side to
upgrade** — the ADR-0055 posture, applied to a Plugin instead of a Link. A Plugin is never
half-loaded and never discovers the mismatch mid-enrichment.

> **Carried out (plugin-system issue 10, 2026-09-17):** there is now an install to check it at.
> `POST /settings/plugins` (a `manifest` and a `module` part) and `POST /settings/plugins/from-url`
> (the manifest's URL, with the module fetched from beside it) refuse a version mismatch with
> `422 PLUGIN_API_VERSION` and the loader's own sentence, which names the side to upgrade in both
> directions. Four other refusals are deliberately **not** that code — an unreadable manifest, a
> duplicate id, a module that will not instantiate, and a source this server will not fetch from —
> because they want four different things done about them.
>
> The mechanism that makes an install take effect without a restart is the one decision 7's
> instance lifecycle left open: the whole `Set` is re-read from disk and a whole new
> `pluginapi.Registry` is built and published in one atomic store (`Registry.Swap`), after which the
> provider, subtitle and sink Managers Reload and only then is the old `Set` closed. Rebuild-and-swap
> rather than a delta, so the state after an install is the state a reboot would have produced.
>
> **Amended (plugin-system issue 19, 2026-09-17):** "every reader picks the new value up on its next
> read" was true of the subtitle builder, the sink Manager and the settings handlers, and NOT of the
> enrichment catalog, which copied the Metadata provider Descriptors into a slice at construction
> and was held for the life of the process by `enrich.Manager`. A provider installed from the
> Plugins screen was therefore keyable at once and could not lead a Library, be forced on or off per
> Library, or be composed into a chain until a restart. `enrich.Catalog` now holds the registry
> pointer and nothing else, deriving its entries on every read, so the sentence above is true of
> every reader named in it.
>
> One rule the ADR does not have, added here and worth knowing. `safefetch` deliberately checks
> redirect targets and **never** the initial request, because an operator pointing a source at a
> mirror on their own LAN is the point of this product. The URL-install path does check the first
> hop, because it is the one fetch in this server whose payload is **executed**: a pasted URL
> resolving into loopback/RFC1918/link-local space is refused, and an operator serving plugins from
> their own LAN uploads the file instead.

**9. The dependency is committed by the first loader issue, not by this ADR.** `go.mod` is
unchanged by this document; `.scratch/plugin-system/issues/09-…` adds
`github.com/tetratelabs/wazero` when it loads the first Installed Event sink. Nothing in
`internal/` or `cmd/` imports a wasm runtime today.

> **Carried out (plugin-system issue 09, 2026-09-17):** `github.com/tetratelabs/wazero v1.12.0` is
> in `go.mod`, and it is the **only** line added — `golang.org/x/sys` was already there at a newer
> version, exactly as decision 2 predicted. The host half lives in `internal/plugins`, the
> manifest and the guest-call wire types were added **additively** to the frozen `pluginapi/v1`
> (`Manifest`, `ManifestProvides`, `ManifestNetwork`, `ManifestSettings`, `SinkDeliverRequest`,
> `SinkDeliverResponse`, `FetchRequest`, `FetchHeader`, `FetchResponse`), and the JSON schema was
> regenerated, so an author in another language has the manifest and the ABI's documents from the
> same file as the rest of the contract.

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
closes rather than something this ADR may claim. (It did not — issue 09 could not either, and
nor could 11, 12, 13, 15 or 16. **Issue 18** closed it; see the note under decision 1.) wazero's `NewRuntimeConfig` selects the
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

> **Carried out (plugin-system issue 15, 2026-09-17):** two OPTIONAL trust and discovery
> features landed, and both are off in the shipped state, so nothing above changed for a
> server that does not opt in.
>
> **Manifest signing.** An author may publish a **detached** ed25519 signature beside the
> manifest (`plugin.sig.json`), and an Admin may pin publisher public keys. The signature
> covers `"obelo-plugin-v1\n" ‖ sha256(manifest) ‖ sha256(module)`. It is detached because
> decision 3's byte-for-byte manifest storage leaves no room for it to be otherwise: a
> `signature` field inside `manifest.json` would change the bytes it covers, and a canonical
> form would be a second spelling of a document the loader already treats as authoritative in
> its original one. With **no keys pinned** — the default — nothing is verified and the
> install path is exactly what issue 10 built. With one or more, an install must carry a
> signature naming a pinned publisher and verifying under that key, checked in
> `Manager.install` between `decodeManifest` and `checkDuplicate`, which is the only point
> where both artifacts are in hand and nothing has been written. An **already-installed**
> Plugin is never re-verified — not at boot, not on enable, disable or re-enable — because a
> change of mind about future installs must not become an outage of present ones.
>
> This does **not** soften decision 4 or the "the host owns every judgment" rule of ADR-0057.
> A signature says who shipped the bytes; it says nothing about what they do, and the sandbox
> is the thing that constrains that. A signed Plugin gets no extra capability of any kind.
>
> **A catalog URL.** A server may be pointed at a JSON index (`CatalogIndex` / `CatalogEntry`,
> new wire types in `pluginapi/v1`), empty by default, and **this project publishes none**
> (ADR-0001). A catalog entry is a manifest URL, so installing one is
> `Manager.InstallFromURL` unchanged — including the first-hop address check of the
> `http_fetch` posture in decision 5, which is why an entry pointing into the server's own
> network is refused with the sentence a pasted address gets. The index itself is **data**,
> so it is fetched under the ordinary `safefetch` policy; that asymmetry is the same one
> decision 5 already draws between reading a poster and running a module.
