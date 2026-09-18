# Writing an Obelo plugin

This is the guide for writing an **Installed plugin**: a WebAssembly module you
build yourself, drop onto somebody's Obelo, and that the server calls as if it had
been compiled in.

Everything here is taught from one working plugin — the **Discord Event sink**, in
its own repository at [`obelo-plugin-discord`](https://github.com/goozakdev/obelo-plugin-discord).
It is not an illustration. Obelo's own acceptance suite builds it from source and
drives it end to end ([`internal/api/discord_plugin_test.go`](../../internal/api/discord_plugin_test.go)),
and **every code block below is extracted from its source by a test**
([`internal/plugins/discordtest/samples_test.go`](../../internal/plugins/discordtest/samples_test.go)),
so the guide cannot drift away from code that runs.

> **In a hurry?** [Build and install](#7-build-and-install-locally) is four
> commands. The rest of this document is why each of them is what it is.

**Contents**

1. [What a Plugin is](#1-what-a-plugin-is)
2. [The three Extension points, and what each is asked](#2-the-three-extension-points-and-what-each-is-asked)
3. [The manifest, by example](#3-the-manifest-by-example)
4. [The ABI: four exports and six imports](#4-the-abi-four-exports-and-six-imports)
5. [The host functions, and what they refuse](#5-the-host-functions-and-what-they-refuse)
6. [How settings reach the guest](#6-how-settings-reach-the-guest)
7. [Build and install locally](#7-build-and-install-locally)
8. [Reading the counters and the last error while you develop](#8-reading-the-counters-and-the-last-error-while-you-develop)
9. [The JSON schema, and where it lives](#9-the-json-schema-and-where-it-lives)
10. [The rules you must not break](#10-the-rules-you-must-not-break)

---

## 1. What a Plugin is

A **Plugin** is one unit of code that talks to something outside this server. That
is the whole definition, and it covers two kinds of thing that are deliberately
indistinguishable downstream:

- a **Built-in** — compiled into the server, registered from the composition root
  in Go (TMDB, MusicBrainz, OpenSubtitles, the Webhook sink);
- an **Installed plugin** — a `.wasm` module and a `manifest.json` an Admin put on
  their server, loaded into a sandbox at boot or on upload.

The two reach the same registry, wear the same `Descriptor`, are configured
through the same settings endpoints, and are called through the same interfaces.
Nothing downstream of registration can tell them apart
([ADR-0057](../adr/0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)).
What differs is everything *below* that line: an Installed plugin runs inside a
[wazero](https://wazero.io) sandbox with no filesystem, no environment, no sockets
and no stdio, and reaches the outside world only through host functions
([ADR-0058](../adr/0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)).

**A Plugin never has identity of its own.** It does not decide what a Title is
called, it does not decide whether a match is good enough, it does not decide who
may watch anything. It answers the question it was asked and states facts. Every
judgment stays on the host's side of the boundary, which is why a hostile or
merely buggy Plugin is a Plugin that is switched off, not a corrupted library.

**Any language that compiles to `wasip1/wasm` works.** This guide's examples are
Go because the reference plugin is Go. There is no SDK to install and nothing to
import: the contract is a JSON schema and a handful of function signatures. The
reference plugin deliberately does **not** import the server's Go module, so that
what you are reading is what an author in Rust or Zig would have to reproduce.

The vocabulary — Plugin, Extension point, Built-in, Installed plugin, Manifest,
Event sink — is defined once in [`CONTEXT.md`](../../CONTEXT.md) and used
unchanged here.

---

## 2. The three Extension points, and what each is asked

The set of seams a Plugin may fill is **closed**. It grows by decision, not by
declaration — a manifest naming a fourth kind is refused at load. Today there are
three.

| Extension point | `kind` | What it is asked |
| --- | --- | --- |
| **Metadata provider** | `metadata-provider` | "What is this Title? What artwork does it have?" |
| **Subtitle provider** | `subtitle-provider` | "Which subtitles exist for this release, and give me one." |
| **Event sink** | `event-sink` | "This just finished." (Outbound HTTP only; it is never asked anything.) |

Your manifest's `provides` list says which you fill, and **one module may fill more
than one** — the host looks up only the exports the declared seams need.

### The exports each seam needs

Three are always required, whatever you provide:

```
obelo_alloc(size u32) -> ptr u32
obelo_free(ptr u32)
last_error() -> i64
```

Then, per seam:

| Seam | Export | Required? |
| --- | --- | --- |
| Event sink | `deliver(ptr, len) -> i64` | yes |
| Subtitle provider | `obelo_subtitle_search(ptr, len) -> i64` | yes |
| | `obelo_subtitle_download(ptr, len) -> i64` | yes |
| Metadata provider | `metadata_lookup(ptr, len) -> i64` | yes |
| | `metadata_search(ptr, len) -> i64` | yes |
| | `metadata_artwork_candidates(ptr, len) -> i64` | yes |
| | `metadata_series_seasons` / `metadata_season_episodes` | behind capability `episode-list` |
| | `metadata_album_tracklist` / `metadata_release_editions` | behind capability `album-tracklist` |
| | `metadata_external_ref` | behind capability `external-ref` |

> **The Event sink's call is `deliver`, not `obelo_deliver`.** The three seams
> landed in three slices and their export names are not harmonised: the sink's is
> bare, the subtitle provider's is `obelo_`-prefixed, the metadata provider's is
> `metadata_`-prefixed. Renaming a live export would break every plugin already
> installed for no gain, so **the names are what they are and this table is the
> authority**. A v2 contract is where that gets tidied.

**Declaring a capability is a promise.** If your manifest declares
`external-ref` and your module does not export `metadata_external_ref`, the host
answers "unavailable" and does not disable you — it is survivable — but you have
shipped a manifest that lies to the operator reading it. Declare what you export.

**Not declaring a capability costs nothing.** An undeclared optional call is never
made, so the host does not even enter the sandbox for it.

Two things a Metadata provider author gets wrong, both learned the hard way:

- **A guest never returns artwork bytes**, only URLs. The host fetches the image
  through its own guarded fetcher and files it.
- **A role the host does not file for that kind is refused at the store.** Use
  `poster`, `background` or `logo`.

And one a Subtitle provider author gets wrong:

- **The host states `maxBytes` on the way in and re-checks it on the way out.**
  Answering with more is refused whole and counted against you — see
  [the rules](#10-the-rules-you-must-not-break).

---

## 3. The manifest, by example

`manifest.json` sits beside your module. It is the Installed half of a
`Descriptor`: the static facts a Built-in would have written in Go. **These fields
ARE the settings screen** — the name, the description, the docs link and the
controls an Admin sees all come from here.

This is the Discord plugin's, whole:

<!-- sample: manifest.json -->
```json
{
  "id": "discord",
  "name": "Discord",
  "version": "1.0.0",
  "apiVersion": 1,
  "description": "Posts a readable line to a Discord channel when a scan or an enrichment pass finishes, when somebody presses play or stop, or when a Library changes.",
  "docsUrl": "https://discord.com/developers/docs/resources/webhook",
  "module": "plugin.wasm",
  "provides": [
    {
      "kind": "event-sink",
      "requiresSecret": false
    }
  ],
  "network": {
    "hosts": ["discord.com"]
  },
  "settings": {
    "requiresSecret": false,
    "defaultUrl": "https://discord.com",
    "fields": [
      {
        "key": "webhook_url",
        "type": "secret",
        "label": "Discord webhook URL",
        "help": "Channel Settings → Integrations → Webhooks → Copy Webhook URL. Anyone holding it can post to the channel, so it is stored as a secret and never shown again.",
        "required": true
      },
      {
        "key": "template_scan_completed",
        "type": "string",
        "label": "Message: scan finished",
        "help": "Placeholders: {library} {kind} {count} {files} {scope} {at}. Leave empty to post nothing for this event.",
        "default": "📚 Scan of **{library}** finished: {count} titles, {files} files"
      },
      {
        "key": "template_enrich_completed",
        "type": "string",
        "label": "Message: enrichment finished",
        "help": "Placeholders: {library} {kind} {count} (matched) {total} {files} (done) {at}.",
        "default": "✨ Enrichment of **{library}** finished: {count} of {total} matched"
      },
      {
        "key": "template_playback_started",
        "type": "string",
        "label": "Message: playback started",
        "help": "Placeholders: {title} {kind} {who} {device} {at}. {who} is a User's display name, or a linked server — never a person on the other side of a Link.",
        "default": "▶️ **{title}** — started by {who}"
      },
      {
        "key": "template_playback_stopped",
        "type": "string",
        "label": "Message: playback stopped",
        "help": "Placeholders: {title} {kind} {who} {device} {at}.",
        "default": "⏹️ **{title}** — stopped by {who}"
      },
      {
        "key": "template_library_changed",
        "type": "string",
        "label": "Message: library changed",
        "help": "Placeholders: {library} {kind} {at}. Off by default — this one fires often.",
        "default": ""
      },
      {
        "key": "mention_role",
        "type": "bool",
        "label": "Mention a role",
        "help": "Prefix every message with a role ping. Off means no message from this plugin ever pings anybody, whatever a Title happens to be called.",
        "default": false
      },
      {
        "key": "role_id",
        "type": "string",
        "label": "Role id",
        "help": "The numeric id of the role to ping. Turn on Developer Mode in Discord, then right-click the role and Copy ID."
      }
    ]
  }
}
```

Field by field:

| Field | |
| --- | --- |
| `id` | Lowercase letters, digits and dashes. It is the settings key, the route segment and the directory on disk, so it is never renamed for you — a Plugin whose id is not a slug is refused. It must not collide with a Built-in's slug. |
| `name`, `description`, `docsUrl` | What the settings screen shows. |
| `version` | **Yours**, and opaque: the host never parses it, orders by it or decides anything from it. It exists so an operator can tell which build they are running. |
| `apiVersion` | The contract major you built against. It must equal the server's. A mismatch is refused **at install and at boot, never at call time**, with a message naming which side has to move: *"this server speaks plugin API v1; the plugin needs v2 — upgrade the server"*. |
| `module` | The file name beside the manifest. Omit it for the conventional `plugin.wasm`. It is a **file name**, never a path. |
| `provides[]` | One entry per seam. `kind`, plus the seam's static facts: `kinds`, `role`, `class`, `capabilities`, `requiresSecret`. |
| `network.hosts[]` | The outbound allowlist. See below. |
| `settings` | `requiresSecret` and `defaultUrl` for the fixed shape; `fields[]` for your own. See [§6](#6-how-settings-reach-the-guest). |

### `network.hosts` is the one to get right

It is the most load-bearing claim in the document and the one the host trusts
least. Exact host names — **no scheme, no port, no path, no wildcards**:

```json
"network": { "hosts": ["api.example.test", "cdn.example.test"] }
```

Matching is exact and case-insensitive on the URL's host with its port removed, so
one entry covers a source on any port. A wildcard is deliberately absent: the whole
value of the list is that an operator can read it and know what your code may talk
to.

**Omitting `network` entirely is legal and much safer** — it means a Plugin that
makes no outbound requests at all.

The host checks this list **from the file on disk, on every fetch**. Nothing your
guest says at call time can widen it.

---

## 4. The ABI: four exports and six imports

The whole calling convention, and it is small enough to state in full.

```
EXPORTS the guest provides
  obelo_alloc(size u32) -> ptr u32   the host asks the guest for a buffer
  obelo_free(ptr u32)                the host gives one back
  <call>(ptr u32, len u32) -> i64    one per contract call: (ptr<<32 | len) of
                                     the JSON response, or 0
  last_error() -> i64                (ptr<<32 | len) of why the last call
                                     answered 0

IMPORTS the host provides, in module "obelo"
  http_fetch(ptr u32, len u32) -> i64
  log(level u32, ptr u32, len u32)
  kv_get(ptr u32, len u32) -> i64
  kv_set(ptr u32, len u32) -> i64
  kv_delete(ptr u32, len u32) -> i64
  settings_get() -> i64
```

Six imports, and **that is the whole world**. A module importing any other
namespace is refused *before* it is instantiated, so "what can this code call" is
answered by reading the module rather than by watching it run.

### The one invariant: the guest owns every buffer, on both sides

The host never fabricates a guest pointer. To hand you a request it calls
`obelo_alloc`, writes into what you returned, calls your export, and frees. When a
host function answers you, it does the same thing — it calls back into
`obelo_alloc` from inside the host function rather than inventing an address.

That is why the glue below can refuse a pointer it did not hand out, and it is the
rule to hold on to if you port this to another language.

<!-- sample: main.go alloc -->
```go
// pinned keeps every buffer the host holds a pointer to reachable, and is how a
// host-supplied pointer becomes a slice again without pointer arithmetic: a
// pointer that did not come out of alloc simply is not in here.
//
// It is a plain map and needs no lock. One Plugin holds one guest instance and
// the host serialises every call through it, so two calls are never inside this
// module at once.
var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport obelo_free
func free(ptr uint32) {
	delete(pinned, ptr)
}

// lastError is why the last call answered 0. The host reads it through
// last_error() and puts it on the Admin's screen, so it is worth a sentence.
var lastError []byte

//go:wasmexport last_error
func lastErrorFn() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export may return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

// fail is the 0 answer: no response, and a sentence saying why.
func fail(msg string) uint64 {
	lastError = []byte(msg)
	return 0
}

// reply encodes a response document and hands it back.
func reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err.Error())
	}
	lastError = nil
	return emit(out)
}
```

`0` is the single sentinel: it means "no response", from your side and from the
host's alike, so you have one thing to check rather than two. When you answer `0`,
put a sentence in `last_error()` — it is what the operator will read on the
Plugins screen.

### The host functions, wired up

<!-- sample: main.go hostfuncs -->
```go
//go:wasmimport obelo http_fetch
func hostFetch(ptr, n uint32) uint64

//go:wasmimport obelo log
func hostLog(level, ptr, n uint32)

// The log levels. Anything else the host reads as info, because a bad number is
// not worth losing the operator's line over.
const (
	levelDebug uint32 = 0
	levelInfo  uint32 = 1
	levelWarn  uint32 = 2
	levelError uint32 = 3
)

// fetch asks the HOST to perform a request. The answer comes back in a buffer the
// host obtained from alloc, so it is already one of ours and freeing it is this
// guest's job.
func fetch(req fetchRequest) fetchResponse {
	in, err := json.Marshal(req)
	if err != nil {
		return fetchResponse{Error: err.Error()}
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := hostFetch(ptr, uint32(len(in)))
	free(ptr)

	if packed == 0 {
		return fetchResponse{Error: "the host answered nothing"}
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return fetchResponse{Error: "the host answered with a buffer this guest does not hold"}
	}
	var resp fetchResponse
	uerr := json.Unmarshal(buf[:rlen], &resp)
	free(rptr)
	if uerr != nil {
		return fetchResponse{Error: uerr.Error()}
	}
	return resp
}

// logLine writes one line to the server log, prefixed by the host with this
// Plugin's id. It is why a guest needs no stdout.
//
// Never log a secret. The webhook URL is one, and nothing in this file puts it
// here.
func logLine(level uint32, msg string) {
	b := []byte(msg)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	hostLog(level, ptr, uint32(len(b)))
	free(ptr)
}
```

---

## 5. The host functions, and what they refuse

### `http_fetch` — the only way out

You supply a URL; the host performs the request and hands you an answer or a
refusal. You never hold a socket, a connection or a handle.

<!-- sample: main.go fetch-shape -->
```go
// FetchRequest / FetchResponse ($defs/FetchRequest, $defs/FetchResponse): the
// host function that is the only way out.
//
// Exactly one of three things is true of a response: Refused is set (the host
// would not make this request, and retrying it unchanged will be refused again),
// Error is set (the host tried and the network failed, which may be worth
// retrying), or neither is and Status is what the target answered. A non-2xx
// status is NOT an error — it is an answer.
type fetchHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type fetchRequest struct {
	Method  string        `json:"method,omitempty"`
	URL     string        `json:"url"`
	Headers []fetchHeader `json:"headers,omitempty"`
	Body    []byte        `json:"body,omitempty"`
}

type fetchResponse struct {
	Status  int           `json:"status,omitempty"`
	Headers []fetchHeader `json:"headers,omitempty"`
	Body    []byte        `json:"body,omitempty"`
	Refused string        `json:"refused,omitempty"`
	Error   string        `json:"error,omitempty"`
}
```

**Exactly one of three things is true of a response.** `Refused` is set, or
`Error` is set, or neither is and `Status` is what the target answered. A non-2xx
status is **not** an error — it is an answer, and what to do about it is your
business.

`Refused` is the host declining. It is **host-authored prose and you must not
branch on the text**: any non-empty `refused` means *this server will not do this,
and retrying it unchanged will be refused again*. For reference, today's sentences
are:

| Refusal | Why |
| --- | --- |
| `host not in allowlist` | the target's host is not in `network.hosts` (and is not the operator's own configured target) |
| `not an absolute http or https URL` | the URL does not parse, or has no host, or is not http(s) |
| `the target resolves to an address this server will not let a plugin reach` | the host you chose resolves into loopback / private / link-local space, **or does not resolve at all** — this check fails closed |
| `the target could not be reached under this server's fetch policy` | the redirect chain was too long, changed scheme, or aimed inward |
| `the response is larger than a plugin may receive` | over the byte cap — see below |
| `the request could not be built` | the method or URL could not make an HTTP request |

Every refusal is also an **audit line** in the server log naming you and the host
you reached for:

```
obelo: plugin audit: refused a fetch: plugin=discord host=not-discord.example.test reason=allowlist
```

…with `reason` one of `allowlist`, `private-address`, `bad-url`, `fetch-policy`,
`oversize`.

Three more things the host decides and you cannot:

- **It sets the `User-Agent`** (`obelo/1.0 (self-hosted; plugin <your id>)`) and
  fills in `Content-Length`. Everything else — an `Authorization` header built
  from your secret, a `Content-Type` — is yours.
- **Redirects are bounded (3 hops) and never followed inward.**
- **Responses are capped at 1 MiB**, and going over is a *refusal*, never a
  truncation: a shortened document is one you would parse as complete.

### `log` — why you need no stdout

One line, prefixed by the host with your Plugin id and a level (`0` debug, `1`
info, `2` warn, `3` error; anything else reads as info). Newlines are stripped and
the line is truncated at 2000 characters, so you cannot forge log structure. **Never
log a secret.**

### `kv_get` / `kv_set` / `kv_delete` — your own namespace

A small durable store for a cursor, an etag, a token's expiry, a tiny response
cache. The namespace is **your Plugin id, supplied by the host from the manifest on
disk** — there is no request field you could put one in, so two Plugins writing the
same key never see each other's value.

| Cap | |
| --- | --- |
| Key | 256 bytes |
| Value | 64 KiB |

Refusals, in the response's `error` (again: prose, do not branch on it):
`this server has no key-value store for plugins` (a legitimate configuration — a
server may simply have none), `a key-value entry needs a key`, `the key is longer
than a plugin may use`, `the value is larger than a plugin may store`. An oversize
value is refused, never truncated.

`kv_delete` removes one key. **There is no "drop my namespace"** — that belongs to
uninstall, and a Plugin that could erase its own installation record would be a
Plugin deciding it is not installed.

Deleting a key that was never written is not an error. It is the state you asked
for.

### `settings_get` — your settings, for the duration of this call

It takes no request, because there is nothing to ask for: you have exactly one
settings row and it is your own.

**Outside a call it answers a zero `Settings`** — not enabled, no secret, no
declared values. That is "secrets at call time only", read literally: nothing is
installed into your module, and a guest rebuilt after a trap starts holding
nobody's credential.

An **Event sink and a Subtitle provider never need it**: their settings ride in the
call envelope. A Metadata provider does, because it has eight calls and eight
per-call envelopes would be eight places to forget a secret.

---

## 6. How settings reach the guest

Every Plugin at every Extension point is configured through the same **fixed
shape**: `enabled`, one `secret`, a `url`, an optional `url2`, a sink's `events`,
and two host-resolved knobs (`language`, `rateLimitMillis`). That shape is what the
settings screens render, and it is not going anywhere.

What it cannot express is a knob nobody compiled the server around — a region
code, a message template, a "mention this role" switch. So a manifest **declares
its own fields**, and the web app renders a form from the declaration.

### Declaring them

```json
"settings": {
  "requiresSecret": false,
  "defaultUrl": "https://api.example.test",
  "fields": [
    { "key": "region",  "type": "enum",    "label": "Region", "options": ["eu", "us"], "default": "eu" },
    { "key": "token",   "type": "secret",  "label": "API token", "required": true },
    { "key": "retries", "type": "integer", "label": "Retries", "min": 1, "max": 10 }
  ]
}
```

| `type` | JSON on the wire |
| --- | --- |
| `string` | a JSON string |
| `secret` | a JSON string — never returned by the API, handed to you only inside a call |
| `url` | a JSON string; an absolute http(s) URL, refused at save otherwise |
| `bool` | `true` / `false`, **never** `"true"` |
| `enum` | a JSON string, one of `options` |
| `multi-select` | an array of strings, every element one of `options`; `[]` is a value |
| `integer` | a JSON **number** with no fractional part — `"7"` is refused, not coerced |

Rules the host enforces, at **load** and again at **save**:

- Keys are `[a-z0-9-_]`, unique, and **may not restate a fixed key** (`enabled`,
  `secret`, `url`, `url2`, `events`, `language`, `rateLimitMillis`). Two controls
  writing one idea is how an operator configures the wrong one.
- `default` is raw JSON **of the field's own type**. A default your own rules
  refuse **refuses the whole Plugin at load** — so test your manifest by
  installing it.
- `options` belong to `enum`/`multi-select` only; `min`/`max` to `integer` only.
- `required` is never applied to a `bool`: `false` is an answer, and "a required
  switch" means "a switch that must be on", which you should express by having no
  switch.
- A key your manifest does not declare is **refused** on save, not ignored.
- Nothing is written unless every field passes.

### Reading them

The declared values arrive in `Settings.Values`, keyed by the key that declared
them, in the JSON shape the type names:

<!-- sample: main.go settings-shape -->
```go
// Settings ($defs/Settings): the fixed shape every Plugin at every Extension
// point is configured through, plus Values — the settings THIS Plugin's own
// manifest declared, keyed by the key that declared them.
//
// Only the two halves this sink reads are declared. URL is the Target URL the
// Admin typed; see the note on it in README.md. Values is where the webhook URL,
// the templates and the mention switch arrive.
type settings struct {
	URL    string         `json:"url"`
	Values map[string]any `json:"values"`
}

// SinkDeliverRequest ($defs/SinkDeliverRequest): what the host hands a sink for
// one event. The settings travel WITH the call and are never installed into the
// guest — which is why a sink needs no settings_get, and why a guest rebuilt
// after a trap starts holding nobody's credential.
type deliverRequest struct {
	Event    sinkEvent `json:"event"`
	Settings settings  `json:"settings"`
}

// SinkDeliverResponse ($defs/SinkDeliverResponse): what a sink answers. There is
// no Outcome: delivery either happened or it did not, and everything that can go
// wrong is transport. Error is in the author's own words and reaches the server
// log and the Plugin's last-error on the Admin's screen, so it should name the
// target and the status rather than restate that something went wrong.
type deliverResponse struct {
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}
```

<!-- sample: main.go values -->
```go
// stringValue and boolValue read one declared setting out of Settings.Values.
//
// The values arrive as ordinary JSON of the type the manifest named — a `bool`
// field is true, never "true", and an `integer` is a number, never "7" — so the
// type assertion is the check. A key that is ABSENT is not the same as one set to
// its zero value, which is why these answer a bare zero rather than an error: an
// unset switch and a switch turned off mean the same thing to this Plugin, and
// nothing else here has to tell them apart.
func stringValue(values map[string]any, key string) string {
	s, _ := values[key].(string)
	return s
}

func boolValue(values map[string]any, key string) bool {
	b, _ := values[key].(bool)
	return b
}
```

- **Absent and zero are different.** A field the Admin never filled is *absent*
  from `values` — unless your manifest declared a `default`, which is what the
  host stores and what you read.
- **A Plugin that declares no fields never sees the `values` key at all.**
- **A save takes effect on your next call.** The host stamps the declared values
  onto every call; there is no rebuild and no restart.
- **Never cache a secret across calls.** It exists in a value the host is handing
  into one call and nowhere else.

The Discord plugin's own settings, read this way:

<!-- sample: main.go config -->
```go
// The keys this Plugin's manifest declares. They are this author's own
// vocabulary — the host stores and returns them and never interprets one — and
// they must match manifest.json exactly, because a key the manifest does not
// declare is REFUSED at save rather than ignored.
const (
	keyWebhookURL  = "webhook_url"
	keyMentionRole = "mention_role"
	keyRoleID      = "role_id"

	// One template per curated event type. The key is the event type with its dot
	// turned into an underscore, so adding an event type to the contract is one
	// manifest line and one constant here.
	keyTemplateScan    = "template_scan_completed"
	keyTemplateEnrich  = "template_enrich_completed"
	keyTemplatePlayed  = "template_playback_started"
	keyTemplateStopped = "template_playback_stopped"
	keyTemplateChanged = "template_library_changed"
)

// config is the settings this delivery will use, read out of Settings.Values.
//
// It is a local value built per call and thrown away with the call. Nothing here
// is cached: a settings save reaches the very next delivery because the host
// stamps the declared values onto every call, and a Plugin that remembered them
// would be a Plugin serving the Admin's last-but-one answer.
type config struct {
	webhookURL  string
	mentionRole bool
	roleID      string
	values      map[string]any
}

func configure(s settings) config {
	return config{
		webhookURL:  strings.TrimSpace(stringValue(s.Values, keyWebhookURL)),
		mentionRole: boolValue(s.Values, keyMentionRole),
		roleID:      strings.TrimSpace(stringValue(s.Values, keyRoleID)),
		values:      s.Values,
	}
}

// mentions is the allowed_mentions block. Discord resolves EVERY @ in a message
// unless it is told not to, so the block is always present and always empty
// unless a role was configured on purpose.
func (c config) mentions() allowedMentions {
	m := allowedMentions{Parse: []string{}}
	if c.mentionRole && c.roleID != "" {
		m.Roles = []string{c.roleID}
	}
	return m
}
```

---

## 7. Build and install locally

From a clean clone of a plugin repository:

```sh
git clone https://github.com/goozakdev/obelo-plugin-discord
cd obelo-plugin-discord
make                       # → plugin.wasm
```

which is exactly:

```sh
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 GOFLAGS= \
  go build -buildmode=c-shared -o plugin.wasm .
```

Three things about that command:

- **`-buildmode=c-shared` is not optional.** It is what makes the module export
  `_initialize` rather than `_start`, which is what the host instantiates. Without
  it the first call traps inside an uninitialised runtime.
- **`GOFLAGS=` is cleared** so a `-mod=vendor` or a `-tags` list from whatever
  shell you are in cannot leak into a module that imports only the standard
  library.
- **TinyGo is optional.** It produces a module about 13× smaller and compiles it
  about 14× faster, and you should use it for something you ship — but Obelo's own
  suite builds with stock Go, and stock Go is the toolchain this guide assumes.

Then, two files — `manifest.json` and `plugin.wasm` — and two ways to install
them. **The layout you publish is identical to the layout on disk**, by
construction:

**Upload.** *Admin → Plugins → Install a plugin*, and pick both files. Or, the same
request by hand:

```sh
curl -X POST http://localhost:8080/api/v1/settings/plugins \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -F manifest=@manifest.json \
  -F module=@plugin.wasm
```

Two named parts, never an archive — the server never unpacks a path it did not
choose.

**From a URL.** Publish both files in one directory and paste the URL of the
`manifest.json`; the module is fetched from beside it, under the name the manifest
gives.

```sh
curl -X POST http://localhost:8080/api/v1/settings/plugins/from-url \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://example.test/obelo-discord/manifest.json"}'
```

> Unlike every other outbound fetch in Obelo, **the first hop is address-checked
> here**: a URL resolving into loopback / private / link-local space is refused,
> because what comes back is code the server executes. Serving plugins off your own
> LAN? Upload them instead.

Either way it **takes effect with no restart**: the module is compiled in a staging
directory *before* anything is installed, so a refusal leaves nothing on disk and
nothing in the database; then the registry is rebuilt and swapped and the Managers
reload.

You can also just **place the files by hand** at
`<dataDir>/plugins/<id>/manifest.json` and `<dataDir>/plugins/<id>/plugin.wasm` and
restart, which is what the layout is.

### What a refused install tells you

| Status | Code | |
| --- | --- | --- |
| 422 | `PLUGIN_INVALID_MANIFEST` | the manifest is not valid JSON, or makes a claim the host refuses |
| 422 | `PLUGIN_API_VERSION` | the message names which side to upgrade |
| 409 | `PLUGIN_DUPLICATE` | the id is already claimed — by an Installed plugin or a Built-in |
| 422 | `PLUGIN_INVALID_MODULE` | no module, or one that will not compile or instantiate |
| 422 | `PLUGIN_SOURCE_REFUSED` | the URL was not fetchable under the rules above |
| 413 | — | a part over the cap (64 MiB module, 256 KiB manifest) |

---

## 8. Reading the counters and the last error while you develop

There is no `/test` button and there is not going to be one: a sink's target is
the operator's own receiver and a "does this work" probe would be a second code
path that can disagree with the real one. What you get instead is the state the
Admin sees.

**Admin → Plugins** (`GET /api/v1/settings/plugins`):

| Field | |
| --- | --- |
| `enabled` | the **Admin's** switch, durable across a restart |
| `disabledByFailure` | **the server refusing to call you** — refused at load, or stopped after repeated failures |
| `lastError` | the sentence that says why |
| `source` | `upload`, the manifest URL that was pasted, or `placed by hand` |

`enabled` and `disabledByFailure` are not the same thing and are never merged: a
Plugin can be switched on and stopped at once, and that is exactly the state that
needs explaining.

**Admin → Event Sinks** (`GET /api/v1/settings/event-sinks`) adds the delivery
tally, per sink, since boot:

| Counter | Reading it |
| --- | --- |
| `delivered` climbing | a working receiver |
| `failed` climbing | a target that refuses or cannot be reached |
| `dropped` climbing | a target too slow for the bounded queue (64 deep, drop-oldest) |
| all three flat | you are not subscribed to anything that is happening |

**`lastError` is where your own words land.** A sink that answers
`{"delivered": false, "error": "..."}` puts that string on the Admin's screen; so
does a `last_error()` after a `0`. Name the target and the status rather than
restating that something went wrong.

**When you have rebuilt the module:** *Re-enable* on the Plugins screen clears the
recorded failure, the consecutive-failure count and the violation count, **and
reloads the module from disk** — so a fixed `plugin.wasm` gets a genuine second
try without a restart. It does not touch the Admin's `enabled` switch.

**`log()`** writes to the server log prefixed with your id:

```
obelo: plugin discord [info] posted one scan.completed message to Discord
```

---

## 9. The JSON schema, and where it lives

The contract is [`pluginapi/v1/pluginapi.schema.json`](../../pluginapi/v1/pluginapi.schema.json)
— JSON Schema draft 2020-12, `$id` `urn:obelo:pluginapi:v1`. It is a **URN and not
a fetchable URL** on purpose: the schema is checked in beside the code it
describes, and a plugin author reads the copy in the release they are building
against.

Every request and response that crosses the ABI is a `$defs` entry in it. Reference
one as `urn:obelo:pluginapi:v1#/$defs/SinkEvent`.

The prose that goes with it is [`pluginapi/v1/doc.go`](../../pluginapi/v1/doc.go)
(`go doc ./pluginapi/v1`), and it is worth reading once.

**v1 is frozen and may only change additively.** A new field is fine; a removed or
retyped one is `v2`. Every type leaves `additionalProperties` open, precisely so
that a module which ignores a field a later server adds keeps working — which is
why the Discord plugin declares only the fields it uses:

<!-- sample: main.go event -->
```go
// EventEntity ($defs/EventEntity): one thing on the server — its id, the name an
// operator would recognise it by, and its kind. The id is always there; the name
// is best-effort and a sink must not identify anything by it.
type eventEntity struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// EventActor ($defs/EventActor): WHO a playback event belongs to, and the one
// field in the contract with a rule rather than a shape.
//
// Exactly one of UserID and LinkID is set. A session relayed from a linked server
// names the LINK and never the person watching on the other side, and Name is
// then the label THIS server's Admin typed for that linked server — never a name
// from the other household. See who() below: printing a person for a Link actor
// is the one thing this Plugin must never do.
type eventActor struct {
	UserID string `json:"userId,omitempty"`
	LinkID string `json:"linkId,omitempty"`
	Name   string `json:"name,omitempty"`
}

// EventScan ($defs/EventScan): the terminal counts of a Scanner pass.
type eventScan struct {
	TitlesFound int    `json:"titlesFound"`
	FilesFound  int    `json:"filesFound"`
	Added       int    `json:"added,omitempty"`
	Removed     int    `json:"removed,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// EventEnrich ($defs/EventEnrich): the terminal counts of an Enrichment pass.
// Retrying is deliberately apart from Failed — a transient blip will be tried
// again and raising an alarm about it is noise.
type eventEnrich struct {
	Total     int `json:"total"`
	Done      int `json:"done"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
	Failed    int `json:"failed,omitempty"`
	Disabled  int `json:"disabled,omitempty"`
	Retrying  int `json:"retrying,omitempty"`
}

// SinkEvent ($defs/SinkEvent): one curated event, whole. ID, Type and At are
// always present; everything else is omitted when the event has nothing to say
// there, so a sink reads one document shape per type rather than a union.
type sinkEvent struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	At      string       `json:"at"`
	Library eventEntity  `json:"library"`
	Title   eventEntity  `json:"title"`
	Actor   eventActor   `json:"actor"`
	Device  eventEntity  `json:"device"`
	Scan    *eventScan   `json:"scan"`
	Enrich  *eventEnrich `json:"enrich"`
}
```

Two things a reader of the schema gets wrong, both from Phase 1:

- **A registration may have no factory.** Some Plugins are declarative only.
- **`url2` need not come from the same registration as `url`.** An image host and
  an API host are separate facts.

---

## 10. The rules you must not break

### Identity is the host's

You state facts; the host decides. Say *"I searched and this is what came back"*,
not *"this is the right answer"*. A Metadata provider that returns a name for a
Title the Authoritative provider already named does not rename it; the host merges
only what was left empty. A sink that is told a play started does not get to decide
whether it should have.

### The allowlist is not yours to widen

`network.hosts` is checked host-side, against the file on disk, on every fetch. You
cannot restate it at call time, and you are not told why a refusal happened beyond
the sentence.

A refused fetch is a **violation**, and violations are counted separately from call
failures and **never reset on their own**. Three of them and the server stops
calling you, with the host you reached for on the Admin's screen. (A Plugin that
misbehaves every third call and answers fine in between is still misbehaving;
letting a success clear the count would make it invisible.) Only an Admin pressing
**Re-enable** clears it.

There is one asymmetry worth knowing: **the operator's own configured target URL is
permitted alongside your allowlist**, and it is not address-checked. An author
cannot know the URL an operator will type for their own receiver, and a receiver on
their own LAN is the point of this product. A host *you* chose is checked against
the address rule; a host *they* typed is not.

### The deadline is real, and the runtime enforces it

One call into your module gets **10 seconds** (one `http_fetch` inside it gets 20).
A sink's whole delivery sits inside a **15-second** host budget.

A guest that spins past its deadline is **unwound by the runtime, not asked to
stop**: the module is closed and the instance discarded. Do not sleep, do not
retry in a loop, do not busy-wait. Three consecutive failures — a trap, a deadline
kill, or answering nothing — and you are disabled with a sentence.

Nothing about a failed delivery is ever shown to a viewer and nothing about it
slows the server down. Delivery is off the publish path, always.

### Memory only grows, so instances get recycled

A wasm linear memory never shrinks. The host retires your instance after **64 MiB**
of response bytes and rebuilds it from the compiled artifact in under two
milliseconds. **Keep nothing in a package variable that you cannot lose** — and in
particular, keep no credential there, because a rebuilt instance must start with
nobody's.

Calls into one Plugin are **serialised**, so you need no locks and you must not
assume concurrency.

### Refuse a cap rather than shorten an answer

The host states the limits: 1 MiB per fetch response, `maxBytes` on a subtitle
download, 64 KiB per kv value. When you cannot answer inside one, say so. An
answer the host has to truncate is one you would have parsed as complete, and the
host re-checks — an oversize answer is refused whole and counted as a violation.

### Every entity you are handed carries an id; show the name, key on the id

Names are best-effort and may be missing. Ids are not.

### Attribution across a Link: never print a person

A playback event that arrived over a link to another household's server carries the
**link's** id and the label *this* server's Admin typed for it — never a user id,
never a name from over there, and **no device block at all**, because a relayed
session's Device is the far household's server presenting itself as one.

The host enforces that on its side. Your job is not to undo it by rendering a link
as if it were a person:

<!-- sample: main.go who -->
```go
// who is the attribution rule, and it is the one place in this Plugin where
// getting it wrong would be a privacy failure rather than a bug.
//
// A playback event that arrived over a Link carries the LINK's id and the label
// THIS server's Admin typed for it. The person on the other sofa is not a User
// here, their name never crossed the wire, and this Plugin must never print
// anything that reads as a person for such an event. So a Link actor is rendered
// as what it is — a linked server — and a local User as their display name.
//
// An event with neither is an unattributed session, which is what the host sends
// when it could not read the session's owner: unattributed is a smaller lie than
// misattributed, and this Plugin keeps it that way.
func who(a eventActor) string {
	switch {
	case a.LinkID != "":
		if a.Name == "" {
			return "a linked server"
		}
		return a.Name + " (a linked server)"
	case a.UserID != "":
		if a.Name == "" {
			return "someone on this server"
		}
		return a.Name
	default:
		return "someone"
	}
}
```

If the actor cannot be read at all, both ids are absent. That is deliberate:
unattributed is a smaller lie than misattributed, and your rendering should keep it
that way.

### Sign nothing you did not send, and send what you signed

If you sign an outbound document, sign the **exact bytes of the body you post**.
Re-encoding between signing and sending signs one document and sends another. (The
Discord plugin signs nothing — Discord authenticates by the webhook URL itself,
which is why that URL is a `secret` field.)

---

## The whole of the Discord plugin's delivery path

For reference, end to end — the export, the send, and the rendering in between.

<!-- sample: main.go deliver -->
```go
//go:wasmexport deliver
func deliver(ptr, n uint32) uint64 {
	// The host passes a pointer it got from obelo_alloc. A pointer that is not in
	// `pinned` is one this guest never handed out, and refusing it is what makes
	// "the guest owns every buffer on both sides" checkable from in here.
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var req deliverRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail("the request is not a SinkDeliverRequest: " + err.Error())
	}
	return reply(post(req))
}
```

<!-- sample: main.go post -->
```go
// post renders the event and sends it to the Discord webhook the Admin pasted
// into this Plugin's settings.
//
// The webhook URL is a CREDENTIAL — anyone holding it can post to the channel —
// so it lives in a `secret` settings field, it is handed over only inside this
// call, and it is never written down, never logged and never cached in a package
// variable. A guest rebuilt after a trap must start with nothing.
func post(req deliverRequest) deliverResponse {
	cfg := configure(req.Settings)
	if cfg.webhookURL == "" {
		return deliverResponse{Error: "no Discord webhook URL is configured for this plugin"}
	}
	content := render(cfg, req.Event)
	if content == "" {
		// A message template an Admin cleared is an instruction not to post this
		// event type, not a failure — so the delivery succeeded and nothing was
		// sent. Reporting it as a failure would disable the Plugin for doing as it
		// was told.
		return deliverResponse{Delivered: true}
	}

	body, err := json.Marshal(discordMessage{
		Content:         content,
		AllowedMentions: cfg.mentions(),
	})
	if err != nil {
		return deliverResponse{Error: "the message could not be encoded: " + err.Error()}
	}

	resp := fetch(fetchRequest{
		Method:  "POST",
		URL:     cfg.webhookURL,
		Headers: []fetchHeader{{Name: "Content-Type", Value: "application/json"}},
		Body:    body,
	})
	switch {
	case resp.Refused != "":
		// The host would not make this request and will refuse it again, so there
		// is nothing to retry and nothing to be clever about. The refusal text is
		// the HOST's prose: report it, never branch on it.
		logLine(levelError, "the host refused the request: "+resp.Refused)
		return deliverResponse{Error: "refused by the host: " + resp.Refused}
	case resp.Error != "":
		return deliverResponse{Error: "the request to Discord failed: " + resp.Error}
	case resp.Status == 429:
		// Discord's rate limit. The host's per-call deadline is already ticking, so
		// sleeping in here would only get the instance killed; the honest answer is
		// a failure the operator can see on the sink's counters.
		return deliverResponse{Error: "Discord rate-limited this webhook (429)"}
	case resp.Status < 200 || resp.Status > 299:
		return deliverResponse{Error: "Discord answered " + itoa(resp.Status)}
	}
	logLine(levelInfo, "posted one "+req.Event.Type+" message to Discord")
	return deliverResponse{Delivered: true}
}
```

<!-- sample: main.go render -->
```go
// render turns one event into the line that will be posted, or "" for an event
// this Admin has switched off by clearing its template.
func render(c config, ev sinkEvent) string {
	tmpl, ok := c.template(ev.Type)
	if !ok || strings.TrimSpace(tmpl) == "" {
		return ""
	}
	out := substitute(tmpl, placeholders(ev))
	if c.mentionRole && c.roleID != "" {
		out = "<@&" + c.roleID + "> " + out
	}
	return out
}

// template is the message template for one event type, or "" when the contract
// grows a type this build has no template for — in which case this Plugin says
// nothing rather than inventing a line.
func (c config) template(eventType string) (string, bool) {
	var key string
	switch eventType {
	case "scan.completed":
		key = keyTemplateScan
	case "enrich.completed":
		key = keyTemplateEnrich
	case "playback.started":
		key = keyTemplatePlayed
	case "playback.stopped":
		key = keyTemplateStopped
	case "library.changed":
		key = keyTemplateChanged
	default:
		return "", false
	}
	value, present := c.values[key]
	if !present {
		// ABSENT and empty are different instructions. A key the Admin never filled
		// is absent from Values unless the manifest declared a default, so absence
		// here means "the manifest declares no default and nobody typed one" — post
		// nothing. An empty string means the Admin cleared it — also post nothing,
		// but deliberately.
		return "", false
	}
	text, _ := value.(string)
	return text, true
}

// placeholders is what a template may substitute, and the whole list of it. Every
// value is derived from the event document and from nothing else — there is no
// other source of truth in here, and there is no way to ask for one.
func placeholders(ev sinkEvent) map[string]string {
	p := map[string]string{
		"type":    ev.Type,
		"at":      ev.At,
		"library": nameOr(ev.Library, "a library"),
		"title":   nameOr(ev.Title, "a title"),
		"kind":    firstNonEmpty(ev.Title.Kind, ev.Library.Kind),
		"who":     who(ev.Actor),
		"device":  nameOr(ev.Device, ""),
		"count":   "",
		"total":   "",
		"files":   "",
		"scope":   "",
	}
	switch {
	case ev.Scan != nil:
		p["count"] = itoa(ev.Scan.TitlesFound)
		p["total"] = itoa(ev.Scan.TitlesFound)
		p["files"] = itoa(ev.Scan.FilesFound)
		p["scope"] = ev.Scan.Scope
	case ev.Enrich != nil:
		p["count"] = itoa(ev.Enrich.Matched)
		p["total"] = itoa(ev.Enrich.Total)
		p["files"] = itoa(ev.Enrich.Done)
	}
	return p
}
```

---

## Where to go next

- **The plugin**: [`obelo-plugin-discord`](https://github.com/goozakdev/obelo-plugin-discord)
  — clone it, change a template, rebuild, re-upload.
- **The contract**: [`pluginapi/v1/`](../../pluginapi/v1/) — the schema, and
  `doc.go` beside it.
- **The decisions**:
  [ADR-0057](../adr/0057-plugins-implement-a-closed-set-of-extension-points-through-a-wire-shaped-contract.md)
  (why the seams are a closed set and wire-shaped) and
  [ADR-0058](../adr/0058-an-installed-plugin-is-a-wasm-guest-called-through-a-hand-rolled-abi-on-wazero.md)
  (why wazero, why a hand-rolled ABI, and what the sandbox withholds).
- **The API**: [`docs/api-contract.md`](../api-contract.md) §3.9 for every
  settings endpoint named above.
