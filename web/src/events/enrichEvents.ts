import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";

// Realtime live-update spine (ADR-0016, realtime-events feature). The server
// publishes a single SSE stream (GET /events) carrying several event types; this
// module is the browser-side fan-out so the whole app shares ONE EventSource
// (the first subscriber opens it, the last closes it) and layers typed hooks on
// top:
//   - `appEvents` is the singleton hub forwarding every (type, data) it receives;
//   - `useEnrichmentActivity` exposes "is an Enrichment pass running" for the
//     header indicator;
//   - `useLibraryLiveRefresh` calls a refresh callback when a Library's contents
//     change — scan progress (Titles appearing as they're indexed), enrichment
//     progress (posters/fields landing), or a `libraryUpdated` nudge — so an open
//     grid live-updates without a manual reload.
//
// Started as the enrichProgress-only spine (external-metadata-enrichment 02);
// broadened to the full event surface when the web app began consuming
// scanProgress / libraryUpdated (realtime-events web slice).

/** The enrichProgress payload (a running Enrichment pass over a Library). */
export interface EnrichProgress {
  libraryId: string;
  total: number;
  done: number;
  matched: number;
  unmatched: number;
  failed: number;
  disabled: number;
  complete: boolean;
}

/** The `scanProgress` SSE payload: a snapshot of a running (or just-finished)
 * scan of one Library. `complete` marks the terminal event; for a Targeted scan
 * (ADR-0030) that terminal event also carries `scope` (the entity label) and the
 * `added`/`removed` "what changed" delta — both 0/absent for a full Library scan. */
export interface ScanProgress {
  libraryId: string;
  titlesFound: number;
  filesFound: number;
  complete: boolean;
  scope?: string;
  added?: number;
  removed?: number;
}

/** A raw event off the SSE stream: its name plus the parsed JSON payload. */
type Listener = (type: string, data: unknown) => void;

class EventsHub {
  private listeners = new Set<Listener>();
  private unsubscribe: (() => void) | null = null;

  /** Register a listener, opening the shared EventSource on the first one. The
   * returned fn removes the listener and closes the stream when none remain.
   * Every event the stream delivers is fanned out to every listener verbatim;
   * each listener filters for the types and Library it cares about. */
  subscribe(fn: Listener): () => void {
    this.listeners.add(fn);
    // Guard: some component tests partial-mock the apiClient singleton without
    // subscribeEvents. Treat its absence as "no realtime here" — the hooks still
    // work, they just never receive events (the polling fallback covers state).
    if (!this.unsubscribe && typeof apiClient.subscribeEvents === "function") {
      this.unsubscribe = apiClient.subscribeEvents((type, data) => {
        for (const l of this.listeners) l(type, data);
      });
    }
    return () => {
      this.listeners.delete(fn);
      if (this.listeners.size === 0 && this.unsubscribe) {
        this.unsubscribe();
        this.unsubscribe = null;
      }
    };
  }
}

export const appEvents = new EventsHub();

/** True while an Enrichment pass is running (optionally scoped to one Library).
 * Goes false on the terminal `complete` event. Drives the header indicator. */
export function useEnrichmentActivity(libraryId?: string): boolean {
  const [active, setActive] = useState(false);
  useEffect(() => {
    return appEvents.subscribe((type, data) => {
      if (type !== "enrichProgress" || !data) return;
      const p = data as EnrichProgress;
      if (libraryId && p.libraryId !== libraryId) return;
      setActive(!p.complete);
    });
  }, [libraryId]);
  return active;
}

// The event types that signal a Library's browsable contents may have changed:
// scanProgress (new Titles indexed), enrichProgress (metadata/artwork written),
// and the libraryUpdated "go refetch" nudge fired at each completion point.
const LIBRARY_CHANGE_TYPES = new Set(["scanProgress", "enrichProgress", "libraryUpdated"]);

// Debounce window for non-terminal progress ticks: a busy scan/enrich emits many
// events a second; we coalesce them into at most one refresh per interval so the
// grid stays live without refetching on every tick. A terminal event (a progress
// `complete`, or any libraryUpdated nudge) bypasses the debounce so the final
// authoritative state always lands.
const REFRESH_DEBOUNCE_MS = 400;

/** Calls `onRefresh` when the given Library's contents change while a grid is
 * open: as Titles are indexed during a scan, as Enrichment writes metadata/
 * artwork, and on the terminal `libraryUpdated` nudge. Progress ticks are
 * debounced; terminal events fire immediately. The callback should be a stable
 * identity (e.g. a list's `refresh`) so this doesn't resubscribe every render. */
export function useLibraryLiveRefresh(libraryId: string, onRefresh: () => void): void {
  // onRefresh is read through a ref so a changing identity doesn't resubscribe
  // (and reset the debounce); only libraryId does.
  const cb = useRef(onRefresh);
  useEffect(() => {
    cb.current = onRefresh;
  }, [onRefresh]);

  useEffect(() => {
    let last = 0;
    return appEvents.subscribe((type, data) => {
      if (!LIBRARY_CHANGE_TYPES.has(type) || !data) return;
      const p = data as { libraryId?: string; complete?: boolean };
      if (p.libraryId !== libraryId) return;
      // libraryUpdated is itself a terminal "refetch now" nudge; a progress event
      // is terminal once it reports complete.
      const terminal = type === "libraryUpdated" || p.complete === true;
      const now = Date.now();
      if (terminal || now - last > REFRESH_DEBOUNCE_MS) {
        last = now;
        cb.current();
      }
    });
  }, [libraryId]);
}
