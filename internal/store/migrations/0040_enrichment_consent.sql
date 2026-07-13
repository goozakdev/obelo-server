-- 0040_enrichment_consent: the first-run Enrichment CONSENT gate (ADR-0032).
-- Distributed default metadata credentials (issue distributed-metadata-keys/01)
-- turn external Enrichment on out of the box, so a fresh install must explicitly
-- consent before any provider is contacted. The decision is persisted on the same
-- singleton metadata_settings row shipped in 0018.
--
--   enrichment_consent_granted — NULL = UNDECIDED (show the first-run prompt; make
--                                NO outbound calls); 0 = declined; 1 = granted.
--                                NULL is DISTINCT from a real 0, exactly like the
--                                0019 behavior columns.
--   enrichment_consent_at      — when the decision was recorded (SQLite
--                                datetime('now') text), NULL while undecided.
--
-- Grandfather clause: an EXISTING install (the singleton row already exists at
-- migration time) is set to GRANTED. An operator already running the server has
-- effectively opted in — configuring a provider was itself the opt-in — and this
-- slice ships no default keys yet, so nothing enriches until a provider is keyed
-- regardless. A brand-new install has NO row here yet (SeedIfEmpty creates it on
-- first boot AFTER migrations), so the UPDATE matches nothing and the column stays
-- NULL/undecided — the prompt then fires. First boot may seed a decision from
-- JUICEBOX_ENRICHMENT_CONSENT (headless deploys / the test harness).
ALTER TABLE metadata_settings ADD COLUMN enrichment_consent_granted INTEGER;
ALTER TABLE metadata_settings ADD COLUMN enrichment_consent_at TEXT;

UPDATE metadata_settings
   SET enrichment_consent_granted = 1,
       enrichment_consent_at      = datetime('now')
 WHERE id = 1 AND enrichment_consent_granted IS NULL;
