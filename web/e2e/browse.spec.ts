import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end browse flow (issue 03 acceptance criteria) against the REAL
// embedded Go server. The browse UI needs data and there is no admin UI yet, so
// we SEED via the API with Playwright's request context (the operator's job
// today), then drive the BROWSER through login → libraries → grid → detail.
//
// Seeding mirrors an Admin: ensure the first Admin exists (claim-token setup —
// idempotent across the suite since auth.spec.ts may have already created it),
// log in for a bearer token, create a Movie library pointing at a checked-in
// fixtures dir, trigger a synchronous scan, and confirm it landed idle. We use
// the `naming` fixtures because they include a title WITH artwork ("Extras
// Movie" has poster.jpg/fanart.jpg) alongside several titles WITHOUT, so we can
// assert both a real poster load and the placeholder fallback.

const here = dirname(fileURLToPath(import.meta.url));
// here is web/e2e, so the repo root is two levels up (web/e2e → web → repo).
const repoRoot = resolve(here, "..", "..");
const FIXTURES = join(repoRoot, "internal", "api", "testdata", "naming");
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

async function login(request: APIRequestContext): Promise<string> {
  const res = await request.post("/api/v1/auth/login", {
    data: {
      username: ADMIN_USER,
      password: ADMIN_PASS,
      device: { name: "seed", platform: "test", clientId: "e2e-seed-browse" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// seedLibrary creates a Movie library at the fixtures dir and scans it to
// completion, returning the library id. Reused by the tests below.
async function seedLibrary(
  request: APIRequestContext,
  token: string,
): Promise<string> {
  const auth = { Authorization: `Bearer ${token}` };
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name: "Movies", kind: "movie", rootFolders: [FIXTURES] },
  });
  expect(
    create.ok(),
    `create library failed: ${create.status()} ${await create.text()}`,
  ).toBeTruthy();
  const libId = (await create.json()).id as string;

  // Synchronous scan: the POST returns the resulting status (state "idle").
  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, {
    headers: auth,
  });
  expect(scan.ok(), `scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();
  const status = await scan.json();
  expect(status.state).toBe("idle");
  expect(status.titlesFound).toBeGreaterThan(1);
  return libId;
}

// Browser login through the real UI, landing on Home.
async function uiLogin(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("browse: libraries, poster grid, title detail", () => {
  let libId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    libId = await seedLibrary(request, token);
    await request.dispose();
  });

  test("library list → open library → grid renders the scanned titles", async ({ page }) => {
    await uiLogin(page);
    // The home screen's link into the library area (the AppHeader's
    // nav-libraries only exists once inside the browse screens).
    await page.getByTestId("browse-link").click();

    await expect(page.getByTestId("library-list-screen")).toBeVisible();
    // At least the Movies library we seeded is listed; open it.
    const item = page.getByTestId("library-item").filter({ hasText: "Movies" });
    await expect(item).toBeVisible();
    await item.click();

    await expect(page.getByTestId("library-grid-screen")).toBeVisible();
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    // The naming fixtures yield several movies (Cut/Edition/Extras/Pinned/Split…).
    const tiles = page.getByTestId("poster-tile");
    await expect(tiles.first()).toBeVisible();
    expect(await tiles.count()).toBeGreaterThan(1);
    // "Extras Movie" (has artwork) is present.
    await expect(
      page.getByTestId("poster-tile").filter({ hasText: "Extras Movie" }),
    ).toBeVisible();
  });

  test("posters: the artworked title loads a real <img>; others show placeholders", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    // The artworked title's poster <img> actually decodes (naturalWidth > 0).
    const artworked = page
      .getByTestId("poster-tile")
      .filter({ hasText: "Extras Movie" });
    const img = artworked.getByTestId("poster-img");
    await expect(img).toBeVisible();
    await expect
      .poll(async () => img.evaluate((el: HTMLImageElement) => el.naturalWidth))
      .toBeGreaterThan(0);

    // A title with no artwork (e.g. "Cut Movie") falls back to the placeholder.
    const noArt = page
      .getByTestId("poster-tile")
      .filter({ hasText: "Cut Movie" });
    await expect(noArt.getByTestId("poster-placeholder")).toBeVisible();
  });

  test("sort changes the grid order", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    const titlesNow = async () =>
      page.getByTestId("poster-title").allInnerTexts();

    const byTitle = await titlesNow();
    // Default sort is by title — alphabetical.
    const sorted = [...byTitle].sort((a, b) => a.localeCompare(b));
    expect(byTitle).toEqual(sorted);

    // Switch to date-added; the order should differ from strict alphabetical
    // (the fixtures were added in scan order, not alphabetical).
    await page.getByTestId("sort-select").selectOption("dateAdded");
    await expect
      .poll(async () => (await titlesNow()).join("|"))
      .not.toBe(byTitle.join("|"));
  });

  test("opening a title shows its detail: editions/files + watch-state indicator", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    await page
      .getByTestId("poster-tile")
      .filter({ hasText: "Extras Movie" })
      .click();

    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    await expect(page.getByTestId("detail-title")).toContainText("Extras Movie");
    // Editions → Files rendered with quality/version info.
    await expect(page.getByTestId("edition").first()).toBeVisible();
    await expect(page.getByTestId("file-row").first()).toBeVisible();
    await expect(page.getByTestId("file-container").first()).toBeVisible();
    // Watch-state indicator present (fresh scan → unwatched).
    await expect(page.getByTestId("watch-state")).toBeVisible();
    await expect(page.getByTestId("watch-unwatched")).toBeVisible();
    // The Play affordance is wired (issue 04): enabled because the title has a
    // playable file. (Its behavior is covered by play.spec.ts.)
    await expect(page.getByTestId("play-button")).toBeEnabled();
  });
});
