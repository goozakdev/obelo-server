# The shipped metadata providers are Bundled plugins, not Built-ins

ADR-0057 put the eight metadata providers behind the Plugin contract as **Built-ins** — Go code
compiled into the server, registered through the same door an Installed plugin uses — and
ADR-0058 built the door: a WebAssembly guest on wazero, called through a hand-rolled JSON ABI.
The plugin-system PRD gated Phase 2 on "a source the maintainer decides should not be a
Built-in". This ADR decides the inverse for the sources the maintainer ships: **the seven
metadata providers leave the binary and become Installed plugins the server carries with it**,
and it decides what that costs the host. No release has shipped the plugin system, so the
contract and the ABI were still free to move where this needed them to. See
[PRD](../../.scratch/bundled-plugins/PRD.md).

## Decisions

**1. A shipped metadata provider is a Bundled plugin.** Each of TMDB, OMDb, TheTVDB, AniDB,
MusicBrainz, fanart.tv and TheAudioDB is a WebAssembly module and a manifest, embedded in the
server binary and written into `<dataDir>/plugins/<id>/` on first boot exactly as an Admin's
upload would be. From then on it is an Installed plugin: same sandbox, same allowlist, same
contract, same lifecycle, same screen. **Bundled** names where it came from, nothing else. The
Go provider packages under `internal/enrich` are deleted; `builtins` registers OpenSubtitles
and the Webhook sink and nothing more. The alternatives — a zero-provider server the Admin
populates, or a read-only set loaded from the embed every boot and never uninstallable — both
lose, to ADR-0001's fresh-install posture on one side and to "same lifecycle as any other
Plugin" on the other.

Why do this to the server's own code, which ADR-0057 said needed no sandbox: because a
contract only the outside world crosses is a contract the maintainer never feels. Nine Built-ins
proved the contract *could* express the providers; seven Bundled plugins prove that the ABI,
the allowlist, the deadline model, the byte caps and the settings path are sufficient for the
providers that matter most, on every boot of every server, and every gap they expose is found
by the maintainer rather than by the first outside author.

**2. The server re-asserts its bundled set on every boot, with memory.** The `plugins` row
records an origin, bundled or Admin. A bundled plugin whose installed copy is bundled and
older than the shipped version is replaced in place — settings, per-Library overrides and item
pins untouched, because the id is the same and the rows are keyed by it. One the Admin
uninstalled is remembered as declined and is not reinstalled; the Plugins screen offers
"reinstall the shipped version". A same-id plugin the Admin uploaded themselves is left
alone. First-boot-only would have made "bundled" mean "abandoned at first boot" — a server
release that fixed a TMDB bug or moved to contract v2 would leave every existing server on
the old module — and always-overwrite is the read-only shape decision 1 rejected.

**3. Bundled plugins register first, in the order the server ships them.** Registration order
is load-bearing (the providers screen, Supplement composition, and the default lead of a kind
is the first authoritative-role Full provider registered) and Installed plugins load in
alphabetical directory order, which would have made AniDB the default video lead. The server
carries an ordered list of its bundled ids; Admin-installed plugins follow, alphabetically.
The lead rule itself is unchanged, so uninstalling TMDB falls through to the next eligible
provider rather than to a name the host knows.

**4. Cover Art Archive is not a provider.** Its row's enable switch gated nothing; its only
live effect was a base URL the builder handed to MusicBrainz as a second URL, and its
connection test built a MusicBrainz provider pointed at that host. It becomes exactly that:
the MusicBrainz plugin's second URL, defaulted by that manifest, with `coverartarchive.org`
in that plugin's allowlist. The `coverart` row, its first-boot seed and any per-Library
override naming it are migrated away. A real Cover Art Archive plugin — an Artwork-only music
Supplement of its own — was considered and rejected because it is a behaviour change the
music chain cannot compose today, and a module-less manifest was rejected because it invents
a kind of Plugin for one URL field.

**5. Guests pace themselves; the host-side throttle clause of ADR-0058 is withdrawn.** ADR-0058
decision 5 said `http_fetch` applies the ADR-0049 limiter keyed on host. It never did, and
this ADR decides it will not. One instance per Plugin, serialized, already makes a guest's
own pacing process-wide for its source, which is the property ADR-0049 wanted; what is given
up is a shared budget between two *different* Plugins on one host, and that case does not
exist among the seven. The operator's rate-limit setting reaches every provider through the
fixed Settings shape (until now only MusicBrainz received it). The authoring guide says "pace
yourself".

**6. A fetch always returns before the call deadline, and a metadata call has time to pace.**
Under ADR-0058 a fetch ran under the call's own context, so a slow upstream killed the guest
at the 10-second call deadline, the kill counted toward the three-strikes disable, and three
slow lookups parked a whole provider — the ADR-0048 violation, applied to a source instead of
an item. Now: Metadata provider calls get a 30-second default budget, raisable per manifest to
a host cap; each `http_fetch` is bounded by the remaining budget minus a grace margin; a slow
upstream comes back to the guest as a fetch error, the guest answers `unavailable`, the item
takes ADR-0048's backoff, and no failure is counted. A deadline kill now means one thing —
the guest itself spun — which is what the threshold was written for. Exempting bundled
plugins from the threshold was rejected: it special-cases origin and leaves a third-party
provider with the violation.

