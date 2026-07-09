import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage } from "../screens/errorMessage";

// The generic cursor-paginated-list data hook. It drives infinite scroll over
// any endpoint that returns one page at a time and APPENDS each page so the list
// grows with no duplicates and no gaps. It is the shared engine behind the Movie
// title grid (useTitleGrid), the TV Show grid, and the Music Artist list. It owns:
//
//   - the accumulated items, the current nextCursor, and the "more?" flag,
//   - de-dup by id (a defensive guard so a re-fired loadMore can never double an
//     item even if the server/cursor hiccups),
//   - first-page vs subsequent-page loading (so the grid shows a skeleton on
//     first load but an inline "loading more" on scroll),
//   - reset+refetch from page one whenever its inputs change (a new fetcher
//     identity — e.g. a new library or sort — invalidates the old cursor).
//
// Aborts in-flight requests on unmount / input change so a stale page can't
// append after the user has moved on. Double-fetch protection (in-flight /
// hasMore guards) lives in loadMore.

/** One page of results: the items plus the cursor for the next page (null when
 * this was the last page). */
export interface Page<T> {
  items: T[];
  nextCursor: string | null;
}

/** Fetch one page. `cursor` is null for the first page. Must honor `signal`. */
export type PageFetcher<T> = (
  cursor: string | null,
  signal: AbortSignal,
) => Promise<Page<T>>;

export interface PaginatedListState<T> {
  items: T[];
  /** First page is still loading (show the grid skeleton/empty-vs-loading). */
  loading: boolean;
  /** A subsequent page is loading (show an inline "loading more"). */
  loadingMore: boolean;
  /** True while more pages remain (nextCursor present). */
  hasMore: boolean;
  /** Render-readable error from the last failed fetch, if any. */
  error: string | null;
  /** Fetch the next page and append it. No-op while a fetch is in flight or when
   * there are no more pages. The grid calls this from its scroll sentinel. */
  loadMore: () => void;
  /** Retry after an error (re-fetches the page that failed). */
  retry: () => void;
  /** Re-fetch the currently-loaded window IN PLACE and merge it by id, without
   * clearing the list first: existing items update in place (so React reuses
   * their DOM by key — no remount, no flicker), newly-appearing items slot into
   * server order, and removed items drop out. Silent (no loading flag) and a
   * no-op while another fetch is in flight. Drives live updates while a Library
   * is scanning/enriching (realtime-events web slice) — distinct from the
   * destructive reset a new fetcher identity triggers (a different list). */
  refresh: () => void;
}

/**
 * @param fetchPage  the per-page fetcher. Pass a STABLE identity (useCallback);
 *   a new identity resets the list and refetches page one.
 * @param getId      id accessor for de-dup across pages.
 */
