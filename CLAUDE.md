# Obelo

A fully self-hosted media server (server + management web app + client-facing API). See `CONTEXT.md` for the domain glossary and `docs/adr/` for architectural decisions.

## Build artifacts

Never check in `internal/webui/dist/index.html` with real build output. It is a committed placeholder only. The local web build overwrites it with hashed asset references; before committing any code, restore the placeholder:

```
git checkout internal/webui/dist/index.html
git diff --quiet internal/webui/dist/index.html || echo "STILL DIRTY — do not commit"
```

**Nothing enforces this at commit time, and it silently rotted once.** The two guards want
*opposite* things — `make check-bundle` fails unless the bundle is REAL (a release must not ship
the placeholder), while this rule wants the committed file to be the PLACEHOLDER — and only the
release side is automated. Between 2026-07 and 2026-08-08 the committed `index.html` was real
Vite output referencing `/assets/index-ijKWYbjt.js`, an asset that had long since stopped
existing and was never committed (`.gitignore` ignores all of `dist/*`; `index.html` is tracked
only because it was force-added). So a fresh clone built with plain `go build` served a blank
page requesting two 404s, and `check-bundle` reported success the whole time, because it only
looks for the `obelo-spa-placeholder` marker and real output never contains it.

Verify both directions after touching this file: with the placeholder committed
`go run ./internal/webui/cmd/checkbundle` must exit **1**, and after `make web` it must exit
**0**. If the committed file ever references a hashed asset, it is wrong by construction.

## Agent skills

### Issue tracker

Issues and PRDs live as local markdown under `.scratch/<feature-slug>/` (no git remote; solo project). External PRs are not a triage surface. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical label vocabulary, unchanged: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix` — recorded as a `Status:` line in each issue file. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
