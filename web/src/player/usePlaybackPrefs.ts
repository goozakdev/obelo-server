import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/session";

// Playback preferences (now-playing-bar/02): the user's volume + mute setting.
//
// UNLIKE the Queue (session-scoped, in `sessionStorage`), volume/mute are a
// durable PREFERENCE, so they live in `localStorage` — kept across sessions and
// tab closes — keyed PER logged-in user, mirroring how the Queue keys itself so
// one browser's two users don't share a volume. The bar applies them to the
// media element on mount and persists every change.
//
// The load/save are pure, storage-injected helpers (the hook passes
// `localStorage`; tests pass a fake/real Storage), so the round-trip is
// unit-testable at the store seam without React — the same seam philosophy as
// the Queue's persist.ts.

const STORAGE_PREFIX = "juicebox.playback-prefs";

/** Volume ∈ [0, 1] and mute. Defaults to full volume, un-muted. */
export interface PlaybackPrefs {
  volume: number;
  muted: boolean;
}

export const DEFAULT_PREFS: PlaybackPrefs = { volume: 1, muted: false };

/** Clamp a volume into the media element's valid [0, 1] range. */
function clampVolume(v: number): number {
  if (!Number.isFinite(v)) return DEFAULT_PREFS.volume;
  return Math.min(Math.max(v, 0), 1);
}

/** The per-user storage key (a logged-out/anon session gets its own bucket). */
export function playbackPrefsKey(userId: string | null): string {
  return `${STORAGE_PREFIX}.${userId ?? "anon"}`;
}

/** Load a user's persisted prefs, defensively. Any malformed/partial payload
 * degrades to the defaults (full volume, un-muted) rather than throwing — a
 * corrupt pref must never break the player. */
export function loadPlaybackPrefs(storage: Storage, userId: string | null): PlaybackPrefs {
  try {
    const raw = storage.getItem(playbackPrefsKey(userId));
    if (!raw) return { ...DEFAULT_PREFS };
    const parsed = JSON.parse(raw) as Partial<PlaybackPrefs>;
    const volume =
      typeof parsed.volume === "number" ? clampVolume(parsed.volume) : DEFAULT_PREFS.volume;
    const muted = typeof parsed.muted === "boolean" ? parsed.muted : DEFAULT_PREFS.muted;
    return { volume, muted };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

/** Persist a user's prefs. Storage failures (quota/private mode) are swallowed:
 * the in-memory pref still works, it just won't survive a reload. */
export function savePlaybackPrefs(
  storage: Storage,
  userId: string | null,
  prefs: PlaybackPrefs,
): void {
  try {
    storage.setItem(playbackPrefsKey(userId), JSON.stringify(prefs));
  } catch {
    // ignore — persistence is best-effort.
  }
}

/** Read the logged-in user's id synchronously from the SAME storage the auth
 * layer hydrates from, so the hook can load the right user's prefs on its FIRST
 * render (before the async auth hydrate). The `useAuth` reconcile (below) then
 * follows a login/logout/switch. Mirrors useQueue's `persistedUserId`. */
function persistedUserId(): string | null {
  try {
    const raw = window.localStorage.getItem("juicebox.user");
    if (!raw) return null;
    const u = JSON.parse(raw) as { id?: unknown };
    return typeof u?.id === "string" ? u.id : null;
  } catch {
    return null;
  }
}

/** The prefs as the bar consumes them: the current volume/mute plus setters that
 * commit and persist. */
export interface PlaybackPrefsStore {
  volume: number;
  muted: boolean;
  setVolume: (volume: number) => void;
  setMuted: (muted: boolean) => void;
  toggleMuted: () => void;
}

/** Per-user volume + mute, loaded from `localStorage` on mount and persisted on
 * every change. Reconciles to the logged-in user (from `useAuth`) once auth has
 * hydrated, so a login/switch loads that user's saved volume. */
export function usePlaybackPrefs(): PlaybackPrefsStore {
  const { session, ready } = useAuth();
  const userId = session?.user.id ?? null;

  // The user whose prefs are currently loaded. Initialized synchronously (from
  // storage) so the first render already carries the right user's volume; the
  // reconcile effect below follows auth once it hydrates.
  const [loadedUserId, setLoadedUserId] = useState<string | null>(persistedUserId);
  const [prefs, setPrefs] = useState<PlaybackPrefs>(() =>
    loadPlaybackPrefs(window.localStorage, persistedUserId()),
  );

  // Follow a login/logout/switch, but only once auth has hydrated (`ready`) —
  // before that `session` is transiently null and must not be read as a logout.
  useEffect(() => {
    if (!ready || userId === loadedUserId) return;
    setPrefs(loadPlaybackPrefs(window.localStorage, userId));
    setLoadedUserId(userId);
  }, [ready, userId, loadedUserId]);

  // Persist on every change (keyed to the currently-loaded user).
  useEffect(() => {
    savePlaybackPrefs(window.localStorage, loadedUserId, prefs);
  }, [prefs, loadedUserId]);

  const setVolume = useCallback(
    (volume: number) => setPrefs((p) => ({ ...p, volume: clampVolume(volume) })),
    [],
  );
  const setMuted = useCallback((muted: boolean) => setPrefs((p) => ({ ...p, muted })), []);
  const toggleMuted = useCallback(() => setPrefs((p) => ({ ...p, muted: !p.muted })), []);

  return { volume: prefs.volume, muted: prefs.muted, setVolume, setMuted, toggleMuted };
}
