import { useCallback, useEffect, useRef } from "react";

// Infinite-scroll wiring for a paginated grid. Returns a CALLBACK REF to put on a
// scroll sentinel element below the grid; whenever that node mounts it attaches an
// IntersectionObserver that calls `onMore` as the sentinel scrolls into view, and
// detaches when the node unmounts.
//
// Why a callback ref (not a useRef + useEffect): the sentinel only renders once
// the first page has loaded, so a setup effect that runs on mount sees a null ref
// and — unless its dependency array happens to change when the sentinel appears —
// never re-runs to observe it. A callback ref fires exactly when the node mounts/
// unmounts, so the observer is wired the moment there is something to watch,
// independent of unrelated state changes. (This was the Movies-grid pagination
// bug: the grid was stuck at the first page because nothing ever observed the
// sentinel.)
//
// `onMore` is held in a ref so a changing identity doesn't tear down and re-create
// the observer; the latest callback is always invoked. Double-fetch protection
// (in-flight / hasMore guards) lives in `onMore` itself (the data hook's loadMore).

export function useInfiniteScrollSentinel(
  onMore: () => void,
  rootMargin = "400px",
): (node: HTMLElement | null) => void {
  const onMoreRef = useRef(onMore);
  useEffect(() => {
    onMoreRef.current = onMore;
  }, [onMore]);

  const observerRef = useRef<IntersectionObserver | null>(null);

  return useCallback(
    (node: HTMLElement | null) => {
      // Detach any previous observer (node changed or unmounted).
      observerRef.current?.disconnect();
      observerRef.current = null;
      if (!node) return;
      const obs = new IntersectionObserver(
        (entries) => {
          if (entries.some((e) => e.isIntersecting)) onMoreRef.current();
        },
        { rootMargin },
      );
      obs.observe(node);
      observerRef.current = obs;
    },
    [rootMargin],
  );
}
