import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Cascade — "also apply to children" (item-editing/05, ADR-0019) against
// the REAL embedded Go server with its TMDB stub. An Admin opens a Show's detail,
// uses the parent Fix-info picker, ticks the "also apply to children" checkbox (shown
// only because a Show HAS children), picks a candidate, and Applies it — the server
// pins the Show as a durable Enrichment override AND cascades to its episodes
// positionally, returning a summary the picker renders. Here we drive the checkbox +
// summary through the BROWSER; the mapping/durability/skip rules are asserted by the
// Go black-box tests. It reuses the shared boot-server TMDB stub (which returns a
// canned episode for any episode lookup, so the one-episode Show cascades to 1
// updated child) and builds its OWN unique temp TV fixtures dir.

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const TV = join(repoRoot, "internal", "api", "testdata", "tv");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-cascade" },
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

test.describe.serial("edit-item: cascade to children (Show → episodes)", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Unique temp TV fixtures: one Show with a single episode (copied from the Go TV
    // testdata) so the cascade has an episode child to update.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-cascade-"));
    const seasonDir = join(fixturesDir, "The Bear (2022)", "Season 01");
    mkdirSync(seasonDir, { recursive: true });
    cpSync(
      join(TV, "The Bear (2022)", "Season 01", "The Bear (2022) - S01E01 - System.mkv"),
      join(seasonDir, "The Bear (2022) - S01E01 - System.mkv"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Cascade E2E ${Date.now()}`, kind: "tv", rootFolders: [fixturesDir] },
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

  test("an Admin ticks 'also apply to children' and sees the cascade summary", async ({
    page,
  }) => {
    await uiLogin(page);

    // Open the Show's detail from the TV grid.
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "The Bear" }).first().click();
    await expect(page.getByTestId("show-detail-screen")).toBeVisible();

    // Open the "Edit item" dialog and select the Search tab (ADR-0019 unified Search);
    // the cascade flow drives Update (an Enrichment override with "apply to children").
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await page.getByTestId("edit-item-tab-search").click();

    const picker = page.getByTestId("entity-enrichment-override-picker");
    await expect(picker).toBeVisible();

    // The "also apply to children" checkbox is shown on a parent (Show HAS children).
    const cascade = picker.getByTestId("entity-enrichment-cascade");
    await expect(cascade).toBeVisible();
    await cascade.locator("input").check();

    // Search TMDB tv, SELECT a candidate, and Update — with cascade ticked the server
    // re-resolves the Show's episodes positionally.
    await picker.getByTestId("entity-enrichment-search-input").fill("Game of Thrones");
    await picker.getByTestId("entity-enrichment-search-button").click();
    const candidate = picker.getByTestId("entity-enrichment-candidate").first();
    await expect(candidate).toBeVisible();
    await candidate.click();
    await picker.getByTestId("edit-apply-update").click();

    // The post-apply summary reports the children updated (the one episode maps).
    const summary = picker.getByTestId("entity-enrichment-cascade-summary");
    await expect(summary).toBeVisible();
    await expect(summary).toContainText("1 updated");
  });
});
