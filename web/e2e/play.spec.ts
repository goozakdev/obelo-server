import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// End-to-end playback flow (issue 04 + TRANSCODE issue 05 acceptance criteria)
// against the REAL embedded Go server. As in browse.spec.ts there is no admin UI
// yet, so we SEED via the API (ensure first Admin → login → create a Movie
// library at the checked-in `movies` fixtures → synchronous scan), then drive
// the BROWSER.
//
// The `movies` fixtures give us both playback paths:
//   - "Dune (2021).mp4"        mp4/h264/aac  → the browser plays it → directPlay
//                                (progressive <video src>).
//   - "Blade Runner (1982).mkv" matroska/mpeg4 → the browser can't direct-play it,
//                                so the server TRANSCODES it and delivers HLS; the
//                                player plays it via hls.js (real MSE in Chromium).
//
// We assert: Play streams Dune in a <video> bound to a session stream URL, a
// byte-range stream request is served, progress is reported, and playing to the
// end advances watch state server-side. Blade Runner plays via the HLS path
// (the player fetches the .m3u8 + segments and currentTime advances), with watch
// state recorded via the API. And with the transcode cap at 1 (set by
// boot-server.mjs), occupying the slot then opening a transcode title shows the
// SERVER_BUSY busy/retry UX — not a broken <video> and not the old dead-end.

