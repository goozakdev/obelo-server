# Plugins implement a closed set of Extension points through a wire-shaped contract

The server's external sources — eight metadata providers and OpenSubtitles — each sit behind
a narrow Go interface with a static registry, DB-backed settings and a hot-swapping Manager
(metadata-providers, ADR-0021). This ADR decides that those seams, plus a new Event sink,
become one **contract** every such unit of code implements, that the unit is called a
**Plugin** whether compiled in or (later) installed, and what the contract may and may not
express. It deliberately does **not** decide how an installed Plugin is loaded or sandboxed;
that is a second ADR, written when Phase 2 of `.scratch/plugin-system` opens on its stated
gate. See [PRD](../../.scratch/plugin-system/PRD.md).

## Decisions

**1. Three Extension points, and the set is closed.** Metadata provider, Subtitle provider,
Event sink. **Identity is not one**: the naming convention is the identity authority
(ADR-0002) and a pluggable parser could re-key watch state (ADR-0014). **Transcoding and
probing are not one**: child-process and GPU control stay in core. **Authentication is not
one**: ADR-0001 says the server owns identity and delegates to no IdP; LDAP/OIDC is a
trust-model question for its own ADR, not a loader question. The set grows by ADR, not by a
Plugin asking.

**2. The contract is wire-shaped from day one.** Plain structs that round-trip through JSON;
no interfaces, callbacks or streams in a signature; every call takes a context with a
deadline; byte payloads come back whole and size-capped; paging is offset-based. Outcomes
that today are wrapped error sentinels (`ErrNoMatch`, `ErrMatchRejected`,
`ErrSearchUnavailable`, the external-ref errors) are an **explicit outcome enum** at the
contract edge, mapped back to the sentinels by adapters so `errors.Is` callers in the service
are untouched. Nothing crosses a boundary yet; the shape is chosen so that when something
does, the contract does not migrate a second time across nine Built-ins.

**3. The host owns every judgment.** A Plugin supplies data and declares capabilities
(`search`, `artwork-candidates`, `album-tracklist`, `external-ref`); the host decides whether
to believe it. The ADR-0050 title acceptance test moves out of the MusicBrainz provider into
the service, applied to music kinds only so behavior is unchanged. This is what makes
decision 4 safe.

**4. An installed Plugin may be a Library's Authoritative provider.** Role and Class
(ADR-0027) are carried by the contract exactly as the registry carries them today, and the
Enrichment policy may repoint a Library at a Plugin-provided Full provider. The alternative —
Supplement-only — was considered and rejected because the case that most wants a Plugin is a
Library whose right lead is a source the core does not ship.

**5. Built-ins go first, through the same door.** The eight metadata providers and
OpenSubtitles register through the contract from the composition root (explicit
registration, no `init()`), registries become values the builder consumes, and the
`internal/api` black-box suites must pass unmodified. If the contract cannot express TMDB's
image host or MusicBrainz's release-group identity, that is discovered while the contract can
still change freely.

**6. Event sinks see a curated set, best-effort, idempotent by id.** A translator derives
five terminal events (`scan.completed`, `enrich.completed`, `playback.started`,
`playback.stopped`, `library.changed`) from the Broker's UI snapshots; the snapshots
themselves are not a public API. Delivery is in-memory with a per-sink bounded queue,
drop-oldest, a dropped counter, and a deadline, **off the publish path** so a slow sink can
never stall a scan or a play. Every event carries a stable id and the sink call is idempotent
on it, so an at-least-once outbox can be added behind the same call later without changing a
sink. A relayed session names the Link, never a person (ADR-0054).

**7. The contract lives in `internal/` until Phase 2.** `internal/pluginapi/v1` is free to
churn while the Built-ins expose its gaps. The first Phase 2 issue moves it to the module
root, freezes it additively, and generates a JSON schema for authors in other languages. The
Go package is a convenience; the schema is the contract.

> **Carried out (plugin-system issue 07, 2026-09-17):** the package is now
> `pluginapi/v1` at the module root and is **frozen additively** — fields and enum values may
> be added within v1, never removed, renamed or retyped, and `additionalProperties` is left
> open on every type so a v1.0 guest tolerates a later v1.x host's fields. The schema is
> generated from the Go structs into `pluginapi/v1/pluginapi.schema.json` (`go generate
> ./pluginapi/v1`), a test fails when the checked-in copy is stale, and every golden document
> in the contract suite is validated against it. Two rules that were prose became real schema
> constraints: an event actor carries a user id **or** a link id and never both (both absent
> is legal), and a sink event's `scan` and `enrich` blocks are mutually exclusive.

## Why

Jellyfin pays for plugins on every major release because its internal types *are* its wire
types. Obelo can avoid that only by separating them before the first external author exists,
and the cheapest moment to do that is while the only authors are the Built-ins. The
host-owns-judgment rule exists because ADR-0049's "a confident wrong overview is worse than an
empty one" has to hold for code whose author never read ADR-0049, and Authoritative
eligibility is only defensible once it does. The Event sink is in scope now because it is the
one Extension point with no existing seam, so its semantics need settling while the contract
is fluid, and because a Webhook is the one piece of this the maintainer would use today.

## Consequences

- `acceptsTitle` leaves `musicbrainz.go`; the `search-rejected` reason is produced by the
  service. Video kinds do not gain acceptance by this ADR — that is a separate decision.
- The settings shape stays fixed (enabled, secret, url, optional url2, and an `events` list
  for sinks). A generic manifest-declared schema is Phase 2.

  > **Carried out (plugin-system issue 13, 2026-09-17):** the fixed shape did not move. An
  > INSTALLED plugin's manifest may now declare typed fields of its own
  > (`ManifestSettings.Fields`), whose values travel in a single new field beside the fixed
  > ones (`Settings.Values`) — one field on one struct, so a Built-in's dialog and settings
  > row are untouched and a guest that declares nothing is unchanged. The host validates a
  > save against the manifest on disk and refuses per field; a declared secret is masked on
  > read and reaches a guest only inside a call, exactly as the fixed `secret` does.
  >
  > The same slice closed a hole this ADR's "keyed ⇒ active" inference left: a provider that
  > declares it needs no credential could not be turned on at all, because every composition
  > gate asked whether its key was non-empty. `ProviderConfig` now carries an explicit
  > per-provider active fact for any Plugin the binary has no named key field for — from
  > Phase 2, every Installed one — and key presence still answers for the eight Built-ins,
  > which is why none of their behaviour moved.
- The transport comparison (stdlib `plugin`, go-plugin, WebAssembly via wazero) is recorded
  in the PRD as a leaning and is **not** decided here.
- CONTEXT.md gains Plugin, Extension point, Built-in, Installed plugin, Event sink.
