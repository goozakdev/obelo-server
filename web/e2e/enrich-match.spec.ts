import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end enrichment-match correction + attention surface (external-metadata-
// enrichment issue 05) against the REAL embedded Go server with its TMDB stub. A
// Title the stub cannot match ("Nomatch Movie") surfaces on the admin attention
// "Metadata match" list; the Admin enters a correcting TMDB id, the server
// re-enriches just that Title (the stub resolves it BY ID), and the Title leaves
// the list with its enriched overview now showing. Identity/watch state are
// preserved server-side (asserted by the Go black-box tests); here we drive the
// correct-match flow through the BROWSER.
//
// Like libraries-attention.spec.ts, this builds its OWN unique temp fixtures dir
// (one movie folder whose title the stub no-matches) so it never collides with
// the shared FIXTURES library other enrichment specs own.

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-enrich-match" },
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

test.describe.serial("enrichment match: attention surface + correct a no-match", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Unique temp fixtures: one movie folder the stub deliberately no-matches.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-enrich-match-fixtures-"));
    const movieDir = join(fixturesDir, "Nomatch Movie (2099)");
    mkdirSync(movieDir, { recursive: true });
    cpSync(
      join(NAMING, "Yearless Movie", "Yearless Movie.mp4"),
      join(movieDir, "Nomatch Movie (2099).mp4"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Enrich Match E2E ${Date.now()}`, kind: "movie", rootFolders: [fixturesDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();

    // Enrich the library: "Nomatch Movie" no-matches → enrichmentStatus unmatched.
    const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();
    const result = await enrich.json();
    expect(result.unmatched, `expected an unmatched title: ${JSON.stringify(result)}`).toBeGreaterThan(0);

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

  test("the no-match Title surfaces, a corrected id re-enriches it, and it leaves the list", async ({
    page,
  }) => {
    await uiLogin(page);

    // Attention tab → select our library → the "Metadata match" list shows the
    // unmatched Title.
    await page.goto("/admin/attention");
    await expect(page.getByTestId("admin-attention")).toBeVisible();
    await page.getByTestId("attention-library-select").selectOption(libId);

    await expect(page.getByTestId("enrichment-attention-list")).toBeVisible();
    const item = page
      .getByTestId("enrichment-attention-item")
      .filter({ hasText: "Nomatch Movie" });
    await expect(item).toBeVisible();
    await expect(item.getByTestId("enrichment-attention-status")).toHaveText("unmatched");

    // Open the match form, enter the correcting TMDB id (the stub resolves /movie/
    // {id}), and apply.
    await item.getByTestId("enrichment-match-button").click();
    const form = item.getByTestId("enrichment-match-form");
    await expect(form).toBeVisible();
    await form.getByTestId("enrichment-match-tmdb").fill("777");
    await form.getByTestId("enrichment-match-submit").click();

    // The corrected Title re-enriches and leaves the attention list.
    await expect(
      page.getByTestId("enrichment-attention-item").filter({ hasText: "Nomatch Movie" }),
    ).toHaveCount(0);

    // The Title now carries its enriched overview on the detail page.
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "Nomatch Movie" }).click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    await expect(page.getByTestId("detail-overview")).toContainText("dunes and destiny");
  });
});
