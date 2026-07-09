import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { ScanStatus } from "../api/types";

// The per-Library scan-status poller (issue 06). There is no SSE in v1, so the
// admin hub watches a running scan by polling GET /libraries/{id}/scan on a
// short interval and STOPS the moment the scan settles (idle/error). Keeping the
// poll loop here — one owner per library row — means the screen just reads the
// current status and renders running → idle/error + counts.
//
// Lifecycle:
//   - `status` holds the latest scan status (null until the first read lands).
//   - `refresh()` does one immediate read (used right after the initial mount so
//     a row that was already mid-scan starts polling without waiting a tick).
//   - `begin(initial)` seeds the status from a scan trigger's response and, if it
//     came back "running", starts the interval; a fast synchronous scan that is
//     already "idle" needs no polling.
//   - The interval clears itself once a poll returns a non-running state, and on
//     unmount, so a settled (or removed) library never keeps fetching.

/** How often a running scan is re-polled (ms). ~1.5s per the PRD's polling note —
 * frequent enough to feel live, light enough not to hammer the server. */
export const SCAN_POLL_INTERVAL_MS = 1500;

export interface ScanStatusController {
  /** Latest status, or null before the first read resolves. */
  status: ScanStatus | null;
  /** A poll/refresh error message (the status read failed), or null. */
  error: string | null;
  /** Seed the status from a trigger response; starts polling iff it is running. */
  begin(initial: ScanStatus): void;
  /** Read the status once now (and start polling if it turns out to be running). */
  refresh(): void;
}

/** Poll the scan status for one Library. `enabled` lets a screen mount the hook
 * lazily (false → it neither reads nor polls). The poll interval is injectable
 * for tests (fake timers). */
export function useScanStatus(
  libraryId: string,
  opts: { enabled?: boolean; intervalMs?: number } = {},
): ScanStatusController {
  const { enabled = true, intervalMs = SCAN_POLL_INTERVAL_MS } = opts;
  const [status, setStatus] = useState<ScanStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;

  const stop = useCallback(() => {
    if (timerRef.current !== null) {
      clearInterval(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  // One status read. On a settled state (anything other than "running") it stops
  // the interval; on "running" it ensures the interval is ticking.
  const poll = useCallback(async () => {
    if (!enabledRef.current) return;
    try {
      const next = await apiClient.getScanStatus(libraryId);
      if (!enabledRef.current) return;
      setStatus(next);
      setError(null);
      if (next.state !== "running") stop();
    } catch (err) {
      if (!enabledRef.current) return;
      // A failed status read shouldn't spin forever: surface it and stop polling.
      setError(errorMessage(err));
      stop();
    }
  }, [libraryId, stop]);

  const ensurePolling = useCallback(() => {
    if (timerRef.current === null && enabledRef.current) {
      timerRef.current = setInterval(() => void poll(), intervalMs);
    }
  }, [poll, intervalMs]);

  const begin = useCallback(
    (initial: ScanStatus) => {
      setStatus(initial);
      setError(null);
      if (initial.state === "running") ensurePolling();
      else stop();
    },
    [ensurePolling, stop],
  );

  const refresh = useCallback(() => {
    void (async () => {
      if (!enabledRef.current) return;
      try {
        const next = await apiClient.getScanStatus(libraryId);
        if (!enabledRef.current) return;
        setStatus(next);
        setError(null);
        if (next.state === "running") ensurePolling();
        else stop();
      } catch (err) {
        if (!enabledRef.current) return;
        setError(errorMessage(err));
        stop();
      }
    })();
  }, [libraryId, ensurePolling, stop]);

  // On mount (when enabled), do one read so a library that is already mid-scan
  // — e.g. another admin/the scheduled scan kicked it off — begins polling on
  // its own. Tear the interval down on unmount or when disabled.
  useEffect(() => {
    if (!enabled) {
      stop();
      return;
    }
    refresh();
    return () => stop();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [libraryId, enabled]);

  return { status, error, begin, refresh };
}
