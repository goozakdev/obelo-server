import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import type { NeedsReviewItem } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// Loads a Library's needs-review items (issue 07, fixed). These are the
// Movies / Episodes / Tracks / Shows the scanner filed from an uncertain identity
// parse (no year, or non-SxxExx episode numbering). The server now answers this
// in one Admin-only call — GET /libraries/{id}/needs-review — which descends into
// TV/Music for us and tags each Movie with its folder for inline fix-match.
//
// This replaces an earlier client-side page-walk that filtered the *browse*
// listing on `needsReview`. That walk silently returned NOTHING for TV and Music
// libraries: their listing endpoint returns Shows / Artists (`{ shows }` /
// `{ artists }`), not `{ titles }`, so the Title-shaped parse found zero items and
// every TV/Music library reported "Nothing needs review" regardless of state.

export interface NeedsReviewState {
  /** The Library's still-flagged needs-review items. */
  items: NeedsReviewItem[];
  /** True while the list is loading. */
  loading: boolean;
  /** Render-readable error from a failed load, if any. */
  error: string | null;
  /** Re-fetch the list. */
  reload: () => void;
}

/** Load `libraryId`'s needs-review items (all kinds), refetchable after a fix. */
export function useNeedsReview(libraryId: string): NeedsReviewState {
  const [items, setItems] = useState<NeedsReviewItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);

  const load = useCallback(async () => {
    abortRef.current?.abort();
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setLoading(true);
    setError(null);
    try {
      const list = await apiClient.listNeedsReview(libraryId, ctrl.signal);
      if (ctrl.signal.aborted) return;
      setItems(list);
    } catch (err) {
      if (ctrl.signal.aborted || isAbort(err)) return;
      setError(errorMessage(err));
    } finally {
      if (!ctrl.signal.aborted) setLoading(false);
    }
  }, [libraryId]);

  useEffect(() => {
    void load();
    return () => abortRef.current?.abort();
  }, [load]);

  const reload = useCallback(() => void load(), [load]);

  return { items, loading, error, reload };
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
