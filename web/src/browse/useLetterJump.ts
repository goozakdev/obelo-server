import { useCallback, useEffect, useRef, useState } from "react";

// Jump-to-letter for a cursor-paginated, alphabetically-ordered poster grid. The
// library grids (Movies/TV/Music) render in alphabetical order but only load a
// window at a time (infinite scroll), so the item a letter-range button targets
// may not be in the DOM yet. This hook scrolls to the first item at/after a
// target letter, pulling further pages on demand until that item exists (or the
// list ends).
//
// The target mirrors the backend sort key: lower-cased and article-stripped
// ("The Matrix" files under M) — see sortKey in internal/store and the scanner's
// sortTitle — so the buttons land where the server actually ordered things.

const ARTICLES = ["the ", "an ", "a "];

/** First letter of an item's sort key: lower-cased, leading article stripped
 * (longest match first so "an" wins over "a"), matching the backend sortTitle. */
export function sortFirstChar(title: string): string {
  let s = title.toLowerCase().trim();
  for (const article of ARTICLES) {
    if (s.startsWith(article)) {
      s = s.slice(article.length).trim();
      break;
    }
  }
  return s.charAt(0);
}

/** The pager surface useLetterJump needs — the shared shape of every grid's
 * paginated-list state (usePaginatedList / useTitleGrid). */
export interface LetterJumpPager {
  loadMore: () => void;
  hasMore: boolean;
  loading: boolean;
  loadingMore: boolean;
}

export interface LetterJump {
  /** Attach to the poster-grid <ul>; the hook scrolls its children by index. */
  gridRef: React.RefObject<HTMLUListElement>;
  /** Scroll to the first item whose sort key is at/after `startChar` (lower-
   * case), loading more pages first if it isn't loaded yet. */
  jumpTo: (startChar: string) => void;
}

export function useLetterJump<T>(
  items: T[],
  name: (item: T) => string,
  pager: LetterJumpPager,
): LetterJump {
  const gridRef = useRef<HTMLUListElement>(null);
  // The pending target letter, or null when idle. Set by jumpTo; cleared once
  // the effect has scrolled (or run off the end of the list).
  const [target, setTarget] = useState<string | null>(null);

  // name is usually an inline accessor (fresh identity each render); hold it in
  // a ref so it isn't an effect dependency that churns the jump loop.
  const nameRef = useRef(name);
  useEffect(() => {
    nameRef.current = name;
  }, [name]);

  const jumpTo = useCallback((startChar: string) => {
    setTarget(startChar);
  }, []);

  const { loadMore, hasMore, loading, loadingMore } = pager;

  // Drive the jump. Re-runs as items grow / paging settles, so a target beyond
  // the loaded window resolves once the pages it needs have landed.
  useEffect(() => {
    if (target === null || loading) return;
    const grid = gridRef.current;
    const idx = items.findIndex(
      (it) => sortFirstChar(nameRef.current(it)) >= target,
    );
    if (idx >= 0) {
      scrollToChild(grid, idx);
      setTarget(null);
      return;
    }
    // Every loaded item sorts before the target — pull the next page and let
    // this effect re-fire when it arrives. loadMore no-ops while a fetch is in
    // flight, and the loadingMore dependency re-runs us when it settles.
    if (hasMore) {
      if (!loadingMore) loadMore();
      return;
    }
    // Reached the end without a match (e.g. no V–Z items) — land on the last
    // item so the click still moves the user to the bottom of the list.
    if (items.length > 0) scrollToChild(grid, items.length - 1);
    setTarget(null);
  }, [target, items, hasMore, loading, loadingMore, loadMore]);

  return { gridRef, jumpTo };
}

function scrollToChild(grid: HTMLUListElement | null, index: number) {
  const el = grid?.children[index] as HTMLElement | undefined;
  el?.scrollIntoView({ behavior: "smooth", block: "start" });
}
