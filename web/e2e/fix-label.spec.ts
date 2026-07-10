import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Edit-item "Fix label" (item-editing/03, ADR-0019) against the REAL
// embedded Go server with its TMDB stub. An Admin opens a Movie detail and:
//   1. hand-edits the overview → the field is Locked (a badge appears), proving the
//      manual-edit form writes-and-locks through the browser;
//   2. opens the poster image picker → the provider's posters list (from the stub's
//      /movie/{id}/images), picks a non-default one → the poster role is Locked.
// It also asserts the box VISIBLY SEPARATES "Fix label" from "Fix info" (the wrong-
// record correction) so a rename is never mistaken for a re-identification. The
// server-side invariants (local-wins, no cascade, identity/watch untouched) are
// covered by the Go black-box tests; here we drive the flow through the BROWSER.
//
// Builds its OWN unique temp fixtures dir so it never collides with the shared
// library the other enrichment specs own.

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const NAMING = join(repoRoot, "internal", "api", "testdata", "naming");
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

let fixturesDir = "";
let libId = "";

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

async function login(request: APIRequestContext): Promise<string> {
  const res = await request.post("/api/v1/auth/login", {
    data: {
      username: ADMIN_USER,
      password: ADMIN_PASS,
      device: { name: "seed", platform: "test", clientId: "e2e-seed-fix-label" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("edit-item: Fix label — manual edit + image picker", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // A single Movie carrying an embedded {tmdb-777} id so enrichment resolves BY id
    // (and the image picker can list /movie/777/images). Its detail starts enriched
    // with a pinned id and can be Fix-label'd.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-fix-label-fixtures-"));
    const movieDir = join(fixturesDir, "Fixlabel Movie (2099) {tmdb-777}");
    mkdirSync(movieDir, { recursive: true });
    cpSync(
      join(NAMING, "Yearless Movie", "Yearless Movie.mp4"),
      join(movieDir, "Fixlabel Movie (2099) {tmdb-777}.mp4"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Fix Label E2E ${Date.now()}`, kind: "movie", rootFolders: [fixturesDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
    for (let i = 0; i < 100; i++) {
      const st = await (
        await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth })
      ).json();
      if (st.state && st.state !== "running") break;
      await new Promise((r) => setTimeout(r, 50));
    }

    const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();

    await request.dispose();
  });

  test.afterAll(async ({ playwright, baseURL }) => {
    if (!fixturesDir) return;
    try {
      const request = await playwright.request.newContext({ baseURL });
      const token = await login(request);
      if (libId) {
        await request.delete(`/api/v1/libraries/${libId}`, {
          headers: { Authorization: `Bearer ${token}` },
        });
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
  });

  test("an Admin hand-edits a field (Locked) and picks a non-default poster (Locked)", async ({
    page,
  }) => {
    await uiLogin(page);

    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "Fixlabel Movie" }).click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();

    // Open the "Edit item" dialog (ADR-0019 unified Search). Fix label and Search are
    // VISIBLY SEPARATE tabs, so a rename is never mistaken for a re-identification:
    // both tab buttons are present, and selecting Fix label shows only that form.
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await expect(page.getByTestId("edit-item-tab-search")).toBeVisible();
    await page.getByTestId("edit-item-tab-fix-label").click();

    const editor = page.getByTestId("fix-label-editor");
    await expect(editor).toBeVisible();
    // The tab/editor is presented as "Details" (the fix-label ACTION name stays
    // in code/testids; the visible label was renamed in the flat-UI redesign).
    await expect(editor).toContainText("Details");

    // (AC) Hand-edit the overview → save → the field becomes Locked (a badge shows).
    await editor.getByTestId("edit-overview").fill("My hand-written overview.");
    await editor.getByTestId("save-metadata").click();
    await expect(editor.getByTestId("lock-badge-overview")).toBeVisible();
    // The edit is reflected on the detail overview.
    await expect(page.getByTestId("detail-overview")).toContainText("My hand-written overview.");

    // (AC) The poster picker now lives in its own Poster tab (artwork-management/01),
    // which AUTO-SEARCHES on open — no "Choose image" pre-click. Switch to it, wait
    // for the candidate grid, pick a non-default provider image → the role is Locked.
    const dialog = page.getByTestId("edit-item-dialog");
    await dialog.getByTestId("edit-item-tab-poster").click();
    const grid = dialog.getByTestId("artwork-grid-poster");
    await expect(grid).toBeVisible();
    const choices = grid.getByTestId("artwork-choice");
    await expect(choices.first()).toBeVisible();
    // Pick the SECOND poster (a non-default one).
    await choices.nth(1).click();
    await expect(dialog.getByTestId("lock-badge-poster")).toBeVisible();
  });
});
