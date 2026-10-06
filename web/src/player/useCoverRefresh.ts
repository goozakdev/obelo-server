import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { useLibraryLiveRefresh } from "../events/enrichEvents";

/** How long the library's change signal must go quiet before the playing track's
 * cover is re-read (a scan/enrich burst ticks every ~100 ms). */
export const COVER_REFRESH_QUIET_MS = 2_000;

/** The album cover identity re-read for a playing track: its album (a track can be
 * moved to another album) and that album's artwork version. */
export interface FreshCover {
  albumId?: string;
  version?: string;
}

/**
 * Keeps a playing Track's album cover current. The Title detail is read once per
 * track, so a cover re-picked (or a track moved to another album) while it plays
 * would only show from the next track. The Library's live-refresh signal (the one the
 * browse grids use) fires on every scan/enrich tick, so it only arms a trailing
 * debounce: one re-read after the burst has been quiet for COVER_REFRESH_QUIET_MS.
 *
 * `titleId` is the playing Track (null: none); `enabled` is whether it has an album
 * cover to refresh. Returns the fresh cover for THIS track, or undefined until a
 * re-read lands (callers fall back to the detail they already hold). Leaving a track
 * drops its fresh cover, so coming back to it shows the freshly read detail.
 */
export function useCoverRefresh(
  titleId: string | null,
  libraryId: string,
  enabled: boolean,
): FreshCover | undefined {
  const [fresh, setFresh] = useState<(FreshCover & { titleId: string }) | null>(null);
  const ctrl = useRef<AbortController | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      clearTimeout(timer.current);
      ctrl.current?.abort();
      setFresh(null);
    },
    [titleId],
  );
  const refresh = useCallback(() => {
    if (!titleId || !enabled) return;
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      ctrl.current?.abort();
      const c = new AbortController();
      ctrl.current = c;
      apiClient
        .getTitle(titleId, c.signal)
        .then((d) => {
          if (!c.signal.aborted)
            setFresh({
              titleId,
              albumId: d?.track?.albumId,
              version: d?.track?.albumArtworkVersion,
            });
        })
        .catch(() => {}); // keep the cover already shown
    }, COVER_REFRESH_QUIET_MS);
  }, [titleId, enabled]);
  useLibraryLiveRefresh(libraryId, refresh);
  return fresh && fresh.titleId === titleId ? fresh : undefined;
}
