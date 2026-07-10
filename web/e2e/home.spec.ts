import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Home flow (issue 05 acceptance criteria) against the REAL embedded
// Go server. As in browse.spec.ts / play.spec.ts there is no admin UI yet, so we
// SEED via the API (ensure first Admin → login → create a Movie library at the
// checked-in `movies` fixtures → synchronous scan), then SEED WATCH STATE by
// starting playback and reporting progress, then drive the BROWSER.
//
// The server computes Home: Continue Watching = titles with a resume position in
// the 2%–90% band, most-recent first; a title played past 90% is marked watched
// and drops OUT. So we:
//   - start playback on "Dune" and report ~40% of its duration → it lands in
//     Continue Watching with a resume marker,
//   - start playback on "Sample Movie" and report past 90% → marked watched →
//     absent from Continue Watching,
// then load Home and assert Continue Watching shows Dune (not Sample) and
// Recently Added lists the freshly-scanned titles. A separate test on a fresh
// empty library asserts both rows render their empty states.

const here = dirname(fileURLToPath(import.meta.url));
// here is web/e2e, so the repo root is two levels up (web/e2e → web → repo).
const repoRoot = resolve(here, "..", "..");
// We seed from the `naming` fixtures (the same dir browse.spec.ts uses) rather
// than `movies` (owned by play.spec.ts): the server enforces a no-overlap rule
// on library root folders, so two specs can't each create a library over the
// same dir. seedMovies below is idempotent — on a FOLDER_OVERLAP it reuses the
// library browse.spec.ts already created over this dir — so whichever spec runs
// first owns it and the rest reuse it. The `naming` fixtures include playable
// mp4 titles with real durations ("Extras Movie", "Pinned Movie", "Yearless
// Movie", "Cut Movie", "Split Movie"), which we need to anchor the resume band.
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

async function login(request: APIRequestContext, clientId: string): Promise<string> {
  const res = await request.post("/api/v1/auth/login", {
    data: {
      username: ADMIN_USER,
      password: ADMIN_PASS,
      device: { name: "seed", platform: "test", clientId },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// A permissive device profile so the mp4 fixtures direct-play (so a real session
// + stream-backed duration is available to anchor the resume band against).
const DEVICE_PROFILE = {
  containers: ["mp4", "mkv", "fmp4"],
  videoCodecs: [
    { codec: "h264", maxLevel: "5.2", maxResolution: "2160p" },
    { codec: "hevc", maxResolution: "2160p" },
    { codec: "mpeg4", maxResolution: "2160p" },
  ],
  audioCodecs: ["aac", "ac3", "eac3", "flac", "mp3"],
  maxAudioChannels: 8,
  textSubtitleFormats: ["webvtt"],
};

async function seedMovies(
  request: APIRequestContext,
  token: string,
  name: string,
  roots: string[],
): Promise<{ libId: string; titles: Record<string, string> }> {
  const auth = { Authorization: `Bearer ${token}` };
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name, kind: "movie", rootFolders: roots },
  });
  let libId: string;
  if (create.ok()) {
    libId = (await create.json()).id as string;
  } else if (create.status() === 409) {
    // Another spec (browse.spec.ts) already created a library over this root
    // (the server forbids overlap). Reuse it — find the library whose root
    // matches one we asked for.
    const libs = await (await request.get("/api/v1/libraries", { headers: auth })).json();
    const match = (libs.libraries ?? []).find((l: { rootFolders?: { path: string }[] }) =>
      (l.rootFolders ?? []).some((r) => roots.includes(r.path)),
    );
    if (!match) throw new Error(`409 on create but no existing library covers ${roots.join(",")}`);
    libId = match.id as string;
  } else {
    throw new Error(`create library failed: ${create.status()} ${await create.text()}`);
  }

  // The scan runs ASYNCHRONOUSLY (202 → "running"), so poll until it settles
  // (same pattern as enrich-tv-music.spec.ts) — the titles page below needs the
  // scanned Titles to exist.
  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();
  let status = (await scan.json()) as { state: string };
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline && status.state === "running") {
    await new Promise((r) => setTimeout(r, 100));
    const s = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    if (s.ok()) status = (await s.json()) as typeof status;
  }
  expect(status.state).toBe("idle");

  const pageRes = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, {
    headers: auth,
  });
  const body = await pageRes.json();
  const titles: Record<string, string> = {};
  for (const t of body.titles ?? []) titles[t.title as string] = t.id as string;
  return { libId, titles };
}

