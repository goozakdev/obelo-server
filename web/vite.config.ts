/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

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
// (`go run ./cmd/juicebox`) before `npm run dev`. The proxy is dev-only;
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
  },
});