**7. The host owns the User-Agent.** Every fetch a guest makes carries the identity the
Built-ins sent: `obelo/<server.Version> ( <project contact> )` with the plugin id and version
appended as a comment. A guest-supplied agent is dropped. The contact on an agent is for
whoever ships the code behind the request pattern, and for code running inside Obelo's
fetcher and under Obelo's fetch policy that is still Obelo; MusicBrainz requires the shape and
throttles anonymous agents harder. This also retires a hardcoded `obelo/1.0` and a two-header
bug in the host function.

**8. The manifest declares its own connection probe.** "Test connection" was a switch over
eight slugs, each with a hand-picked reference the author knew that source could answer, and
an Installed provider fell into "unknown provider". The Metadata provider entry of a manifest
gains an additive `probe`: one media reference the author knows their source answers. The
host runs an ordinary lookup with it and keeps the judgment: matched or no-match proves the
host was reached and the credential accepted; unavailable, refused or a fetch error is a
failed test. A ninth contract call was rejected as surface for something a lookup already
proves; one fixed probe per kind was rejected because AniDB cannot search by name.

**9. One Go module per provider in the server repo, sharing a Go SDK.** The seven live under
`plugins/<id>/`, each its own module so "cannot import `internal/`" is a compile-time fact,
joined by a workspace file. They share a Go SDK module — the ABI glue, typed wrappers for the
six host functions, a dispatcher from a Go value to the exported functions — and the contract
package and the SDK become Go modules of their own inside the repo so an outside author can
import them without the server's dependency graph. The Discord reference plugin stays
SDK-free and remains the proof that none is required. Seven stdlib-only copies of the glue
(the Discord shape) were rejected as a drift surface no test covers; sibling repositories with
vendored copies (also the Discord shape) were rejected because these plugins ship inside the
server and version with its contract, and a clean clone must build the server.

**10. Stock Go builds the modules; they are embedded compressed and never committed.** The
build is the one the reference plugin and the amd64 harness already use. A stock-Go module
compresses to about a megabyte, the server decompresses each once when it writes it to the
data directory, and the binary grows by about seven megabytes rather than twenty-four. TinyGo
was rejected as a second toolchain on every machine and in the Dockerfile, with reflection
limits that `encoding/xml` (AniDB) would meet mid-port. `make plugins` builds into a
gitignored embed directory with a committed marker; a test in that package fails with "run
make plugins" when a module is missing, a check fails if a module is ever tracked by git, and
both run under `make check` — the web bundle's shape with the half of the guard that rotted
now automated.

**11. The port is a conversion, not a redesign.** The seven move with zero behaviour change,
proven the way Phase 1 was: the `internal/api` black-box suites pass unmodified, apart from
assertions about the Cover Art Archive row. Environment variables, shipped default keys
(ADR-0032), provider ids, settings rows, per-Library overrides and the consent gate are
untouched. The contract's five named id fields, the music chain's two hardwired Supplements
and the web app's hardcoded provider pickers are pre-existing gaps for a third-party provider,
not things this conversion worsens, and each is its own follow-up decision. OpenSubtitles
stays a Built-in; bundling it is the obvious next use of the machinery and its own decision,
because a subtitle download is the one call where a byte cap changes what a viewer sees.

*Amended 2026-09-18 (.scratch/bundled-plugins issue 09): OpenSubtitles is a Bundled plugin
too*, the eighth, registered after the seven because the order only matters to metadata.
The two numbers the paragraph above left open were decided by **parity**, not raised or
lowered: the plugin asks for an **8 MiB** fetch limit (`maxFetchBytes`) and a **30-second**
call budget (`callBudgetMillis`) — exactly what the Built-in accepted (its own and
`internal/subfetch`'s 8 MiB cap) and waited (the request handler's 30 s). The seam defaults
a third-party subtitle provider still gets are unchanged at 1 MiB and 10 s. Both knobs were
honoured on a `metadata-provider` entry only; the host now honours them on a
`subtitle-provider` entry too, so no subtitle that downloads today is discarded tomorrow
and no slow search is killed sooner than before. Evidence for the cap was sought and not
found locally — the development machine holds no fetched-subtitle cache — so it rests on
parity, and on a typical SubRip file being tens of kilobytes while a heavily typeset ASS
file runs to single megabytes; the Comments of issue 09 carry the command to measure a real
library. One behaviour is deliberately NOT parity: OpenSubtitles' spent daily download
quota (a 406) is answered as `unavailable`, because a Subtitle provider's clean error is
still a strike and three of them would disable the plugin for the rest of the day. After
this the word **Built-in** names only the Webhook sink.

*Amended 2026-09-19 ([ADR-0060](./0060-a-record-id-is-namespaced-and-a-pin-holds-only-a-decision.md),
.scratch/bundled-plugins issue 10): the named-id gap is closed.* `MediaRef.ExternalIDs` carries
every id the host holds, keyed by namespace; the five named fields are v1 mirrors filled from it.
A Title's record is namespaced in `title_external_ids`, so `videoIDColumnProvider` — the one
shipped name this ADR's port could not retire — is gone.

