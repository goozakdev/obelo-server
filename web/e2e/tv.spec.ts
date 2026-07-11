import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end TV browse + play flow (tv-music issue 01 acceptance criteria)
// against the REAL embedded Go server. As in browse.spec.ts there is no admin UI
// yet, so we SEED via the API (ensure first Admin → login → create a TV library
// at the checked-in `tv` fixtures → synchronous scan), then drive the BROWSER:
// library → Show grid → Show detail (Seasons/Episodes) → open an Episode → play.
//
// We assert: a TV library renders a Show grid (links to /shows/...); the Show
// detail lists Seasons (incl. Specials) and Episodes in order; opening an Episode
// shows its Show/Season context and a Play button; playing advances currentTime
// (real playback through the unchanged player) and records watch state via the
// API.

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const FIXTURES = join(repoRoot, "internal", "api", "testdata", "tv");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-tv" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// seedTV creates (or reuses) a TV library at the fixtures dir and scans it,
// returning the id. Find-or-create so it coexists with other specs that point a
// TV library at the same root (roots can't overlap — 409 FOLDER_OVERLAP).
async function seedTV(request: APIRequestContext, token: string): Promise<string> {
  const auth = { Authorization: `Bearer ${token}` };
  const libsRes = await request.get("/api/v1/libraries", { headers: auth });
  const libs = (await libsRes.json()).libraries as {
    id: string;
    rootFolders?: { path: string }[];
  }[];
  const existing = libs.find((l) => (l.rootFolders ?? []).some((r) => r.path === FIXTURES));
  if (existing) return existing.id;

  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name: "Shows", kind: "tv", rootFolders: [FIXTURES] },
  });
  expect(
    create.ok(),
    `create tv library failed: ${create.status()} ${await create.text()}`,
  ).toBeTruthy();
  const libId = (await create.json()).id as string;

  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();
  let status = await scan.json();
  for (let i = 0; i < 100 && status.state !== "idle"; i++) {
    await new Promise((r) => setTimeout(r, 100));
    const s = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    status = await s.json();
  }
  expect(status.state).toBe("idle");
  expect(status.titlesFound).toBeGreaterThan(1);
  return libId;
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("tv: show grid, seasons/episodes, play", () => {
  let libId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    libId = await seedTV(request, token);
    await request.dispose();
  });

  test("TV library renders a Show grid linking to Show detail", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await expect(page.getByTestId("library-grid-screen")).toHaveAttribute("data-kind", "tv");
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    const bear = page.getByTestId("poster-tile").filter({ hasText: "The Bear" });
    await expect(bear).toBeVisible();
    // The tile links into the Show detail (not a Title).
    await expect(bear.locator("a")).toHaveAttribute("href", /\/shows\//);
    // The Show poster shows its unwatched-episode count (the watched affordance):
    // nothing has been watched in this freshly-seeded library, so the badge is
    // present with a positive count (issue tv-music/04).
    await expect(bear.getByTestId("badge-unwatched-count")).toBeVisible();
  });

  test("Show detail offers a Season picker (incl. Specials) and lists Episodes in order", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "The Bear" }).click();

    await expect(page.getByTestId("show-detail")).toBeVisible();
    await expect(page.getByTestId("show-title")).toContainText("The Bear");

    // The picker offers a Specials option and a Season 1 option; only one Season's
    // block renders at a time (default: the first non-specials Season).
    const select = page.getByTestId("season-select");
    await expect(select.locator("option", { hasText: "Specials" })).toHaveCount(1);
    await expect(select.locator("option", { hasText: "Season 1" })).toHaveCount(1);
    await expect(page.getByTestId("season-block")).toHaveCount(1);

    const rows = page.getByTestId("episode-row");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0).getByTestId("episode-title")).toHaveText("System");
    await expect(rows.nth(0).getByTestId("episode-code")).toHaveText("1");

    // Choosing Specials swaps the list to that Season's Episodes.
    await select.selectOption({ label: "Specials" });
    await expect(page.getByTestId("season-block")).toHaveAttribute(
      "data-season-number",
      "0",
    );
  });

  test("open an Episode → shows Show context → play advances currentTime + records watch state", async ({
    page,
    playwright,
    baseURL,
  }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "The Bear" }).click();
    await expect(page.getByTestId("show-detail")).toBeVisible();

    // The row's three-dots menu → Edit opens the Episode's Title detail (clicking
    // the row itself now plays the Episode instead of navigating).
    const firstRow = page.getByTestId("episode-row").nth(0);
    await firstRow.hover();
    await firstRow.getByTestId("episode-menu-toggle").click();
    await firstRow.getByTestId("episode-menu-edit").click();

    // Episode Title detail: parent context + a Play affordance (playback reused).
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
    await expect(page.getByTestId("episode-context")).toContainText("The Bear");
    await expect(page.getByTestId("episode-context")).toContainText("S01E01");
    const titleUrl = page.url();
    const id = titleUrl.split("/titles/")[1];

    await expect(page.getByTestId("play-button")).toBeEnabled();
    await page.getByTestId("play-button").click();

    // The player renders a <video> bound to a session stream URL and currentTime
    // advances on the short clip — real playback through the unchanged player.
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), {
        timeout: 20000,
      })
      .toBeGreaterThan(0);

    // Watch state recorded via the API (server owns the threshold).
    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    await expect
      .poll(
        async () => {
          const res = await request.get(`/api/v1/titles/${id}`, {
            headers: { Authorization: `Bearer ${token}` },
          });
          const body = await res.json();
          return Boolean(body.watched) || (body.resumePositionMs ?? 0) > 0;
        },
        { timeout: 10000 },
      )
      .toBeTruthy();
    await request.dispose();
  });
});

