# A record id is namespaced, and a pin holds only a decision

[ADR-0059](./0059-the-shipped-metadata-providers-are-bundled-plugins.md) decision 11 left the
contract's five named id fields as a pre-existing gap for a third-party provider. This ADR closes
it, and in doing so decides what a Title's record *is* once more than one source can supply it.
See `.scratch/bundled-plugins/issues/10-follow-up-a-source-namespaced-external-id.md`.

## Where it stood

`pluginapi.MediaRef` carried `TMDBID`, `IMDBID`, `MusicbrainzID`, `TheTVDBID`, `AniDBID` — one
field per shipped source — and a Title kept its record in columns named the same way:
`enrichment_tmdb_id`, `enrichment_imdb_id`, `musicbrainz_id`. Three things followed:

- **A third-party source had nowhere to put an id**, on the way in or on the way back.
- **A non-TMDB video lead wrote its ids into the TMDB column.** `ExternalMatchForKind` maps
  every video record onto `enrichment_tmdb_id`, so an AniDB-led Library stored AniDB aids
  there; `pinnedProviderFor` then read the column's *name* and pinned those Titles to TMDB,
  where the next pass asked TMDB for an AniDB number. The same held one level up: a parent's
  `entity_enrichment.external_id` carried no namespace, so a repointed Library looked an old
  Show's id up at the new lead.
- **Two named fields were dead on input.** The host never filled `TheTVDBID` or `AniDBID`.

## Decisions

**1. An id is keyed by NAMESPACE, not by plugin.** The namespaces are `tmdb`, `imdb`,
`musicbrainz`, `thetvdb`, `anidb`, and any third-party source's own. A record's `Source` names
the namespace its `ExternalID` belongs to. A plugin's namespace is its plugin id; every shipped
source that returns an `ExternalID` already satisfies that, so there is no manifest field to
declare a different one until a real plugin needs it. Keying by plugin was rejected because
ids and their readers do not line up one-to-one: IMDb ids belong to no plugin (OMDb reads
them), and MusicBrainz ids are read by fanart.tv and TheAudioDB too.

**2. A Title's record ids live in `title_external_ids`**, one row per `(title_id, namespace)`,
and `titles.enrichment_id_namespace` names the row that is the Title's RECORD — the one a pin
is keyed on. `enrichment_tmdb_id`, `enrichment_imdb_id` and `titles.musicbrainz_id` migrate
into rows and are dropped. A table rather than a generic column pair because a Title can hold
several ids at once (an AniDB record and an IMDb cross-reference for OMDb), and because adding
a namespace must never need a schema change. ADR-0045 rejected "one opaque
`enrichment_external_id`" only because it "would have to carry a namespace tag"; a table
carries one natively. **Identity is untouched:** `tmdb_id` / `imdb_id` stay Scanner-owned
identity columns, filled from folder tokens and spelled in `identity_key` (ADR-0002), because a
`{tmdb-…}` token is TMDB by definition. ADR-0045's precedence — Admin's override, then the
folder's id, then a pass's own fill-only resolution — reads the same, over rows instead of
columns.

**3. A parent's record carries its namespace too.** `entity_enrichment.external_id_namespace`
sits beside `external_id`. A parent keeps one id, because it has exactly one Authoritative
provider (ADR-0045's own reason for its single column), and gets the same pin rule as a leaf.

**4. Existing rows are namespaced by the source that wrote them, where that is provable.** A
`matched` row's `enrichment_source` was written in the same statement as its id, so it names
the id's namespace when it is an Authoritative namespace (`tmdb`, `anidb`, `thetvdb` for
video). Everything else falls back to the namespace the column has always been read as:
`enrichment_tmdb_id` → `tmdb`, `enrichment_imdb_id` → `imdb`, `musicbrainz_id` →
`musicbrainz`, and a parent to its kind's default lead. The guard is `matched` because an
override resets the status to `pending` without touching `enrichment_source`, so a stale source
can sit beside a newer id only on a row that is not `matched`. This rescues the AniDB aids that
leaked into the TMDB column instead of making them TMDB ids for good.

