import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient, type ApiClient } from "./api/client";
import { ApiError, NetworkError } from "./api/errors";
import type { ServerInfo } from "./api/types";

export type ServerState =
  | { status: "loading" }
  | { status: "ready"; info: ServerInfo }
  | { status: "unreachable"; message: string }
  | { status: "error"; code: string; message: string };

// Backoff between handshake retries while the server has not answered: doubles
// from the base to the cap.
const RETRY_BASE_MS = 1000;
const RETRY_MAX_MS = 30000;
// A re-read after the first success (refresh) retries at most this many times:
// the handshake in hand is still good, so this only narrows a stale window.
const REREAD_MAX_RETRIES = 5;

// useServerInfoHandshake runs the handshake (GET /api/v1/server) on mount and
// exposes a discriminated state the shell renders from. It distinguishes a
// reachable server (ready) from an unreachable one (NetworkError) from a
// server-side error (ApiError) so the UI can say something honest in each case.
//
// Until the server answers it keeps trying — with backoff, and at once when the
// browser comes back online or the tab regains focus — so a page loaded during a
// server restart recovers without a manual reload (otherwise `feature()` would
// read false for the whole tab). `refresh` re-reads on demand (after first-run
// setup, so `setupRequired` is not left stale); a failed re-read never replaces
// a handshake that already succeeded, and is retried with the same backoff for a
// bounded number of attempts.
export function useServerInfoHandshake(client: ApiClient = apiClient): {
  state: ServerState;
  refresh: () => void;
} {
  const [state, setState] = useState<ServerState>({ status: "loading" });
  const [nonce, setNonce] = useState(0);
  const ready = useRef(false);
  const failures = useRef(0);
  const bump = useCallback(() => setNonce((n) => n + 1), []);
  // The exposed refresh starts a fresh retry budget; the timer's own re-reads
  // (bump) keep spending the current one.
  const refresh = useCallback(() => {
    failures.current = 0;
    bump();
  }, [bump]);

  useEffect(() => {
    const controller = new AbortController();
    let active = true;
    let retry: ReturnType<typeof setTimeout> | null = null;

    const fail = (next: ServerState) => {
      if (!active) return;
      if (ready.current) {
        // A re-read that fails leaves the last good handshake standing, but is
        // still retried (bounded) so a blip does not leave it stale for good.
        if (failures.current >= REREAD_MAX_RETRIES) return;
      } else {
        setState(next);
      }
      const delay = Math.min(RETRY_BASE_MS * 2 ** failures.current, RETRY_MAX_MS);
      failures.current++;
      retry = setTimeout(bump, delay);
    };

    client
      .getServerInfo(controller.signal)
      .then((info) => {
        if (!active) return;
        ready.current = true;
        failures.current = 0;
        setState({ status: "ready", info });
      })
      .catch((err: unknown) => {
        if (!active) return;
        if (err instanceof DOMException && err.name === "AbortError") return;
        if (err instanceof NetworkError) {
          fail({ status: "unreachable", message: err.message });
        } else if (err instanceof ApiError) {
          fail({ status: "error", code: err.code, message: err.message });
        } else {
          fail({
            status: "error",
            code: "UNKNOWN",
            message: err instanceof Error ? err.message : String(err),
          });
        }
      });

    return () => {
      active = false;
      controller.abort();
      if (retry) clearTimeout(retry);
    };
  }, [client, nonce, bump]);

  useEffect(() => {
    const retryNow = () => {
      if (!ready.current) bump();
    };
    window.addEventListener("online", retryNow);
    window.addEventListener("focus", retryNow);
    return () => {
      window.removeEventListener("online", retryNow);
      window.removeEventListener("focus", retryNow);
    };
  }, [bump]);

  return { state, refresh };
}
