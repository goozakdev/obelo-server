import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end album Fix-info SEARCH relevance (item-editing/search-improvements)
// against the REAL embedded Go server with its MusicBrainz stub. Regression guard
// for the phrase-query fix: our album search used to send an exact-phrase
// `releasegroup:"<query>"`, so a folder/descriptor-carrying query like
// "Anastasia Soundtrack" matched NOTHING (the canonical release-group title is just
// "Anastasia", secondary-type Soundtrack). The fix sends relevance-ranked terms.
//
// The boot-server MusicBrainz stub mimics real MusicBrainz for the Anastasia case:
// bare relevance terms FIND the soundtrack; an exact-phrase query returns empty. So
// this spec fails if the phrase fix ever regresses — the candidate would vanish.
// It also asserts the type-hint badge ("Album · Soundtrack") the same slice added.
//
// It seeds its OWN unique temp MUSIC library so it never collides with the shared
// music libraries other specs own (mirroring enrich-override-parent.spec.ts).

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const MUSIC = join(repoRoot, "internal", "api", "testdata", "music");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-album-relevance" },
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

test.describe.serial("album Fix-info: relevance-ranked search finds a descriptor-named album", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Unique temp MUSIC fixtures: one Artist (Radiohead) with one Album (OK Computer),
    // copied from the Go music testdata, so the browse grid has an album whose Fix-info
    // picker we can drive. Scan only — the picker is an Admin affordance on any album,
    // so enrichment isn't needed to reach it.
    fixturesDir = mkdtempSync(join(tmpdir(), "juicebox-album-relevance-"));
    const albumDir = join(fixturesDir, "Radiohead", "OK Computer (1997)");
    mkdirSync(albumDir, { recursive: true });
    for (const f of ["01 - Airbag.m4a", "02 - Paranoid Android.m4a"]) {
      cpSync(join(MUSIC, "Radiohead", "OK Computer (1997)", f), join(albumDir, f));
    }

    const create = await request.post("/api/v1/libraries", {
      headers: auth,
      data: { name: `Album Relevance E2E ${Date.now()}`, kind: "music", rootFolders: [fixturesDir] },
    });
    expect(create.ok(), `create: ${create.status()} ${await create.text()}`).toBeTruthy();
    libId = (await create.json()).id as string;

    const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    expect(scan.ok(), `scan: ${scan.status()}`).toBeTruthy();
    // The scan is asynchronous (202 → "running"); wait for it to settle before driving
    // the browser so the album is browsable.
    const deadline = Date.now() + 15_000;
    while (Date.now() < deadline) {
      const st = await (
        await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth })
      ).json();
      if (st.state && st.state !== "running") {
        expect(st.titlesFound, `scan found no titles: ${JSON.stringify(st)}`).toBeGreaterThan(0);
        break;
      }
      await new Promise((r) => setTimeout(r, 100));
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

  test("searching a descriptor-carrying query surfaces the album with a type badge", async ({
    page,
  }) => {
    await uiLogin(page);

    // Navigate music grid → Radiohead artist → OK Computer album detail.
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "Radiohead" }).first().click();
    await expect(page.getByTestId("artist-detail")).toBeVisible();
    await page.getByTestId("poster-tile").filter({ hasText: "OK Computer" }).first().click();
    await expect(page.getByTestId("album-detail")).toBeVisible();

    // The album Fix-info picker: clear the pre-filled artist scope (the Anastasia
    // soundtrack is not a Radiohead release) and search a folder-style query whose
    // descriptor word ("Soundtrack") is NOT part of the canonical release-group title.
    // Open the "Edit item" dialog and select the Search tab (ADR-0019 unified Search).
    await page.getByTestId("edit-item-button").click();
    await expect(page.getByTestId("edit-item-dialog")).toBeVisible();
    await page.getByTestId("edit-item-tab-search").click();

    const picker = page.getByTestId("entity-enrichment-override-picker");
    await expect(picker).toBeVisible();
    await picker.getByTestId("entity-enrichment-artist-input").fill("");
    await picker.getByTestId("entity-enrichment-search-input").fill("Anastasia Soundtrack");
    await picker.getByTestId("entity-enrichment-search-button").click();

    // With relevance-ranked terms the stub returns the canonical "Anastasia"
    // release-group; an exact-phrase query would have returned zero (the regressed
    // path shows the no-candidates message instead).
    const candidate = picker.getByTestId("entity-enrichment-candidate").first();
    await expect(candidate).toBeVisible();
    await expect(candidate.getByTestId("entity-enrichment-candidate-title")).toContainText(
      "Anastasia",
    );
    // The type-hint badge (item-editing/search-improvements) disambiguates same-titled
    // hits — here it marks the soundtrack.
    await expect(candidate.getByTestId("entity-enrichment-candidate-type")).toContainText(
      "Soundtrack",
    );
  });
});
