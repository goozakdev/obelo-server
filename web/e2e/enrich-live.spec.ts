import { test, expect, type APIRequestContext } from "@playwright/test";
import { cpSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end LIVE-update flow (external-metadata-enrichment issue 02): with the
// library grid open, an Enrichment pass triggered out-of-band publishes
// enrichProgress over SSE; the browser's EventSource refetches the grid and
// cache-busts the posters, so a just-fetched poster replaces its placeholder
// with NO manual reload. Runs against the REAL embedded server whose TMDB
// provider is the local stub in boot-server.mjs (auto-enrich is OFF there, so
// the pass below is the only enrichment and the grid starts un-decorated).
//
// This spec uses its OWN throwaway library dir (a copy of one movie fixture) so
// its root can never overlap another spec's library (roots are exclusive).

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const MOVIE_FIXTURE = join(repoRoot, "internal", "api", "testdata", "movies", "Dune (2021)");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-enrich-live" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()}`).toBeTruthy();
  return (await res.json()).token as string;
}

async function uiLogin(page: import("@playwright/test").Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("enrichment: live grid update over SSE", () => {
  let libId = "";
  let token = "";
  let libDir = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    // A throwaway library dir holding a copy of one movie fixture, so this root
    // is unique to this spec and never overlaps another's.
    libDir = mkdtempSync(join(tmpdir(), "ms-enrich-live-"));
    cpSync(MOVIE_FIXTURE, join(libDir, "Dune (2021)"), { recursive: true });

    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: "Live Movies", kind: "movie", rootFolders: [libDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    // Scan only — leave Titles un-enriched (auto-enrich is OFF in boot-server).
    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
    await request.dispose();
  });

  test.afterAll(() => {
    if (libDir) rmSync(libDir, { recursive: true, force: true });
  });

  test("a poster appears live after an out-of-band enrich, with no reload", async ({
    page,
    request,
  }) => {
    const auth = { Authorization: `Bearer ${token}` };

    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    const tile = page.getByTestId("poster-tile").filter({ hasText: "Dune" });
    await expect(tile).toBeVisible();

    // Re-trigger a full enrich each poll iteration: this defeats any race where
    // the browser's EventSource hadn't finished connecting before the first pass,
    // while still proving the grid updates LIVE (no page.reload below). Once the
    // browser is subscribed, a pass's enrichProgress refetches the grid and the
    // cache-busted poster <img> decodes.
    await expect
      .poll(
        async () => {
          await request.post(`/api/v1/libraries/${libId}/enrich?mode=full`, { headers: auth });
          const img = tile.getByTestId("poster-img");
          if ((await img.count()) === 0) return 0;
          return img
            .first()
            .evaluate((el: HTMLImageElement) => el.naturalWidth)
            .catch(() => 0);
        },
        { timeout: 30000, intervals: [1000, 1000, 1500, 2000] },
      )
      .toBeGreaterThan(0);
  });
});
