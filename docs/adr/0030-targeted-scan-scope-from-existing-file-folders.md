# Targeted scan: scope derived from an entity's existing File folders

A **Targeted scan** ([CONTEXT](../../CONTEXT.md)) — "Scan" on a Movie/Show/Album/Artist detail page — re-walks only the on-disk folders that entity's Files currently occupy, rather than the whole Library. The scope is computed *from the existing catalog*: the distinct parent folders of the entity's Files (via the same path→anchor rules the Scanner keys Match overrides on — [ADR-0002](./0002-naming-convention-is-identity-authority.md)). Inside that scope it is a normal incremental scan — same identity derivation, same Match-override preservation, same soft-delete of Missing Files ([ADR-0008](./0008-incremental-scan-soft-delete-missing-files.md)) — just narrower.

## Why this shape

The point is the everyday "I added Episode 9 / replaced this cut / dropped a new album in this artist's folder" flow, without paying for a full-Library walk. Deriving scope from the entity's *own* Files is the cheapest way to answer "which folders are this thing's?" deterministically and offline, and it keeps the blast radius legible: a scan launched from an entity touches that entity's folders and nothing else (a shared folder is still walked whole, so it can legitimately file a sibling entity's media that lives in the same directory).

## Considered and rejected

- **Walk the entity's parent directory (or the whole Library) to catch renames/new folders.** Rejected for v1: it blurs the scope and blast radius, and re-introduces the full-scan cost the feature exists to avoid.

## Consequences

- **Renames, moves, and brand-new top-level folders are invisible to a Targeted scan** — the stored `File.Path` points at the old location, which reads back `ENOENT`, so a rename looks like "nothing changed." These remain a full-Library-scan job. The feature's promise is deliberately narrow: *changes **within** an entity's existing folders*, nothing about the folders themselves.
- **Resurrecting a fully-hidden (all-Missing) Title is out of scope for v1** — its detail page isn't browsable, so there is nowhere to click Scan; the come-back path is a full scan.
- Because scope names each folder, the Targeted scan can be **more precise than the Library scan's prune guard**: it soft-deletes per reached folder and skips only the unreachable ones, instead of the Library scan's coarse all-or-nothing "any root down → skip all pruning." See [ADR-0031](./0031-targeted-scan-per-folder-soft-delete-and-shared-lock.md).
