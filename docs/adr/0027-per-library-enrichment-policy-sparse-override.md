# Per-library enrichment overrides layer sparsely over the global config

Enrichment is configured server-wide (which Metadata providers are enabled, their keys, the metadata language), but each Library may deviate through an **Enrichment policy**: a *sparse* set of deltas — enrich on/off, metadata language, the Library's Authoritative provider, and per-provider enable/disable — where any key left unset inherits the global configuration **live**. We model the leading provider as a per-Library **pointer** (the Authoritative provider), not a per-provider role toggle; constrain that pointer to *usable* Full providers of the Library's kind; and keep the pointed-at provider **always active** while enrichment is on, so a Library can lead with a provider that is disabled for general use (e.g. an anime library leading with AniDB).

This is what makes the four motivating cases collapse into one mechanism: Home Videos sets `enrich=off`; a foreign-film library sets `language`; a documentary library toggles supplements; an anime library repoints its Authoritative provider — no per-library chain rebuild, just a diff over the global chain.

## Considered options

- **Full per-library provider profile (rejected).** Each Library owns a complete, independent copy of the provider configuration, seeded from global at creation. Rejected: silent drift — a new global provider or a newly-added API key reaches no existing Library, turning every global improvement into an N-library chore, and a library that merely wants the defaults is indistinguishable from one that deliberately froze them.
- **Per-provider `role` toggle for authoritative (rejected).** Promote/demote providers per Library via a role field. Rejected in favour of a single pointer: "which one leads" is genuinely a per-Library scalar, and a per-provider role invites multiple authoritatives and resurrects a per-Library ordering problem we don't otherwise have. Supplements stay in global catalog order; no per-library reordering.

## Consequences

- The provider registry gains a **Full provider vs Artwork-only provider** class. The Authoritative-provider pointer lists only Full providers of the Library's kind; Artwork-only providers (fanart.tv, Cover Art Archive, TheAudioDB) can never lead.
- `enabled` and *keyed* are separate gates. The pointer bypasses a provider's global `enabled` flag but not its credential requirement — a Full provider is selectable as an Authoritative provider only once its key is configured.
- The **only** way to stop a Library's enrichment is `enrich_enabled = false`. Force-disabling the current Authoritative provider via its per-provider override is a contradiction and is a no-op.
- An Authoritative provider that becomes unreachable *after* selection (its key is later cleared, or it's globally disabled) does not stall the Library. Enrichment falls back to the global default authoritative for the Library's kind and files an attention-list item; it never blocks — consistent with Enrichment's graceful-degradation posture.
- **AniDB is added** as the first anime-capable Full video provider, shipped globally disabled so it touches no library until one explicitly leads with it.
- **Changing a Library's Enrichment policy re-enriches that Library immediately** — otherwise the change appears to do nothing until the next scan. On that pass, precedence follows the most-specific-wins rule already established for corrections: Locked fields survive (only unlocked fields refresh); a per-item Enrichment override still wins as long as its record's provider stays reachable (enabled + keyed); and if the same policy change makes that provider unreachable, the orphaned override is filed to the Admin attention list (Needs-review), never silently dropped.
