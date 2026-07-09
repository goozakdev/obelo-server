import { defineConfig, devices } from "@playwright/test";

// Playwright E2E config — the primary browser seam (PRD Testing Decisions).
//
// The webServer below is the harness: it runs `e2e/boot-server.mjs`, which
// (1) builds the frontend into internal/webui/dist, (2) `go build`s the real
// juicebox binary that embeds that bundle, and (3) boots it against a fresh
// temp data dir on PORT. Playwright waits for the URL to answer, then runs the
// browser specs against the REAL embedded server — UI + API client + Go server
// as one black box, exactly like the server's own httptest harness but driven
// through a browser.
//
// Port: env-overridable via E2E_PORT, default 8099 (away from the dev :8080).

const PORT = Number(process.env.E2E_PORT ?? 8099);
const BASE_URL = `http://127.0.0.1:${PORT}`;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? "github" : "list",
  use: {
    baseURL: BASE_URL,
    trace: "on-first-retry",
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
  ],
  // Build + boot the real embedded server before tests; tear it down after.
  webServer: {
    command: `node e2e/boot-server.mjs`,
    url: `${BASE_URL}/api/v1/server`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    env: {
      E2E_PORT: String(PORT),
    },
    stdout: "pipe",
    stderr: "pipe",
  },
});
