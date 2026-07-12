# Targeted scan: per-folder soft-delete, and the shared per-library scan lock

A **Targeted scan** ([CONTEXT](../../CONTEXT.md), [ADR-0030](./0030-targeted-scan-scope-from-existing-file-folders.md)) soft-deletes Missing Files **per reached folder**: a File absent from a folder the scan successfully walked is marked Missing, while a folder that reads back unreachable (`ENOENT`) is skipped untouched and the run is reported as partial. It does **not** adopt the Library scan's coarse prune guard.

## Why this diverges from the Library scan

[ADR-0008](./0008-incremental-scan-soft-delete-missing-files.md)'s prune guard is all-or-nothing — *if **any** root is unreachable, skip the entire prune pass* — because a whole-Library walk can't cheaply attribute an absence to a specific folder, so it can't tell "file deleted" from "the share holding it is down." A Targeted scan doesn't have that problem: its scope names each folder up front, so it *can* attribute absence. A File gone from a folder that was reachable this run is genuinely gone, regardless of a sibling folder being offline. Being narrow is exactly what makes the precise rule safe. Handling the "replace a file" case correctly (drop the old cut, add the new one → old Edition goes Missing immediately) is the payoff.

## Concurrency and status

- **One scan per Library at a time, full or Targeted, sharing the existing per-Library lock.** No concurrent writers to the same catalog rows. A Targeted scan requested while a scan is already running is rejected with "scan already running" (idempotent, mirroring the existing in-flight behavior); the in-flight scan will pick up the change anyway. Chosen over finer-grained per-folder locking: unnecessary for a household/solo server, and it avoids all row-race questions.
- **The per-Library status row is the single source of truth**, reused and **scope-tagged** with the entity being scanned, so the admin surface honestly shows "Scanning *The Wire*…" vs. a whole-Library "Scanning…" — and the row's `running` state is what correctly explains a rejected full scan. A separate ephemeral status channel was rejected because it would desync "is a scan running" from the lock that actually gates it.

## Consequences

- The catalog's soft-delete pass has a folder-scoped variant (`MarkFilesMissingUnder`) that trusts per-folder reachability, distinct from the Library scan's root-level guard.
- Partial reachability is handled non-destructively: an unreachable scope folder is spared (its Files stay present, never marked Missing) and the scan still commits the adds/changes from the folders it did reach — so a partial run is a success, not an error. Only when **every** scope folder is unreachable does the scan error (`ErrRootsUnavailable`), mirroring the full scan's guard rather than committing a walk that saw nothing. In v1 the set of skipped folders is not surfaced beyond the non-destructive spare + the "what changed" delta; a louder "N folders were offline" signal is a possible follow-up.
