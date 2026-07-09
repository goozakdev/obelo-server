# External subtitle fetching mirrors the enrichment provider seam

Fetching subtitles from an external provider (OpenSubtitles first) was **deferred** by the enrichment PRD (`.scratch/external-metadata-enrichment/PRD.md:169`). It is now in scope, and it is built by **mirroring the existing `internal/enrich` architecture**, not beside it:

- **Provider seam** — a narrow `SubtitleProvider` interface (analogue of `MetadataProvider` in `enrich/provider.go`): `Search(ref, lang) []Candidate` and `Download(candidate) (bytes, format)`. A no-match is a sentinel error, never fatal. OpenSubtitles is the first concrete provider in the `registry.go` catalog with `RequiresKey: true`.
- **DB-backed settings**, seeded once from env/config, hot-swapped via a `Manager.Reload`, with an on/off toggle that degrades to zero outbound calls when disabled — identical to enrichment ([ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md) offline-first).
- **Storage** — a Fetched subtitle is cached **identity-keyed on the filesystem** under the data dir (exactly like `cacheArtwork` → `artwork/`, [ADR-0007](./0007-sqlite-plus-filesystem-caches.md)), recorded by a DB row whose **`source` discriminator** is `embedded | sidecar | fetched`. Local sources (embedded Stream, Sidecar subtitle) win over fetched at serve time; a rescan rewrites only local rows, so fetched subs survive — the artwork `'local'|'fetched'` pattern verbatim. Fetched subs are **never written into library folders** (read-only-mount-safe).
- **Pick-persistence** — when a user picks one candidate among many for a language, that choice **locks** (mirrors `PickTitleArtwork` locking an artwork role); re-fetch is explicit, never silent.

## Decisions specific to subtitles

- **Matching order: `moviehash` → `imdb_id` → filename query.** The OpenSubtitles **moviehash** (filesize + a checksum of the first and last 64 KiB) is **net-new** — no media-file content hash exists today; identity is path+mtime+size ([ADR-0002](./0002-naming-convention-is-identity-authority.md)). It is added because release-exact matching is the only reliable way to get **in-sync** subtitles; the already-stored `imdb_id` and a filename query are fallbacks for files with no hash match (and for un-enriched titles with no `imdb_id`).
- **Trigger: on-demand, plus an opt-in auto-fetch knob that is OFF by default.** A viewer/admin can always "search online" from the player when a title lacks a subtitle in the wanted language. A `metadata_settings`-style knob (mirroring `auto_enrich_after_scan`) can additionally auto-fetch a chosen language after scan, but defaults off — OpenSubtitles' small free-tier download quota makes fetch-everything a footgun.
- **Access: any User (including Members) can trigger an on-demand fetch.** This is a deliberate widening of the browse+play-only Member role ([CONTEXT.md](../../CONTEXT.md)): in a household trust model, the person who wants subtitles should be able to get them. The exposure — a Member spending the shared provider quota — is accepted; per-User fetch limits are a future concern, not v1.

## Why
The enrichment seam already solves provider abstraction, key management, offline degradation, background scheduling, per-host throttling, identity-keyed caching, and durable overrides. Subtitle fetching has the same shape (external, key-gated, quota-limited, cache-to-filesystem, pick-and-lock), so reusing the pattern keeps one mental model and one set of operational guarantees. The only genuinely new primitive is the content hash, justified by sync quality.

## Consequences
- A **media-file content hash** (OpenSubtitles moviehash) is introduced — the first time the server hashes media bytes. It is computed lazily (on fetch), not stored as a new identity authority; ADR-0002 is unchanged.
- The Member role gains one outward, cost-bearing action (fetch). If quota abuse becomes real, the ADR-0009-style answer is a per-User/Device limit, whose design hook already exists in the Device registry.
- Fetched subtitles are keyed to parsed identity, so a **Match override** ([ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md)) re-keys them exactly as it does watch state.
