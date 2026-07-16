import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// The phone's half of the Device authorization grant (ADR-0036), end to end
// against the REAL embedded Go server: a TV starts a flow over the API, the
// browser walks the /link route a QR code would open, and the TV's poll collects
// a session.
//
// This exercises the one thing neither the Go tests nor the component tests can:
// the login BOUNCE. An unauthenticated visitor to /link/:code is redirected to
// /login by RequireAuth and must return to the code — and the code has to
// survive that round trip. It is a path parameter precisely because
// LoginScreen restores only `location.state.from.pathname` and drops the query
// string, so a `?code=` form would come back empty and silently ask the user to
// retype what they had already scanned.

const here = dirname(fileURLToPath(import.meta.url));
const CLAIM_TOKEN_FILE = join(here, ".claim-token");

const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

function readClaimToken(): string {
  for (let i = 0; i < 50; i++) {
    try {
      const tok = readFileSync(CLAIM_TOKEN_FILE, "utf8").trim();
      if (tok) return tok;
    } catch {
      // not written yet
    }
  }
  throw new Error(`claim token file not found at ${CLAIM_TOKEN_FILE}`);
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

interface DeviceFlow {
  deviceCode: string;
  userCode: string;
  verificationUri: string;
  verificationUriComplete: string;
}

/** Stands in for the Apple TV: asks the server for a code pair. */
async function startFlowAsTV(
  request: APIRequestContext,
  clientId: string,
): Promise<DeviceFlow> {
  const res = await request.post("/api/v1/auth/device/code", {
    data: { device: { name: "Living Room TV", platform: "tvos", clientId } },
  });
  expect(res.ok(), `start failed: ${res.status()} ${await res.text()}`).toBeTruthy();
  return (await res.json()) as DeviceFlow;
}

/** The TV's poll, once. Returns the session body on success, or the error envelope. */
async function pollAsTV(request: APIRequestContext, deviceCode: string) {
  const res = await request.post("/api/v1/auth/device/token", {
    data: { deviceCode },
  });
  return { status: res.status(), body: await res.json() };
}

/**
 * Poll the way a real TV does: on an interval, backing off when told to.
 *
 * A single poll is not enough, and the reason is worth stating. The server paces
 * polls and answers SLOW_DOWN to anything faster than the interval it granted —
 * so a test that polls, drives a browser for a second, and polls again gets a
 * 400 for polling too fast rather than the session it was checking for. That is
 * the rule working, not a bug, and the fix is for the test to be a well-behaved
 * client instead of asserting that one poll always wins a race with a human.
 */
async function pollUntilSession(request: APIRequestContext, deviceCode: string) {
  const deadline = Date.now() + 20_000;
  let last = await pollAsTV(request, deviceCode);
  while (Date.now() < deadline) {
    if (last.status === 200) return last;
    const code = last.body?.error?.code;
    // Anything other than "wait" is terminal — an expired or unknown code will
    // never turn into a session, and spinning on it just burns the deadline.
    if (code !== "AUTHORIZATION_PENDING" && code !== "SLOW_DOWN") return last;
    await new Promise((r) => setTimeout(r, 2_100));
    last = await pollAsTV(request, deviceCode);
  }
  return last;
}

async function uiLogin(page: Page): Promise<void> {
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
}

test.describe("device authorization grant — the phone's half", () => {
  test.beforeAll(async ({ playwright, baseURL }) => {
    const request = await playwright.request.newContext({ baseURL });
    await ensureAdmin(request);
    await request.dispose();
  });

  test("scanning a QR while signed out: login bounce keeps the code, and the TV signs in", async ({
    page,
    playwright,
    baseURL,
  }) => {
    const tv = await playwright.request.newContext({ baseURL });
    const flow = await startFlowAsTV(tv, "e2e-tv-bounce");

    // The TV is waiting.
    const before = await pollAsTV(tv, flow.deviceCode);
    expect(before.status).toBe(400);
    expect(before.body.error.code).toBe("AUTHORIZATION_PENDING");

    // Scan the QR. verificationUriComplete is literally what the QR encodes, so
    // navigating to it IS the scan — signed out, so the guard bounces to login.
    await page.goto(flow.verificationUriComplete);
    await expect(page.getByTestId("login-screen")).toBeVisible();

    await uiLogin(page);

    // Back at /link WITH the code intact, and approved without further typing.
    // If the code had been lost in the bounce, this would be the entry form
    // (link-screen) instead — asking the user to retype what they scanned.
    await expect(page.getByTestId("link-approved")).toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/link/${flow.userCode}$`));
    await expect(page.getByTestId("link-approved-device")).toContainText("Living Room TV");

    // The TV collects a real session.
    const after = await pollUntilSession(tv, flow.deviceCode);
    expect(after.status, `poll failed: ${JSON.stringify(after.body)}`).toBe(200);
    expect(after.body.token).toBeTruthy();
    expect(after.body.user.username).toBe(ADMIN_USER);
    expect(after.body.device.clientId).toBe("e2e-tv-bounce");

    // And that token is a real session on the Public scope.
    const asTV = await playwright.request.newContext({
      baseURL,
      extraHTTPHeaders: { Authorization: `Bearer ${after.body.token}` },
    });
    const devices = await asTV.get("/api/v1/devices");
    expect(devices.ok()).toBeTruthy();
    const names = ((await devices.json()).devices as Array<{ clientId: string }>).map(
      (d) => d.clientId,
    );
    expect(names).toContain("e2e-tv-bounce");

    await asTV.dispose();
    await tv.dispose();
  });

  test("scanning while already signed in approves with no further interaction", async ({
    page,
    playwright,
    baseURL,
  }) => {
    const tv = await playwright.request.newContext({ baseURL });
    const flow = await startFlowAsTV(tv, "e2e-tv-warm");

    // Establish a session first, then scan.
    await page.goto("/login");
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    await page.goto(flow.verificationUriComplete);

    // No login, no typing, no confirm — scan to signed-in.
    await expect(page.getByTestId("link-approved")).toBeVisible();

    const after = await pollUntilSession(tv, flow.deviceCode);
    expect(after.status, `poll failed: ${JSON.stringify(after.body)}`).toBe(200);
    await tv.dispose();
  });

  test("typing the code by hand works when there is no QR to scan", async ({
    page,
    playwright,
    baseURL,
  }) => {
    const tv = await playwright.request.newContext({ baseURL });
    const flow = await startFlowAsTV(tv, "e2e-tv-typed");

    await page.goto("/login");
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    // The bare /link route — what verificationUri (no code) points at, for
    // someone reading the URL off the TV rather than scanning it.
    await page.goto("/link");
    await expect(page.getByTestId("link-screen")).toBeVisible();
    await page.getByTestId("link-code").fill(flow.userCode);
    await page.getByTestId("link-submit").click();

    await expect(page.getByTestId("link-approved")).toBeVisible();
    const after = await pollUntilSession(tv, flow.deviceCode);
    expect(after.status, `poll failed: ${JSON.stringify(after.body)}`).toBe(200);
    await tv.dispose();
  });

  test("a lowercase, hyphenated retype still approves", async ({ page, playwright, baseURL }) => {
    const tv = await playwright.request.newContext({ baseURL });
    const flow = await startFlowAsTV(tv, "e2e-tv-sloppy");

    await page.goto("/login");
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    await page.goto("/link");
    // How a code actually gets retyped: lowercased by a phone keyboard, and
    // grouped with a separator because that is how people copy four characters.
    const sloppy = `${flow.userCode.slice(0, 2)}-${flow.userCode.slice(2)}`.toLowerCase();
    await page.getByTestId("link-code").fill(sloppy);
    await page.getByTestId("link-submit").click();

    await expect(page.getByTestId("link-approved")).toBeVisible();
    const after = await pollUntilSession(tv, flow.deviceCode);
    expect(after.status, `poll failed: ${JSON.stringify(after.body)}`).toBe(200);
    await tv.dispose();
  });

  test("a wrong code is refused and can be corrected in place", async ({ page }) => {
    await page.goto("/login");
    await uiLogin(page);
    await expect(page.getByTestId("home-screen")).toBeVisible();

    await page.goto("/link/ZZZZ");

    await expect(page.getByTestId("link-error")).toBeVisible();
    // The recovery is retyping, so the form must still be there — a dead end
    // would mean rescanning a code that is still perfectly live on the TV.
    await expect(page.getByTestId("link-code")).toBeVisible();
  });
});
