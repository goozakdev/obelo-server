import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Regression for the "music never plays — video player + endless spinner" bug.
//
// A FLAC Track that the browser reports it can decode (Chrome/Firefox claim FLAC
// support) but whose container the browser cannot demux used to negotiate the
// directStream (remux) tier. Remux copies the audio verbatim into the HLS segment
// container (MPEG-TS) — but MPEG-TS cannot carry FLAC, so it muxed as an
// unplayable "private data stream" and playback hung forever
// (DEMUXER_ERROR_COULD_NOT_PARSE). The fix forces a codec MPEG-TS cannot carry to
// the transcode tier (FLAC→AAC) instead, which plays.
//
// The existing music.spec.ts only plays the m4a/aac track in the browser (which
// DIRECT-PLAYS) and exercises FLAC via the API only — so this audio-only HLS path
// through the player was never covered in a real browser. This spec opens the FLAC
// track in Chromium and asserts currentTime advances (real decode), which it could
// not do before the fix.

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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-audiohls" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

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
  expect(create.ok(), `create music library failed: ${create.status()}`).toBeTruthy();
  const libId = (await create.json()).id as string;
  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan failed: ${scan.status()}`).toBeTruthy();
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

test("FLAC track plays audio-only HLS in the browser (currentTime advances)", async ({
  page,
  playwright,
  baseURL,
}) => {
  const request = await playwright.request.newContext({ baseURL });
  await ensureAdmin(request);
  const token = await login(request);
  const libId = await seedMusic(request, token);

  // Resolve the FLAC track id (the scan is async — poll until artists appear).
  const auth = { Authorization: `Bearer ${token}` };
  let artists: { id: string; name: string }[] = [];
  for (let i = 0; i < 60; i++) {
    const r = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, { headers: auth });
    artists = ((await r.json()).artists ?? []) as { id: string; name: string }[];
    if (artists.some((a) => a.name === "Radiohead")) break;
    await new Promise((res) => setTimeout(res, 250));
  }
  const radio = artists.find((a) => a.name === "Radiohead");
  expect(radio, "Radiohead artist seeded").toBeTruthy();
  const albums = (await (await request.get(`/api/v1/artists/${radio!.id}/albums`, { headers: auth })).json())
    .albums as { id: string; title: string }[];
  const lossless = albums.find((a) => a.title === "Lossless Single")!;
  const tracks = (await (await request.get(`/api/v1/albums/${lossless.id}/tracks`, { headers: auth })).json())
    .tracks as { id: string }[];
  const flacId = tracks[0].id;
  await request.dispose();

  await uiLogin(page);
  // Play the way a user does: the old /titles/{id}/play route was retired with
  // the standalone PlayerScreen (it now redirects Home) — playback is driven
  // from the Track detail's Play button into the persistent Now Playing bar.
  await page.goto(`/music/tracks/${flacId}`);
  await expect(page.getByTestId("detail")).toBeVisible();
  await expect(page.getByTestId("play-button")).toBeEnabled();
  await page.getByTestId("play-button").click();

  // The player renders a <video> for the HLS audio tier (not the unsupported
  // dead-end). The FLAC must transcode (FLAC→AAC) — a remux into MPEG-TS would be
  // unplayable — so the negotiated tier is transcode, never directStream.
  const video = page.getByTestId("player-video");
  await expect(video).toBeVisible();
  await expect(page.getByTestId("player-unsupported")).toHaveCount(0);
  await expect(video).toHaveAttribute("data-tier", "transcode");

  // The crux: playback actually starts (real decode), not an endless spinner.
  await expect
    .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
    .toBeGreaterThan(0);

  // And no media error surfaced (the old bug set error code 4,
  // DEMUXER_ERROR_COULD_NOT_PARSE, on the FLAC-in-MPEG-TS private data stream).
  const mediaError = await video.evaluate((el: HTMLVideoElement) =>
    el.error ? el.error.code : null,
  );
  expect(mediaError, "no media decode error").toBeNull();
});
