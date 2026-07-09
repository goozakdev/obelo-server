import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Edit-item "Fix info" (item-editing/01, ADR-0019) against the REAL
// embedded Go server with its TMDB stub. A Movie the stub initially no-matches
// ("Nomatch Movie") carries no overview; the Admin opens its detail, uses the Fix
// info search picker to search the provider, picks a candidate, and Applies it —
// the server pins the record as a durable Enrichment override, re-enriches the
// Title, and the detail now shows the picked record's overview. Identity/watch
// state are preserved server-side (asserted by the Go black-box tests); here we
// drive the search-pick-apply flow through the BROWSER.
//
// Mirrors enrich-match.spec.ts, but drives the new search picker instead of the
// raw-id form. It builds its OWN unique temp fixtures dir so it never collides
// with the shared library other enrichment specs own.

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-enrich-override" },
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

test.describe.serial("edit-item: search a provider and apply an Enrichment override", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Unique temp fixtures: one movie folder the stub deliberately no-matches, so
    // it starts with no overview until the Admin fixes it.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-enrich-override-fixtures-"));
    const movieDir = join(fixturesDir, "Nomatch Movie (2098)");
    mkdirSync(movieDir, { recursive: true });
    cpSync(
      join(NAMING, "Yearless Movie", "Yearless Movie.mp4"),
      join(movieDir, "Nomatch Movie (2098).mp4"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Enrich Override E2E ${Date.now()}`, kind: "movie", rootFolders: [fixturesDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();

    // The scan POST is async (202, "running"); wait for it to settle so the enrich
    // pass below sees the scanned Title (avoids a scan/enrich race).
    for (let i = 0; i < 100; i++) {
      const st = await (
        await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth })
      ).json();
      if (st.state && st.state !== "running") break;
      await new Promise((r) => setTimeout(r, 50));
    }

    const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();
    const result = await enrich.json();
    expect(result.total, `expected the scanned Title: ${JSON.stringify(result)}`).toBeGreaterThan(0);

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

  test("an Admin searches, picks a candidate, and the Movie detail reflects the new record", async ({
    page,
  }) => {
    await uiLogin(page);

    // Open the (initially un-enriched) Movie's detail from the library grid.
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "Nomatch Movie" }).click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    // It has no overview yet (the stub no-matched it by name).
    await expect(page.getByTestId("detail-overview")).toHaveCount(0);

    // The Edit-item forms now live behind the "Edit item" dialog (ADR-0019 unified
    // Search): open it and select the Search tab before driving the picker.
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await page.getByTestId("edit-item-tab-search").click();

    // Search: query the provider, SELECT a candidate row, then apply with Update
    // (the safe Fix info — an Enrichment override).
    const picker = page.getByTestId("enrichment-override-picker");
    await expect(picker).toBeVisible();
    // No Enrichment override in effect yet (the Title never matched a record).
    await expect(picker.getByTestId("enrichment-override-current")).toHaveCount(0);
    await picker.getByTestId("enrichment-search-input").fill("Dune");
    await picker.getByTestId("enrichment-search-button").click();

    const candidate = picker.getByTestId("enrichment-candidate").first();
    await expect(candidate).toBeVisible();
    await expect(candidate.getByTestId("enrichment-candidate-title")).toContainText("Dune");
    await candidate.click();
    await picker.getByTestId("edit-apply-update").click();

    // The detail now carries the picked record's overview (from /movie/777) AND the
    // picker's "current record" line reflects the newly-pinned id — both WITHOUT a
    // page reload (the whole title object refreshed from the apply response).
    await expect(page.getByTestId("detail-overview")).toContainText("dunes and destiny");
    await expect(picker.getByTestId("enrichment-override-current")).toContainText("777");
  });
});
