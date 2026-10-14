-- A Show or Movie whose folder name has no year takes it from its matched
-- metadata. year_from_metadata says the stored year came from there rather than
-- from the folder name or an Admin, so a rescan of the still-yearless folder keeps
-- it (and keeps needs_review clear) while a year the folder does carry, or an
-- Admin's identity fix, replaces it and resets the flag.
ALTER TABLE shows  ADD COLUMN year_from_metadata INTEGER NOT NULL DEFAULT 0;
ALTER TABLE titles ADD COLUMN year_from_metadata INTEGER NOT NULL DEFAULT 0;

-- Back-fill Movies from the release date enrichment already stored.
UPDATE titles SET
    year = CAST(substr(release_date, 1, 4) AS INTEGER),
    year_from_metadata = 1,
    needs_review = 0
  WHERE kind = 'movie' AND year IS NULL
    AND release_date GLOB '[0-9][0-9][0-9][0-9]*'
    AND CAST(substr(release_date, 1, 4) AS INTEGER) > 0;

-- A Show has no stored air date, so queue every yearless matched Show for one
-- re-enrichment: the next enrichment pass (a scheduled/Library scan, or Refresh
-- metadata) re-resolves it and fills the year from the provider.
UPDATE entity_enrichment SET enrichment_status = 'pending',
    enrichment_attempts = 0, enrichment_retry_at = ''
  WHERE entity_type = 'show' AND enrichment_status = 'matched'
    AND entity_id IN (SELECT id FROM shows WHERE year IS NULL);