*Amended 2026-09-19 ([ADR-0061](./0061-the-music-chain-composes-every-music-supplement.md),
.scratch/bundled-plugins issue 11): the music chain's gap is closed.* It composes every active
music Supplement in registration order, the video chain's shape, and MusicBrainz declares its
synthesized artist Overview so TheAudioDB's biography still replaces it. Decision 4's *reason*
for rejecting a Cover Art Archive plugin no longer holds; the decision stands.

*Amended 2026-09-19 (.scratch/bundled-plugins issue 12): the web app's pickers name no
provider.* The Edit-item and Needs-Fixing pickers no longer choose TMDB or MusicBrainz by media
kind, or decide with a local regex whether the box holds an id. Every `enrichmentCandidates`
search first asks the item's Library's lead to read the query as a reference, through its
`external-ref` capability, or through the host's own reader when that reader's source leads.
A resolved one comes back flagged `resolvedRef` and is selected. The host reader no longer
answers for a lead whose namespace it isn't, so an AniDB-led Library's bare number is a
search term, not a TMDB id.

*Amended 2026-09-22: a guest's real clocks gained a cap.*
Decision 6's fetch-bounded-by-budget rule assumed a guest paces
itself with a Nanosleep that measures nothing, because that is what wazero
defaulted to; enabling the REAL walltime, nanotime and Nanosleep a guest needs to
pace at all (`pluginsdk.Pacer.Wait` is a no-op against a fake clock) turned out to
reopen the hole decision 6 closed: wazero's own deadline enforcement
(`WithCloseOnContextDone`) only takes effect when the guest re-enters wasm, not
while it is blocked inside a real `time.Sleep`, so a guest that slept past its
call budget was killed anyway, just LATER — at however long the sleep ran, not at
the budget, still counted as a strike. Two mechanisms, host and SDK, close it. The
host caps every guest's Nanosleep at what remains of the CURRENT call's deadline
(`internal/plugins/guest.go`), so a deadline kill — when one still happens —
happens at the budget, never later; that is a backstop and nothing more, and a
guest that sleeps past its budget still pays for it with a kill, exactly as one
that traps does. The HOST additionally tells a guest the time REMAINING until
the deadline it is actually enforcing on the wire
(`pluginapi.Settings.CallRemainingMillis`, additive and optional, resolved after
any wait for the Plugin's own call lock) and the SDK builds a
`context.WithTimeout` a margin short of it for every export
(`pluginsdk.CallContext`), so a guest that honours ctx — `Pacer.Wait`, `GetJSON`
— notices first and answers `unavailable` instead of being killed at all, which
the host counts as an answer, not a failure. `plugins/musicbrainz`'s own
guest-side 88-second guard is unaffected; the other seven Bundled plugins now get
the same protection without a guard of their own.

*Amended 2026-09-23: the remaining time is stamped after the call lock, at
every seam.* The number above has to be the time actually left once a call
clears `internal/plugins`' own serializing lock on its Plugin, not the seam's
nominal budget computed before that wait — a call queued behind another
in-flight call on the same Plugin that was told the nominal number could build
a ctx that already outlives the host's real deadline, turning a clean
`unavailable` back into the deadline kill this amendment exists to avoid. Every
seam — `deliver`, the two subtitle calls, `settings_get` for a Metadata
provider — reads it from the bounded context `callGuestUnder` builds after
taking that lock.

## Consequences

- CONTEXT.md gains **Bundled plugin**; **Built-in** now names OpenSubtitles and the Webhook
  sink; Cover Art Archive leaves the Artwork-only provider examples. *(After issue 09,
  **Built-in** names only the Webhook sink, and **Bundled plugin** names eight.)*
- **(2026-09-18, after issue 08)** ADR-0058 decision 7 carries a second amendment closing the
  last behaviour change this conversion made: a Metadata provider guest that runs to completion
  and cleanly answers an error keeps its instance and costs no strike, so a rejected key parks
  the items exactly as the Built-in did instead of disabling the provider after three. Traps,
  deadline kills and malformed responses still count, and an Event sink's or Subtitle provider's
  clean error still counts. `internal/api:TestARejectedKeyParksTheItemsAndLeavesThePluginRunning`
  replaces the characterization test that pinned the old answer.
- ADR-0058 decision 5 is amended (throttle withdrawn, User-Agent host-owned) and decisions 6
  and 7 are amended (fetch bounded inside the call budget; 30-second metadata budget). ADR-0057
  decision 5 gains a note that "Built-ins go first" was carried out and then superseded for
  the Metadata provider Extension point.
- The Dockerfile gains a plugin build stage and copies the module-root trees it currently
  omits (the contract package is already missing from it on `main`).
- `go install .../cmd/obelo@latest` stops working once the repo holds nested modules joined by
  `replace`; nobody installs the server that way (ADR-0006: Docker first), and it is recorded
  here so nobody wonders.
