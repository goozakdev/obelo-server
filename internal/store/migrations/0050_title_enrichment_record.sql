-- 0050_title_enrichment_record: give a Title's ENRICHMENT RECORD its own columns,
-- separate from the id its NAME asserts (ADR-0045).
--
-- titles.tmdb_id / imdb_id have carried two claims with different owners since
-- 0010_enrichment reused the identity columns it found rather than adding its own:
--
--   * the id LOCAL NAMING asserts — an embedded {tmdb-438631} token or a
--     folder-anchored Match override. An identity claim (ADR-0002), owned by the
--     Scanner, re-derivable from disk on every pass, and already spelled a second
--     time in identity_key ("tmdb:438631").
--   * the record an ADMIN chose — Fix info on a Movie or a Track, an Episode
--     pin's series, a Cascade — plus the record Enrichment resolved on its own.
--     A decoration claim, deliberately NOT an identity one (ADR-0019), and
--     derivable from nothing.
--
-- Sharing one column made the Scanner and the Admin overwrite each other.
-- .../issues/01 fixed the loud half by ordering the two writers ("a scan may fill
-- an id it has, never blank one it has nothing to say about"); this migration
-- removes the shared column, which is the only actual cure. Every other entity
-- type has had the split since 0011/0020 — a Show/Artist/Album keeps its record in
-- entity_enrichment.external_id with its own external_id_locked flag, in a table
-- the Scanner never writes — which is exactly why Shows were immune to issue 01's
-- bug while Titles were not.
--
-- After this, the Scanner owns tmdb_id/imdb_id outright (it writes them
-- unconditionally again) and never touches the enrichment columns of an existing
-- row; every read that asks "which record decorates this Title" resolves it as
-- COALESCE(NULLIF(enrichment_tmdb_id,''), tmdb_id) — the override when there is
-- one, the folder's id otherwise.
--
-- There is deliberately NO enrichment_musicbrainz_id. Music identity is embedded
-- tags, not an id (ADR-0002), so nothing derives a MusicBrainz id from local
-- naming: titles.musicbrainz_id has only ever had enrichment writers and IS the
-- enrichment column already. enrichment_id_locked is namespace-neutral and covers
-- it.
ALTER TABLE titles ADD COLUMN enrichment_tmdb_id TEXT NOT NULL DEFAULT '';
ALTER TABLE titles ADD COLUMN enrichment_imdb_id TEXT NOT NULL DEFAULT '';

-- enrichment_id_locked marks the enrichment record as an Admin's explicit choice
-- rather than a per-pass auto-resolved id — the per-Title twin of
-- entity_enrichment.external_id_locked (0020). Written only by the Admin paths
-- (SetTitleExternalMatch, applyPinsTx); a normal enrichment pass fills the id
-- columns and leaves this at 0. It is the signal `childHasOwnOverride` should read
-- instead of guessing from "the column is non-empty" (.../issues/03).
ALTER TABLE titles ADD COLUMN enrichment_id_locked INTEGER NOT NULL DEFAULT 0;

-- Split the existing rows by their OWN identity_key, which is the one fact that
-- says whether local naming ever asserted the id: identity_key IS "tmdb:<id>"
-- exactly when the id came from a folder name or a Match override
-- (scanner.identityKey). Anything else in the column got there from Fix info, an
-- Episode pin, a Cascade or an enrichment pass — all decoration — so it moves.
UPDATE titles
   SET enrichment_tmdb_id = tmdb_id,
       enrichment_id_locked = 1
 WHERE IFNULL(tmdb_id, '') <> ''
   AND identity_key <> 'tmdb:' || tmdb_id;
UPDATE titles
   SET tmdb_id = ''
 WHERE IFNULL(tmdb_id, '') <> ''
   AND identity_key <> 'tmdb:' || tmdb_id;

UPDATE titles
   SET enrichment_imdb_id = imdb_id,
       enrichment_id_locked = 1
 WHERE IFNULL(imdb_id, '') <> ''
   AND identity_key <> 'imdb:' || lower(imdb_id);
UPDATE titles
   SET imdb_id = ''
 WHERE IFNULL(imdb_id, '') <> ''
   AND identity_key <> 'imdb:' || lower(imdb_id);

-- A Track's record never moved (musicbrainz_id was always the enrichment column),
-- but it needs the same marker so the lock means one thing across both namespaces.
UPDATE titles
   SET enrichment_id_locked = 1
 WHERE IFNULL(musicbrainz_id, '') <> '';

-- WHY THE BACKFILL LOCKS EVERYTHING IT MOVES, including ids no Admin ever picked.
-- Before this migration nothing distinguished an Admin's choice from an id an
-- enrichment pass resolved and persisted, and the code that reads it —
-- enrich.childHasOwnOverride — treats EVERY non-empty value as the Admin's. Marking
-- them all locked reproduces that reading exactly, so no existing correction is
-- silently demoted into something a later Cascade may overwrite. The improvement
-- applies going forward: from here an auto-resolved id leaves the flag at 0.
