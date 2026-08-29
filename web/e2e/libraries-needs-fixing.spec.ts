import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end admin NEEDS-FIXING + DEVICES flow against the REAL embedded Go
// server. As an Admin: see a needs-review Title and an Unmatched file in one
// queue, fix the Unmatched file by SEARCHING the provider (never by typing an id
// — that was the old screen's only tool), and confirm the correction is recorded
// AND survives a rescan; then list the user's devices and revoke one, asserting
// via the API that the revoked device's token is rejected.
//
// FOLDER_OVERLAP avoidance + fixtures: the Playwright webServer is reused for the
// whole run, so libraries created by other specs PERSIST and own the checked-in
// fixtures dirs. This spec builds its OWN unique temp fixtures dir under the OS
// temp dir and copies in exactly the two cases issue 07 needs:
//   - a YEARLESS folder ("Yearless Movie/Yearless Movie.mp4") → a needs-review
//     Title (filed from a partial parse, no year),
//   - a bare quality-token file ("1080p.mkv") → an Unmatched file (a recognized
//     media file with no extractable identity).
// The library is created at this temp dir and DELETED in afterAll; the temp dir
// is removed too.
//
// Spec ordering: this file sorts AFTER auth.spec.ts (which needs the fresh
// server's one-time setup), like libraries-admin.spec.ts. Seeding is idempotent
// (ensureAdmin reuses an existing admin; create-library reuses on 409).

const here = dirname(fileURLToPath(import.meta.url));
// here is web/e2e, so the repo root is two levels up (web/e2e → web → repo).
const repoRoot = resolve(here, "..", "..");
const NAMING = join(repoRoot, "internal", "api", "testdata", "naming");
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

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

