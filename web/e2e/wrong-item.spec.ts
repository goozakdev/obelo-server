import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Edit-item "Wrong item" (item-editing/04, ADR-0019/ADR-0014) against
// the REAL embedded Go server with its TMDB stub. A movie folder parsed as "Wrongly
// Named (2098)" is genuinely a different work; the Admin opens its detail, uses the
// DESTRUCTIVE Wrong-item box (visibly distinct from Fix info / Fix label — it warns
// that it resets watch history and clears locks), searches the provider, picks the
// correct work, CONFIRMS the re-identification, and the detail re-identifies to the
// picked work (title + overview change). Identity re-key + watch reset + lock clear
// are asserted server-side by the Go black-box tests; here we drive the confirm-and-
// re-identify flow through the BROWSER, confirming the box distinguishes it from a
// label edit.
//
// Reuses the shared boot-server.mjs TMDB stub (its /search/movie returns a "Dune"
// candidate id 777 and /movie/777 resolves the record). Builds its OWN unique temp
// fixtures dir so it never collides with the other enrichment specs' libraries.

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-wrong-item" },
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

test.describe.serial("edit-item: Wrong item re-identifies a Movie", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Unique temp fixtures: a movie whose folder parses to a DIFFERENT work than it
    // actually is, so an Admin re-identifies it with Wrong item.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-wrong-item-fixtures-"));
    const movieDir = join(fixturesDir, "Wrongly Named (2098)");
    mkdirSync(movieDir, { recursive: true });
    cpSync(
      join(NAMING, "Yearless Movie", "Yearless Movie.mp4"),
      join(movieDir, "Wrongly Named (2098).mp4"),
    );

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Wrong Item E2E ${Date.now()}`, kind: "movie", rootFolders: [fixturesDir] },
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

  test("an Admin re-identifies a Movie and the box distinguishes it from a label edit", async ({
    page,
  }) => {
    await uiLogin(page);

    // Open the mis-named Movie's detail from the library grid.
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "Wrongly Named" }).click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    await expect(page.getByTestId("detail-title")).toHaveText("Wrongly Named");

    // Open the "Edit item" dialog (ADR-0019 unified Search). The two actions live in
    // SEPARATE tabs — Search and Fix label — so a re-identification is never mistaken
    // for a label edit; the destructive Replace lives inside the Search tab, styled as
    // a danger button with a consequence hint (resets watch state + your edits).
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await expect(page.getByTestId("edit-item-tab-search")).toBeVisible();
    await expect(page.getByTestId("edit-item-tab-fix-label")).toBeVisible();
    await page.getByTestId("edit-item-tab-search").click();

    const picker = page.getByTestId("enrichment-override-picker");
    await expect(picker).toBeVisible();

    // Search the provider, SELECT the correct work, and Replace it (the destructive
    // identity correction — a single click; the red styling + hint are the guardrail).
    await picker.getByTestId("enrichment-search-input").fill("Dune");
    await picker.getByTestId("enrichment-search-button").click();
    const candidate = picker.getByTestId("enrichment-candidate").first();
    await expect(candidate).toBeVisible();
    await candidate.click();
    const replace = picker.getByTestId("edit-apply-replace");
    await expect(replace).toBeVisible();
    await replace.click();

    // The detail RE-IDENTIFIES to the picked work — the hero now names "Dune"
    // (only Wrong item does this; Fix info would leave the title) and the picked
    // record's overview now decorates it — without a page reload. Re-enrichment
    // fetched the picked work's logo, which stands in for the title text
    // (logo-hero), so the name lands in the logo's alt text.
    await expect(page.getByTestId("detail-logo")).toHaveAttribute("alt", "Dune");
    await expect(page.getByTestId("detail-title")).toHaveCount(0);
    await expect(page.getByTestId("detail-overview")).toContainText("dunes and destiny");
  });
});
