-- 0054_musicbrainz_tag_ids: carry the MusicBrainz ids a tagger already wrote into
-- the files through to Enrichment, so a tagged library resolves by LOOKUP instead
-- of by search (ADR-0049). Purely additive: three ADD COLUMNs with constant
-- defaults, no rebuild, no existing row touched.
--
-- Obelo already read one of these ids (musicbrainz_releasegroupid) and used it for
-- ALBUM IDENTITY (ADR-0038) — then dropped it. Enrichment, which is the one
-- consumer that could turn an exact id into an exact record, never saw it and
-- issued a fuzzy `/ws/2/recording?query=...` per track instead.
--
-- These columns are DECORATION anchors, not identity. identity_key is untouched
-- (ADR-0002: tags are the music identity authority, and the release-group id
-- already does its identity job inside the album key). Nothing here changes what
-- a Title IS; it changes which provider record decorates it, and how cheaply that
-- record is found.
--
-- They are also distinct from the ENRICHMENT RECORD columns, exactly as
-- titles.tmdb_id is distinct from titles.enrichment_tmdb_id (ADR-0045). These
-- hold what the FILE asserts, are re-derived from disk on every scan, and lose to
-- an Admin's Fix-info override every time.

-- The recording MBID of a Track, from the file's tags. Deliberately NOT reusing
-- titles.musicbrainz_id: that column is the enrichment RECORD (written by a pass's
-- own result or by an Admin's correction), and a scanner that wrote to it would
-- overwrite the Admin's choice on the next scan — the precise bug ADR-0045 exists
-- to prevent, which cost this codebase an ADR the first time round.
ALTER TABLE titles ADD COLUMN musicbrainz_recording_id TEXT NOT NULL DEFAULT '';

-- The artist MBID (from the album-artist tag, falling back to the track artist)
-- and the release-group MBID of an Album. Both are scanner-owned and re-derived
-- on every scan.
ALTER TABLE artists ADD COLUMN musicbrainz_id TEXT NOT NULL DEFAULT '';
ALTER TABLE albums  ADD COLUMN musicbrainz_id TEXT NOT NULL DEFAULT '';
