import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Locked-field loop (external-metadata-enrichment issue 04) against the
// REAL embedded Go server with its TMDB stub (no live network). An Admin hand-edits
// a Title's overview in the BROWSER, the field becomes Locked, a full re-enrich
// leaves the hand-edit intact (the lock wins), and releasing the lock lets the next
// pass refresh it again. We edit "Extras Movie" so we never disturb the "Pinned
// Movie" assertions in enrich.spec.ts (the suite runs serially, workers: 1).

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const FIXTURES = join(repoRoot, "internal", "api", "testdata", "naming");
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

// The stub serves this overview for every movie; a hand-edit must differ from it
// so the lock-vs-refresh behavior is observable.
const STUB_OVERVIEW = "dunes and destiny";
const HAND_EDIT = "My hand-written summary that the stub will never produce.";

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-locks" },
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

test.describe.serial("locked fields: hand-edit survives re-enrich, releasable", () => {
  let libId = "";
  let token = "";
  let baseURLRef = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    baseURLRef = baseURL ?? "";
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Find-or-create a Movie library at the naming fixtures (shared with other
    // enrichment specs; reuse on a 409 since roots can't overlap).
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
    const enrich = await request.post(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    expect(enrich.ok(), `enrich: ${enrich.status()} ${await enrich.text()}`).toBeTruthy();

    await request.dispose();
  });

  test("edit overview locks it, survives a full re-enrich, then releases back to auto", async ({
    page,
    playwright,
  }) => {
    const request = await playwright.request.newContext({ baseURL: baseURLRef });
    const auth = { Authorization: `Bearer ${token}` };
    const reEnrichFull = async () => {
      const r = await request.post(`/api/v1/libraries/${libId}/enrich?mode=full`, { headers: auth });
      expect(r.ok(), `re-enrich: ${r.status()} ${await r.text()}`).toBeTruthy();
    };

    await uiLogin(page);
    const openExtrasMovie = async () => {
      await page.goto(`/libraries/${libId}`);
      await expect(page.getByTestId("poster-grid")).toBeVisible();
      await page.getByTestId("poster-tile").filter({ hasText: "Extras Movie" }).click();
      await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    };

    await openExtrasMovie();
    // Starts with the stub's enriched overview, no lock.
    await expect(page.getByTestId("detail-overview")).toContainText(STUB_OVERVIEW);
    await expect(page.getByTestId("lock-badge-overview")).toHaveCount(0);

    // 1. Hand-edit the overview and save → it locks.
    await page.getByTestId("edit-overview").fill(HAND_EDIT);
    await page.getByTestId("save-overview").click();
    await expect(page.getByTestId("lock-badge-overview")).toBeVisible();
    await expect(page.getByTestId("detail-overview")).toContainText(HAND_EDIT);

    // 2. A full re-enrich (which would otherwise rewrite the overview to the stub
    //    value) leaves the LOCKED field untouched.
    await reEnrichFull();
    await openExtrasMovie();
    await expect(page.getByTestId("detail-overview")).toContainText(HAND_EDIT);
    await expect(page.getByTestId("lock-badge-overview")).toBeVisible();

    // 3. Release the lock; the field is no longer pinned.
    await page.getByTestId("release-overview").click();
    await expect(page.getByTestId("lock-badge-overview")).toHaveCount(0);

    // 4. The next full pass refreshes the now-unlocked overview back to the stub.
    await reEnrichFull();
    await openExtrasMovie();
    await expect(page.getByTestId("detail-overview")).toContainText(STUB_OVERVIEW);
    await expect(page.getByTestId("lock-badge-overview")).toHaveCount(0);

    await request.dispose();
  });
});
