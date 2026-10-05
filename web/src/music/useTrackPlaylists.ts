import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import type { PlaylistSummary } from "../api/types";

// The caller's playlists for a track list's row menus, loaded lazily and shared by
// every row: the first menu to open fetches, the rest reuse the answer, so a 20-row
// album costs one GET /playlists rather than one per opened menu. A result older
// than STALE_MS is fetched again on the next open, so a playlist created elsewhere
// shows up without a remount; a failed load is reported (not shown as an empty
// list) and `retry` runs it again.

const STALE_MS = 30_000;

export interface TrackPlaylists {
  /** The last successful answer; null until one lands. */
  playlists: PlaylistSummary[] | null;
  /** The most recent load failed. */
  failed: boolean;
  /** Load if nothing is loaded, in flight, or fresh. */
  ensure: () => void;
  /** Load again now (after a failure). */
  retry: () => void;
}

export function useTrackPlaylists(): TrackPlaylists {
  const [playlists, setPlaylists] = useState<PlaylistSummary[] | null>(null);
  const [failed, setFailed] = useState(false);
  const inflight = useRef<AbortController | null>(null);
  const loadedAt = useRef(0);

  const load = useCallback(() => {
    inflight.current?.abort();
    const ctrl = new AbortController();
    inflight.current = ctrl;
    setFailed(false);
    apiClient
      .listPlaylists(ctrl.signal)
      .then((pls) => {
        if (ctrl.signal.aborted) return;
        inflight.current = null;
        loadedAt.current = Date.now();
        setPlaylists(pls);
      })
      .catch(() => {
        if (ctrl.signal.aborted) return;
        inflight.current = null;
        setFailed(true);
      });
  }, []);

  const ensure = useCallback(() => {
    if (inflight.current) return;
    if (loadedAt.current > 0 && Date.now() - loadedAt.current < STALE_MS) return;
    load();
  }, [load]);

  useEffect(() => () => inflight.current?.abort(), []);

  return { playlists, failed, ensure, retry: load };
}
