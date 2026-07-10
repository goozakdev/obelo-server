import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Show detail backdrop (show-detail redesign): the Show's fetched
// TMDB Background renders as a viewport-pinned layer behind the page, and the
// black veil on top of it deepens with scroll — from the at-rest 15% up to a
// hard 50% cap — driven by DetailBackdrop.tsx writing --backdrop-fade.
//
// As in enrich-tv-music.spec.ts we seed via the API (ensure Admin → login →
// TV library at the enrich fixtures → scan → enrich against the boot-server
// TMDB stub, which serves a real PNG for the backdrop), then drive the browser
// to the Show detail and assert on the backdrop element itself: it exists,
// its image decodes, it is position: fixed (does not move when the page
// scrolls), and the veil opacity tracks scroll without ever passing 0.5.

const here = dirname(fileURLToPath(import.meta.url));
const TV_FIXTURES = join(here, "fixtures", "enrich-tv");
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

// The veil constants under test (mirrors DetailBackdrop.tsx).
const FADE_BASE = 0.15;
const FADE_MAX = 0.5;
const FADE_DISTANCE = 600;

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-backdrop" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// findOrCreateLibrary reuses an existing library at root (another spec may have
// created one — roots can't overlap), else creates it. Returns its id.
async function findOrCreateLibrary(
  request: APIRequestContext,
  auth: Record<string, string>,
): Promise<string> {
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name: "Enriched Shows", kind: "tv", rootFolders: [TV_FIXTURES] },
  });
  if (create.ok()) return (await create.json()).id as string;
  if (create.status() === 409) {
    const libs = (await (await request.get("/api/v1/libraries", { headers: auth })).json())
      .libraries as Array<{ id: string; rootFolders: Array<{ path: string }> }>;
    const existing = libs.find((l) => l.rootFolders.some((r) => r.path === TV_FIXTURES));
    expect(existing, `library at ${TV_FIXTURES} not found after 409`).toBeTruthy();
    return existing!.id;
  }
  throw new Error(`create tv library: ${create.status()} ${await create.text()}`);
}

async function scanAndEnrich(
  request: APIRequestContext,
  auth: Record<string, string>,
  libId: string,
): Promise<void> {
  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
  // The scan runs asynchronously; enrich only matches Titles that exist when it
  // runs, so wait for the scan to settle first (see enrich-tv-music.spec.ts).
  const deadline = Date.now() + 15_000;
  let settled: { state: string; titlesFound: number } = { state: "running", titlesFound: 0 };
  while (Date.now() < deadline && settled.state === "running") {
    await new Promise((r) => setTimeout(r, 100));
    const res = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    if (res.ok()) settled = (await res.json()) as typeof settled;
  }
  expect(settled.titlesFound, `scan found no titles: ${JSON.stringify(settled)}`).toBeGreaterThan(0);
  const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
  expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();
  const result = await enrich.json();
  // This fixture root is shared with enrich-tv-music.spec.ts, so the library may
  // already be fully enriched by the time this spec runs (total 0). Only a run
  // that HAD candidates and matched none is a failure.
  if (result.total > 0) {
    expect(result.matched, `enrich matched none: ${JSON.stringify(result)}`).toBeGreaterThan(0);
  }
}

async function uiLogin(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("show detail: fixed TMDB backdrop with scroll fade", () => {
  let tvLibId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };
    tvLibId = await findOrCreateLibrary(request, auth);
    await scanAndEnrich(request, auth, tvLibId);
    await request.dispose();
  });

  // A short viewport so even the small fixture Show overflows and the page
  // actually scrolls (the veil is scroll-driven).
  test.use({ viewport: { width: 1280, height: 480 } });

  test("backdrop is pinned behind the page and the veil tracks scroll, capped at 50%", async ({
    page,
  }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${tvLibId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "The Bear" }).click();
    await expect(page.getByTestId("show-detail")).toBeVisible();

    // The Background layer renders, pinned to the viewport, showing the
    // fetched Background artwork (the enrichment stub served a real PNG).
    const backdrop = page.getByTestId("detail-backdrop");
    await expect(backdrop).toBeVisible();
    expect(await backdrop.evaluate((el) => getComputedStyle(el).position)).toBe("fixed");
    const img = backdrop.locator(".detail-backdrop-img");
    await expect(img).toHaveAttribute("src", /\/artwork\/background/);
    await expect
      .poll(async () => img.evaluate((el: HTMLImageElement) => el.naturalWidth))
      .toBeGreaterThan(0);

    // veil() reads the rendered veil opacity; expected() derives what
    // DetailBackdrop should have written for the current scroll position.
    const veil = () =>
      backdrop
        .locator(".detail-backdrop-fade")
        .evaluate((el) => Number(getComputedStyle(el).opacity));
    const expected = (scrollY: number) =>
      FADE_BASE + (FADE_MAX - FADE_BASE) * Math.min(scrollY / FADE_DISTANCE, 1);

    // At the top of the page: the at-rest veil only.
    expect(await veil()).toBeCloseTo(FADE_BASE, 2);

    // Scrolled to the bottom: the veil deepened to match the formula and never
    // passes the 50% cap; the pinned backdrop itself did not move.
    const before = await backdrop.boundingBox();
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    const scrollY = await page.evaluate(() => window.scrollY);
    expect(scrollY).toBeGreaterThan(0);
    await expect.poll(veil).toBeCloseTo(expected(scrollY), 2);
    expect(await veil()).toBeLessThanOrEqual(FADE_MAX);
    expect(await backdrop.boundingBox()).toEqual(before);
  });
});
