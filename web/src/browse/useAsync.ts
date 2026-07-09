import { useEffect, useState } from "react";
import { errorMessage } from "../screens/errorMessage";

// A tiny load-once async hook for the browse screens (library list, title
// detail). It owns the loading/error/data state and a render-readable error
// message (from the API error envelope, PRD user story 34). Aborts the in-flight
// request on unmount or when a dependency key changes, so a fast back-navigation
// never lands a stale result.
//
// Pagination (the grid) has its own hook (useTitleGrid) because it appends pages
// rather than replacing data; this one is for single-shot fetches.

export type AsyncState<T> =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; data: T };

/** Run `fn(signal)` once per change of `deps`, exposing loading/error/ready.
 *
 * With `opts.keepPreviousData`, a deps-triggered REFETCH keeps the previous ready
 * data on screen while the new request is in flight (stale-while-revalidate) instead
 * of flashing back to a loading state. The initial load (no data yet) still shows
 * loading. A parent detail screen uses this so an Admin edit that bumps a reload key
 * doesn't unmount the Edit-item picker — which would wipe its transient post-apply
 * cascade summary (item-editing/05). */
export function useAsync<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  deps: ReadonlyArray<unknown>,
  opts?: { keepPreviousData?: boolean },
): AsyncState<T> {
  const [state, setState] = useState<AsyncState<T>>({ status: "loading" });
  const keepPrevious = opts?.keepPreviousData ?? false;

  useEffect(() => {
    const ctrl = new AbortController();
    setState((prev) =>
      keepPrevious && prev.status === "ready" ? prev : { status: "loading" },
    );
    // async/await with a single try/catch keeps the rejection handled within one
    // continuation — a .then().catch() chain leaves the intermediate derived
    // promise transiently unhandled, which strict rejection detectors flag.
    void (async () => {
      try {
        const data = await fn(ctrl.signal);
        if (!ctrl.signal.aborted) setState({ status: "ready", data });
      } catch (err) {
        if (ctrl.signal.aborted || isAbort(err)) return;
        setState({ status: "error", message: errorMessage(err) });
      }
    })();
    return () => ctrl.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  return state;
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
