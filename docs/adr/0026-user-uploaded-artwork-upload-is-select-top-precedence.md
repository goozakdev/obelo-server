# User-uploaded artwork: upload-is-select, with top serve precedence over Local and Fetched

An Admin can already **pick a provider image** for an **Artwork role** (Poster/Background/Artist photo/Album cover) from a live candidate grid, which stores it Fetched-and-Locked ([ADR-0019](./0019-item-editing-preserves-local-identity.md)). This ADR adds the one thing that flow can't do: supply an image the providers don't offer, by **drag-drop or Browse upload** in the same Edit-item dialog. It also promotes the artwork pickers out of the "Fix label" tab into dedicated per-role tabs that auto-search on open. See [CONTEXT.md](../../CONTEXT.md) (**Uploaded artwork**, **Artwork source**, **Artwork candidate**, **Artwork role**).

The picker machinery already exists (`ArtworkPicker`, the `ArtworkCandidates` provider seam, the per-role pick+lock endpoints); this is an extension of it, not a rebuild. Upload is the only genuinely new server capability — there is no upload endpoint anywhere today.

## Scope — this pass

- **Entities:** Movie and Show (Poster + Background), Artist (Artist photo), Album (Album cover). **No** per-Episode still, per-Season poster, or per-Track image; **no** Background for music.
- **Admin-only**, like every other correction action; changes emit the existing realtime (SSE) update so other signed-in users see the new image without a refresh.
- **Deferred, deliberately:** in-app **re-crop** and a **persistent pool of several posters per role** (a durable gallery to switch among). Provider candidates are re-queryable so they need no persistence; only the single selected image is stored. These are extensions over this surface, not reversals of it.

## Decisions

- **Uploading *is* selecting (no candidate pool).** Dropping in / browsing to an image applies it immediately: store the bytes in the identity-keyed artwork cache (never in the library folder — [ADR-0007](./0007-sqlite-for-durable-state-filesystem-for-fetched-artwork.md)), fill the role, and Lock it. There is no accumulating list of uploads to choose among; a new upload replaces the last. This reuses the existing pick path (bytes instead of a provider URL → set + lock) rather than adding a new pool table.
- **A distinct `source = 'uploaded'`.** The `artwork` / `entity_artwork` `source` CHECK grows from `('local','fetched')` to include `'uploaded'` (migration). Uploaded is not `local` (that means scanner-discovered folder art, which rescans reconcile against disk and could drop an upload) and not `fetched` (which loses to Local). The upload's cache filename is source-qualified so it never collides with the Fetched cache file for the same role.
- **Serve precedence: Uploaded > Local > Fetched.** An Uploaded image outranks *everything*, including a `poster.jpg` in the library folder. A picked provider image stays Fetched, so it still loses to Local — the existing rule is unchanged. Upload is the **sole** source that beats Local.
- **The Uploaded image is lock-coupled.** It exists only while the role is Locked; **releasing the artwork Lock deletes the Uploaded file and reverts the role to auto** (Fetched/Local). That doubles as "undo my upload" — no separate delete affordance.
- **Validation:** accept **JPEG / PNG / WebP** only (reject SVG/HEIC/animated/PDF with a clear error); **16 MiB** cap (the existing artwork fetch limit); no minimum-dimension enforcement and no server-side crop/resize — bytes stored as-is.
- **Tabs auto-search on open** and **auto-apply on select** (no "Choose image" pre-click, no Save button). Immediacy reuses the existing `artworkVersion` cache-bust token so the on-screen `<img>` reloads. Artist-photo candidates — empty today (`MusicBrainz.ArtworkCandidates` returns `nil` for non-album) — are wired up by surfacing fanart.tv's already-decoded `artistthumb[]` (plus TheAudioDB), which the auto-enrich pass currently collapses to one.

## Why upload beats Local

Local-folder-art-always-wins ([ADR-0019](./0019-item-editing-preserves-local-identity.md), story 21) exists so file-based art management isn't silently overridden by in-app *fetches* — and a provider-*pick* is still "choose among the provider's options," a weaker human signal than a file the Admin placed on disk. An **upload is the opposite**: the Admin is handing over the exact bytes to display. "I uploaded a poster and still see the old one" is a worse surprise than the small inconsistency that an upload beats folder art while a pick does not. The conflict is rare in practice (someone managing art via files usually isn't also uploading), and it is fully reversible with a lock release.

## Consequences

- **Schema + serve change:** the `source` CHECK gains `'uploaded'`; the per-role serve ordering becomes `uploaded → local → fetched` (was `local → fetched`). Read paths that assume one local + one fetched row per role now admit a third source.
- **New API surface:** a multipart **upload endpoint** per artwork role (Admin-only) alongside the existing candidates/pick endpoints — the first `multipart`/`FormFile` handler in the server.
- **Provider growth:** `ArtworkCandidates` gains an artist branch (fanart.tv `artistthumb[]` + TheAudioDB), so artist photos become a real candidate list like every other role.
- **Web:** the Edit-item dialog grows per-role artwork tabs (Poster/Background/Artist Photo/Album Cover) that auto-load candidates and add an upload drop-zone + Browse button; the artwork pickers leave the "Fix label" tab (which keeps its text-field editing).
- **Lifecycle coupling:** lock-release must delete the Uploaded file, so the release path becomes source-aware — releasing an Uploaded role is destructive (the bytes are gone), unlike releasing a picked/fetched role.
