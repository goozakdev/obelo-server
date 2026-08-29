import { useEffect, useState } from "react";
import { apiClient } from "../api/client";

// How many things need fixing in each Library, for the queue's Library selector.
//
// Without it the selector is a blind guess: the Admin has to open each Library in
// turn to discover which one actually has work in it. With it, "where is the work?"
// is answered before the first click.
//
// It costs four reads per Library (the same four lists the queue folds together —
// there is no server-side count endpoint, and adding one to save a badge is not
// worth the API surface). That is fine here and nowhere else: this is an Admin
// screen on a household server with a handful of Libraries, the reads are already
// the ones the queue makes, and it runs exactly once per mount.
//
// Every part of it is best-effort. A Library whose counts fail to load is simply
// absent from the map and renders with no badge — a failed count must never stop
// the Admin from selecting that Library and fixing things in it.

/** Open-problem counts keyed by Library id. A missing key means "not counted". */
export type FixCounts = Record<string, number>;

export function useFixCounts(libraryIds: string[]): FixCounts {
  const [counts, setCounts] = useState<FixCounts>({});
  // Depend on the ids' identity, not the array's: the caller rebuilds the array on
  // every render, and re-running this effect per render would loop the network.
  const key = libraryIds.join(",");

  useEffect(() => {
    if (key === "") return;
    const ctrl = new AbortController();
    const ids = key.split(",");

    void Promise.all(
      ids.map(async (id) => {
        try {
          const [unmatched, review, enrichment, overrides] = await Promise.all([
            apiClient.listUnmatched(id, ctrl.signal),
            apiClient.listNeedsReview(id, ctrl.signal),
            apiClient.listEnrichmentAttention(id, ctrl.signal),
            apiClient.listOverrides(id, ctrl.signal),
          ]);
          // Only ORPHANED corrections count as problems; the rest are finished work
          // (the queue makes the same split — see buildFixItems).
          const total =
            unmatched.length +
            review.length +
            enrichment.length +
            overrides.filter((o) => o.orphaned).length;
          if (!ctrl.signal.aborted) {
            setCounts((cur) => ({ ...cur, [id]: total }));
          }
        } catch {
          // Best-effort: no badge for this Library, and no error anywhere else.
        }
      }),
    );

    return () => ctrl.abort();
  }, [key]);

  return counts;
}
