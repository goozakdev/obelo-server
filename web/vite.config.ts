/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { FailureLogReporter } from "./vitest-failure-log";

// Vite build config for the SPA.
//
// outDir points at ../internal/webui/dist, the directory the Go package
// `internal/webui` embeds via go:embed and the monolith serves on the same
// origin/port as the API (ADR-0006, ADR-0012). Building straight into the
// embed directory is the build seam: `make web` runs this, then `go build`
// embeds the result. Because the SPA is served same-origin in production,
// runtime API calls are relative (`/api/v1/...`) — see src/api/client.ts.
//
// Dev proxy: `npm run dev` serves the SPA on :5173 with HMR and proxies
// `/api/v1` to a locally running Go server on :8080, so local development hits
// the real API without CORS. Start the Go server separately
// (`go run ./cmd/obelo`) before `npm run dev`. The proxy is dev-only;
// production has no proxy because everything is same-origin.
export default defineConfig({
  plugins: [react()],
  build: {
    // Emit straight into the Go embed directory so `go build` picks it up.
    outDir: "../internal/webui/dist",
    // Fail the build (and surface in CI) rather than silently emitting nothing.
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  // Component-test runner (PRD "Secondary — component tests faking the one API
  // client"). jsdom + Testing Library, offline, fast. The Playwright E2E suite
  // (./e2e) is excluded so vitest never tries to run browser specs.
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    css: false,
    // The default reporter for humans watching, plus FailureLogReporter, which
    // writes test-results/vitest-last-run.txt every run. That file is the whole
    // point: this suite failed twice with `1 failed | 774 passed` and the name of
    // the failing test was lost both times, because the output was captured with
    // a summary-only grep. Now the run names itself whether or not anyone thought
    // to capture stdout. See vitest-failure-log.ts and
    // .scratch/web-app/issues/08-an-unidentified-flaky-web-test-and-a-suite-nothing-runs.md.
    //
    // NOTE the deliberate absence of `retry`. Retrying would convert a flake into
    // a green run, and `make check` going green while a test is broken is the
    // exact failure mode CLAUDE.md's "Build artifacts" scar records. A flake has
    // to fail the gate; the reporter is what makes it cheap to diagnose when it does.
    reporters: ["default", new FailureLogReporter()],
  },
});
