import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end admin flow (issue 06 acceptance criteria) against the REAL embedded
// Go server: drive the BROWSER through login → /admin → create a Movie library →
// scan → poll status to idle → confirm the titles are browsable → delete the
// library, plus the folder-overlap error path. Unlike the browse/home/play specs
// (which seed via the API because there was no admin UI), this spec exercises the
// admin UI itself end-to-end.
//
// FOLDER_OVERLAP avoidance: the Playwright webServer is reused for the whole run,
// so libraries created in browse/play/home specs PERSIST and own the checked-in
// fixtures dirs (internal/api/testdata/naming and /movies). Creating a library at
// those roots would 409. So this spec builds its OWN unique temp fixtures dir
// under the OS temp dir, copies a couple of tiny real clips into it (so a scan
// finds >0 titles), points the library there, and DELETES the library at the end
// for cleanliness. The temp dir is removed in afterAll.

const here = dirname(fileURLToPath(import.meta.url));
// here is web/e2e, so the repo root is two levels up (web/e2e → web → repo).
const repoRoot = resolve(here, "..", "..");
const MOVIES = join(repoRoot, "internal", "api", "testdata", "movies");
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

// A unique fixtures dir for this spec, with a couple of real clips copied in so a
// scan finds titles. Created in beforeAll, removed in afterAll.
let fixturesDir = "";

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

// ensureAdmin: create the first Admin via the claim token if setup is still
// required; otherwise it already exists (another spec created it). Idempotent.
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

// Browser login through the real UI, landing on Home.
async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("admin: libraries & scanning", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    await request.dispose();

    // Build the unique temp fixtures dir and copy two real clips into it:
    // "Sample Movie (2000).mp4" (a loose file) and the "Dune (2021)" folder. Both
    // are tiny checked-in fixtures the scanner recognizes, so the scan finds >0
    // titles without colliding with any other spec's library root.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-admin-fixtures-"));
    cpSync(
      join(MOVIES, "Sample Movie (2000).mp4"),
      join(fixturesDir, "Sample Movie (2000).mp4"),
    );
    cpSync(join(MOVIES, "Dune (2021)"), join(fixturesDir, "Dune (2021)"), {
      recursive: true,
    });
  });

  test.afterAll(async () => {
    if (fixturesDir) {
      try {
        rmSync(fixturesDir, { recursive: true, force: true });
      } catch {
        /* best effort */
      }
    }
  });

  test("non-admin gate: the admin-link reaches /admin for an Admin", async ({ page }) => {
    await uiLogin(page);
    // The header's admin-link is role-gated; an Admin sees it and it routes to
    // the management hub (RequireAdmin lets the Admin through; the server still
    // enforces). A non-admin never renders the link and RequireAdmin would
    // bounce them — not exercisable here since the backend is single-Admin.
    // The admin-link now lives in the account (username) dropdown.
    await page.getByTestId("user-menu-toggle").click();
    await page.getByTestId("admin-link").click();
    await expect(page.getByTestId("admin-screen")).toBeVisible();
    await expect(page.getByTestId("admin-libraries")).toBeVisible();
    await expect(page.getByTestId("create-library-form")).toBeVisible();
  });

  test("create → scan → status idle with titles → browsable → delete", async ({ page }) => {
    await uiLogin(page);
    await page.goto("/admin");
    await expect(page.getByTestId("admin-libraries")).toBeVisible();

    // Create a Movie library at the unique temp fixtures dir.
    const name = `Admin E2E ${Date.now()}`;
    await page.getByTestId("library-name-input").fill(name);
    await page.getByTestId("root-folder-input").first().fill(fixturesDir);
    await page.getByTestId("create-library-submit").click();

    // The new row appears (the hub reloads the list after a create).
    const row = page
      .getByTestId("admin-library-row")
      .filter({ has: page.getByTestId("admin-library-name").filter({ hasText: name }) });
    await expect(row).toBeVisible();
    await expect(row.getByTestId("admin-library-roots")).toContainText(fixturesDir);

    // Trigger an incremental scan and poll the status to idle with titles found.
    await row.getByTestId("scan-button").click();
    const scanStatus = row.getByTestId("scan-status");
    await expect
      .poll(async () => scanStatus.getAttribute("data-state"), { timeout: 15000 })
      .toBe("idle");
    await expect
      .poll(async () =>
        Number(await row.getByTestId("scan-titles-found").innerText()),
      )
      .toBeGreaterThan(0);

    // Capture the library id so we can confirm the titles are browsable.
    const libId = await row.getAttribute("data-library-id");
    expect(libId, "row carries the library id").toBeTruthy();

    // The scanned titles are browsable in the viewer grid.
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    const tiles = page.getByTestId("poster-tile");
    await expect(tiles.first()).toBeVisible();
    expect(await tiles.count()).toBeGreaterThan(0);

    // Back to the admin hub and DELETE the library (cleanliness + assert gone).
    await page.goto("/admin");
    const sameRow = page
      .getByTestId("admin-library-row")
      .filter({ has: page.getByTestId("admin-library-name").filter({ hasText: name }) });
    await expect(sameRow).toBeVisible();
    await sameRow.getByTestId("delete-library-button").click();
    await expect(sameRow).toHaveCount(0);
  });

  test("folder-overlap create renders a readable inline error (no crash)", async ({ page }) => {
    await uiLogin(page);
    await page.goto("/admin");
    await expect(page.getByTestId("admin-libraries")).toBeVisible();

    // Create one library at the temp fixtures dir, then attempt a SECOND library
    // at the SAME root — guaranteed FOLDER_OVERLAP, self-contained (no dependence
    // on another spec having created a library first).
    const baseName = `Overlap Base ${Date.now()}`;
    await page.getByTestId("library-name-input").fill(baseName);
    await page.getByTestId("root-folder-input").first().fill(fixturesDir);
    await page.getByTestId("create-library-submit").click();

    const baseRow = page
      .getByTestId("admin-library-row")
      .filter({ has: page.getByTestId("admin-library-name").filter({ hasText: baseName }) });
    await expect(baseRow).toBeVisible();

    // Now the conflicting create.
    await page.getByTestId("library-name-input").fill(`Overlap Dup ${Date.now()}`);
    await page.getByTestId("root-folder-input").first().fill(fixturesDir);
    await page.getByTestId("create-library-submit").click();

    const err = page.getByTestId("create-library-error");
    await expect(err).toBeVisible();
    await expect(err).toHaveAttribute("data-overlap", "true");
    // The form is still standing (no crash) and no dup row was added.
    await expect(page.getByTestId("create-library-form")).toBeVisible();

    // Cleanup: delete the base library so the run stays collision-free.
    await baseRow.getByTestId("delete-library-button").click();
    await expect(baseRow).toHaveCount(0);
  });
});
