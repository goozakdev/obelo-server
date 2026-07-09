import type { ErrorEnvelope } from "./types";

// ApiError is the single typed error the client raises for any non-2xx
// response. It carries the server's machine-readable `code`, the human
// `message`, the HTTP `status`, and any `details` — so callers branch on
// `code`/`status` (e.g. 401 → login) and show `message` to the user
// (PRD user story 34: API errors surfaced as readable messages).
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly details?: Record<string, unknown>;

  constructor(
    status: number,
    code: string,
    message: string,
    details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
  }

  /** True when the server rejected our credentials; the client clears the
   * session and routes to login on this (PRD auth model). */
  get isUnauthorized(): boolean {
    return this.status === 401;
  }
}

// NetworkError is raised when the request never reached a well-formed HTTP
// response — the server is unreachable, DNS failed, or fetch threw. The shell
// renders this as "server not reachable" rather than a server-side code.
export class NetworkError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "NetworkError";
  }
}

// parseErrorEnvelope turns a non-2xx Response into an ApiError. It tolerates a
// body that is not the standard envelope (e.g. a proxy's plain-text 502): in
// that case it synthesizes a generic code from the status, so callers always
// get an ApiError with a usable message.
export async function parseErrorEnvelope(res: Response): Promise<ApiError> {
  let code = `HTTP_${res.status}`;
  let message = res.statusText || `request failed with status ${res.status}`;
  let details: Record<string, unknown> | undefined;

  try {
    const body = (await res.json()) as Partial<ErrorEnvelope>;
    if (body && typeof body === "object" && body.error) {
      if (typeof body.error.code === "string") code = body.error.code;
      if (typeof body.error.message === "string") message = body.error.message;
      if (body.error.details) details = body.error.details;
    }
  } catch {
    // Body was empty or not JSON; keep the status-derived defaults.
  }

  return new ApiError(res.status, code, message, details);
}
