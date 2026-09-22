import { test, expect, type APIRequestContext } from "@playwright/test";
import { cpSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { waitEnrichPass } from "./enrich-wait";

// End-to-end Locked-field loop (external-metadata-enrichment issue 04) against the
// REAL embedded Go server with its TMDB stub (no live network). An Admin hand-edits
// a Title's overview in the BROWSER, the field becomes Locked, a full re-enrich
// leaves the hand-edit intact (the lock wins), and releasing the lock lets the next
// pass refresh it again. We edit "Extras Movie" so we never disturb the "Pinned
// Movie" assertions in enrich.spec.ts (the suite runs serially, workers: 1).
//
// Fixtures: a PRIVATE mkdtempSync copy of `naming` per run (not the shared root
// other specs point a library at), so `--repeat-each` gets a brand-new Library
// every time instead of reusing one via a 409. Reusing the shared library was the
// residue: this spec's own re-enrich calls never waited for the pass they started
// (see enrich.spec.ts's waitEnrichPass, absent here until now), so a repeat could
// still have an EARLIER repeat's pass in flight against the same Library ID; the
// server's per-Library "a pass is already running" dedup (enrich_handlers.go's
// handleEnrich) then answers a later POST with THAT stale pass instead of starting
// a new one, and the stale pass had snapshotted the lock as held, so it never
// rewrote the overview back to the stub value. A fresh Library per run has no
// earlier pass to collide with; waitEnrichPass on every call closes the same race
// within a single run too. The temp dir is removed in afterAll.

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const NAMING = join(repoRoot, "internal", "api", "testdata", "naming");
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
  let fixturesDir = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    baseURLRef = baseURL ?? "";
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    fixturesDir = mkdtempSync(join(tmpdir(), "e2e-locked-fields-"));
    cpSync(NAMING, fixturesDir, { recursive: true });

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: "Enriched Movies", kind: "movie", rootFolders: [fixturesDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
    // The scan runs ASYNCHRONOUSLY (202 → "running"); enrichment only matches
    // Titles that exist when it runs, so wait for the scan to settle first.
    for (let i = 0; i < 100; i++) {
      const st = await (
        await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth })
      ).json();
      if (st.state && st.state !== "running") break;
      await new Promise((r) => setTimeout(r, 50));
    }
    await waitEnrichPass(request, auth, libId);

    await request.dispose();
  });

  test.afterAll(() => {
    if (fixturesDir) rmSync(fixturesDir, { recursive: true, force: true });
  });

  test("edit overview locks it, survives a full re-enrich, then releases back to auto", async ({
    page,
    playwright,
  }) => {
    const request = await playwright.request.newContext({ baseURL: baseURLRef });
    const auth = { Authorization: `Bearer ${token}` };
    // Not waitEnrichPass: its baseline is "a lastPass whose finishedAt differs from
    // the one read just before POSTing", which this test's two BACK-TO-BACK full
    // passes over a 7-title library (fast enough to finish inside the same
    // wall-clock SECOND — finishedAt has no sub-second resolution) can fail even
    // though each POST genuinely started its own pass: `started: true` already says
    // so unambiguously, with no earlier-pass ambiguity to resolve via a timestamp
    // diff (this Library is private to this run — nothing else can be racing it).
    // So: assert `started`, then just poll for `state === "idle"`.
    const reEnrichFull = async () => {
      const start = await request.post(`/api/v1/libraries/${libId}/enrich?mode=full`, { headers: auth });
      const startBody = await start.json();
      expect(
        startBody.started,
        `re-enrich did not start a new pass: ${JSON.stringify(startBody)}`,
      ).toBeTruthy();
      for (let i = 0; i < 150; i++) {
        const st = await (
          await request.get(`/api/v1/libraries/${libId}/enrich`, { headers: auth })
        ).json();
        if (st.state === "idle") return;
        await new Promise((r) => setTimeout(r, 100));
      }
      throw new Error(`re-enrich on library ${libId} did not settle within timeout`);
    };

    await uiLogin(page);
    const openExtrasMovie = async () => {
      await page.goto(`/libraries/${libId}`);
      await expect(page.getByTestId("poster-grid")).toBeVisible();
      await page.getByTestId("poster-tile").filter({ hasText: "Extras Movie" }).click();
      await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    };

    // The hand-edit form lives in the Edit-item dialog's "Details" tab
    // (fix-label): open it, and close it again to read the detail underneath.
    const openDetailsTab = async () => {
      await page.getByTestId("edit-item-button").click();
      await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
      await page.getByTestId("edit-item-tab-fix-label").click();
      await expect(page.getByTestId("fix-label-editor")).toBeVisible();
    };
    const closeDialog = async () => {
      await page.getByTestId("edit-item-close").click();
      await expect(page.getByTestId("edit-item-dialog")).not.toBeVisible();
    };

    await openExtrasMovie();
    // Starts with the stub's enriched overview.
    await expect(page.getByTestId("detail-overview")).toContainText(STUB_OVERVIEW);

    // 1. Hand-edit the overview and save → it locks (badge inside the editor),
    //    and the detail behind the dialog reflects the edit.
    await openDetailsTab();
    await expect(page.getByTestId("lock-badge-overview")).toHaveCount(0);
    await page.getByTestId("edit-overview").fill(HAND_EDIT);
    await page.getByTestId("save-metadata").click();
    await expect(page.getByTestId("lock-badge-overview")).toBeVisible();
    await closeDialog();
    await expect(page.getByTestId("detail-overview")).toContainText(HAND_EDIT);

    // 2. A full re-enrich (which would otherwise rewrite the overview to the stub
    //    value) leaves the LOCKED field untouched.
    await reEnrichFull();
    await openExtrasMovie();
    await expect(page.getByTestId("detail-overview")).toContainText(HAND_EDIT);

    // 3. Release the lock (in the Details tab); the field is no longer pinned.
    await openDetailsTab();
    await expect(page.getByTestId("lock-badge-overview")).toBeVisible();
    await page.getByTestId("release-overview").click();
    await expect(page.getByTestId("lock-badge-overview")).toHaveCount(0);
    await closeDialog();

    // 4. The next full pass refreshes the now-unlocked overview back to the stub.
    await reEnrichFull();
    await openExtrasMovie();
    await expect(page.getByTestId("detail-overview")).toContainText(STUB_OVERVIEW);

    await request.dispose();
  });
});
