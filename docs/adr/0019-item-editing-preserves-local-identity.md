# Item editing preserves local-identity authority

An Admin can correct any browsable item (Movie, Show, Episode/Special, Artist, Album, Track) through one edit affordance that exposes **three separated actions**, none of which may undermine local-identity authority ([ADR-0002](./0002-naming-convention-is-identity-authority.md)) or the identity-keyed watch state ([ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md)):

- **Fix info** — search the authoritative provider for the correct external record and apply it as an **Enrichment override**. Changes only *which* record decorates the item; identity and watch state are untouched. The common case ("filed right, matched the wrong record" — e.g. the wrong Nirvana in MusicBrainz).
- **Wrong item** *(Movie/Show only)* — the file is genuinely a different work. Applying a searched record writes a folder-keyed **Match override** (correcting identity) *plus* an Enrichment override. Resets watch state and clears Locked fields — a different work is a clean slate.
- **Fix label** — hand-edit descriptive fields, or pick a specific image the provider already offers; each is written as a **Locked field**, per-item, and never cascades.

The admin's *choice of action* is the tell for blast radius — the system never infers whether an Apply changed identity.

**Search** targets the one authoritative provider for the entity kind (TMDB for video, MusicBrainz for music); the fill-only supplements re-fill automatically once the record is pinned. A candidate carries enough to disambiguate (title, year, thumbnail, disambiguation hint); album candidates preview their tracklist for the positional map below.

**Cascade** ("apply to children", opt-in, shown only when children exist) re-resolves children under the applied parent, best-effort:
- Album → tracks and Show → episodes map **positionally** (disc+track / season+episode number).
- Artist → albums map **by title** (+year), then recurse into each album's tracks positionally.
- Each mapped child gets a **durable** per-child Enrichment override (a later enrichment pass or rescan won't silently re-auto-match it).
- A child's own prior Enrichment override or Locked field **always wins** and is skipped.
- Children that don't line up (count/number mismatch, Missing files, no title match) are surfaced in the existing Admin **attention list** — never silently changed, never aborting the whole cascade.

## Considered and rejected

- **Provider record *is* the identity** (Plex agent-GUID model): every Apply re-keys identity and resets watch state. Rejected — it reverses [ADR-0002](./0002-naming-convention-is-identity-authority.md)/[ADR-0014](./0014-watch-state-keyed-to-parsed-identity.md) and would wipe watch state in the common poster-fix case, where identity never changed.
- **Wrong-item for music/episodes:** rejected for v1. Music identity is embedded tags with no folder anchor, and the fix for genuinely-wrong tags is re-tagging the files, not an in-app identity override. Episodes have no per-episode override anchor.
- **Atomic (all-or-nothing) cascade:** rejected — unusable on the messy libraries that most need fixing; a single count mismatch would block the whole correction.
- **Confirm-each-row cascade mapping:** deferred — the friction isn't worth it; positional/title best-effort with the attention-list backstop is enough for v1.

## Consequences

- **New provider capability required:** `Search(kind, query) → candidates` and a list-artwork-candidates call. The existing `Lookup(ref)`-by-id seam stays; it can't search.
- **Locking, hand-edit, and artwork mechanisms — today Title-only — must extend to the parent entities** (Show/Season/Artist/Album). `entity_enrichment` gains a lock mechanism and edit endpoints it doesn't have.
- Seasons are not independently editable in v1 (edit at the Show or Episode grain).
- All three actions are **Admin-only**, consistent with Match override and Locked field.
