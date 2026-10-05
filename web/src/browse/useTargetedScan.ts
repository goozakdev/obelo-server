import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, apiClient } from "../api/client";
import type { TargetedScanEntity } from "../api/types";
import { appEvents, type ScanProgress } from "../events/enrichEvents";
import { errorMessage } from "../screens/errorMessage";

// The detail-page Targeted scan (ADR-0030): an Admin re-walks just the entity
// they're viewing (this Movie / Show / Album / Artist), rather than its whole
// Library. Like the full-Library scan the trigger is asynchronous (202 with a
// running status), so completion arrives over the shared scanProgress SSE stream
// — the same channel useLibraryLiveRefresh listens on. This hook owns that little
// lifecycle for one detail screen: fire the scan, flip to "scanning" until the
// terminal event for its Library lands, then report the "what changed" delta and
// nudge the caller to refetch its detail in place.
//
// It learns the Library to watch from the scan's own 202 response (which carries
// libraryId), so a caller that doesn't already hold the Library id — an Album or
// Artist detail — needs no extra plumbing.

/** How long a settled result message lingers before clearing itself (ms), so it
 * reads as a transient toast-like note rather than sticking around. */
const MESSAGE_LINGER_MS = 6000;

/** How long a scan may stay "scanning" with no terminal event (SSE down) before
 * the controller gives up waiting and re-enables the trigger (ms). */
const SCAN_TIMEOUT_MS = 5 * 60 * 1000;

export interface TargetedScanController {
  /** True from the moment a scan is triggered until its terminal event lands. */
  scanning: boolean;
  /** A transient note: the settled delta, or a readable error. Auto-clears. */
  message: string | null;
  /** Trigger a Targeted scan of one entity (Admin-only, enforced server-side). */
  scan: (entityType: TargetedScanEntity, id: string) => void;
}

/** Drive a detail page's Targeted scan. `onScanned` runs once the scan settles so
 * the screen refetches (read through a ref, so a changing identity doesn't
 * resubscribe the SSE listener — mirrors useLibraryLiveRefresh). */
export function useTargetedScan(onScanned: () => void): TargetedScanController {
  const [scanning, setScanning] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const onScannedRef = useRef(onScanned);
  useEffect(() => {
    onScannedRef.current = onScanned;
  }, [onScanned]);

  // The SSE listener reads these through refs so it stays out of the effect's
  // dependency list (a resubscribe mid-scan could drop the terminal event).
  const scanningRef = useRef(scanning);
  scanningRef.current = scanning;
  // The Library to watch, learned from the scan's 202 response; null until then
  // (or if the terminal event races ahead of it — see below).
  const libraryIdRef = useRef<string | null>(null);

  // Show a note and schedule it to clear itself; a newer note supersedes it.
  const clearTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const flash = useCallback((text: string) => {
    if (clearTimer.current) clearTimeout(clearTimer.current);
    setMessage(text);
    clearTimer.current = setTimeout(() => setMessage(null), MESSAGE_LINGER_MS);
  }, []);
  // The give-up timer for a scan whose terminal event never arrives, and any
  // terminal events seen before the 202 said which Library is ours.
  const scanTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const earlyTerminals = useRef<ScanProgress[]>([]);
  useEffect(
    () => () => {
      if (clearTimer.current) clearTimeout(clearTimer.current);
      if (scanTimer.current) clearTimeout(scanTimer.current);
    },
    [],
  );

  // A terminal scanProgress ends the scan and carries the delta. We match on the
  // Library learned from the 202; if that response hasn't landed yet (a tiny
  // no-change scan can finish first), a null libraryId means "accept the next
  // terminal event" — we only get here while OUR scan is the one in flight.
  const settle = useCallback(
    (p: ScanProgress) => {
      if (scanTimer.current) clearTimeout(scanTimer.current);
      scanTimer.current = null;
      earlyTerminals.current = [];
      setScanning(false);
      const added = p.added ?? 0;
      const removed = p.removed ?? 0;
      flash(
        added === 0 && removed === 0
          ? "Scan complete — no changes"
          : `Scan complete — added ${added} · removed ${removed}`,
      );
      onScannedRef.current();
    },
    [flash],
  );
  useEffect(() => {
    return appEvents.subscribe((type, data) => {
      if (type !== "scanProgress" || !data || !scanningRef.current) return;
      const p = data as ScanProgress;
      if (!p.complete) return;
      // Until the 202 names our Library we can't tell whose terminal event this
      // is: hold it and let the 202 handler claim a match.
      if (libraryIdRef.current === null) {
        earlyTerminals.current.push(p);
        return;
      }
      if (p.libraryId !== libraryIdRef.current) return;
      settle(p);
    });
  }, [settle]);

  const scan = useCallback(
    (entityType: TargetedScanEntity, id: string) => {
      setScanning(true);
      setMessage(null);
      libraryIdRef.current = null;
      earlyTerminals.current = [];
      // If the SSE stream is down no terminal event ever arrives; don't leave the
      // Scan item disabled forever.
      if (scanTimer.current) clearTimeout(scanTimer.current);
      scanTimer.current = setTimeout(() => {
        scanTimer.current = null;
        setScanning(false);
        flash("Scan is taking longer than expected — check back shortly");
      }, SCAN_TIMEOUT_MS);
      apiClient.scanEntity(entityType, id).then(
        (status) => {
          libraryIdRef.current = status.libraryId;
          // A tiny no-change scan can finish before this response lands.
          const early = earlyTerminals.current.find((p) => p.libraryId === status.libraryId);
          earlyTerminals.current = [];
          if (early && scanningRef.current) settle(early);
        },
        (err: unknown) => {
          if (scanTimer.current) clearTimeout(scanTimer.current);
          scanTimer.current = null;
          setScanning(false);
          flash(
            err instanceof ApiError && err.code === "NO_FILES"
              ? "Nothing to scan — this item has no files on disk"
              : errorMessage(err),
          );
        },
      );
    },
    [flash, settle],
  );

  return { scanning, message, scan };
}
