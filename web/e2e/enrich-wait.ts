import { expect, type APIRequestContext } from "@playwright/test";

// isPlainObject rejects null, arrays, and scalars — `typeof x === "object"`
// alone lets `[]` and `null` through, which then read as "no lastPass yet"
// instead of failing loudly.
function isPlainObject(v: unknown): boolean {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

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
  const beforeText = await before.text();
  expect(before.ok(), `enrich status: ${before.status()} ${beforeText}`).toBeTruthy();
  let beforeJson;
  try {
    beforeJson = JSON.parse(beforeText);
  } catch (err) {
    throw new Error(
      `enrich status on library ${libId} returned a non-JSON 200 body: ${before.status()} ${beforeText} (${err})`,
    );
  }
  if (!isPlainObject(beforeJson)) {
    throw new Error(
      `enrich status on library ${libId} returned a JSON body that isn't an object: ${before.status()} ${beforeText}`,
    );
  }
  const baselineFinishedAt = beforeJson?.lastPass?.finishedAt ?? null;

  const qs = opts?.mode ? `?mode=${encodeURIComponent(opts.mode)}` : "";
  const start = await request.post(`/api/v1/libraries/${libId}/enrich${qs}`, { headers: auth });
  const startText = await start.text();
  expect(start.ok(), `enrich: ${start.status()} ${startText}`).toBeTruthy();
  let startBody;
  try {
    startBody = JSON.parse(startText);
  } catch (err) {
    throw new Error(
      `enrich start on library ${libId} returned a non-JSON 200 body: ${start.status()} ${startText} (${err})`,
    );
  }
  if (!isPlainObject(startBody)) {
    throw new Error(
      `enrich start on library ${libId} returned a JSON body that isn't an object: ${start.status()} ${startText}`,
    );
  }
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
      let status;
      try {
        status = JSON.parse(lastBody);
      } catch (err) {
        throw new Error(
          `enrich status on library ${libId} returned a non-JSON 200 body: ${lastStatus} ${lastBody} (${err})`,
        );
      }
      if (!isPlainObject(status)) {
        throw new Error(
          `enrich status on library ${libId} returned a JSON body that isn't an object: ${lastStatus} ${lastBody}`,
        );
      }
      if (status.state === "idle" && status.lastPass) {
        // A truthy `lastPass` that isn't a plain object (e.g. a bare string
        // or number) can't be compared against the baseline or read back by
        // the caller — fail loudly instead of forwarding it as though it
        // were a finished pass's counts.
        if (!isPlainObject(status.lastPass)) {
          throw new Error(
            `enrich status on library ${libId} returned a lastPass that isn't an object: ${lastStatus} ${lastBody}`,
          );
        }
        if (status.lastPass.finishedAt !== baselineFinishedAt) {
          return status.lastPass;
        }
      }
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(
    `enrich pass on library ${libId} did not settle within timeout (last status GET: ${lastStatus} ${lastBody})`,
  );
}
