-- A Targeted scan (a scan restricted to one browsable entity's folders,
-- ADR-0030/0031) reuses the per-Library scan_status row — the shared lock means
-- a full and a targeted scan can't both run — but tags it with the entity being
-- scanned so the admin surface can honestly show "Scanning The Wire…" versus a
-- whole-Library "Scanning…". scope is the entity's display label; '' for a full
-- Library scan. Set at scan start (running); left as-is on finish/error so the
-- row still names the last run, and read only while state = 'running'.
ALTER TABLE scan_status ADD COLUMN scope TEXT NOT NULL DEFAULT '';
