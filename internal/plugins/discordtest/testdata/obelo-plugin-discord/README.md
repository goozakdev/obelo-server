# Obelo → Discord

An [Obelo](https://github.com/goozakdev/obelo-server) **Event sink**: it posts a
readable line to a Discord channel when a scan finishes, when an enrichment pass
finishes, when somebody presses play or stop, or when a Library changes.

It is also Obelo's **reference Installed plugin**. Every code sample in the
server's [plugin authoring guide](https://github.com/goozakdev/obelo-server/blob/main/docs/plugins/authoring.md)
(`docs/plugins/authoring.md` in the server repo) is extracted from `main.go` by a
test, and the server's own acceptance suite builds this plugin from source and
drives it end to end. If the contract changes under it, the server's build breaks.

```
📚 Scan of **Movies** finished: 214 titles, 214 files
▶️ **Dune** — started by brandon
▶️ **Dune** — started by The other household (a linked server)
```

---

## Build

Stock Go 1.26, no dependencies, no toolchain to install:

```sh
make            # GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o plugin.wasm .
```

`-buildmode=c-shared` is not optional: it is what makes the module export
`_initialize` rather than `_start`, which is what the host instantiates. A module
built without it traps on its first call inside an uninitialised runtime.

[TinyGo](https://tinygo.org) produces a module about 13× smaller and compiles it
about 14× faster (`make tinygo`). It is entirely optional — Obelo's own test suite
builds this plugin with stock Go.

## Install

Two files make a Plugin: `manifest.json` and `plugin.wasm`. There are two ways to
get them onto a server, and they are the same two for every plugin:

**Upload.** Admin → Plugins → *Install a plugin*, and pick both files.

**From a URL.** Publish the two files in one directory and paste the URL of the
`manifest.json`; the server fetches the module from beside it. The server refuses
a URL that resolves into loopback / private / link-local space — what comes back
is code it will execute — so serve it from a public address or upload instead.

Both take effect immediately. There is no restart.

## Configure

Admin → **Plugins** → Discord, for this plugin's own settings:

| Setting | |
| --- | --- |
| **Discord webhook URL** | *Channel Settings → Integrations → Webhooks → Copy Webhook URL.* Stored as a secret: it never comes back out of the API, and Obelo hands it to the module only for the duration of one delivery. |
| **Message: …** | One template per event type. `{placeholders}` are substituted from the event; clear a template to post nothing for that event. A `{name}` this plugin does not know is left alone, so a typo shows up as a typo. |
| **Mention a role** + **Role id** | Off by default. With it off, nothing this plugin posts can ping anybody, whatever a Title happens to be called — the message is sent with an empty `allowed_mentions.parse`. |

Then Admin → **Event Sinks** → Discord, for the sink half:

| Setting | |
| --- | --- |
| **Enabled** | on |
| **Target URL** | `https://discord.com` |
| **Events** | whichever of the five you want |

### Why there are two URLs

Obelo's fixed settings shape gives every Event sink one **Target URL**, and it
refuses to enable a sink that has none — for the Webhook built in to the server,
that URL *is* where the document goes.

A Discord webhook URL cannot live there, because it is a **credential**: anybody
holding it can post to the channel, and the fixed Target URL is returned in plain
text by the settings API. So the webhook URL is a `secret` field this plugin
declares for itself, and the Target URL states the **host** this plugin is allowed
to reach: `https://discord.com`.

Setting it to anything else does not widen what this plugin can do — the manifest
allowlists `discord.com` and nothing else, and the server checks that allowlist
from the file on disk on every request — but it is the value that matches what
this plugin actually does, and a future contract that lets a manifest mark the
fixed URL secret would collapse the two.

### Placeholders

| | scan | enrich | playback | library.changed |
| --- | --- | --- | --- | --- |
| `{library}` | ✓ | ✓ | | ✓ |
| `{title}` | | | ✓ | |
| `{who}` | | | ✓ | |
| `{device}` | | | ✓ (local plays only) | |
| `{kind}` | ✓ | ✓ | ✓ | ✓ |
| `{count}` | titles found | titles matched | | |
| `{total}` | titles found | titles considered | | |
| `{files}` | files found | titles done | | |
| `{scope}` | targeted scans | | | |
| `{at}` `{type}` | ✓ | ✓ | ✓ | ✓ |

### `{who}`, and the rule behind it

A play that arrived over a **link to another household's server** names the *link*
and never a person. Obelo enforces that on its side — such an event carries the
link's id, the label your Admin typed for it, and no user id at all — and this
plugin renders it as what it is:

```
▶️ **Dune** — started by The other household (a linked server)
```

The name of the person on the other sofa never crosses the wire, and nothing in
this plugin can print one. A local play names the Obelo User who started it.

## Develop

Obelo's Plugins screen is the debugger:

- **Last error** — the sentence the module returned, or the reason the server
  stopped calling it (a refused fetch names the host it reached for).
- **Delivered / dropped / failed**, on the Event Sinks screen — the delivery tally
  since boot. `delivered` climbing is a working webhook; `failed` is a target that
  refuses or cannot be reached; `dropped` is a target too slow for the queue.
- **Re-enable**, on the Plugins screen — forgives a recorded failure *and* reloads
  the module from disk, so a rebuilt `plugin.wasm` gets a genuine second try.
- `log()` writes to the server log, prefixed with the plugin id. Never log the
  webhook URL.

The server's [authoring guide](https://github.com/goozakdev/obelo-server/blob/main/docs/plugins/authoring.md)
covers the rest: the contract's JSON schema, the six host functions and what each
refuses, and the rules a plugin author must not break.

## Licence

AGPL-3.0-or-later, matching the server. See [LICENSE](./LICENSE).
