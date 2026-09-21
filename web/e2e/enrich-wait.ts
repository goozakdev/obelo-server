import { expect, type APIRequestContext } from "@playwright/test";

// waitEnrichPass — the shared "start a pass, wait for it to finish, read its
// counts" step for specs against POST/GET /libraries/{id}/enrich
// (internal/api/enrich_handlers.go). The endpoint is asynchronous: POST answers
// 202 with a pass HANDLE (no counts), and the finished summary is read back from
// GET's `lastPass` once `state` returns to "idle".
//
// To avoid reading a `lastPass` an EARLIER pass over this Library left behind
// (another spec's seed, or a previous call in the same beforeAll), this reads
// the status BEFORE posting and remembers that pass's `finishedAt` (or its
// absence); it then only accepts a `lastPass` whose `finishedAt` differs from
// that baseline, i.e. one that finished strictly after this call started.
export async function waitEnrichPass(
  request: APIRequestContext,
  auth: Record<string, string>,
  libId: string,
  opts?: { mode?: string; timeoutMs?: number },
): Promise<{
  total: number;
  matched: number;
  unmatched: number;
  failed: number;
  disabled: number;
  retrying: number;
}> {
  const before = await request.get(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
  expect(before.ok(), `enrich status: ${before.status()} ${await before.text()}`).toBeTruthy();
  const baselineFinishedAt = (await before.json()).lastPass?.finishedAt ?? null;

  const qs = opts?.mode ? `?mode=${encodeURIComponent(opts.mode)}` : "";
  const start = await request.post(`/api/v1/libraries/${libId}/enrich${qs}`, { headers: auth });
  expect(start.ok(), `enrich: ${start.status()} ${await start.text()}`).toBeTruthy();
  const startBody = await start.json();
  // `started: false` means the POST found a pass ALREADY running and reported
  // that one instead of starting a new one — its counts belong to whichever
  // caller actually started it, not to this call. Fail loudly rather than hand
  // back somebody else's numbers.
  expect(
    startBody.started,
    `enrich on library ${libId} did not start a new pass — one was already running: ${JSON.stringify(startBody)}`,
  ).toBeTruthy();

  const deadline = Date.now() + (opts?.timeoutMs ?? 15_000);
  let lastStatus = -1;
  let lastBody = "";
  while (Date.now() < deadline) {
    const res = await request.get(`/api/v1/libraries/${libId}/enrich`, { headers: auth });
    lastStatus = res.status();
    lastBody = await res.text();
    if (res.ok()) {
      const status = JSON.parse(lastBody);
      if (
        status.state === "idle" &&
        status.lastPass &&
        status.lastPass.finishedAt !== baselineFinishedAt
      ) {
        return status.lastPass;
      }
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(
    `enrich pass on library ${libId} did not settle within timeout (last status GET: ${lastStatus} ${lastBody})`,
  );
}
