# SQLite for durable state, filesystem for bulk/transient files

All durable structured state — the catalog (Titles/Editions/Files), Users, Devices, watch state, settings — lives in a single embedded SQLite database (one file in the mounted data directory), using WAL mode and FTS5 for browse search.

Bulk and transient files — fetched artwork, generated thumbnails, and live HLS transcode segments — live on the filesystem under the data directory, never in the database. Transcode segments are scratch and are cleaned up when their Playback session ends.

## Why
A self-hosted monolith should have the fewest moving parts; SQLite needs no separate server process and handles household-scale catalogs easily. Requiring operators to run Postgres would betray the low-friction goal of [ADR-0006](./0006-docker-first-modular-monolith.md).

## Consequences
- SQLite is single-writer; acceptable for this write volume with WAL, but a deliberate ceiling on write concurrency. Postgres remains a future option if scale demands it.
- The data directory holds both the DB and the caches and is the single backup target.
