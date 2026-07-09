# Local on-disk information is the authority for media identity

The scanner derives a Title's identity (and groups Files into Editions) purely from **local on-disk information**, which is deterministic and works fully offline. The local source differs by media kind:

- **Video (Movie/TV):** the file/folder **naming convention** (the path).
- **Music:** the files' **embedded tags** (ID3v2 / Vorbis comments / MP4 atoms — Artist, Album, Album Artist, Disc/Track #, Title), with the path as a fallback only when tags are absent.

Both are local, baked into the files, and require no network — they are simply different local sources. (Originally this ADR named only the naming convention; amended when adding Music, whose identity comes from tags.)

External metadata lookup is a separate, optional enrichment step keyed by the parsed title/year. Matching never depends on an external call succeeding — a server with no internet still files everything correctly, just with sparser metadata.

## Considered and rejected
Plex-style fingerprint/agent-driven matching (guess from filename, confirm against an external canonical ID). Rejected because it makes correct filing depend on an external service, violating [ADR-0001](./0001-fully-self-hosted-no-vendor-dependency.md)'s offline requirement.

## Consequences
- We must publish a clear, strict naming convention; misnamed files will be misfiled or unmatched.
- We need a manual "fix match / merge / split" affordance in the web app for the cases the convention can't resolve.
