# Web app (React + TypeScript SPA)

The management & viewing SPA for the media server. Built with **Vite**, served
by the Go monolith on the same origin/port as the API — embedded into the
binary via `go:embed` (ADR-0012, ADR-0006). This is the walking skeleton
(issue 01): app shell + the single typed API client + the Playwright harness.
No auth/browse/player yet.

## Layout

```
web/
  index.html              Vite entry
  vite.config.ts          build → ../internal/webui/dist; dev proxy /api → :8080
  src/
    main.tsx              React mount
    App.tsx               responsive app shell, renders handshake state
    useServerInfo.ts      hook running the handshake (loading/ready/unreachable/error)
    index.css             responsive styles (phone/tablet/desktop)
    api/
      client.ts           the ONE I/O seam: typed fetch wrapper over /api/v1
      types.ts            wire types (camelCase, matches the server)
      errors.ts           ApiError / NetworkError + error-envelope parsing
      token.ts            bearer-token storage (localStorage; login wires it in issue 02)
  e2e/
    boot-server.mjs       Playwright webServer: builds frontend + binary, boots it
    smoke.spec.ts         browser smoke test against the real embedded server
  playwright.config.ts
```

## The single API client (the one seam)

All HTTP to `/api/v1` goes through `src/api/client.ts`. Components/hooks call
its typed methods and never call `fetch` directly. It:

- prefixes every path with `/api/v1` (relative → same-origin in production),
- attaches `Authorization: Bearer <token>` when the token store holds one,
- maps the standard error envelope to a typed `ApiError` (`.code`, `.status`,
  `.details`, `.isUnauthorized`), and an unreachable server to `NetworkError`.

`getServerInfo()` (the handshake) is the only endpoint in this slice. Later
slices add methods here.

## Build order (important)

The Go binary embeds `internal/webui/dist` via `go:embed`. **The frontend must
build before `go build`.** From the repo root:

```
make build      # runs `make web` (npm install + vite build → internal/webui/dist),
                # then `go build` the binary that embeds it
```

A committed placeholder `internal/webui/dist/index.html` keeps plain
`go build ./...` / `go test ./...` working without Node. `make check-bundle`
(→ `webui.IsPlaceholder()`) fails loudly if a release binary still has the
placeholder instead of a real bundle. A genuinely missing `dist/` is a Go
**compile error** — the intended loud failure.

## Local development

```
# terminal 1: the real API
go run ./cmd/juicebox          # serves :8080

# terminal 2: the SPA with HMR
cd web && npm install && npm run dev   # serves :5173, proxies /api/v1 → :8080
```

The Vite dev proxy (`vite.config.ts`) forwards `/api` to `http://localhost:8080`
so dev hits the real API with no CORS. The proxy is **dev-only**; production is
same-origin (the monolith serves both), so no proxy/CORS is needed there.

## Tests

Two seams (PRD Testing Decisions):

```
npm test                # Vitest component tests (jsdom, offline, fast)
npm run test:e2e        # Playwright: builds frontend + binary, boots it, runs browser specs
```

**Component tests (`npm test`, Vitest + Testing Library).** Render components
against a faked API client — the PRD's one seam. The fake is a *real* `ApiClient`
wired to a fake `fetch` returning canned JSON (`src/test/fakeClient.ts`), so each
test exercises the client's request shaping AND the `omitempty`/RFC3339
normalize layer alongside the component. Config lives in `vite.config.ts`
(`test:` block, jsdom, `src/test/setup.ts`); only `src/**/*.test.{ts,tsx}` run
here — the Playwright `e2e/` specs are separate. Coverage: cursor pagination
(append without dupes, stop on null cursor), sort switching, poster placeholder
on artwork error, watch-state badge/resume rendering, RFC3339→local formatting,
the `omitempty` normalization, and error-envelope display.

**E2E (`npm run test:e2e`, Playwright).** `e2e/boot-server.mjs` builds the
frontend, `go build`s the real binary, and boots it against a fresh temp data
dir on `E2E_PORT` (default 8099) — the browser analog of the server's httptest
harness. `browse.spec.ts` SEEDS data via the API (the operator's job; no admin
UI yet): ensure the first Admin (claim token), log in, create a Movie library at
`internal/api/testdata/naming/` (multiple titles, one — "Extras Movie" — with
`poster.jpg`/`fanart.jpg`), scan to idle, then drive the browser through
login → libraries → grid → detail. Requires a Go toolchain on PATH; the one-time
`npm install` and `npx playwright install` need network.
