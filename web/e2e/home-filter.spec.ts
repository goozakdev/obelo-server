import { test, expect, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// "See all" on a Home Discovery row opens the Library browse with the row's filter
// in the URL (?genre= / ?studio= / ... — see browse/homeRows.ts). The e2e fake
// metadata cannot yield a Discovery row (a value needs 8 visible Titles sharing
// it), so this drives the DEEP LINK a See-all click navigates to, against the real
// server: the chip shows, the server filters (an impossible decade -> empty), and
// clearing the chip restores the unfiltered grid. The See-all href itself is
// vitest-covered (browse/homeRows.test.ts, screens/HomeScreen.test.tsx).

const here = dirname(fileURLToPath(import.meta.url));
const FIXTURES = join(resolve(here, "..", ".."), "internal", "api", "testdata", "naming");
const ADMIN_USER = "operator";
const ADMIN_PASS = "correct horse battery staple";

function readClaimToken(): string | null {
  try {
    return readFileSync(join(here, ".claim-token"), "utf8").trim() || null;
  } catch {
    return null;
  }
}

async function seedLibraryId(request: APIRequestContext): Promise<string> {
  const info = await (await request.get("/api/v1/server")).json();
  if (info.setupRequired) {
    const claimToken = readClaimToken();
    if (!claimToken) throw new Error("setup required but no claim token captured");
    await request.post("/api/v1/setup", {
      data: { claimToken, username: ADMIN_USER, password: ADMIN_PASS },
    });
  }
  const login = await request.post("/api/v1/auth/login", {
    data: {
      username: ADMIN_USER,
      password: ADMIN_PASS,
      device: { name: "seed", platform: "test", clientId: "e2e-seed-home-filter" },
    },
  });
  expect(login.ok()).toBeTruthy();
  const auth = { Authorization: `Bearer ${(await login.json()).token}` };
  const create = await request.post("/api/v1/libraries", {
    headers: auth,
    data: { name: "Filter Films", kind: "movie", rootFolders: [FIXTURES] },
  });
  if (create.ok()) {
    const id = (await create.json()).id as string;
    await request.post(`/api/v1/libraries/${id}/scan`, { headers: auth });
    for (let i = 0; i < 150; i++) {
      const s = await request.get(`/api/v1/libraries/${id}/scan`, { headers: auth });
      if (s.ok() && (await s.json()).state !== "running") break;
      await new Promise((r) => setTimeout(r, 100));
    }
    return id;
  }
  // Another spec owns a library over this root (the server forbids overlap).
  const libs = await (await request.get("/api/v1/libraries", { headers: auth })).json();
  const match = (libs.libraries ?? []).find((l: { rootFolders?: { path: string }[] }) =>
    (l.rootFolders ?? []).some((r) => r.path === FIXTURES),
  );
  if (!match) throw new Error(`create library failed: ${create.status()}`);
  return match.id as string;
}

test("a filtered browse deep link shows a clearable chip and the server filters", async ({
  page,
  playwright,
  baseURL,
}) => {
  const request = await playwright.request.newContext({ baseURL });
  const libId = await seedLibraryId(request);
  await request.dispose();

  await page.goto("/login");
  await page.getByTestId("login-username").fill(ADMIN_USER);
  await page.getByTestId("login-password").fill(ADMIN_PASS);
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("home-screen")).toBeVisible();

  // A decade no fixture is in: the chip is shown and the grid is filtered empty.
  await page.goto(`/libraries/${libId}?decade=2090`);
  await expect(page.getByTestId("filter-chip")).toHaveText(/2090s/);
  await expect(page.getByTestId("grid-empty")).toBeVisible();
  await expect(page.getByTestId("poster-tile")).toHaveCount(0);

  // Clearing drops the param and restores the unfiltered grid.
  await page.getByTestId("filter-clear").click();
  await expect(page).not.toHaveURL(/decade=/);
  await expect(page.getByTestId("filter-chip")).toHaveCount(0);
  await expect(page.getByTestId("poster-tile").first()).toBeVisible();
});
