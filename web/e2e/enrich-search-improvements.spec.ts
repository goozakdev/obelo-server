import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end for item-editing/search-improvements: the paste-a-provider-id/URL escape
// hatch ("when search isn't enough"). Against the REAL embedded Go server + its TMDB
// stub, an Admin opens a Movie the stub initially no-matches, opens the "Have a
// MusicBrainz/TMDB ID or URL?" affordance in the Fix-info picker, pastes a TMDB id,
// PREVIEWS the record (a by-id lookup, no search), and Applies it — the detail then
// reflects the pinned record. Kind-validation / 404-on-stale-id are asserted at the Go
// layer; here we drive the paste → preview → apply flow through the BROWSER.
//
// Modeled on enrich-override.spec.ts, with its OWN unique temp fixtures dir so it never
// collides with the shared library other enrichment specs own.

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-search-improvements" },
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

test.describe.serial("edit-item: paste a provider id/URL to apply an Enrichment override", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-search-improvements-fixtures-"));
    const movieDir = join(fixturesDir, "Nomatch Movie (2097)");
    mkdirSync(movieDir, { recursive: true });
    cpSync(
      join(NAMING, "Yearless Movie", "Yearless Movie.mp4"),
      join(movieDir, "Nomatch Movie (2097).mp4"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: {
        name: `Search Improvements E2E ${Date.now()}`,
        kind: "movie",
        rootFolders: [fixturesDir],
      },
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

  test("an Admin pastes an id, previews the record, and applies it", async ({ page }) => {
    await uiLogin(page);

    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "Nomatch Movie" }).click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    await expect(page.getByTestId("detail-overview")).toHaveCount(0);

    // Open the "Edit item" dialog and select the Search tab (ADR-0019 unified Search).
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await page.getByTestId("edit-item-tab-search").click();

    const picker = page.getByTestId("enrichment-override-picker");
    await expect(picker).toBeVisible();

    // The single Search input doubles as the paste escape hatch: pasting a TMDB URL
    // (the /movie/777 stub resolves it) is detected client-side as a by-id lookup, not
    // a search — the resolved record comes back as a single AUTO-SELECTED candidate row.
    await picker
      .getByTestId("enrichment-search-input")
      .fill("https://www.themoviedb.org/movie/777-dune");
    await picker.getByTestId("enrichment-search-button").click();

    const previewCard = picker.getByTestId("enrichment-candidate").first();
    await expect(previewCard).toBeVisible();
    await expect(previewCard.getByTestId("enrichment-candidate-title")).toContainText("Dune");
    // The pasted-id row is auto-selected, so the Update button is ready immediately.
    await picker.getByTestId("edit-apply-update").click();

    // Applying the pasted id pins the record and re-enriches — the detail now carries
    // its overview and the picker reflects the newly-pinned id.
    await expect(page.getByTestId("detail-overview")).toContainText("dunes and destiny");
    await expect(picker.getByTestId("enrichment-override-current")).toContainText("777");
  });
});
