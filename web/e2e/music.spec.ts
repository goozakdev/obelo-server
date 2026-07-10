import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end Music browse + play flow (tv-music issue 03 acceptance criteria)
// against the REAL embedded Go server. As in tv.spec.ts there is no admin UI yet,
// so we SEED via the API (ensure first Admin → login → create a Music library at
// the checked-in `music` fixtures → synchronous scan), then drive the BROWSER:
// library → Artist list → Artist detail (Albums) → Album detail (Tracks) → open a
// Track → play.
//
// We assert: a Music library renders an Artist list (links to /artists/...); the
// Artist detail lists Albums (links to /albums/...); the Album detail lists Tracks
// in order; opening a Track shows its Artist/Album context and a Play button;
// playing advances currentTime (real playback through the unchanged player) and
// records watch state via the API. The FLAC→AAC transcode case is asserted via
// the API (the negotiated tier is transcode with a fetchable audio HLS) since the
// browser's codec support for raw FLAC is engine-dependent.

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const FIXTURES = join(repoRoot, "internal", "api", "testdata", "music");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-music" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// seedMusic creates a Music library at the fixtures dir and scans it, reusing an
// existing library over the same root if one is already present (roots may not
// overlap), returning the id.
async function seedMusic(request: APIRequestContext, token: string): Promise<string> {
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
    data: { name: "Music", kind: "music", rootFolders: [FIXTURES] },
  });
  expect(
    create.ok(),
    `create music library failed: ${create.status()} ${await create.text()}`,
  ).toBeTruthy();
  const libId = (await create.json()).id as string;

  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();

  // The scan runs ASYNCHRONOUSLY: the POST returns 202 with state "running". Poll
  // the pollable scan status until it settles (mirrors the Go harness's
  // waitScanSettled), then assert it found titles.
  const settled = await waitScanSettled(request, token, libId);
  expect(settled.state, `scan did not settle: ${JSON.stringify(settled)}`).not.toBe("running");
  expect(settled.titlesFound).toBeGreaterThan(1);
  return libId;
}