// titleDurationMs reads the chosen File's duration from the title detail, so we
// can report a position that lands inside (or beyond) the server's resume band.
async function titleDurationMs(
  request: APIRequestContext,
  token: string,
  titleId: string,
): Promise<number> {
  const res = await request.get(`/api/v1/titles/${titleId}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  const body = await res.json();
  for (const ed of body.editions ?? []) {
    for (const f of ed.files ?? []) {
      if ((f.durationMs ?? 0) > 0) return f.durationMs as number;
    }
  }
  return 0;
}

// reportProgressAt starts a playback session on the title and posts a single
// progress report at positionMs, returning the server-resolved watch state.
async function reportProgressAt(
  request: APIRequestContext,
  token: string,
  titleId: string,
  positionMs: number,
): Promise<{ resumePositionMs: number; watched: boolean }> {
  const auth = { Authorization: `Bearer ${token}` };
  const decision = await request.post(`/api/v1/titles/${titleId}/playback`, {
    headers: auth,
    data: { deviceProfile: DEVICE_PROFILE, constraints: {}, startPosition: 0 },
  });
  expect(
    decision.ok(),
    `playback negotiate failed: ${decision.status()} ${await decision.text()}`,
  ).toBeTruthy();
  const sessionId = (await decision.json()).sessionId as string;

  const prog = await request.post(`/api/v1/sessions/${sessionId}/progress`, {
    headers: auth,
    data: { positionMs, state: "paused" },
  });
  expect(prog.ok(), `progress failed: ${prog.status()} ${await prog.text()}`).toBeTruthy();
  const out = await prog.json();
  await request.delete(`/api/v1/sessions/${sessionId}`, {
    headers: auth,
    data: { positionMs },
  });
  return { resumePositionMs: out.resumePositionMs ?? 0, watched: Boolean(out.watched) };
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("home: continue watching + recently added", () => {
  let titles: Record<string, string> = {};

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request, "e2e-seed-home");
    // "Home Films" name is ignored on the reuse path (browse.spec may own it).
    ({ titles } = await seedMovies(request, token, "Home Films", [FIXTURES]));
    expect(Object.keys(titles).length, "titles seeded").toBeGreaterThan(0);

    const find = (...cands: string[]): string => {
      for (const name of Object.keys(titles)) {
        if (cands.some((c) => name.includes(c))) return titles[name];
      }
      throw new Error(`no seeded title matched ${cands.join("/")}: ${Object.keys(titles).join(", ")}`);
    };

    // "Extras Movie" (a playable mp4 with artwork) → report ~40% so it lands in
    // Continue Watching with a resume marker.
    const inProgressId = find("Extras Movie");
    const inProgressDur = await titleDurationMs(request, token, inProgressId);
    expect(inProgressDur, "in-progress title has a measured duration").toBeGreaterThan(0);
    const cw = await reportProgressAt(request, token, inProgressId, Math.round(inProgressDur * 0.4));
    expect(cw.watched, "mid-band title is not watched").toBeFalsy();
    expect(cw.resumePositionMs, "in-progress title has a resume position").toBeGreaterThan(0);

    // "Pinned Movie" (a playable mp4) → report past 90% so the server marks it
    // watched → absent from Continue Watching.
    const watchedId = find("Pinned Movie");
    const watchedDur = await titleDurationMs(request, token, watchedId);
    expect(watchedDur, "watched title has a measured duration").toBeGreaterThan(0);
    const watched = await reportProgressAt(request, token, watchedId, Math.round(watchedDur * 0.95));
    expect(watched.watched, "title past 90% is watched").toBeTruthy();

    await request.dispose();
  });

  test("Home shows the in-progress title in Continue Watching (the watched one is absent) and lists Recently Added", async ({
    page,
  }) => {
    await uiLogin(page);
    // Home is the post-login landing.
    await expect(page.getByTestId("home-screen")).toBeVisible();

    const cw = page.getByTestId("home-continue-watching");
    await expect(cw).toBeVisible();
    // "Extras Movie" (mid-band) is present with a resume affordance.
    const inProgress = cw.getByTestId("poster-tile").filter({ hasText: "Extras Movie" });
    await expect(inProgress).toBeVisible();
    await expect(inProgress.getByTestId("badge-resume")).toBeVisible();
    // "Pinned Movie" was watched past the threshold → not in Continue Watching.
    await expect(
      cw.getByTestId("poster-tile").filter({ hasText: "Pinned Movie" }),
    ).toHaveCount(0);

    // Recently Added lists the freshly-scanned titles (Extras Movie among them).
    const ra = page.getByTestId("home-recently-added");
    await expect(ra).toBeVisible();
    await expect(ra.getByTestId("poster-tile").first()).toBeVisible();
    await expect(
      ra.getByTestId("poster-tile").filter({ hasText: "Extras Movie" }),
    ).toBeVisible();

    // A card links to the title's detail page (reuses the grid's PosterTile).
    await inProgress.getByRole("link").first().click();
    await expect(page.getByTestId("title-detail-screen")).toBeVisible();
  });
});

test.describe("home: empty-state rendering matches the API", () => {
  // The E2E server boots against a fresh temp data dir, but specs accumulate
  // titles into a shared catalog within a run, so we can't force Recently Added
  // empty mid-suite. Instead we assert the UI is FAITHFUL to the server's /home:
  // each row shows its cards iff the API returned items, and its empty state iff
  // the API returned none. That deterministically exercises the empty-state path
  // for whichever row the server reports empty (Continue Watching is per-user, so
  // a freshly-logged-in browser session with no in-progress titles yields an
  // empty Continue Watching unless this run seeded one). The pure both-rows-empty
  // path is also covered deterministically by the component tests.
  test("each Home row shows cards or its empty state, matching GET /home", async ({
    page,
  }) => {
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    // Ground truth from the API, using the browser's stored token.
    const home = await page.evaluate(async () => {
      const token = window.localStorage.getItem("juicebox.token");
      const res = await fetch("/api/v1/home", {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      });
      return (await res.json()) as {
        continueWatching: unknown[];
        recentlyAdded: unknown[];
      };
    });

    const cwEmpty = (home.continueWatching ?? []).length === 0;
    const raEmpty = (home.recentlyAdded ?? []).length === 0;

    if (cwEmpty) {
      await expect(page.getByTestId("home-continue-watching-empty")).toBeVisible();
    } else {
      await expect(
        page.getByTestId("home-continue-watching").getByTestId("poster-tile").first(),
      ).toBeVisible();
    }

    if (raEmpty) {
      await expect(page.getByTestId("home-recently-added-empty")).toBeVisible();
    } else {
      await expect(
        page.getByTestId("home-recently-added").getByTestId("poster-tile").first(),
      ).toBeVisible();
    }
  });
});
