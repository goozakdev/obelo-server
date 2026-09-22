import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

// The local TMDB/MusicBrainz stub boot-server.mjs runs answers 400 on a
// malformed %-escape in /recording/{id} (boot-server.mjs's decodeURIComponent
// guard) — a path no server-under-test request ever reaches, so nothing else in
// this suite exercised it. The stub listens on an ephemeral port only
// boot-server.mjs knows; it writes that URL to .tmdb-stub-url (mirroring
// .claim-token) for this spec to read.

const here = dirname(fileURLToPath(import.meta.url));
const STUB_URL_FILE = join(here, ".tmdb-stub-url");

function readStubURL(): string {
  const url = readFileSync(STUB_URL_FILE, "utf8").trim();
  if (!url) throw new Error(`${STUB_URL_FILE} is empty`);
  return url;
}

test("TMDB stub answers 400 on a malformed %-escape and keeps serving after", async ({
  request,
}) => {
  const stubURL = readStubURL();

  // "%zz" is not a valid percent-escape — decodeURIComponent throws a URIError
  // boot-server.mjs must catch and turn into a 400, not a crashed stub.
  const bad = await request.get(`${stubURL}/recording/%zz`);
  expect(bad.status()).toBe(400);
  const badBody = await bad.json();
  expect(badBody.error).toBeTruthy();

  // The stub must still be answering afterwards — the malformed request must
  // not have taken the process down.
  const ok = await request.get(`${stubURL}/recording/mb-rec`);
  expect(ok.status()).toBe(200);
  const okBody = await ok.json();
  expect(okBody.id).toBe("mb-rec");
});
