import { useCallback, useEffect, useState } from "react";

// The browse LAYOUT mode (appletv-web-parity §5) — how a Library's browse grid
// draws its rows: an artwork wall (Tile, today's default), a thumbnail + text row
// (Detail), or bare names (List). Governs the four browse grids — Movies, Shows,
// Artists, Albums — and nothing else (Playlists and Home keep their fixed layout).
//
// The choice persists LOCALLY, per Library, in `localStorage` — a browsing
// preference the server has no memory of (unlike Remembered audio/video, which are
// server-owned; see CONTEXT.md). So the load/save are pure, storage-injected
// helpers (callers pass `localStorage`; tests pass a real/fake Storage), and the
// round-trip is unit-testable at the store seam without React — the same seam
// philosophy as playbackPreference / usePlaybackPrefs.
//
// DESIGN CONSTRAINT INHERITED (client ADR-0007): Detail renders ONLY already-loaded
// list-payload row data — no per-row detail fetches. Switching mode never refetches
// the list. So this module owns persistence only; the per-kind Detail row content is
// built by each screen from the summary fields it already holds.

export type LayoutMode = "tile" | "detail" | "list";

/** The three modes, in toggle order. `tile` leads because it is the default and
 * today's only layout (backward-compatible: an unset Library reads as Tile). */
export const LAYOUT_MODES: LayoutMode[] = ["tile", "detail", "list"];

/** Human labels for the toggle buttons. */
export const LAYOUT_LABELS: Record<LayoutMode, string> = {
  tile: "Tile",
  detail: "Detail",
  list: "List",
};

/** The default mode for an unconfigured Library — the artwork wall web has always
 * shown, so nothing changes until the viewer picks another layout. */
export const DEFAULT_LAYOUT_MODE: LayoutMode = "tile";

const STORAGE_PREFIX = "juicebox.browse-layout";

/** The per-Library storage key. Keying per Library (not per user) matches the TV:
 * the layout is a property of how THIS grid is browsed, shared across whoever uses
 * the browser. */
export function layoutModeKey(libraryId: string): string {
  return `${STORAGE_PREFIX}.${libraryId}`;
}

function isLayoutMode(value: unknown): value is LayoutMode {
  return value === "tile" || value === "detail" || value === "list";
}

/** Load the stored mode for a Library, defensively. A missing or malformed entry
 * degrades to the default Tile — a corrupt value must never blank the grid. An
 * empty libraryId (a grid whose Library isn't known yet) is always the default. */
export function loadLayoutMode(storage: Storage, libraryId: string): LayoutMode {
  if (!libraryId) return DEFAULT_LAYOUT_MODE;
  try {
    const raw = storage.getItem(layoutModeKey(libraryId));
    return isLayoutMode(raw) ? raw : DEFAULT_LAYOUT_MODE;
  } catch {
    return DEFAULT_LAYOUT_MODE;
  }
}

/** Persist the chosen mode. Storage failures (quota / private mode) are swallowed:
 * the in-memory choice still governs this session, it just won't survive a reload. */
export function saveLayoutMode(
  storage: Storage,
  libraryId: string,
  mode: LayoutMode,
): void {
  if (!libraryId) return;
  try {
    storage.setItem(layoutModeKey(libraryId), mode);
  } catch {
    // ignore — persistence is best-effort.
  }
}

/** The React binding: `[mode, setMode]` for a Library's browse grid, backed by
 * `localStorage`. Re-reads when the Library changes (so navigating between grids
 * restores each one's own remembered mode) and writes through on every set. */
export function useLayoutMode(
  libraryId: string,
): [LayoutMode, (mode: LayoutMode) => void] {
  const [mode, setModeState] = useState<LayoutMode>(() =>
    loadLayoutMode(window.localStorage, libraryId),
  );

  // Restore this Library's own mode when the id changes (e.g. an Album grid whose
  // libraryId only resolves after the Artist loads, or navigating grid → grid).
  useEffect(() => {
    setModeState(loadLayoutMode(window.localStorage, libraryId));
  }, [libraryId]);

  const setMode = useCallback(
    (next: LayoutMode) => {
      setModeState(next);
      saveLayoutMode(window.localStorage, libraryId, next);
    },
    [libraryId],
  );

  return [mode, setMode];
}
