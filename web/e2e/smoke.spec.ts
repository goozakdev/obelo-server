import { test, expect } from "@playwright/test";

// Stateless smoke tests against the REAL embedded Go server (booted by
// e2e/boot-server.mjs). These assert composition facts that hold no matter what
// auth state the server is in, so they are order-independent. The stateful
// first-run/auth flow lives in auth.spec.ts (a serial describe that owns the
// claim token and the one-time setup).

test("a deep client-side route deep-links and survives a reload", async ({ page }) => {
  // The Go server serves index.html for non-API paths (SPA fallback), so a
  // bookmarked client route loads the app rather than a 404 (PRD user story 37).
  // Unauthenticated, the guards route it to an auth screen (setup on a fresh
  // server, login once an Admin exists), but the SPA itself loads with a 200.
  const resp = await page.goto("/libraries/some-id/titles");
  expect(resp?.status()).toBe(200);
  await expect(
    page.locator("[data-testid='setup-screen'], [data-testid='login-screen']"),
  ).toBeVisible();

  await page.reload();
  await expect(
    page.locator("[data-testid='setup-screen'], [data-testid='login-screen']"),
  ).toBeVisible();
});

test("API namespace is not shadowed by the SPA", async ({ request }) => {
  // The handshake returns JSON, and an unknown /api/v1 path returns the error
  // envelope (NOT index.html) — the composition guard, asserted through HTTP.
  const ok = await request.get("/api/v1/server");
  expect(ok.ok()).toBeTruthy();
  expect((await ok.json()).version).toBeTruthy();

  const miss = await request.get("/api/v1/does-not-exist");
  expect(miss.status()).toBe(404);
  expect((await miss.json()).error.code).toBe("NOT_FOUND");
});
