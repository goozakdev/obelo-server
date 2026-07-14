# Juice Box

A fully self-hosted media server (server + management web app + client-facing API). See `CONTEXT.md` for the domain glossary and `docs/adr/` for architectural decisions.

## Build artifacts

Never check in `internal/webui/dist/index.html` with real build output. It is a committed placeholder only. The local web build overwrites it with hashed asset references; before committing any code, restore the placeholder:

```
git checkout internal/webui/dist/index.html
```

## Agent skills

### Issue tracker

Issues and PRDs live as local markdown under `.scratch/<feature-slug>/` (no git remote; solo project). External PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical label vocabulary, unchanged: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix` — recorded as a `Status:` line in each issue file. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
