import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end auth + first-run flow against the REAL embedded Go server (issue
// 02 acceptance criteria). This is a SERIAL describe because it owns the single
// booted server's one-time first-run state: the first test consumes the claim
// token to create the Admin (closing setup permanently for that process), then
// later tests log that Admin in and out. Token/media-cookie wiring on the server
// side is covered by the Go tests; here we focus on the browser auth flows.
//
// The filename's 00- prefix is load-bearing: specs run alphabetically (one
// worker), and every OTHER spec's beforeAll creates the first Admin via the API
// (ensureAdmin), which closes setup permanently. This spec must run FIRST or
// the setup screen never appears. On a REUSED dev server that is already set
// up (reuseExistingServer), the first test skips instead of failing.

const here = dirname(fileURLToPath(import.meta.url));
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

// The boot harness scrapes the claim token from the server logs and writes it to
// a file (mirroring an operator reading the logs). Poll briefly in case the file
// lands just after the server's /api/v1/server readiness check.
function readClaimToken(): string {
  for (let i = 0; i < 50; i++) {
    try {
      const tok = readFileSync(CLAIM_TOKEN_FILE, "utf8").trim();
      if (tok) return tok;
    } catch {
      // not written yet
    }
  }
  throw new Error(`claim token file not found at ${CLAIM_TOKEN_FILE}`);
}

test.describe.serial("first-run, login, logout, and the 401 path", () => {
  test("fresh server: setup creates the first Admin, lands logged in, and survives reload", async ({ page, request }) => {
    // A reused (already-set-up) dev server can't show the one-time setup flow —
    // skip rather than fail; a fresh boot always runs this.
    const info = await (await request.get("/api/v1/server")).json();
    test.skip(!info.setupRequired, "server already set up (reused dev server)");

    await page.goto("/");

    // The boot gate read setupRequired=true and routed to setup.
    await expect(page.getByTestId("setup-screen")).toBeVisible();

    // A wrong claim token surfaces the API's error message and does NOT proceed.
    await page.getByTestId("setup-claim-token").fill("definitely-wrong-token");
    await page.getByTestId("setup-username").fill(ADMIN_USER);
    await page.getByTestId("setup-password").fill(ADMIN_PASS);
    await page.getByTestId("setup-submit").click();
    await expect(page.getByTestId("setup-error")).toBeVisible();
    await expect(page.getByTestId("setup-screen")).toBeVisible();

    // The real claim token creates the Admin and auto-logs-in → Home.
    await page.getByTestId("setup-claim-token").fill(readClaimToken());
    await page.getByTestId("setup-submit").click();

    await expect(page.getByTestId("home-screen")).toBeVisible();
    await expect(page.getByTestId("current-user")).toHaveText(ADMIN_USER);
    // Admin role → the admin-only link is visible (role gate), and /admin loads.
    // The link lives in the account (username) dropdown.
    await page.getByTestId("user-menu-toggle").click();
    await expect(page.getByTestId("admin-link")).toBeVisible();
    await page.getByTestId("admin-link").click();
    await expect(page.getByTestId("admin-screen")).toBeVisible();

    // Session survives a reload (token restored from storage) — still on an
    // authed screen, not bounced to login.
    await page.goto("/");
    await page.reload();
    await expect(page.getByTestId("home-screen")).toBeVisible();
    await expect(page.getByTestId("current-user")).toHaveText(ADMIN_USER);
  });

  test("setup screen is not shown once an Admin exists", async ({ page }) => {
    // Visiting /setup post-bootstrap redirects to /login (we are logged out in a
    // fresh context here — no stored token).
    await page.goto("/setup");
    await expect(page.getByTestId("login-screen")).toBeVisible();
  });

  test("logout clears the session and returns to login; returning login works", async ({ page }) => {
    // Log in fresh in this context.
    await page.goto("/login");
    await expect(page.getByTestId("login-screen")).toBeVisible();
    await page.getByTestId("login-username").fill(ADMIN_USER);
    await page.getByTestId("login-password").fill(ADMIN_PASS);
    await page.getByTestId("login-submit").click();
    await expect(page.getByTestId("home-screen")).toBeVisible();

    // The media cookie was set by the server at login.
    const cookies = await page.context().cookies();
    expect(cookies.some((c) => c.name === "ms_media" && c.value.length > 0)).toBeTruthy();

    // Logout → back to login, and the media cookie is cleared. Sign out lives in
    // the account (username) dropdown.
    await page.getByTestId("user-menu-toggle").click();
    await page.getByTestId("logout-button").click();
    await expect(page.getByTestId("login-screen")).toBeVisible();
    const afterLogout = await page.context().cookies();
    expect(afterLogout.some((c) => c.name === "ms_media" && c.value.length > 0)).toBeFalsy();

    // Returning login from the same screen lands back on Home.
    await page.getByTestId("login-username").fill(ADMIN_USER);
    await page.getByTestId("login-password").fill(ADMIN_PASS);
    await page.getByTestId("login-submit").click();
    await expect(page.getByTestId("home-screen")).toBeVisible();
  });

  test("a wrong password surfaces the API error and stays on login", async ({ page }) => {
    await page.goto("/login");
    await page.getByTestId("login-username").fill(ADMIN_USER);
    await page.getByTestId("login-password").fill("not the password");
    await page.getByTestId("login-submit").click();
    await expect(page.getByTestId("login-error")).toBeVisible();
    await expect(page.getByTestId("login-screen")).toBeVisible();
  });

  test("a stale/garbage token (401) clears the session and routes to login", async ({ page }) => {
    // Seed an optimistic session with a token the server will reject. On load the
    // provider restores it, then verifySession() 401s → global handler clears it
    // → the guard redirects to /login (PRD user story 5).
    await page.goto("/login");
    await page.evaluate(() => {
      localStorage.setItem("juicebox.token", "garbage-revoked-token");
      localStorage.setItem(
        "juicebox.user",
        JSON.stringify({ id: "x", username: "ghost", role: "admin" }),
      );
    });
    await page.goto("/");
    // The 401 tears the session down; we land on login, not Home.
    await expect(page.getByTestId("login-screen")).toBeVisible();
    await expect(page.getByTestId("home-screen")).toHaveCount(0);
  });
});
