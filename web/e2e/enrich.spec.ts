import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Enrichment flow (external-metadata-enrichment issue 01) against the
// REAL embedded Go server, whose TMDBProvider + ArtworkFetcher are pointed at the
// local TMDB stub in boot-server.mjs (no live network). We seed a Movie library
// via the API (the operator's job today), trigger an Enrichment pass, then drive
// the BROWSER to a title detail and assert the decorated fields + a real poster.

const here = dirname(fileURLToPath(import.meta.url));
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-enrich" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
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

test.describe.serial("enrichment: decorate movies + render enriched detail", () => {
  let libId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Find-or-create a Movie library at the naming fixtures: another spec in the
    // suite may have already created one at this root (roots can't overlap), so
    // reuse it on a 409 rather than failing.
    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: "Enriched Movies", kind: "movie", rootFolders: [FIXTURES] },
    });
    if (create.ok()) {
      libId = (await create.json()).id as string;
    } else if (create.status() === 409) {
      const libs = (await (await request.get("/api/v1/libraries", { headers: auth })).json())
        .libraries as Array<{ id: string; rootFolders: Array<{ path: string }> }>;
      const existing = libs.find((l) => l.rootFolders.some((r) => r.path === FIXTURES));
      expect(existing, "library at fixtures root not found after 409").toBeTruthy();
      libId = existing!.id;
    } else {
      throw new Error(`create: ${create.status()} ${await create.text()}`);
    }

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();

    // Trigger an Enrichment pass against the local TMDB stub.
    const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();
    const result = await enrich.json();
    expect(result.matched, `enrich matched none: ${JSON.stringify(result)}`).toBeGreaterThan(0);

    await request.dispose();
  });

  test("title detail renders enriched overview, genres, cast, content rating + a real logo hero", async ({
    page,
  }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    // "Pinned Movie" enriches by its embedded {tmdb-…} id.
    await page
      .getByTestId("poster-tile")
      .filter({ hasText: "Pinned Movie" })
      .click();

    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    // Enriched descriptive fields from the stub payload.
    await expect(page.getByTestId("detail-overview")).toContainText("dunes and destiny");
    await expect(page.getByTestId("detail-content-rating")).toContainText("PG-13");
    await expect(page.getByTestId("detail-runtime")).toContainText("2h 35m");
    await expect(page.getByTestId("detail-genres")).toContainText("Science Fiction");
    await expect(page.getByTestId("detail-cast")).toContainText("Timothée Chalamet");

    // Logo-hero: the fetched logo stands in for the title text (and the hero
    // shows no poster), and the <img> actually decodes (the stub served a real
    // PNG). The text heading only renders when a title has no logo.
    const img = page.getByTestId("detail-logo");
    await expect(img).toBeVisible();
    await expect
      .poll(async () => img.evaluate((el: HTMLImageElement) => el.naturalWidth))
      .toBeGreaterThan(0);
    await expect(page.getByTestId("detail-title")).toHaveCount(0);
  });
});
