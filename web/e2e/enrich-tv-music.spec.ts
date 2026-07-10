import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end TV + Music Enrichment (external-metadata-enrichment issue 03)
// against the REAL embedded Go server, whose providers are pointed at the local
// TMDB + MusicBrainz/Cover Art stubs in boot-server.mjs (no live network). We
// seed a TV and a Music library via the API, enrich both, then drive the BROWSER
// to an enriched Show detail and an enriched Album detail and assert the
// decorated fields + real fetched artwork.

const here = dirname(fileURLToPath(import.meta.url));
// Dedicated fixture roots (copies of the api testdata trees) so enriching them
// never perturbs the tv/music browse specs, which point at the testdata roots and
// assert un-enriched display titles — mirroring how enrich.spec.ts uses the
// `naming` root rather than the `movies` root browse.spec.ts uses.
const TV_FIXTURES = join(here, "fixtures", "enrich-tv");
const MUSIC_FIXTURES = join(here, "fixtures", "enrich-music");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-enrich-tvm" },
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
  name: string,
  kind: string,
  root: string,
): Promise<string> {
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name, kind, rootFolders: [root] },
  });
  if (create.ok()) return (await create.json()).id as string;
  if (create.status() === 409) {
    const libs = (await (await request.get("/api/v1/libraries", { headers: auth })).json())
      .libraries as Array<{ id: string; rootFolders: Array<{ path: string }> }>;
    const existing = libs.find((l) => l.rootFolders.some((r) => r.path === root));
    expect(existing, `library at ${root} not found after 409`).toBeTruthy();
    return existing!.id;
  }
  throw new Error(`create ${kind}: ${create.status()} ${await create.text()}`);
}

async function scanAndEnrich(
  request: APIRequestContext,
  auth: Record<string, string>,
  libId: string,
): Promise<void> {
  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
  // The scan runs ASYNCHRONOUSLY (202 → "running"). Enrichment matches the Titles
  // that exist when it runs, so we MUST wait for the scan to settle first — else it
  // enriches an empty (still-scanning) Library and matches nothing.
  const settled = await waitScanSettled(request, auth, libId);
  expect(settled.titlesFound, `scan found no titles: ${JSON.stringify(settled)}`).toBeGreaterThan(0);
  const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
  expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();
  const result = await enrich.json();
  expect(result.matched, `enrich matched none: ${JSON.stringify(result)}`).toBeGreaterThan(0);
}

// waitScanSettled polls GET /libraries/{id}/scan until the Library leaves the
// "running" state (or it times out), returning the settled status.
async function waitScanSettled(
  request: APIRequestContext,
  auth: Record<string, string>,
  libId: string,
): Promise<{ state: string; titlesFound: number }> {
  const deadline = Date.now() + 15_000;
  let last: { state: string; titlesFound: number } = { state: "running", titlesFound: 0 };
  while (Date.now() < deadline) {
    const res = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    if (res.ok()) {
      last = (await res.json()) as { state: string; titlesFound: number };
      if (last.state !== "running") return last;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  return last;
}

async function uiLogin(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

async function imageDecodes(locator: import("@playwright/test").Locator): Promise<void> {
  await expect(locator.first()).toBeVisible();
  await expect
    .poll(async () => locator.first().evaluate((el: HTMLImageElement) => el.naturalWidth))
    .toBeGreaterThan(0);
}

test.describe.serial("enrichment: TV & Music decorated detail", () => {
  let tvLibId = "";
  let musicLibId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    tvLibId = await findOrCreateLibrary(request, auth, "Enriched Shows", "tv", TV_FIXTURES);
    musicLibId = await findOrCreateLibrary(request, auth, "Enriched Music", "music", MUSIC_FIXTURES);
    await scanAndEnrich(request, auth, tvLibId);
    await scanAndEnrich(request, auth, musicLibId);

    await request.dispose();
  });

  test("Show detail renders enriched overview, genres, content rating + a real logo hero", async ({
    page,
  }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${tvLibId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    await page.getByTestId("poster-tile").filter({ hasText: "The Bear" }).click();

    await expect(page.getByTestId("show-detail")).toBeVisible();
    await expect(page.getByTestId("show-overview")).toContainText("Iron Throne");
    await expect(page.getByTestId("show-genres")).toContainText("Drama");
    await expect(page.getByTestId("show-content-rating")).toContainText("TV-MA");
    await expect(page.getByTestId("show-network")).toContainText("HBO");

    // Logo-hero: the fetched Show logo stands in for the title text (and the
    // hero shows no poster), and it actually decodes (the stub served a real
    // PNG). The text heading only renders when a show has no logo.
    await imageDecodes(page.getByTestId("detail-logo"));
    await expect(page.getByTestId("show-title")).toHaveCount(0);

    // An episode shows its canonical name + still.
    await expect(page.getByTestId("episode-title").first()).toContainText("The Suitcase");
  });

  test("Album detail renders enriched genres + a real fetched cover", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${musicLibId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    await page.getByTestId("poster-tile").filter({ hasText: "Radiohead" }).click();
    await expect(page.getByTestId("artist-detail")).toBeVisible();
    // Enriched artist bio + genres from the MusicBrainz stub.
    await expect(page.getByTestId("artist-overview")).toContainText("English rock band");
    await expect(page.getByTestId("artist-genres")).toContainText("alternative rock");

    await page.getByTestId("poster-tile").filter({ hasText: "OK Computer" }).click();
    await expect(page.getByTestId("album-detail")).toBeVisible();
    await expect(page.getByTestId("album-genres")).toContainText("alternative rock");

    // The fetched album cover (Cover Art Archive stub) decodes.
    await imageDecodes(page.getByTestId("poster-img"));
  });
});