const here = dirname(fileURLToPath(import.meta.url));
// here is web/e2e, so the repo root is two levels up (web/e2e → web → repo).
const repoRoot = resolve(here, "..", "..");
const FIXTURES = join(repoRoot, "internal", "api", "testdata", "movies");
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
      device: { name: "seed", platform: "test", clientId: "e2e-seed-play" },
    },
  });
  expect(res.ok(), `login failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()).token as string;
}

// seedMovies creates a Movie library at the `movies` fixtures and scans it,
// returning { libId, token, titles } where titles maps a title name → id.
async function seedMovies(
  request: APIRequestContext,
  token: string,
): Promise<{ libId: string; titles: Record<string, string> }> {
  const auth = { Authorization: `Bearer ${token}` };
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name: "Films", kind: "movie", rootFolders: [FIXTURES] },
  });
  expect(
    create.ok(),
    `create library failed: ${create.status()} ${await create.text()}`,
  ).toBeTruthy();
  const libId = (await create.json()).id as string;

  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();
  // The scan is dispatched async (202 → "running"); poll the status until it
  // settles idle before reading titles (mirrors tv.spec.ts). On CI the scan is
  // usually done by the first read; a slower host takes a few polls.
  let status = await scan.json();
  for (let i = 0; i < 100 && status.state !== "idle"; i++) {
    await new Promise((r) => setTimeout(r, 100));
    const s = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    status = await s.json();
  }
  expect(status.state).toBe("idle");

  // Map title name → id from the library's titles.
  const page = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, {
    headers: auth,
  });
  const body = await page.json();
  const titles: Record<string, string> = {};
  for (const t of body.titles ?? []) titles[t.title as string] = t.id as string;
  return { libId, titles };
}

// ffmpegAvailable reports whether ffmpeg is on PATH (the in-band HLS subtitle test
// synthesizes a subtitled clip; it self-skips otherwise, like the Go real-ffmpeg
// tests).
function ffmpegAvailable(): boolean {
  try {
    execFileSync("ffmpeg", ["-version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

// generateSubtitledRemuxFixture writes a ~12s h264/aac mkv (NO embedded subtitle
// streams — a `-c copy` remux into MPEG-TS chokes on embedded subrip) plus an
// English SIDECAR SubRip whose cues span the clip, into a fresh temp dir, and
// returns the dir. With matroska suppressed in the browser profile the container
// is the only mismatch, so the File REMUXES (directStream) — an HLS tier carrying
// the sidecar subtitle in-band, and one that is NOT transcode-cap-governed, so
// this test never contends for the single slot the serial suite shares. Isolated
// from the checked-in fixtures so it disturbs no other test.
function generateSubtitledRemuxFixture(): string {
  const root = mkdtempSync(join(tmpdir(), "e2e-subs-"));
  const movieDir = join(root, "Subbed Movie (2004)");
  mkdirSync(movieDir, { recursive: true });
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error",
    "-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
    "-f", "lavfi", "-i", "sine=frequency=440:duration=12",
    "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
    "-c:a", "aac", "-shortest",
    join(movieDir, "Subbed Movie (2004).mkv"),
  ], { stdio: "ignore" });
  const srt =
    "1\n00:00:01,000 --> 00:00:04,000\nOpening line\n\n" +
    "2\n00:00:05,000 --> 00:00:08,000\nMiddle line\n\n" +
    "3\n00:00:09,000 --> 00:00:11,000\nClosing line\n";
  writeFileSync(join(movieDir, "Subbed Movie (2004).en.srt"), srt);
  return root;
}

// generateImageSubtitleFixture writes a ~12s h264/aac mkv plus a dummy IMAGE
// SIDECAR (.sup — a PGS extension the scanner classifies as an image track by
// name). Selecting it in the player BURNS it in via a fresh transcode negotiation
// (subtitles/04). The bytes are dummy: this fixture exercises the menu entry + the
// burnSubtitleId re-negotiation contract, not the pixel-accurate burn (which needs
// a real bitmap subtitle no lavfi source can synthesize — see the Go args tests).
function generateImageSubtitleFixture(): string {
  const root = mkdtempSync(join(tmpdir(), "e2e-imgsub-"));
  const movieDir = join(root, "Bitmap Movie (2006)");
  mkdirSync(movieDir, { recursive: true });
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error",
    "-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
    "-f", "lavfi", "-i", "sine=frequency=440:duration=12",
    "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
    "-c:a", "aac", "-shortest",
    join(movieDir, "Bitmap Movie (2006).mkv"),
  ], { stdio: "ignore" });
  writeFileSync(join(movieDir, "Bitmap Movie (2006).de.sup"), "dummy pgs fixture\n");
  return root;
}

// generateMultiAudioMkvFixture writes a ~12s h264 mkv carrying TWO aac audio
// Streams — English (default) + Japanese, distinct sine tones so a switch is
// measurably different — with the language metadata the scanner captures
// (audio-streams/01). With matroska suppressed in the browser profile the container
// is the only mismatch, so the File REMUXES (directStream) into a DEMUXED HLS
// session: a video-only variant plus one in-band AUDIO rendition per Stream
// (audio-streams/03). directStream is NOT transcode-cap-governed, so this never
// contends for the single slot the serial suite shares. Isolated from the
// checked-in fixtures so it disturbs no other test.
function generateMultiAudioMkvFixture(): string {
  const root = mkdtempSync(join(tmpdir(), "e2e-multiaudio-mkv-"));
  const movieDir = join(root, "Dubbed Movie (2010)");
  mkdirSync(movieDir, { recursive: true });
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error",
    "-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
    "-f", "lavfi", "-i", "sine=frequency=440:duration=12",
    "-f", "lavfi", "-i", "sine=frequency=880:duration=12",
    "-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
    "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
    "-c:a", "aac",
    "-metadata:s:a:0", "language=eng",
    "-metadata:s:a:1", "language=jpn",
    "-disposition:a:0", "default",
    join(movieDir, "Dubbed Movie (2010).mkv"),
  ], { stdio: "ignore" });
  return root;
}

// generateMultiAudioMp4Fixture writes the same TWO-audio content in an mp4/h264/aac
// container the browser can DIRECT-PLAY. Direct play carries only the default audio,
// so picking the non-default Japanese Stream escalates ONCE via a fresh negotiation
// carrying audioStreamId (into a remux/directStream demuxed HLS session,
// audio-streams/04) — the audio parallel of the image-subtitle burn-in loop. faststart
// so the browser plays it progressively.
function generateMultiAudioMp4Fixture(): string {
  const root = mkdtempSync(join(tmpdir(), "e2e-multiaudio-mp4-"));
  const movieDir = join(root, "Dubbed Feature (2011)");
  mkdirSync(movieDir, { recursive: true });
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error",
    "-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
    "-f", "lavfi", "-i", "sine=frequency=440:duration=12",
    "-f", "lavfi", "-i", "sine=frequency=880:duration=12",
    "-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
    "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
    "-c:a", "aac",
    "-metadata:s:a:0", "language=eng",
    "-metadata:s:a:1", "language=jpn",
    "-disposition:a:0", "default",
    "-movflags", "+faststart",
    join(movieDir, "Dubbed Feature (2011).mp4"),
  ], { stdio: "ignore" });
  return root;
}

// generateAacAc3Mp4Fixture writes an mp4/h264 with AAC (default) + AC3 — the "Bee
// Movie" shape. The browser direct-plays the AAC default; picking the AC3 track (a
// codec the browser can't decode) escalates to a TRANSCODE that copies the h264 video
// and re-encodes the audio to AAC (ADR-0024). ~16s + a 2s keyframe interval so the
// session spans several segments and playback must cross segment boundaries.
function generateAacAc3Mp4Fixture(): string {
  const root = mkdtempSync(join(tmpdir(), "e2e-aac-ac3-mp4-"));
  const movieDir = join(root, "Bee Movie (2007)");
  mkdirSync(movieDir, { recursive: true });
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error",
    "-f", "lavfi", "-i", "testsrc=duration=64:size=320x240:rate=24",
    "-f", "lavfi", "-i", "sine=frequency=440:duration=64",
    "-f", "lavfi", "-i", "aevalsrc=0.1*sin(660*t):duration=64:channel_layout=5.1",
    "-map", "0:v:0", "-map", "1:a:0", "-map", "2:a:0",
    // B-frames + a 2s GOP like a real movie rip: the copied-video session then spans
    // MANY ~4s segments, so the test genuinely crosses several segment boundaries
    // (a short single-segment fixture can pass while a boundary-crossing bug hides).
    "-c:v", "libx264", "-bf", "3", "-g", "48", "-preset", "veryfast", "-pix_fmt", "yuv420p",
    "-c:a:0", "aac", "-ac:a:0", "2", "-metadata:s:a:0", "language=eng", "-disposition:a:0", "default",
    "-c:a:1", "ac3", "-ac:a:1", "6", "-metadata:s:a:1", "language=eng", "-disposition:a:1", "0",
    "-movflags", "+faststart",
    join(movieDir, "Bee Movie (2007).mp4"),
  ], { stdio: "ignore" });
  return root;
}

// seedLibraryAt creates a Movie library at root, scans it (polling to idle), and
// returns a title-name → id map.
async function seedLibraryAt(
  request: APIRequestContext,
  token: string,
  name: string,
  root: string,
): Promise<Record<string, string>> {
  const auth = { Authorization: `Bearer ${token}` };
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name, kind: "movie", rootFolders: [root] },
  });
  expect(create.ok(), `create ${name} library failed: ${create.status()} ${await create.text()}`).toBeTruthy();
  const libId = (await create.json()).id as string;

  const scan = await request.post(`/api/v1/libraries/${libId}/scan`, { headers: auth });
  expect(scan.ok(), `${name} scan failed: ${scan.status()} ${await scan.text()}`).toBeTruthy();
  let status = await scan.json();
  for (let i = 0; i < 100 && status.state !== "idle"; i++) {
    await new Promise((r) => setTimeout(r, 100));
    const s = await request.get(`/api/v1/libraries/${libId}/scan`, { headers: auth });
    status = await s.json();
  }
  expect(status.state).toBe("idle");

  const page = await request.get(`/api/v1/libraries/${libId}/titles?limit=100`, { headers: auth });
  const body = await page.json();
  const titles: Record<string, string> = {};
  for (const t of body.titles ?? []) titles[t.title as string] = t.id as string;
  return titles;
}

// A playback request body whose device profile mirrors a typical browser
// (mp4/h264/aac, NOT matroska/mpeg4), so the Blade Runner mkv negotiates to the
// transcode tier — the same outcome the browser reaches. Used to occupy the
// single transcode slot from the API so the browser then hits SERVER_BUSY.
function transcodeForcingRequest(): Record<string, unknown> {
  return {
    deviceProfile: {
      containers: ["mp4"],
      videoCodecs: [{ codec: "h264", maxResolution: "1080p" }],
      audioCodecs: ["aac"],
      maxAudioChannels: 2,
      textSubtitleFormats: ["webvtt"],
    },
    constraints: { maxBitrate: 100_000_000, maxResolution: "1080p" },
  };
}

// sessionIdFromStreamUrl pulls the session id out of a decision's HLS/stream URL
// (".../sessions/{id}/hls/..." or ".../sessions/{id}/stream"), so a test can end its
// own server-side session explicitly via the API. Returns "" when the URL isn't one.
function sessionIdFromStreamUrl(streamUrl: string | null): string {
  return streamUrl?.match(/\/sessions\/([^/]+)\//)?.[1] ?? "";
}

// endSessionViaApi DELETEs a playback session server-side and waits for it. Unlike a
// browser navigate-away (the persistent Now Playing bar keeps playing across routes,
// and a closed Playwright context fires no reliable unload DELETE), this is
// DETERMINISTIC: End() frees a held transcode cap slot (ADR-0009) synchronously under
// its lock, so a downstream cap-governed test sees the slot free. A 404 (already
// gone) is fine. Best-effort — a transient failure is swallowed.
async function endSessionViaApi(
  request: APIRequestContext,
  token: string,
  sessionId: string,
): Promise<void> {
  if (!sessionId) return;
  try {
    await request.delete(`/api/v1/sessions/${sessionId}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
  } catch {
    /* best-effort teardown */
  }
}

