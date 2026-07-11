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
//
// `reobserveKey` closes the "stalled while still on-screen" gap: an
// IntersectionObserver only fires on intersection *transitions*, so when a
// freshly-loaded page doesn't grow the list enough to push the sentinel out of
// view (a big library on a wide window — the first page or two don't overflow the
// viewport), no transition happens and loading stalls until the user scrolls or
// resizes. Pass a value that changes each time content settles (e.g. the loaded
// item count); when it changes we re-deliver the sentinel's CURRENT intersection
// (unobserve→observe), so loadMore keeps firing until the sentinel is genuinely
// off-screen (or loadMore no-ops because there are no more pages).

export function useInfiniteScrollSentinel(
  onMore: () => void,
  reobserveKey?: unknown,
  rootMargin = "400px",
): (node: HTMLElement | null) => void {
  const onMoreRef = useRef(onMore);
  useEffect(() => {
    onMoreRef.current = onMore;
  }, [onMore]);

  const nodeRef = useRef<HTMLElement | null>(null);
  const observerRef = useRef<IntersectionObserver | null>(null);

  const setRef = useCallback(
    (node: HTMLElement | null) => {
      // Detach any previous observer (node changed or unmounted).
      observerRef.current?.disconnect();
      observerRef.current = null;
      nodeRef.current = node;
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

  // After each content settle, re-deliver the sentinel's current intersection so
  // a page that left it on-screen keeps loading (see reobserveKey note above). A
  // no-op transition (sentinel now off-screen) simply doesn't fire onMore.
  useEffect(() => {
    const node = nodeRef.current;
    const obs = observerRef.current;
    if (!node || !obs) return;
    obs.unobserve(node);
    obs.observe(node);
  }, [reobserveKey]);

  return setRef;
}