// The Up Next Home row (tv-music issue 02 acceptance criteria) against the REAL
// embedded server. We SEED watch state through the API (mark The Bear S01E01
// watched), then drive the BROWSER's Home and assert the Up Next row shows the
// NEXT episode (S01E02, "Hands") with its Show/episode parent context — i.e. Up
// Next advanced after an episode was watched.
test.describe.serial("home: TV Up Next row appears and advances", () => {
  let s1EpisodeIds: { e1: string; e2: string } = { e1: "", e2: "" };

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };
    // The first describe block already created a TV library over these fixtures;
    // the server forbids overlapping roots, so reuse it (find the library whose
    // root is the fixtures dir) rather than creating a second.
    const libsRes = await request.get("/api/v1/libraries", { headers: auth });
    const libs = (await libsRes.json()).libraries as {
      id: string;
      rootFolders?: { path: string }[];
    }[];
    const existing = libs.find((l) => (l.rootFolders ?? []).some((r) => r.path === FIXTURES));
    const libId = existing ? existing.id : await seedTV(request, token);

    // Resolve The Bear → Season 1 → its two episodes (System=E01, Hands=E02).
    const showsRes = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, {
      headers: auth,
    });
    const shows = (await showsRes.json()).shows as { id: string; title: string }[];
    const bear = shows.find((s) => s.title === "The Bear");
    expect(bear, "The Bear show seeded").toBeTruthy();

    const seasonsRes = await request.get(`/api/v1/shows/${bear!.id}/seasons`, { headers: auth });
    const seasons = (await seasonsRes.json()).seasons as { id: string; seasonNumber: number }[];
    const s1 = seasons.find((s) => s.seasonNumber === 1);
    expect(s1, "The Bear Season 1 seeded").toBeTruthy();

    const epsRes = await request.get(`/api/v1/seasons/${s1!.id}/episodes`, { headers: auth });
    const eps = (await epsRes.json()).episodes as { id: string; episodeNumber: number }[];
    const e1 = eps.find((e) => e.episodeNumber === 1)!;
    const e2 = eps.find((e) => e.episodeNumber === 2)!;
    s1EpisodeIds = { e1: e1.id, e2: e2.id };

    // Mark E01 watched manually → the Show is started and Up Next advances to E02.
    const put = await request.put(`/api/v1/titles/${e1.id}/watchState`, {
      headers: auth,
      data: { watched: true },
    });
    expect(put.ok(), `watchState failed: ${put.status()} ${await put.text()}`).toBeTruthy();

    await request.dispose();
  });

  test("Up Next shows the next episode after one is watched", async ({ page }) => {
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    // The TV-only Up Next row is present (a Show is in progress).
    const up = page.getByTestId("home-up-next");
    await expect(up).toBeVisible();

    // It surfaces E02 ("Hands") — the next unwatched episode — with parent context.
    const next = up.getByTestId("poster-tile").filter({ hasText: "Hands" });
    await expect(next).toBeVisible();
    await expect(next.getByTestId("poster-context")).toContainText("The Bear");
    await expect(next.getByTestId("poster-context")).toContainText("S01E02");
    // The card links to the Episode's detail/play page.
    await expect(next.getByRole("link").first()).toHaveAttribute(
      "href",
      `/titles/${s1EpisodeIds.e2}`,
    );
  });
});

// The Show detail page's resume point (issue 02 acceptance criteria, ADR-0028)
// against the REAL embedded server: SEED watch state (mark The Bear S01E01 watched
// through the API), then drive the BROWSER to the Show detail and assert the hero
// leads with the next-episode block (S01E02 "Hands") + a Play that starts from it.
test.describe.serial("show detail: resume point block", () => {
  let bearId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };
    const libId = await seedTV(request, token);

    // Resolve The Bear → Season 1 → E01 (System), and mark it watched so the resume
    // point advances to the next unwatched Episode (S01E02).
    const showsRes = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, { headers: auth });
    const shows = (await showsRes.json()).shows as { id: string; title: string }[];
    const bear = shows.find((s) => s.title === "The Bear")!;
    expect(bear, "The Bear show seeded").toBeTruthy();
    bearId = bear.id;

    const seasonsRes = await request.get(`/api/v1/shows/${bearId}/seasons`, { headers: auth });
    const seasons = (await seasonsRes.json()).seasons as { id: string; seasonNumber: number }[];
    const s1 = seasons.find((s) => s.seasonNumber === 1)!;
    const epsRes = await request.get(`/api/v1/seasons/${s1.id}/episodes`, { headers: auth });
    const eps = (await epsRes.json()).episodes as { id: string; episodeNumber: number }[];
    const e1 = eps.find((e) => e.episodeNumber === 1)!;
    const put = await request.put(`/api/v1/titles/${e1.id}/watchState`, {
      headers: auth,
      data: { watched: true },
    });
    expect(put.ok(), `watchState failed: ${put.status()} ${await put.text()}`).toBeTruthy();

    await request.dispose();
  });

  test("the Show detail leads with the resume point and plays from it", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/shows/${bearId}`);
    await expect(page.getByTestId("show-detail")).toBeVisible();

    // The hero's next-episode block surfaces the next unwatched Episode after the
    // watched E01 = S01E02 ("Hands"), with a single Play (the anchor is watched).
    const block = page.getByTestId("resume-point");
    await expect(block).toBeVisible();
    await expect(block).toHaveAttribute("data-mode", "next");
    await expect(page.getByTestId("resume-point-code")).toHaveText("S01E02");
    await expect(page.getByTestId("resume-point-title")).toHaveText("Hands");

    // Play from the resume point starts real playback in the persistent bar.
    await page.getByTestId("resume-play-button").click();
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), {
        timeout: 20000,
      })
      .toBeGreaterThan(0);
  });
});