// waitScanSettled polls GET /libraries/{id}/scan until the Library leaves the
// "running" state (or it times out), returning the settled status.
async function waitScanSettled(
  request: APIRequestContext,
  token: string,
  libId: string,
): Promise<{ state: string; titlesFound: number }> {
  const auth = { Authorization: `Bearer ${token}` };
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

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

test.describe.serial("music: artist list, albums, tracks, play", () => {
  let libId = "";

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    libId = await seedMusic(request, token);
    await request.dispose();
  });

  test("Music library renders an Artist list linking to Artist detail", async ({ page }) => {
    await uiLogin(page);
    // A music library belongs to the dedicated music experience: the shared
    // /libraries/{id} URL redirects to /music/libraries/{id} (old links keep
    // working) and renders the Artist list inside the music shell.
    await page.goto(`/libraries/${libId}`);
    await expect(page).toHaveURL(/\/music\/libraries\//);
    await expect(page.getByTestId("music-library-screen")).toBeVisible();
    await expect(page.getByTestId("poster-grid")).toBeVisible();

    const radio = page.getByTestId("poster-tile").filter({ hasText: "Radiohead" });
    await expect(radio).toBeVisible();
    await expect(radio.locator("a")).toHaveAttribute("href", /\/artists\//);
  });

  test("Artist detail lists Albums; Album detail lists Tracks in order", async ({ page }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "Radiohead" }).click();

    await expect(page.getByTestId("artist-detail")).toBeVisible();
    await expect(page.getByTestId("artist-name")).toContainText("Radiohead");

    // Open OK Computer → its track list in disc/track order.
    await page.getByTestId("poster-tile").filter({ hasText: "OK Computer" }).click();
    await expect(page.getByTestId("album-detail")).toBeVisible();
    await expect(page.getByTestId("album-title")).toContainText("OK Computer");

    // Album header: artist name links to the Artist detail; year below it.
    const artistLink = page.getByTestId("album-artist").getByRole("link");
    await expect(artistLink).toHaveText("Radiohead");
    await expect(artistLink).toHaveAttribute("href", /\/music\/artists\//);
    await expect(page.getByTestId("album-year")).toHaveText("1997");

    const rows = page.getByTestId("track-row");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0).getByTestId("track-title")).toHaveText("Airbag");
    await expect(rows.nth(0).getByTestId("track-number")).toHaveText("1");
    await expect(rows.nth(1).getByTestId("track-title")).toHaveText("Paranoid Android");

    // Each row shows the track length (mm:ss) and an artist link to the artist view;
    // the title links to the track view.
    await expect(rows.nth(0).getByTestId("track-length")).toHaveText(/^\d+:\d{2}$/);
    await expect(rows.nth(0).getByTestId("track-artist")).toHaveText("Radiohead");
    await expect(rows.nth(0).getByTestId("track-artist")).toHaveAttribute(
      "href",
      /\/music\/artists\//,
    );
    await expect(rows.nth(0).getByTestId("track-open")).toHaveAttribute(
      "href",
      /\/music\/tracks\//,
    );
    // The three-dots actions menu opens with its four items.
    await rows.nth(0).getByTestId("track-menu-toggle").click();
    await expect(rows.nth(0).getByTestId("track-menu-play-next")).toBeVisible();
    await expect(rows.nth(0).getByTestId("track-menu-add-queue")).toBeVisible();
    await expect(rows.nth(0).getByTestId("track-menu-edit")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(rows.nth(0).getByTestId("track-menu")).toHaveCount(0);

    // Playing from a row reflects in the row (→ pause control) and starts the bar,
    // via the shared playback transport.
    await rows.nth(0).getByTestId("track-play").click();
    await expect(page.getByTestId("now-playing-bar")).toBeVisible();
    await expect(rows.nth(0).getByTestId("track-play")).toHaveAttribute(
      "aria-label",
      /^Pause /,
    );
  });

  test("open a Track → shows Artist/Album context → play advances currentTime + records watch state", async ({
    page,
    playwright,
    baseURL,
  }) => {
    await uiLogin(page);
    await page.goto(`/libraries/${libId}`);
    await page.getByTestId("poster-tile").filter({ hasText: "Radiohead" }).click();
    await page.getByTestId("poster-tile").filter({ hasText: "OK Computer" }).click();
    await expect(page.getByTestId("album-detail")).toBeVisible();

    await page.getByTestId("track-row").nth(0).getByTestId("track-open").click();

    // Track detail (music): Artist/Album context + a Play affordance (playback reused).
    await expect(page.getByTestId("track-detail-screen")).toBeVisible();
    await expect(page.getByTestId("track-context")).toContainText("Radiohead");
    await expect(page.getByTestId("track-context")).toContainText("OK Computer");
    const titleUrl = page.url();
    const id = titleUrl.split("/music/tracks/")[1];

    await expect(page.getByTestId("play-button")).toBeEnabled();
    await page.getByTestId("play-button").click();

    // The player renders a <video> bound to a session stream URL and currentTime
    // advances on the short clip — real playback through the unchanged player
    // (an audio-only Track negotiates direct play / remux / transcode → HLS).
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

  test("a FLAC Track negotiates a transcode to an audio AAC HLS rendition", async ({
    playwright,
    baseURL,
  }) => {
    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const auth = { Authorization: `Bearer ${token}` };

    // Resolve Radiohead → the Lossless Single album → its FLAC track.
    const artistsRes = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, {
      headers: auth,
    });
    const artists = (await artistsRes.json()).artists as { id: string; name: string }[];
    const radio = artists.find((a) => a.name === "Radiohead");
    expect(radio, "Radiohead artist seeded").toBeTruthy();

    const albumsRes = await request.get(`/api/v1/artists/${radio!.id}/albums`, { headers: auth });
    const albums = (await albumsRes.json()).albums as { id: string; title: string }[];
    const lossless = albums.find((a) => a.title === "Lossless Single");
    expect(lossless, "Lossless Single album seeded").toBeTruthy();

    const tracksRes = await request.get(`/api/v1/albums/${lossless!.id}/tracks`, { headers: auth });
    const tracks = (await tracksRes.json()).tracks as { id: string; title: string }[];
    expect(tracks.length).toBe(1);

    // A profile that supports aac but not flac → the FLAC must transcode to AAC.
    const dec = await request.post(`/api/v1/titles/${tracks[0].id}/playback`, {
      headers: auth,
      data: {
        deviceProfile: { containers: ["mp4", "fmp4"], audioCodecs: ["aac"], maxAudioChannels: 8 },
        constraints: { maxBitrate: 100000000 },
      },
    });
    expect(dec.ok(), `playback failed: ${dec.status()} ${await dec.text()}`).toBeTruthy();
    const decision = await dec.json();
    expect(decision.tier).toBe("transcode");
    expect(String(decision.streamUrl)).toContain("/hls/");

    // The HLS playlist is fetchable (audio-only HLS is valid).
    const pl = await request.get(decision.streamUrl, { headers: auth });
    expect(pl.ok(), `playlist fetch failed: ${pl.status()}`).toBeTruthy();
    const manifest = await pl.text();
    expect(manifest).toContain("#EXTM3U");

    // Release the transcode session so its slot is freed — the boot-server caps
    // concurrent transcodes at 1, and the session reaper is disabled in E2E, so a
    // leaked transcode session would starve other specs' transcode-requiring
    // titles (a shared embedded server across all specs).
    await request.delete(`/api/v1/sessions/${decision.sessionId}`, { headers: auth });

    await request.dispose();
  });
});