// API login returning the bearer token + the registered device (used to seed an
// extra device we can revoke, and to assert the revoked token is rejected). Each
// call passes a distinct clientId so it registers a SEPARATE device.
async function apiLogin(
  request: APIRequestContext,
  clientId: string,
  name: string,
): Promise<{ token: string; deviceId: string }> {
  const res = await request.post("/api/v1/auth/login", {
    data: {
      username: ADMIN_USER,
      password: ADMIN_PASS,
      device: { name, platform: "e2e", clientId },
    },
  });
  if (!res.ok()) throw new Error(`login failed: ${res.status()} ${await res.text()}`);
  const body = await res.json();
  return { token: body.token, deviceId: body.device.id };
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("admin: the Needs-Fixing queue & devices", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    await request.dispose();

    // Unique temp fixtures: a yearless folder (→ needs-review) and a bare
    // quality-token file (→ Unmatched). Both are tiny checked-in fixtures.
    fixturesDir = mkdtempSync(join(tmpdir(), "obelo-needs-fixing-fixtures-"));
    cpSync(
      join(NAMING, "Yearless Movie"),
      join(fixturesDir, "Yearless Movie"),
      { recursive: true },
    );
    cpSync(join(NAMING, "1080p.mkv"), join(fixturesDir, "1080p.mkv"));
  });

  test.afterAll(async ({ playwright, baseURL }) => {
    // Delete the library this spec created (cleanliness) and remove the temp dir.
    if (fixturesDir) {
      try {
        const request = await playwright.request.newContext({ baseURL });
        const { token } = await apiLogin(request, "needs-fixing-cleanup", "cleanup");
        const libs = await (
          await request.get("/api/v1/libraries", {
            headers: { Authorization: `Bearer ${token}` },
          })
        ).json();
        for (const lib of libs.libraries ?? []) {
          const onOurDir = (lib.rootFolders ?? []).some(
            (r: { path: string }) => r.path === fixturesDir,
          );
          if (onOurDir) {
            await request.delete(`/api/v1/libraries/${lib.id}`, {
              headers: { Authorization: `Bearer ${token}` },
            });
          }
        }
        await request.dispose();
      } catch {
        /* best effort */
      }
      try {
        rmSync(fixturesDir, { recursive: true, force: true });
      } catch {
        /* best effort */
      }
    }
  });

  test("needs-review + Unmatched surface, fix-match persists across a rescan", async ({
    page,
  }) => {
    await uiLogin(page);

    // Create the library at the unique temp dir, then scan it so the yearless
    // folder becomes a needs-review Title and the bare file becomes Unmatched.
    await page.goto("/admin");
    await expect(page.getByTestId("admin-libraries")).toBeVisible();
    const name = `Attention E2E ${Date.now()}`;
    // Add via the wizard: pick the Movie kind, name it, give it the root path.
    await page.getByTestId("add-library-button").click();
    await page.getByTestId("add-library-kind-movie").click();
    await page.getByTestId("add-library-next").click();
    await page.getByTestId("add-library-name-input").fill(name);
    await page.getByTestId("add-library-next").click();
    await page.getByTestId("add-library-path-input").fill(fixturesDir);
    await page.getByTestId("add-library-submit").click();

    const row = page
      .getByTestId("admin-library-row")
      .filter({ has: page.getByTestId("admin-library-name").filter({ hasText: name }) });
    await expect(row).toBeVisible();
    const libId = await row.getAttribute("data-library-id");
    expect(libId).toBeTruthy();

    // Scan lives in the row's ⋮ menu (it moved there in the Libraries redesign).
    await row.getByTestId("library-menu-toggle").click();
    await row.getByTestId("scan-button").click();
    // Wait for the scan to actually FIND titles (not just for the indicator to
    // read idle — a never-scanned row already shows idle on mount, so polling
    // data-state alone races the scan). titlesFound > 0 means the catalog is
    // populated, so the queue's reads that follow won't see a stale empty.
    await expect
      .poll(async () => Number(await row.getByTestId("scan-title-count").innerText()), {
        timeout: 15000,
      })
      .toBeGreaterThan(0);

    // Needs Fixing tab: the needs-review Title (yearless) and the Unmatched file
    // (1080p.mkv) both surface, in ONE queue rather than in two separate lists.
    await page.getByTestId("admin-tab-needs-fixing").click();
    await expect(page.getByTestId("admin-needs-fixing")).toBeVisible();
    // The picker defaults to the first library; select ours explicitly by id.
    await page.getByTestId("needs-fixing-library-select").selectOption(libId!);

    await expect(page.getByTestId("needs-fixing-list")).toBeVisible();
    await expect(
      page.getByTestId("fix-item").filter({ hasText: "Yearless Movie" }),
    ).toBeVisible();

    // Every row names the file it is about — the Unmatched row by its path.
    const unmatchedRow = page.getByTestId("fix-item").filter({ hasText: "1080p" });
    await expect(unmatchedRow).toBeVisible();
    await expect(unmatchedRow).toHaveAttribute("data-problem", "unidentified");
    await expect(unmatchedRow.getByTestId("fix-item-path")).toContainText("1080p.mkv");

    // Fix it by SEARCHING, not by typing an id: opening the row runs the provider
    // search on its own (the TMDB stub answers /search/movie with "Dune"), and one
    // click applies the best guess as a folder-keyed correction.
    await unmatchedRow.getByTestId("fix-item-toggle").click();
    await expect(unmatchedRow.getByTestId("fix-best-guess")).toBeVisible();
    await expect(unmatchedRow.getByTestId("fix-candidate-title")).toContainText("Dune");
    await unmatchedRow.getByTestId("fix-use-best-guess").click();

    // The correction is recorded, and the queue says it lands on the next scan
    // rather than pretending the file is already re-filed (a Match override is read
    // by the scanner — ADR-0002/0014).
    await expect(page.getByTestId("needs-fixing-rescan")).toBeVisible();

    // It shows up under the corrections log, which is kept out of the work queue.
    await page.getByTestId("needs-fixing-corrections-toggle").click();
    await expect(
      page.getByTestId("override-item").filter({ hasText: "Dune" }),
    ).toBeVisible();

    // Persists across a rescan: trigger a full scan, then the correction is still
    // recorded (server-owned, survives rescans — ADR-0002/0014).
    await page.getByTestId("admin-tab-libraries").click();
    await row.getByTestId("library-menu-toggle").click();
    await row.getByTestId("full-scan-button").click();
    await expect
      .poll(async () => row.getByTestId("scan-status").getAttribute("data-state"), {
        timeout: 15000,
      })
      .toBe("idle");

    await page.getByTestId("admin-tab-needs-fixing").click();
    await page.getByTestId("needs-fixing-library-select").selectOption(libId!);
    await page.getByTestId("needs-fixing-corrections-toggle").click();
    await expect(
      page.getByTestId("override-item").filter({ hasText: "Dune" }),
    ).toBeVisible();
  });

  test("devices list shows the user's devices and revoking one rejects its token", async ({
    page,
    playwright,
    baseURL,
  }) => {
    // Seed a SEPARATE, revocable device via the API so revoking it does not log
    // the browser out. Capture its token to prove the revoke invalidates it.
    const request = await playwright.request.newContext({ baseURL });
    const clientId = `revoke-me-${Date.now()}`;
    const { token, deviceId } = await apiLogin(request, clientId, "Revoke Me");

    // The seeded token works before revoke.
    const before = await request.get("/api/v1/devices", {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(before.status()).toBe(200);

    // Drive the browser: the devices tab lists the user's devices, including the
    // seeded one; revoke it through the UI.
    await uiLogin(page);
    await page.goto("/admin/devices");
    await expect(page.getByTestId("admin-devices")).toBeVisible();
    await expect(page.getByTestId("devices-list")).toBeVisible();

    const revokeRow = page
      .getByTestId("device-item")
      .filter({ hasText: "Revoke Me" });
    await expect(revokeRow).toBeVisible();
    expect(await revokeRow.getAttribute("data-device-id")).toBe(deviceId);
    await revokeRow.getByTestId("device-revoke").click();
    // The row is removed from the list after a successful revoke.
    await expect(revokeRow).toHaveCount(0);

    // The revoked device's token is now rejected (401) — immediate invalidation.
    const after = await request.get("/api/v1/devices", {
      headers: { Authorization: `Bearer ${token}` },
    });
    expect(after.status()).toBe(401);
    await request.dispose();
  });
});
