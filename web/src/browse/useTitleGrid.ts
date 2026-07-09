import { useCallback } from "react";
import type { ApiClient } from "../api/client";
import type { TitleSort, TitleSummary } from "../api/types";
import { usePaginatedList } from "./usePaginatedList";

// The Movie title grid's data hook (issue 03): cursor pagination over
// GET /libraries/{id}/titles. It is a thin adapter over the generic
// usePaginatedList engine — it supplies a page-fetcher bound to listTitles (with
// the current library/sort/pageSize) and an id accessor; the engine owns append,
// de-dup, first-vs-subsequent loading, reset-on-change, abort, and the in-place
// `refresh`. Because the fetcher's identity changes when library/sort/pageSize
// change, the engine resets to page one — i.e. changing sort RE-FETCHES from
// scratch (a new ordering invalidates the old cursor). A live update during a
// scan/enrich pass instead calls `refresh` (merge in place, no reset/flicker).

export interface TitleGridState {
  titles: TitleSummary[];
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
  /** Merge a fresh copy of the loaded window in place (no reset/flicker). The
   * grid calls this on a scan/enrich live-update so newly-indexed Titles appear
   * and freshly-enriched ones update without a manual reload. */
  refresh: () => void;
}

export function useTitleGrid(
  client: ApiClient,
  libraryId: string,
  sort: TitleSort,
  pageSize?: number,
): TitleGridState {
  const fetchPage = useCallback(
    async (cursor: string | null, signal: AbortSignal) => {
      const page = await client.listTitles(
        libraryId,
        { cursor, sort, limit: pageSize },
        signal,
      );
      return { items: page.titles, nextCursor: page.nextCursor };
    },
    [client, libraryId, sort, pageSize],
  );

  const { items, loading, loadingMore, hasMore, error, loadMore, retry, refresh } =
    usePaginatedList(fetchPage, getTitleId);

  return { titles: items, loading, loadingMore, hasMore, error, loadMore, retry, refresh };
}

function getTitleId(t: TitleSummary): string {
  return t.id;
}
