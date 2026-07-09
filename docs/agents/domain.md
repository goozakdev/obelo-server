# Domain Docs

How the engineering skills should consume this repo's domain documentation when exploring the codebase. This repo is **single-context**.

## Before exploring, read these

- **`CONTEXT.md`** at the repo root — the domain glossary.
- **`docs/adr/`** — read ADRs that touch the area you're about to work in.

If any of these files don't exist, **proceed silently**. Don't flag their absence; don't suggest creating them upfront. The `/domain-modeling` skill (reached via `/grill-with-docs` and `/improve-codebase-architecture`) creates them lazily when terms or decisions actually get resolved.

## File structure

Single-context layout:

```
/
├── CONTEXT.md
├── docs/adr/
│   ├── 0001-fully-self-hosted-no-vendor-dependency.md
│   └── 0002-naming-convention-is-identity-authority.md
└── ...
```

(There is no `CONTEXT-MAP.md`; this is not a multi-context/monorepo setup.)

## Use the glossary's vocabulary

When your output names a domain concept (an issue title, a refactor proposal, a hypothesis, a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary explicitly lists under `_Avoid_` (e.g. use **Title**, not "Item"; **Stream**, not "Track" for in-container elements; **Missing**, not "Deleted").

If the concept you need isn't in the glossary yet, that's a signal — either you're inventing language the project doesn't use (reconsider) or there's a real gap (note it for `/domain-modeling`).

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly rather than silently overriding:

> _Contradicts ADR-0014 (watch state keyed to parsed Title identity) — but worth reopening because…_