export function usePaginatedList<T>(
  fetchPage: PageFetcher<T>,
  getId: (item: T) => string,
): PaginatedListState<T> {
  const [items, setItems] = useState<T[]>([]);
  // cursorRef is the source of truth for reads; this state only exists to
  // trigger a re-render when the cursor advances, so the value isn't bound.
  const [, setCursor] = useState<string | null>(null);
  const [hasMore, setHasMore] = useState(true);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Refs the async callback reads without being a dependency (so loadMore is a
  // stable identity the scroll observer can hold). seenIds is the de-dup set.
  const cursorRef = useRef<string | null>(null);
  const hasMoreRef = useRef(true);
  const inFlight = useRef(false);
  const seenIds = useRef<Set<string>>(new Set());
  const abortRef = useRef<AbortController | null>(null);
  // The accumulated items, mirrored to a ref so refresh() can read how far the
  // user has loaded (the window to re-fetch) without being a callback dependency.
  const itemsRef = useRef<T[]>(items);
  useEffect(() => {
    itemsRef.current = items;
  }, [items]);

  // getId is read through a ref so it isn't a dependency of the load callback
  // (callers usually pass an inline accessor; a changing identity must not reset
  // the list — only a new fetchPage does that).
  const getIdRef = useRef(getId);
  useEffect(() => {
    getIdRef.current = getId;
  }, [getId]);

  const load = useCallback(
    async (useCursor: string | null) => {
      if (inFlight.current) return;
      inFlight.current = true;
      setError(null);
      const isFirst = useCursor === null;
      if (isFirst) setLoading(true);
      else setLoadingMore(true);

      const ctrl = new AbortController();
      abortRef.current = ctrl;
      try {
        const page = await fetchPage(useCursor, ctrl.signal);
        if (ctrl.signal.aborted) return;
        setItems((prev) => {
          const next = isFirst ? [] : prev.slice();
          if (isFirst) seenIds.current = new Set();
          for (const item of page.items) {
            const id = getIdRef.current(item);
            if (seenIds.current.has(id)) continue; // no duplicates across pages
            seenIds.current.add(id);
            next.push(item);
          }
          return next;
        });
        cursorRef.current = page.nextCursor;
        setCursor(page.nextCursor);
        hasMoreRef.current = page.nextCursor !== null;
        setHasMore(page.nextCursor !== null);
      } catch (err) {
        if (ctrl.signal.aborted || isAbort(err)) return;
        setError(errorMessage(err));
      } finally {
        if (!ctrl.signal.aborted) {
          setLoading(false);
          setLoadingMore(false);
        }
        // Only clear in-flight if WE are still the current operation. A reset /
        // refresh that superseded this request has already installed its own
        // controller and set inFlight; a superseded request must not clobber it.
        if (abortRef.current === ctrl) inFlight.current = false;
      }
    },
    [fetchPage],
  );

  // (Re)load page one whenever the fetcher changes. Resetting the accumulated
  // state here (not inside load) keeps the reset atomic with the effect that
  // owns the lifecycle.
  useEffect(() => {
    seenIds.current = new Set();
    cursorRef.current = null;
    hasMoreRef.current = true;
    // We're about to abort whatever was in flight and start over, so this is the
    // sole operation again — clear the guard (the aborted request's finally is
    // controller-gated and won't clobber the load below).
    inFlight.current = false;
    setItems([]);
    setCursor(null);
    setHasMore(true);
    setError(null);
    void load(null);
    return () => abortRef.current?.abort();
  }, [load]);

  const loadMore = useCallback(() => {
    if (inFlight.current || !hasMoreRef.current) return;
    void load(cursorRef.current);
  }, [load]);

  const retry = useCallback(() => {
    void load(cursorRef.current);
  }, [load]);

  // refresh re-fetches the loaded window (page one onward, until it has covered
  // at least the items currently held or reached the end) and merges the result
  // into the list by id, in server order, WITHOUT a blanking setItems([]). It
  // single-flights against load/loadMore via the same inFlight guard, so a busy
  // grid simply skips this tick (a later progress event or the terminal
  // libraryUpdated nudge refreshes once things settle). Silent: no loading flag,
  // and a failed refresh leaves the existing list intact (it's a background
  // live-update, not a user action).
  const refresh = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const prev = itemsRef.current;
    const want = prev.length; // cover at least what the user has loaded
    try {
      const fresh: T[] = [];
      const ids = new Set<string>();
      let cursor: string | null = null;
      do {
        const page = await fetchPage(cursor, ctrl.signal);
        if (ctrl.signal.aborted) return;
        for (const item of page.items) {
          const id = getIdRef.current(item);
          if (ids.has(id)) continue; // de-dup within the refreshed window
          ids.add(id);
          fresh.push(item);
        }
        cursor = page.nextCursor;
      } while (cursor && fresh.length < want);
      // Keep any previously-loaded items the refreshed window didn't re-cover
      // (e.g. an item pushed just past the window by a new insertion), in their
      // existing order, so nothing the user had scrolled to vanishes.
      const tail = prev.filter((item) => !ids.has(getIdRef.current(item)));
      for (const item of tail) ids.add(getIdRef.current(item));
      seenIds.current = ids;
      setItems(fresh.concat(tail));
      cursorRef.current = cursor;
      setCursor(cursor);
      hasMoreRef.current = cursor !== null;
      setHasMore(cursor !== null);
    } catch (err) {
      if (ctrl.signal.aborted || isAbort(err)) return;
      // Swallow: a background refresh that fails leaves the list as-is.
    } finally {
      if (abortRef.current === ctrl) inFlight.current = false;
    }
  }, [fetchPage]);

  return {
    items,
    loading,
    loadingMore,
    hasMore,
    error,
    loadMore,
    retry,
    refresh: useCallback(() => void refresh(), [refresh]),
  };
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
