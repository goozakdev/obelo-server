import { useSyncExternalStore } from "react";

// The playing element's position + duration (seconds), held OUTSIDE React state.
// `timeupdate` fires ~4 times a second; as component state it re-rendered the whole
// player core (every menu, icon and label) on each tick, so the core writes here
// instead and only the few time-dependent children (progress bar, Skip marker, Up
// Next) subscribe, each to the one value it shows.

export interface MediaTime {
  currentTime: number;
  duration: number;
}

export interface MediaTimeStore {
  get(): MediaTime;
  /** Merge a patch; subscribers are notified only when a value actually changed. */
  set(patch: Partial<MediaTime>): void;
  subscribe(listener: () => void): () => void;
}

export function createMediaTimeStore(): MediaTimeStore {
  let value: MediaTime = { currentTime: 0, duration: 0 };
  const listeners = new Set<() => void>();
  return {
    get: () => value,
    set(patch) {
      const next = { ...value, ...patch };
      if (next.currentTime === value.currentTime && next.duration === value.duration) return;
      value = next;
      listeners.forEach((l) => l());
    },
    subscribe(listener) {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
  };
}

/** Subscribe to one primitive derived from the media time: the component re-renders
 * only when `select`'s result changes (it must return a primitive). */
export function useMediaTime<T extends number | string | boolean>(
  store: MediaTimeStore,
  select: (t: MediaTime) => T,
): T {
  return useSyncExternalStore(store.subscribe, () => select(store.get()));
}
