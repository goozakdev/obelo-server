# Incremental scanning with soft-delete of missing files

Scans are incremental by default: only files new, changed (path/mtime/size), or missing since the last scan are processed. A full re-scan is an explicit, rare operator action.

Three layered triggers: manual ("Scan now"), a scheduled periodic scan as the always-on safety net, and best-effort filesystem watching (inotify) for instant pickup — never relied upon alone, since it is unreliable on network mounts.

Removals are soft: a File absent from disk is marked **missing**, not deleted. A Title whose Files are all missing is hidden, not removed. Watch state survives disappearance/reappearance, keyed to the convention-derived Title identity ([ADR-0002](./0002-naming-convention-is-identity-authority.md)).

**Prune guard — an unreachable root is not evidence of deletion.** The soft-delete pass infers "missing" from a File's absence in the current walk. But when a root itself is unreachable (an unmounted share stats as `ENOENT`), the walk comes back empty — or short — for a reason that has nothing to do with the media being gone. So the scanner distinguishes *"root present but empty"* from *"root offline"*: if **any** configured root is unreachable, it skips the entire prune pass (mark-missing, hide, and orphan-surfacing of match overrides) and records the scan as errored (`ErrRootsUnavailable`) rather than a silent "0 items found". Upserts for roots that *were* reachable still commit, so a healthy root keeps updating while a sibling share is down; only the destructive step is withheld until every root is reachable again. Without this guard, a transient outage — including an automatic scheduled scan firing while the share is down — empties the browse view until the next manual rescan.

## Why
A lot of self-hosted media sits on network mounts that unmount transiently. Hard-deleting on absence would let an unmounted NAS wipe the catalog and watch history. Soft-delete makes absence recoverable — and the prune guard keeps a transient unmount from marking everything missing in the first place, so the library never even flickers empty.

## Consequences
- The catalog must distinguish present / missing states throughout, and the UI must surface "missing" without alarming the user about transient unmounts.
- A future "purge missing" maintenance action is needed to actually reclaim entries the operator confirms are gone for good.
- The prune guard is conservative: it cannot tell a *temporarily* unmounted root from one *permanently* removed via a single stat, so a multi-root library defers all pruning until every root is reachable. An operator who genuinely removes a root must drop it from the library config for missing files under it to be reclaimed; leaving a dead root path in the config permanently suppresses pruning (and surfaces as a scan error).
