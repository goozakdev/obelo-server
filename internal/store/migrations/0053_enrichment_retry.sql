-- 0053_enrichment_retry: a transient Enrichment failure is RETRIED, not parked
-- (ADR-0048). Purely additive — two ADD COLUMNs per enrichment-bearing table,
-- both with constant defaults, so every existing row backfills without a rebuild
-- and no other column, index or constraint moves.
--
-- The status vocabulary is UNCHANGED: 'failed' still means "the provider
-- errored". What is new is the answer to the second question the old schema
-- could not express — "does the server intend to try again?" — carried by
-- enrichment_retry_at rather than by a sixth status value. Keeping it out of the
-- status keeps the CHECK constraint (and so the titles table) untouched, and it
-- models the two facts honestly: the status says what happened, the retry column
-- says what happens next.
--
--   retry_at = ''          → parked. Nobody is coming back for it; it sits on the
--                            Admin's attention list (TitlesNeedingMatch). This is
--                            every non-transient failure and every failure the
--                            pre-0053 server recorded.
--   retry_at = <timestamp> → scheduled. The next only-new pass at or after this
--                            instant picks the row up again, exactly as if it
--                            were 'pending'.
--
-- enrichment_attempts counts CONSECUTIVE failed lookups and is what the backoff
-- schedule reads (enrich/retry.go). It is reset to 0 by any settled outcome —
-- matched, unmatched, disabled — so a row that fails, recovers, and fails again
-- much later starts its backoff over rather than inheriting an old streak.

ALTER TABLE titles ADD COLUMN enrichment_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE titles ADD COLUMN enrichment_retry_at TEXT    NOT NULL DEFAULT '';

ALTER TABLE entity_enrichment ADD COLUMN enrichment_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entity_enrichment ADD COLUMN enrichment_retry_at TEXT    NOT NULL DEFAULT '';

-- Heal the rows the old behavior stranded. Every 'failed' row in an existing
-- database was parked by a pass that could not tell a provider outage from a
-- definitive error, so the population is a mix and the server cannot now say
-- which was which. Scheduling them all for one immediate retry re-asks the
-- provider the question exactly once: a genuinely transient failure resolves on
-- the next pass, and a genuinely permanent one fails again and parks itself
-- properly (or escalates) under the new rules. The alternative — leaving them
-- parked — makes the fix apply only to failures that happen after the upgrade
-- and leaves the existing attention list full of items nobody will retry.
--
-- The sentinel is a timestamp in the past, so the row is due the moment the next
-- pass runs; attempts stays 0 so the retry that follows is the streak's first and
-- gets the shortest backoff.
UPDATE titles
   SET enrichment_retry_at = '1970-01-01T00:00:00Z'
 WHERE enrichment_status = 'failed';

UPDATE entity_enrichment
   SET enrichment_retry_at = '1970-01-01T00:00:00Z'
 WHERE enrichment_status = 'failed';
