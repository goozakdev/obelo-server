// Test helpers for the component tests. The PRD's "one seam" is the typed API
// client; components fake it. The cleanest fake is a REAL ApiClient wired to a
// fake fetch returning canned JSON — that exercises the client's request shaping
// AND the omitempty/RFC3339 normalize layer alongside the component, so the
// tests assert the user-observable result of the whole client→component path.

import { ApiClient } from "../api/client";
import { memoryTokenStore } from "../api/token";

/** A route table keyed by "METHOD path" (path WITHOUT the /api/v1 prefix, with
 * the query string). The value produces a fake Response. */
export type Routes = Record<string, () => Response>;

/** JSON Response with status 200 (or a given status). */
export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** An error-envelope Response (the server's standard error shape). */
export function errorResponse(
  status: number,
  code: string,
  message: string,
): Response {
  return jsonResponse({ error: { code, message } }, status);
}

/** Build an ApiClient whose fetch is served from a route table. `path` is matched
 * after stripping the "/api/v1" prefix and the leading origin, so routes read
 * like "GET /libraries". A handler may be a function (called per request, so it
 * can return a different page each call) for pagination tests. */
export function fakeClient(routes: Routes): ApiClient {
  const fetchImpl: typeof fetch = async (input, init) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = (init?.method ?? "GET").toUpperCase();
    const path = url.replace(/^.*\/api\/v1/, "");
    const key = `${method} ${path}`;
    const handler = routes[key];
    if (!handler) {
      return errorResponse(404, "NOT_FOUND", `no fake route for ${key}`);
    }
    return handler();
  };

  return new ApiClient({
    fetchImpl,
    tokenStore: memoryTokenStore("fake-token"),
  });
}