**5. The HOST stamps an Admin's pick with its namespace.** It always knows which provider it
asked: a Fix-info search went to a known lead, and a paste was answered by a known plugin or by
the host's own reader for a namespace it owns. The API's candidate JSON carries `source`, the
apply request echoes it, and a request without it means the current lead's namespace, so an
older web bundle keeps working. An Episode pin and a Cascade inherit the parent's namespace.
`pluginapi.SearchCandidate` gains nothing: a guest cannot know anything about which provider
it is that the host does not, and a second place to state it is a second place to disagree
(ADR-0057 decision 3: the source reports, the host decides). `parseTMDBRef` survives as the
host's reader for the `tmdb` namespace, no longer because a column is named after TMDB.

**6. A pin holds only a DECISION.** A Title or parent resolves via its record's namespace's
provider, rather than the Library's lead, when the record is:

- **chosen or cascaded** — ADR-0046's `RecordOrigin.Locked()`: an Admin's Fix info, Wrong
  item, Episode pin, or a parent's Cascade; or
- **asserted by the folder** — a `{tmdb-…}` / `{imdb-…}` token, the user's naming decision
  (ADR-0002).

When that provider is unreachable the item is ORPHANED and filed to the attention list, as
before. A record an enrichment pass resolved on its own (origin `''`) in a namespace that is
not the current lead's is **not** a pin: the next pass re-resolves it via the lead and replaces
the id and namespace. Repointing a Library now means what it says.

*This is a behaviour change for video.* Before, `pinnedProviderFor` ignored origin and pinned
every video Title with any id to TMDB, so repointing a video Library moved nothing already
matched while TMDB stayed reachable. It was harmless only because video had one source; music
never noticed because its pin was a no-op. The call site always described itself as "per-item
Enrichment-override precedence (ADR-0027)" — overrides, not every id.

**7. `MediaRef.ExternalIDs` is the contract's id carrier.** `ExternalIDs map[string]string`,
keyed by namespace, sits beside the five named fields (the frozen-additively rule allows it).
The host fills it with every id it holds for the entity and its parents — record rows and
identity ids — and fills the five named fields from the same map for v1 guests, so `TheTVDBID`
and `AniDBID` finally arrive. The SDK's `ref.ID(ns)` reads the map and falls back to the named
field; the shipped plugins read ids through it, and the named fields are documented as v1
mirrors a new plugin should not read. `MetadataRecord` is unchanged: `ExternalID` plus
`Source` already is a namespaced id once `Source` means the namespace.

**8. Delivery.** Four child issues under `.scratch/bundled-plugins/issues/` (13 contract + SDK,
14 migration + store, 15 enrich, 16 API + web).

## Considered and rejected

- **A generic `enrichment_external_id` + namespace pair beside the named columns.** Smallest
  migration; every reader branches on "named column or generic pair" forever, and a
  third-party Title can hold one id.
- **Reusing `enrichment_source` as the pin's namespace.** A Fix-info pick changes the id before
  any pass runs, so the source would describe the old record, and every pass rewrites it.
- **Taking the column names at face value in the migration.** Simpler, and locks in the
  AniDB-in-the-TMDB-column bug permanently.
- **Pinning every record with an id to its namespace.** Consistent with the old video
  behaviour, and makes repointing a Library inert for anything already enriched — for music, a
  regression the moment music gained a real namespace.
- **`Source` on `SearchCandidate`.** See decision 5.

## Consequences

- ADR-0045's column shape is superseded; its precedence and its identity/record split stand.
- ADR-0046's origin is now also the pin's gate (decision 6); its meaning is unchanged.
- ADR-0059 decision 11's named-id gap is closed. `videoIDColumnProvider` and
  `ExternalMatchForKind` are deleted: nothing in the host picks a namespace by media kind.
