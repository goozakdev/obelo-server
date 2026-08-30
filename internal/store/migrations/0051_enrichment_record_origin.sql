-- 0051_enrichment_record_origin: record WHOSE choice an enrichment record is, not
-- merely THAT it is one (ADR-0046).
--
-- 0050 gave a Title's enrichment record a flag saying it was the Admin's own
-- choice rather than an id a pass resolved — enough to stop a Cascade skipping
-- children that never chose anything (.../issues/03). It is not enough for the
-- next question, because "the Admin's own choice" has two owners:
--
--   * CHOSEN — the Admin opened THIS item and re-pointed it: Fix info or Wrong
--     item on a leaf, an Episode pin on a Slot, Fix info on an Album.
--   * CASCADED — the Admin corrected a PARENT and ticked "apply to children". The
--     child was never looked at; it was mapped under the parent's record. The
--     choice is the parent's, held by the child.
--
-- A Cascade writes its children through the same Admin paths (reenrichEpisode →
-- SetTitleExternalMatch; the Artist recursion → SetEntityExternalMatch), so both
-- locks came back set, and the skip rules read them as the child's own. A SECOND
-- Cascade from the same parent therefore skipped every child the first one
-- reached — silently, reporting Updated: 0 on exactly the children that matter
-- (.../issues/04). The identical defect existed one level up, where an Artist
-- Cascade's own Album pins excluded those Albums (and their Tracks, since the
-- recursion never enters a skipped Album) from the next Artist Cascade.
--
-- So the boolean becomes a three-valued origin. '' is nobody's choice (an
-- enrichment pass resolved it, a split sibling inherited it, a cleared pin wrote
-- the Show's series back); 'chosen' is the item's own; 'cascaded' is its parent's.
-- "Durable" — the property ADR-0019 and ADR-0045 actually wanted, and the one that
-- keeps a pass from re-auto-matching the child — is origin <> '' and is unchanged
-- for both. Only a Cascade from the same parent may overwrite a 'cascaded' record;
-- a 'chosen' one still beats every Cascade.
--
-- The old columns are DROPPED rather than kept in step. A boolean that is a strict
-- function of the origin is a method (store.RecordOrigin.Locked()), not a column:
-- two columns holding one fact is the mirror of the two-facts-one-column trap 0050
-- was written to undo, and it is the shape that lets a future reader consult half
-- the truth — which is how this bug got in.

ALTER TABLE titles ADD COLUMN enrichment_id_origin TEXT NOT NULL DEFAULT '';
ALTER TABLE entity_enrichment ADD COLUMN external_id_origin TEXT NOT NULL DEFAULT '';

-- WHY EVERY BACKFILLED ROW READS 'chosen', INCLUDING ONES A CASCADE WROTE.
--
-- The history is not in the database and cannot be reconstructed: until this
-- migration the two provenances WERE the same bit, and a cascaded record is
-- byte-identical to a directly-chosen one in every other column.
--
-- The two possible readings are not symmetric. Calling an old row 'cascaded' would
-- let the next Cascade silently overwrite a Fix-info correction an Admin really
-- made — destructive, invisible, unrecoverable. Calling it 'chosen' reproduces
-- exactly what that install did yesterday: the row keeps being skipped. That is
-- the bug, preserved — but it is visible (the Cascade summary counts the skip) and
-- repairable (a Fix info, a Wrong item or a cleared pin on the child re-states the
-- record and settles its origin honestly). Preserving the old reading is the only
-- backfill that cannot discard someone's correction, which is the same call 0050
-- made for the same reason.
--
-- The fix therefore applies to every record written after the upgrade; children a
-- Cascade wrote BEFORE it go on being skipped by their parent's later Cascades.
UPDATE titles SET enrichment_id_origin = 'chosen' WHERE enrichment_id_locked <> 0;
UPDATE entity_enrichment SET external_id_origin = 'chosen' WHERE external_id_locked <> 0;

ALTER TABLE titles DROP COLUMN enrichment_id_locked;
ALTER TABLE entity_enrichment DROP COLUMN external_id_locked;