async function uiLogin(page: Page): Promise<void> {
  await page.goto("/login");
  await expect(page.getByTestId("login-screen")).toBeVisible();
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();
}

// Start playback the way a user does: open the Title's detail and click Play. The
// old /titles/{id}/play route was retired with the standalone PlayerScreen (it now
// redirects to Home), so playback is driven through the detail page's Play button.
// The Now Playing bar is global and persistent, so the <video> / busy state renders
// in place without leaving the detail route.
async function playFromDetail(page: Page, id: string): Promise<void> {
  await page.goto(`/titles/${id}`);
  await expect(page.getByTestId("detail")).toBeVisible();
  await expect(page.getByTestId("play-button")).toBeEnabled();
  await page.getByTestId("play-button").click();
}

test.describe.serial("play: direct play, HLS transcode, progress, resume, server-busy", () => {
  let titles: Record<string, string> = {};

  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    const token = await login(request);
    ({ titles } = await seedMovies(request, token));
    await request.dispose();
    expect(Object.keys(titles).length, "movies seeded").toBeGreaterThan(0);
  });

  function titleId(...candidates: string[]): string {
    for (const name of Object.keys(titles)) {
      if (candidates.some((c) => name.includes(c))) return titles[name];
    }
    throw new Error(
      `no seeded title matched ${candidates.join("/")}; have: ${Object.keys(titles).join(", ")}`,
    );
  }

  test("Play on the mp4 title direct-plays in a <video> and serves a range request", async ({
    page,
  }) => {
    await uiLogin(page);
    const id = titleId("Dune");

    // Watch the network: the <video> must fetch the session stream URL, and the
    // browser issues a Range request against it (byte-range / 206 — ADR-0004).
    const streamReq = page.waitForRequest(
      (r) => /\/api\/v1\/sessions\/[^/]+\/stream/.test(r.url()),
    );

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
    await expect(page.getByTestId("play-button")).toBeEnabled();
    await page.getByTestId("play-button").click();

    // The player renders a <video> bound to a session stream URL (directPlay).
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    const src = await video.getAttribute("src");
    expect(src, "video src is a session stream URL").toMatch(
      /\/api\/v1\/sessions\/[^/]+\/stream/,
    );

    // A stream request was issued and served as partial content (range) OR 200.
    const req = await streamReq;
    const resp = await req.response();
    expect(resp, "stream response served").toBeTruthy();
    // The media cookie authorized it (no 401/403/404).
    expect(resp!.status(), `stream status ${resp!.status()}`).toBeLessThan(400);

    // currentTime advances on the short clip (proves real playback, not a stub).
    await expect
      .poll(
        async () =>
          video.evaluate((el: HTMLVideoElement) => el.currentTime),
        { timeout: 8000 },
      )
      .toBeGreaterThan(0);
  });

  test("the detail's Editions & files section lists the available subtitle tracks", async ({
    page,
  }) => {
    await uiLogin(page);
    const id = titleId("Dune");

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();

    // Dune carries an English sidecar (.en.srt), so the detail names it under the
    // Editions & files section — the same labeled set the captions menu shows.
    const summary = page.getByTestId("detail-subtitles");
    await expect(summary).toBeVisible();
    await expect(summary).toContainText("English");
  });

  test("captions menu on the direct-play title lists a text track and renders cues", async ({
    page,
  }) => {
    await uiLogin(page);
    const id = titleId("Dune");

    // Watch for the out-of-band WebVTT fetch the <track> issues when enabled.
    const vttReq = page.waitForRequest((r) =>
      /\/api\/v1\/titles\/[^/]+\/subtitles\/[^/]+\.vtt/.test(r.url()),
    );

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
    await expect(page.getByTestId("play-button")).toBeEnabled();
    await page.getByTestId("play-button").click();
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect(video).toHaveAttribute("data-tier", "directPlay");

    // Dune carries a sidecar English track → the captions button appears and lists
    // Off + English. Subtitles default OFF (no forced track), so nothing shows yet.
    const cc = page.getByTestId("now-playing-captions");
    await expect(cc).toBeVisible();
    await expect(cc).toHaveAttribute("aria-pressed", "false");
    await cc.click();
    const menu = page.getByTestId("now-playing-captions-menu");
    await expect(menu).toBeVisible();
    await expect(page.getByTestId("captions-off")).toBeVisible();
    await expect(menu).toContainText("English");

    // Enable the English track → the .vtt is fetched (media-cookie authenticated)
    // and the browser exposes a SHOWING text track with parsed cues.
    await menu.getByText("English").click();
    const req = await vttReq;
    const resp = await req.response();
    expect(resp, "subtitle .vtt served").toBeTruthy();
    expect(resp!.status(), `vtt status ${resp!.status()}`).toBeLessThan(400);

    // The native TextTrack is showing and has cues (real WebVTT parsed by the
    // browser, not a stub) — switching was instant, no reload.
    await expect
      .poll(
        async () =>
          video.evaluate((el: HTMLVideoElement) => {
            for (let i = 0; i < el.textTracks.length; i++) {
              const tt = el.textTracks[i];
              if (tt.mode === "showing" && (tt.cues?.length ?? 0) > 0) return true;
            }
            return false;
          }),
        { timeout: 8000 },
      )
      .toBe(true);

    // Turning captions off disables the track (no showing track remains).
    await cc.click();
    await page.getByTestId("captions-off").click();
    await expect
      .poll(
        async () =>
          video.evaluate((el: HTMLVideoElement) => {
            for (let i = 0; i < el.textTracks.length; i++) {
              if (el.textTracks[i].mode === "showing") return true;
            }
            return false;
          }),
        { timeout: 4000 },
      )
      .toBe(false);
  });

  test("search online fetches a subtitle, the picked track appears and renders cues", async ({
    page,
  }) => {
    await uiLogin(page);
    const id = titleId("Dune");

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
    await expect(page.getByTestId("play-button")).toBeEnabled();
    await page.getByTestId("play-button").click();
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect(video).toHaveAttribute("data-tier", "directPlay");

    // Open the captions menu → "Search online" is available to any User.
    const cc = page.getByTestId("now-playing-captions");
    await expect(cc).toBeVisible();
    await cc.click();
    await expect(page.getByTestId("now-playing-captions-menu")).toBeVisible();

    // Trigger the online search → the fake OpenSubtitles stub returns one candidate.
    await page.getByTestId("captions-search-online").click();
    const candidate = page.getByTestId("captions-candidate").first();
    await expect(candidate).toBeVisible();

    // Watch for the fetched track's out-of-band WebVTT fetch, then pick the candidate.
    const vttReq = page.waitForRequest((r) =>
      /\/api\/v1\/titles\/[^/]+\/subtitles\/[^/]+\.vtt/.test(r.url()),
    );
    await candidate.click();
    const resp = await (await vttReq).response();
    expect(resp, "fetched subtitle .vtt served").toBeTruthy();
    expect(resp!.status()).toBeLessThan(400);

    // The picked track is enabled and shows the fetched cue — it plays like any
    // other text track, proving the whole fetch → cache → deliver → render path.
    await expect
      .poll(
        async () =>
          video.evaluate((el: HTMLVideoElement) => {
            for (let i = 0; i < el.textTracks.length; i++) {
              const tt = el.textTracks[i];
              if (tt.mode === "showing" && (tt.cues?.length ?? 0) > 0) {
                for (let c = 0; c < tt.cues!.length; c++) {
                  const cue = tt.cues![c] as VTTCue;
                  if (cue.text.includes("Fetched online cue")) return true;
                }
              }
            }
            return false;
          }),
        { timeout: 8000 },
      )
      .toBe(true);
  });

  test("playing to the end advances watch state server-side", async ({ page, playwright, baseURL }) => {
    await uiLogin(page);
    const id = titleId("Dune");

    await playFromDetail(page, id);
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();

    // Drive the clip to the end so the server crosses the Watched threshold.
    await video.evaluate(async (el: HTMLVideoElement) => {
      await el.play().catch(() => {});
      // Seek near the end; the 'ended' handler reports the final position.
      if (Number.isFinite(el.duration) && el.duration > 0) {
        el.currentTime = Math.max(0, el.duration - 0.1);
      }
    });
    // Wait for the ended event to fire and a final progress report to post.
    await video.evaluate(
      (el: HTMLVideoElement) =>
        new Promise<void>((res) => {
          if (el.ended) return res();
          el.addEventListener("ended", () => res(), { once: true });
          // Safety timeout so a non-ending clip doesn't hang the test.
          setTimeout(() => res(), 6000);
        }),
    );

    // Verify via the API that watch state advanced: watched flipped OR a resume
    // position was recorded. The server owns the threshold; we just confirm the
    // raw position we reported moved the needle.
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
        { timeout: 8000 },
      )
      .toBeTruthy();
    await request.dispose();
  });

  test("the matroska title plays via the HLS path (hls.js), advancing currentTime", async ({
    page,
    playwright,
    baseURL,
  }) => {
    await uiLogin(page);
    const id = titleId("Blade Runner", "Blade");

    // The browser can't direct-play matroska/mpeg4, so the server transcodes and
    // delivers HLS. Watch the network for the HLS playlist + at least one segment
    // fetch — that proves hls.js (MSE) is actually pulling the stream.
    const playlistReq = page.waitForRequest((r) =>
      /\/api\/v1\/sessions\/[^/]+\/hls\/.+\.m3u8/.test(r.url()),
    );

    await playFromDetail(page, id);

    // The player renders a <video> for the HLS (transcode) tier — and NOT the old
    // unsupported dead-end. The decision URL is an HLS playlist; hls.js attaches
    // it (no progressive `src` on the element itself).
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect(page.getByTestId("player-unsupported")).toHaveCount(0);
    await expect(video).toHaveAttribute("data-tier", "transcode");
    const streamUrl = await video.getAttribute("data-stream-url");
    expect(streamUrl, "decision streamUrl is an HLS playlist").toMatch(
      /\/api\/v1\/sessions\/[^/]+\/hls\/.+\.m3u8/,
    );

    // The HLS playlist was fetched (media-cookie authenticated — no 401/403).
    const req = await playlistReq;
    const resp = await req.response();
    expect(resp, "playlist response served").toBeTruthy();
    expect(resp!.status(), `playlist status ${resp!.status()}`).toBeLessThan(400);

    // currentTime advances on the ~1s clip — real hls.js playback, not a stub.
    await expect
      .poll(
        async () => video.evaluate((el: HTMLVideoElement) => el.currentTime),
        { timeout: 20000 },
      )
      .toBeGreaterThan(0);

    // Watch state is recorded via the API on the HLS path exactly as for direct
    // play (the client reports raw position; the server owns the threshold).
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

    // Leave the player, then DETERMINISTICALLY free the transcode cap slot this
    // session holds (ADR-0009): navigating away is not enough — the persistent Now
    // Playing bar keeps the session alive across routes, and Playwright closing this
    // test's browser context fires no reliable unload DELETE, so the transcode session
    // would linger until the server reaps it and a downstream cap-governed test (the
    // image-subtitle burn) would flake with 503 SERVER_BUSY. Ending it via the API
    // frees the slot synchronously under End()'s lock.
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
    await endSessionViaApi(request, token, sessionIdFromStreamUrl(streamUrl));
    await request.dispose();
  });

  test("a text subtitle renders IN-BAND over the HLS tier (hls.js master playlist)", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping in-band HLS subtitle e2e");
    test.setTimeout(60_000); // library scan + remux startup + hls.js subtitle load

    // Shape the browser profile so the subtitled h264/aac mkv takes the HLS path
    // via hls.js:
    //   - suppress matroska (video/x-matroska) → the container is the only mismatch,
    //     so the File REMUXES (directStream, an HLS tier that is NOT transcode-cap-
    //     governed — this test never contends for the single slot the suite shares);
    //   - suppress native HLS (mpegurl) → the player drives HLS with hls.js (MSE),
    //     the path real Chrome/Firefox use (ADR-0020: native HLS is the Apple apps').
    await page.addInitScript(() => {
      const orig = HTMLMediaElement.prototype.canPlayType;
      HTMLMediaElement.prototype.canPlayType = function (type: string) {
        return /mpegurl|matroska/i.test(type) ? "" : orig.call(this, type);
      };
    });

    // Seed a library with a subtitled (sidecar) h264/aac clip.
    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const subTitles = await seedLibraryAt(request, token, "Subs HLS", generateSubtitledRemuxFixture());
    await request.dispose();
    const id = Object.entries(subTitles).find(([name]) => name.includes("Subbed Movie"))?.[1];
    expect(id, `Subbed Movie seeded; have: ${Object.keys(subTitles).join(", ")}`).toBeTruthy();

    await uiLogin(page);

    // hls.js should pull the in-band SUBTITLES rendition's WebVTT segment once a
    // track is enabled — proof the in-band path is live (not an out-of-band track).
    const vttReq = page.waitForRequest((r) => /\/hls\/subs_[^/]+\.vtt/.test(r.url()));

    await playFromDetail(page, id!);
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    // matroska suppressed but h264/aac supported → the container-only mismatch
    // remuxes to an HLS master playlist carrying the in-band subtitle rendition.
    await expect(video).toHaveAttribute("data-tier", "directStream");
    const streamUrl = await video.getAttribute("data-stream-url");
    expect(streamUrl, "HLS streamUrl is a master playlist").toMatch(/\/hls\/master\.m3u8$/);

    // On the HLS tier there are NO out-of-band <track> elements — subtitles ride
    // in-band via hls.js.
    expect(
      await video.evaluate((el: HTMLVideoElement) => el.querySelectorAll("track").length),
    ).toBe(0);

    // Get the HLS stream actually playing first (hls.js loads subtitle fragments
    // alongside the video it is buffering).
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
      .toBeGreaterThan(0);

    // Enable the English track from the captions menu (deterministic selection).
    const cc = page.getByTestId("now-playing-captions");
    await expect(cc).toBeVisible();
    await cc.click();
    await page
      .getByTestId("now-playing-captions-menu")
      .locator('[data-sub-lang="en"]')
      .first()
      .click();

    // hls.js pulled the in-band rendition's WebVTT segment (media-cookie authed).
    const vreq = await vttReq;
    const vresp = await vreq.response();
    expect(vresp, "in-band .vtt segment served").toBeTruthy();
    expect(vresp!.status(), `vtt status ${vresp!.status()}`).toBeLessThan(400);

    // A SHOWING text track with parsed cues — real in-band WebVTT rendered by
    // hls.js over the HLS tier.
    await expect
      .poll(
        async () =>
          video.evaluate((el: HTMLVideoElement) => {
            for (let i = 0; i < el.textTracks.length; i++) {
              const tt = el.textTracks[i];
              if (tt.mode === "showing" && (tt.cues?.length ?? 0) > 0) return true;
            }
            return false;
          }),
        { timeout: 15000 },
      )
      .toBe(true);
  });

  test("selecting an image subtitle re-negotiates with burnSubtitleId → transcode (subtitles/04)", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping image-subtitle burn-in e2e");
    test.setTimeout(60_000);

    // Seed a movie carrying a dummy IMAGE sidecar (.sup). The mkv direct-plays;
    // selecting the image track BURNS it in via a fresh transcode negotiation.
    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const seeded = await seedLibraryAt(request, token, "Bitmap Subs", generateImageSubtitleFixture());
    const id = Object.entries(seeded).find(([name]) => name.includes("Bitmap Movie"))?.[1];
    expect(id, `Bitmap Movie seeded; have: ${Object.keys(seeded).join(", ")}`).toBeTruthy();

    await uiLogin(page);
    await playFromDetail(page, id!);
    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect(video).toHaveAttribute("data-tier", "directPlay");

    // The captions menu lists the image track, marked as a burn-in.
    const cc = page.getByTestId("now-playing-captions");
    await expect(cc).toBeVisible();
    await cc.click();
    const menu = page.getByTestId("now-playing-captions-menu");
    const imageItem = menu.locator('[data-sub-kind="image"]').first();
    await expect(imageItem).toBeVisible();
    await expect(imageItem).toContainText("burn in");

    // Selecting it re-negotiates: a new POST /playback carrying burnSubtitleId,
    // whose decision escalates to the transcode tier.
    const burnNeg = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()) && (r.request().postDataJSON()?.burnSubtitleId ?? "") !== "",
    );
    await imageItem.click();

    const resp = await burnNeg;
    expect(resp.status(), `burn negotiation status ${resp.status()}`).toBe(200);
    const decision = await resp.json();
    expect(decision.tier, "image-sub selection escalates to transcode").toBe("transcode");

    // Clean up the burn session we created so it doesn't hold the shared transcode
    // slot the SERVER_BUSY test relies on.
    if (decision.sessionId) {
      await request.delete(`/api/v1/sessions/${decision.sessionId}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
    await request.dispose();
  });

  test("multi-audio: switching audio on the HLS tier is IN-BAND (no session restart)", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping multi-audio in-band e2e");
    test.setTimeout(60_000); // scan + remux startup + hls.js audio-rendition load

    // Force the hls.js path (real Chrome/Firefox's): suppress matroska (container-only
    // mismatch → directStream remux, NOT cap-governed) and native HLS (drive hls.js
    // over MSE). Mirrors the in-band subtitle test.
    await page.addInitScript(() => {
      const orig = HTMLMediaElement.prototype.canPlayType;
      HTMLMediaElement.prototype.canPlayType = function (type: string) {
        return /mpegurl|matroska/i.test(type) ? "" : orig.call(this, type);
      };
    });

    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const seeded = await seedLibraryAt(request, token, "Multi Audio MKV", generateMultiAudioMkvFixture());
    await request.dispose();
    const id = Object.entries(seeded).find(([name]) => name.includes("Dubbed Movie"))?.[1];
    expect(id, `Dubbed Movie seeded; have: ${Object.keys(seeded).join(", ")}`).toBeTruthy();

    await uiLogin(page);

    // Count negotiation POSTs so we can prove the in-band switch does NOT re-negotiate.
    const playbackPosts: string[] = [];
    page.on("request", (r) => {
      if (r.method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url())) {
        playbackPosts.push(r.url());
      }
    });

    const negResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()),
    );
    await playFromDetail(page, id!);
    const decision = await (await negResp).json();
    expect(decision.tier, "container-only mismatch remuxes to HLS").toBe("directStream");
    expect(decision.audioStreams?.length, "two selectable audio Streams").toBe(2);
    const nonDefault = decision.audioStreams.find((s: { isDefault: boolean }) => !s.isDefault);
    expect(nonDefault, "a non-default audio Stream exists").toBeTruthy();

    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await expect(video).toHaveAttribute("data-tier", "directStream");

    // Get the HLS stream actually playing (hls.js buffers the default audio first).
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
      .toBeGreaterThan(0);

    // hls.js pulls the ALTERNATE audio rendition's playlist/segments when we switch
    // to it — proof the switch is in-band (a new rendition, not a re-negotiation).
    const altReq = page.waitForRequest((r) =>
      new RegExp(`/hls/audio_${nonDefault.id}`).test(r.url()),
    );

    // Open the Audio menu and pick the non-default (Japanese) Stream.
    const audioBtn = page.getByTestId("now-playing-audio");
    await expect(audioBtn).toBeVisible();
    await audioBtn.click();
    const menu = page.getByTestId("now-playing-audio-menu");
    await expect(menu).toBeVisible();
    await menu.locator(`[data-audio-id="${nonDefault.id}"]`).click();

    // The alternate rendition was fetched → in-band switch happened.
    await altReq;

    // currentTime keeps advancing across the switch — no restart, no gap beyond a
    // brief buffer.
    const t1 = await video.evaluate((el: HTMLVideoElement) => el.currentTime);
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 15000 })
      .toBeGreaterThan(t1);

    // Exactly ONE negotiation happened (the mount) — the in-band switch never
    // re-requested playback.
    expect(playbackPosts.length, "in-band switch does not re-negotiate").toBe(1);

    // The choice survives a seek: scrub, then re-open the menu — Japanese still active.
    await video.evaluate((el: HTMLVideoElement) => {
      el.currentTime = Math.min((el.duration || 12) - 2, el.currentTime + 3);
    });
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 15000 })
      .toBeGreaterThan(t1);
    await audioBtn.click();
    await expect(
      page.getByTestId("now-playing-audio-menu").locator(`[data-audio-id="${nonDefault.id}"]`),
    ).toHaveAttribute("aria-checked", "true");

    // Leave the player so its session is ended (DELETE).
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
  });

  test("multi-audio: an in-band pick is Remembered — replaying auto-selects it (audio-streams/05)", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping remembered-audio e2e");
    test.setTimeout(60_000); // two scans/negotiations + remux startup

    // Drive hls.js (suppress matroska + native HLS), same as the in-band test.
    await page.addInitScript(() => {
      const orig = HTMLMediaElement.prototype.canPlayType;
      HTMLMediaElement.prototype.canPlayType = function (type: string) {
        return /mpegurl|matroska/i.test(type) ? "" : orig.call(this, type);
      };
    });

    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const seeded = await seedLibraryAt(request, token, "Remember Audio MKV", generateMultiAudioMkvFixture());
    await request.dispose();
    const id = Object.entries(seeded).find(([name]) => name.includes("Dubbed Movie"))?.[1];
    expect(id, `Dubbed Movie seeded; have: ${Object.keys(seeded).join(", ")}`).toBeTruthy();

    await uiLogin(page);

    // First play: default resolves to English; pick Japanese in-band.
    const negResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()),
    );
    await playFromDetail(page, id!);
    const decision = await (await negResp).json();
    const nonDefault = decision.audioStreams.find((s: { isDefault: boolean }) => !s.isDefault);
    expect(nonDefault, "a non-default (Japanese) audio Stream exists").toBeTruthy();

    const video = page.getByTestId("player-video");
    await expect(video).toBeVisible();
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
      .toBeGreaterThan(0);

    // The in-band pick writes memory through the progress surface — wait for that
    // watch-state write (carrying audioStreamId) before we leave, so the replay reads it.
    const memWrite = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        /\/sessions\/[^/]+\/progress$/.test(r.url()) &&
        (r.postDataJSON()?.audioStreamId ?? "") === nonDefault.id,
    );
    const audioBtn = page.getByTestId("now-playing-audio");
    await audioBtn.click();
    await page
      .getByTestId("now-playing-audio-menu")
      .locator(`[data-audio-id="${nonDefault.id}"]`)
      .click();
    await memWrite;

    // Leave the player (ends the session), then REPLAY the same Title.
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();

    const replayResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()),
    );
    await playFromDetail(page, id!);
    const replay = await (await replayResp).json();
    // Server-side memory resolved the Japanese Stream as the session default.
    expect(replay.audioStream?.index, "replay resolves the remembered Japanese Stream").toBe(
      nonDefault.index,
    );

    // And the Audio menu auto-selects and marks it, with no manual pick.
    await expect(page.getByTestId("player-video")).toBeVisible();
    await page.getByTestId("now-playing-audio").click();
    await expect(
      page.getByTestId("now-playing-audio-menu").locator(`[data-audio-id="${nonDefault.id}"]`),
    ).toHaveAttribute("aria-checked", "true");

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
  });

  test("multi-audio: a direct-play non-default pick re-negotiates with audioStreamId → remux", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping multi-audio escalation e2e");
    test.setTimeout(60_000);

    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const seeded = await seedLibraryAt(request, token, "Multi Audio MP4", generateMultiAudioMp4Fixture());
    const id = Object.entries(seeded).find(([name]) => name.includes("Dubbed Feature"))?.[1];
    expect(id, `Dubbed Feature seeded; have: ${Object.keys(seeded).join(", ")}`).toBeTruthy();

    await uiLogin(page);

    const negResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()),
    );
    await playFromDetail(page, id!);
    const decision = await (await negResp).json();
    expect(decision.tier, "mp4/h264/aac direct-plays").toBe("directPlay");
    const nonDefault = decision.audioStreams.find((s: { isDefault: boolean }) => !s.isDefault);
    expect(nonDefault, "a non-default audio Stream exists").toBeTruthy();

    const video = page.getByTestId("player-video");
    await expect(video).toHaveAttribute("data-tier", "directPlay");
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 15000 })
      .toBeGreaterThan(0);

    // Pick the non-default Stream → a fresh negotiation carrying audioStreamId (the
    // one escalating switch: direct play carries only the default audio).
    const escNeg = page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        /\/titles\/[^/]+\/playback$/.test(r.url()) &&
        (r.request().postDataJSON()?.audioStreamId ?? "") === nonDefault.id,
    );
    const audioBtn = page.getByTestId("now-playing-audio");
    await expect(audioBtn).toBeVisible();
    await audioBtn.click();
    await page
      .getByTestId("now-playing-audio-menu")
      .locator(`[data-audio-id="${nonDefault.id}"]`)
      .click();

    const resp = await escNeg;
    expect(resp.status(), `escalation status ${resp.status()}`).toBe(200);
    const esc = await resp.json();
    // Non-default audio escalates off direct play into a remux (directStream).
    expect(esc.tier, "non-default audio escalates to remux").toBe("directStream");
    // The Decision reports the picked Stream as the resolved/delivered audio.
    expect(esc.audioStream?.index, "reported audio is the picked Stream").toBe(nonDefault.index);

    // Playback resumes on the remux tier (near where we were — the client seeks to
    // the captured position on the new stream).
    await expect(video).toHaveAttribute("data-tier", "directStream");
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
      .toBeGreaterThan(0);

    // Clean up the escalated session so it doesn't linger.
    if (esc.sessionId) {
      await request.delete(`/api/v1/sessions/${esc.sessionId}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
    await request.dispose();
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
  });

  test("multi-audio: switching to an AC3 track (transcode) keeps playing (Bee Movie)", async ({
    page,
    playwright,
    baseURL,
  }) => {
    test.skip(!ffmpegAvailable(), "ffmpeg not on PATH — skipping AC3-switch e2e");
    test.setTimeout(90_000);

    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    const seeded = await seedLibraryAt(request, token, "Bee AAC AC3", generateAacAc3Mp4Fixture());
    const id = Object.entries(seeded).find(([name]) => name.includes("Bee Movie"))?.[1];
    expect(id, `Bee Movie seeded; have: ${Object.keys(seeded).join(", ")}`).toBeTruthy();

    await uiLogin(page);

    const negResp = page.waitForResponse(
      (r) => r.request().method() === "POST" && /\/titles\/[^/]+\/playback$/.test(r.url()),
    );
    await playFromDetail(page, id!);
    const decision = await (await negResp).json();
    expect(decision.tier, "mp4/h264/aac direct-plays").toBe("directPlay");
    const ac3 = decision.audioStreams.find((s: { codec: string }) => s.codec === "ac3");
    expect(ac3, "an AC3 audio Stream exists").toBeTruthy();

    const video = page.getByTestId("player-video");
    await expect(video).toHaveAttribute("data-tier", "directPlay");
    await video.evaluate((el: HTMLVideoElement) => el.play().catch(() => {}));
    await expect
      .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 15000 })
      .toBeGreaterThan(0);

    // Switch to AC3 → a fresh negotiation carrying audioStreamId; the browser can't
    // decode AC3, so the server transcodes the audio and copies the h264 video.
    const escNeg = page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        /\/titles\/[^/]+\/playback$/.test(r.url()) &&
        (r.request().postDataJSON()?.audioStreamId ?? "") === ac3.id,
    );
    await page.getByTestId("now-playing-audio").click();
    await page.getByTestId("now-playing-audio-menu").locator(`[data-audio-id="${ac3.id}"]`).click();
    const esc = await (await escNeg).json();
    expect(esc.tier, "AC3 escalates to a transcode (audio re-encoded)").toBe("transcode");

    // The real bug: playback must SUSTAIN across MANY segment boundaries, not stop
    // (the reported failure: a few segments load, then hls.js silently stalls). The
    // fixture has 2s GOPs → ~4s segments; crossing 20s proves ~5 boundaries. Speed
    // up the clock but DO NOT call play() — the real app must resume playback by
    // itself after the escalating switch (a manual play() here masked a player that
    // never resumes: frozen video, ~30s buffered, zero errors — the reported bug).
    // On a stall, dump the media-element state (paused? buffered vs currentTime?).
    await video.evaluate((el: HTMLVideoElement) => {
      el.playbackRate = 4;
      el.muted = true;
    });
    const dumpDiag = async () =>
      video.evaluate((el: HTMLVideoElement) => {
        const ranges: string[] = [];
        for (let i = 0; i < el.buffered.length; i++) {
          ranges.push(`[${el.buffered.start(i).toFixed(3)}..${el.buffered.end(i).toFixed(3)}]`);
        }
        return {
          currentTime: el.currentTime,
          readyState: el.readyState,
          paused: el.paused,
          buffered: ranges.join(" "),
          error: el.error ? `${el.error.code}: ${el.error.message}` : null,
        };
      });
    try {
      await expect
        .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 45000 })
        .toBeGreaterThan(20);
    } catch (err) {
      throw new Error(`stalled: ${JSON.stringify(await dumpDiag())}\n${err}`);
    }

    // SEEK deep into the file (the reported desync/404 case): the copied-video
    // session must realign to the target's exact keyframe boundary and keep
    // playing FROM THE SOUGHT POSITION — not stall on unproduced segments, and not
    // resume at the wrong spot (video content must match the timeline position).
    await video.evaluate((el: HTMLVideoElement) => {
      el.currentTime = 48;
    });
    try {
      await expect
        .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 30000 })
        .toBeGreaterThan(50);
    } catch (err) {
      throw new Error(`stalled after seek: ${JSON.stringify(await dumpDiag())}\n${err}`);
    }

    // IN-BAND switch BACK to the default AAC track on this ESCALATED TRANSCODE
    // session (the reported "switching back does nothing" bug was on exactly this
    // session type; the older in-band e2e only covers a remux session). The proof
    // the switch is real: hls.js must start fetching the AAC rendition's segments
    // (audio_<aacId>_*.ts) — the observable the user checked and found missing.
    const aac = decision.audioStreams.find((s: { codec: string }) => s.codec === "aac");
    expect(aac, "the AAC (default) audio Stream exists").toBeTruthy();
    // dispatchEvent: after the seek phase the immersive stage can leave the menu
    // off-viewport for a positional click; the user's real clicks land (their UI
    // marks the pick), so a synthetic click faithfully reproduces their flow.
    await page.getByTestId("now-playing-audio").dispatchEvent("click");
    const aacFetch = page.waitForRequest((r) => new RegExp(`/hls/audio_${aac.id}`).test(r.url()), {
      timeout: 15000,
    });
    await page
      .getByTestId("now-playing-audio-menu")
      .locator(`[data-audio-id="${aac.id}"]`)
      .dispatchEvent("click");
    await aacFetch;
    // And playback keeps advancing across the in-band switch.
    const tSwitch = await video.evaluate((el: HTMLVideoElement) => el.currentTime);
    try {
      await expect
        .poll(async () => video.evaluate((el: HTMLVideoElement) => el.currentTime), { timeout: 20000 })
        .toBeGreaterThan(tSwitch + 1);
    } catch (err) {
      throw new Error(`stalled after in-band switch back: ${JSON.stringify(await dumpDiag())}\n${err}`);
    }

    if (esc.sessionId) {
      await request.delete(`/api/v1/sessions/${esc.sessionId}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
    }
    await request.dispose();
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
  });

  test("SERVER_BUSY at the transcode cap shows the busy/retry UX", async ({
    page,
    playwright,
    baseURL,
  }) => {
    const id = titleId("Blade Runner", "Blade");

    // Occupy the single transcode slot via the API (negotiate a transcode and do
    // NOT delete it), so the browser's negotiation hits the cap → 503 SERVER_BUSY.
    const request = await playwright.request.newContext({ baseURL });
    const token = await login(request);
    // Saturate the single transcode slot via the API so the browser's negotiation
    // hits the cap → 503 SERVER_BUSY. If the slot is ALREADY held (a prior test's
    // session is still reaping), that's equally fine — the cap is full either way;
    // we just won't have our own session to delete.
    const neg = await request.post(`/api/v1/titles/${id}/playback`, {
      headers: { Authorization: `Bearer ${token}` },
      data: transcodeForcingRequest(),
    });
    const held = neg.ok() ? ((await neg.json()).sessionId as string) : null;
    if (!held) {
      expect(
        neg.status(),
        `occupy-slot expected 200 or 503, got ${neg.status()}`,
      ).toBe(503);
    }

    try {
      await uiLogin(page);
      await playFromDetail(page, id);

      // The cap is full → the player shows the busy state. The hook auto-retries
      // once at the suggested lower bitrate; the matroska still transcodes (the
      // mismatch is by codec, not bitrate), so it stays busy and offers a manual
      // retry. Either way the busy message — not a <video>, not the dead-end — shows.
      await expect(page.getByTestId("player-busy")).toBeVisible();
      await expect(page.getByTestId("player-busy-message")).toBeVisible();
      await expect(page.getByTestId("player-unsupported")).toHaveCount(0);
      // After the single auto-retry exhausts, a manual retry is offered.
      await expect(page.getByTestId("player-busy-retry")).toBeVisible({ timeout: 15000 });
    } finally {
      // Free the slot we held (if any).
      if (held) {
        await request.delete(`/api/v1/sessions/${held}`, {
          headers: { Authorization: `Bearer ${token}` },
        });
      }
      await request.dispose();
    }
  });

  test("manual mark-watched on the detail page is reflected", async ({ page }) => {
    await uiLogin(page);
    const id = titleId("Sample", "Sample Movie");

    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("detail")).toBeVisible();
    // The watched toggle is now an icon button; its label reflects the next action.
    const toggle = page.getByTestId("watch-toggle");
    await expect(toggle).toHaveAttribute("aria-label", "Mark as watched");
    await toggle.click();
    // The badge flips to Watched and the toggle now offers "Mark as unwatched".
    await expect(page.getByTestId("watch-watched")).toBeVisible();
    await expect(page.getByTestId("watch-toggle")).toHaveAttribute(
      "aria-label",
      "Mark as unwatched",
    );

    // Reopening the title reflects the persisted server state.
    await page.goto(`/titles/${id}`);
    await expect(page.getByTestId("watch-watched")).toBeVisible();
  });
});
