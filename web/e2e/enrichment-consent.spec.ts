import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end for the first-run Enrichment consent gate (ADR-0032) against the
// REAL embedded Go server. The shared e2e server is seeded with consent GRANTED
// (boot-server.mjs), so the first-run modal doesn't overlay the other specs; here
// we drive the Admin settings TOGGLE through the browser: revoke consent (which
// tells the server to stop all outbound metadata calls) and grant it back,
// asserting the round-trip against the live API. The spec restores GRANTED at the
// end so downstream specs keep enriching.

const here = dirname(fileURLToPath(import.meta.url));
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

function readClaimToken(): string | null {
  for (let i = 0; i < 50; i++) {
    try {
      const tok = readFileSync(CLAIM_TOKEN_FILE, "utf8").trim();
      if (tok) return tok;
    } catch {
      /* not written yet */
    }
  }
  return null;
}

async function ensureAdmin(request: APIRequestContext): Promise<void> {
  const info = await (await request.get("/api/v1/server")).json();
  if (!info.setupRequired) return;
  const claimToken = readClaimToken();
  if (!claimToken) throw new Error("setup required but no claim token captured");
  const res = await request.post("/api/v1/setup", {
    data: { claimToken, username: ADMIN_USER, password: ADMIN_PASS },
  });
  if (!res.ok() && res.status() !== 409) {
    throw new Error(`setup failed: ${res.status()} ${await res.text()}`);
  }
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("enrichment consent: settings toggle round-trip", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    await request.dispose();
  });

  test("admin can revoke and re-grant enrichment consent", async ({ page }) => {
    await uiLogin(page);
    await page.goto("/admin/providers");

    const control = page.getByTestId("enrichment-consent-control");
    const toggle = page.getByTestId("enrichment-consent-toggle");
    const stateHint = page.getByTestId("enrichment-consent-state");

    // The server booted with consent granted (boot-server.mjs), so the control
    // loads checked. (The toggle is a controlled + save-on-change input, so we
    // drive it with click() and assert on the state hint the server round-trip
    // updates — not uncheck/check, which race the async state flip.)
    await expect(control).toBeVisible();
    await expect(stateHint).toHaveAttribute("data-state", "granted");
    await expect(toggle).toBeChecked();

    // Revoke: clicking saves immediately and the server reports declined.
    await toggle.click();
    await expect(stateHint).toHaveAttribute("data-state", "declined");
    await expect(toggle).not.toBeChecked();

    // The decision persists across a reload (DB-authoritative, not a UI-only flag).
    await page.reload();
    await expect(page.getByTestId("enrichment-consent-state")).toHaveAttribute(
      "data-state",
      "declined",
    );
    await expect(page.getByTestId("enrichment-consent-toggle")).not.toBeChecked();

    // Grant it back so downstream specs keep enriching, and confirm the round-trip.
    await page.getByTestId("enrichment-consent-toggle").click();
    await expect(page.getByTestId("enrichment-consent-state")).toHaveAttribute(
      "data-state",
      "granted",
    );
    await expect(page.getByTestId("enrichment-consent-toggle")).toBeChecked();
  });
});
