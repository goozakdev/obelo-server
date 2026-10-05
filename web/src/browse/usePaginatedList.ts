import { useCallback, useEffect, useRef, useState } from "react";
import { isAbort } from "../api/errors";
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

// Extra delay before the next refresh walk per page the last walk fetched beyond
// the first (a one-page list is never held back).
const REFRESH_COST_MS = 500;

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
  /** Re-fetch the currently-loaded window IN PLACE, without clearing the list
   * first: the refreshed pages REPLACE the loaded list (server order, so existing
   * items update in place and React reuses their DOM by key — no remount, no
   * flicker; new items slot in and removed items drop out) and the cursor
   * continues from where the walk stopped, so no stale tail is kept (D005). An
   * item pushed past the walked window reappears on the next loadMore. Silent (no
   * loading flag); a request made while another fetch is in flight is replayed
   * when that fetch settles. Drives live updates while a Library
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
  // stable identity the scroll observer can hold).
  const cursorRef = useRef<string | null>(null);
  const hasMoreRef = useRef(true);
  const inFlight = useRef(false);
  const abortRef = useRef<AbortController | null>(null);
  // refresh() bookkeeping: a refresh asked for while the list was busy (or still
  // cooling down from a deep walk) is remembered here and replayed, so the LAST
  // request always lands; cooldownMs is how long after a walk to hold the next
  // one, scaled by how many pages that walk cost.
  const pendingRefresh = useRef(false);
  const refreshTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const lastRefreshEnd = useRef(0);
  const cooldownMs = useRef(0);
  const requestRefreshRef = useRef<() => void>(() => {});
  // The accumulated items, mirrored to a ref so refresh() can read how far the
  // user has loaded (the window to re-fetch) without being a callback dependency.
  const itemsRef = useRef<T[]>(items);
  // Written together with the state by commitItems (never from an effect), so a
  // refresh replayed from load's finally already sees the page just appended.
  const commitItems = useCallback((next: T[]) => {
    itemsRef.current = next;
    setItems(next);
  }, []);

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
        // The new list is computed here from itemsRef (always the latest
        // committed list) rather than inside a setItems updater: an updater must
        // stay pure (StrictMode invokes it twice, and a ref-based de-dup set made
        // every page after the first vanish), and a refresh replayed from the
        // finally below must see this page, which a post-render effect would not.
        const base = isFirst ? [] : itemsRef.current;
        const seen = new Set(base.map((item) => getIdRef.current(item)));
        const next = base.slice();
        for (const item of page.items) {
          const id = getIdRef.current(item);
          if (seen.has(id)) continue; // no duplicates across pages
          seen.add(id);
          next.push(item);
        }
        commitItems(next);
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
        if (abortRef.current === ctrl) {
          inFlight.current = false;
          if (!ctrl.signal.aborted && pendingRefresh.current) requestRefreshRef.current();
        }
      }
    },
    [fetchPage, commitItems],
  );

  // (Re)load page one whenever the fetcher changes. Resetting the accumulated
  // state here (not inside load) keeps the reset atomic with the effect that
  // owns the lifecycle.
  useEffect(() => {
    cursorRef.current = null;
    hasMoreRef.current = true;
    // We're about to abort whatever was in flight and start over, so this is the
    // sole operation again — clear the guard (the aborted request's finally is
    // controller-gated and won't clobber the load below).
    inFlight.current = false;
    pendingRefresh.current = false;
    lastRefreshEnd.current = 0;
    cooldownMs.current = 0;
    commitItems([]);
    setCursor(null);
    setHasMore(true);
    setError(null);
    void load(null);
    return () => {
      abortRef.current?.abort();
      if (refreshTimer.current) clearTimeout(refreshTimer.current);
      refreshTimer.current = null;
    };
  }, [load, commitItems]);

  const loadMore = useCallback(() => {
    if (inFlight.current || !hasMoreRef.current) return;
    void load(cursorRef.current);
  }, [load]);

  const retry = useCallback(() => {
    void load(cursorRef.current);
  }, [load]);

  // refresh re-fetches the loaded window (page one onward, until it has covered
  // at least the items currently held or reached the end) and REPLACES the loaded
  // list with the walked pages, in server order, WITHOUT a blanking setItems([]);
  // the cursor continues from where the walk stopped, no stale tail kept (D005). It
  // single-flights against load/loadMore via the same inFlight guard; a request
  // that arrives while busy is remembered (pendingRefresh) and replayed when the
  // in-flight fetch settles, so the terminal libraryUpdated nudge is never lost.
  // A deep walk (many pages) also holds the next one for a cooldown proportional
  // to its cost, so a 400ms scan tick can't keep the server walking forever.
  // Silent: no loading flag, and a failed refresh leaves the existing list intact
  // (it's a background live-update, not a user action).
  const refresh = useCallback(async () => {
    if (inFlight.current) {
      pendingRefresh.current = true;
      return;
    }
    const wait = lastRefreshEnd.current + cooldownMs.current - Date.now();
    if (wait > 0) {
      pendingRefresh.current = true;
      if (!refreshTimer.current) {
        refreshTimer.current = setTimeout(() => {
          refreshTimer.current = null;
          if (pendingRefresh.current && !inFlight.current) requestRefreshRef.current();
        }, wait);
      }
      return;
    }
    pendingRefresh.current = false;
    inFlight.current = true;
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    const prev = itemsRef.current;
    const want = prev.length; // cover at least what the user has loaded
    try {
      const fresh: T[] = [];
      const ids = new Set<string>();
      let cursor: string | null = null;
      let pages = 0;
      do {
        const page = await fetchPage(cursor, ctrl.signal);
        if (ctrl.signal.aborted) return;
        pages++;
        for (const item of page.items) {
          const id = getIdRef.current(item);
          if (ids.has(id)) continue; // de-dup within the refreshed window
          ids.add(id);
          fresh.push(item);
        }
        cursor = page.nextCursor;
      } while (cursor && fresh.length < want);
      // The refreshed pages REPLACE the list and the cursor continues from where
      // the walk stopped. The walk always covers at least what was loaded, so the
      // list never shrinks; an item pushed past the window by an insertion is the
      // next page's head and returns on the next loadMore. Anything the walk lacks
      // inside the window was removed and drops.
      commitItems(fresh);
      setError(null); // a refresh that lands supersedes an earlier loadMore failure
      cooldownMs.current = (pages - 1) * REFRESH_COST_MS;
      cursorRef.current = cursor;
      setCursor(cursor);
      hasMoreRef.current = cursor !== null;
      setHasMore(cursor !== null);
    } catch (err) {
      if (ctrl.signal.aborted || isAbort(err)) return;
      // Swallow: a background refresh that fails leaves the list as-is.
    } finally {
      if (abortRef.current === ctrl) {
        inFlight.current = false;
        lastRefreshEnd.current = Date.now();
        if (!ctrl.signal.aborted && pendingRefresh.current) requestRefreshRef.current();
      }
    }
  }, [fetchPage, commitItems]);

  const requestRefresh = useCallback(() => void refresh(), [refresh]);
  requestRefreshRef.current = requestRefresh;

  return {
    items,
    loading,
    loadingMore,
    hasMore,
    error,
    loadMore,
    retry,
    refresh: requestRefresh,
  };
}
