import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";

// The playback TRANSPORT seam: a tiny shared store for the ONE thing outside the
// Now Playing bar that needs the real media element's live state — whether it's
// currently playing, and a way to toggle play/pause. The bar owns the element (it
// mounts once, survives navigation); this context lets a distant affordance (an
// album track row's play/pause button) reflect and drive that element WITHOUT
// reaching into the bar.
//
// The bar's active player PUBLISHES into this store (`publishPlaying` on
// play/pause, `registerToggle` while it holds the element); consumers READ
// `playing` and call `toggle()`. When no player is mounted (empty queue) the
// store is idle: `playing` false, `toggle()` a no-op. Which Title is current is
// NOT held here — that's the Queue's job (`useQueue().current`); a consumer pairs
// the two (current-title id + `playing`) to decide its icon.

export interface PlaybackTransport {
  /** True while the current media element is actively playing (not paused). */
  playing: boolean;
  /** Toggle play/pause on the current element; a no-op when nothing is loaded. */
  toggle: () => void;
  /** Player-core internal: publish the element's play/pause state. */
  publishPlaying: (v: boolean) => void;
  /** Player-core internal: register (or clear, with null) the toggle bound to the
   * live element. Called on mount/unmount of the active player. */
  registerToggle: (fn: (() => void) | null) => void;
}

const TransportContext = createContext<PlaybackTransport | null>(null);

export function PlaybackTransportProvider({ children }: { children: ReactNode }) {
  const [playing, setPlaying] = useState(false);
  // The toggle is held in a ref so publishing a new one never re-renders
  // consumers — only `playing` changes do.
  const toggleRef = useRef<(() => void) | null>(null);

  const toggle = useCallback(() => {
    toggleRef.current?.();
  }, []);
  const publishPlaying = useCallback((v: boolean) => setPlaying(v), []);
  const registerToggle = useCallback((fn: (() => void) | null) => {
    toggleRef.current = fn;
  }, []);

  const value = useMemo<PlaybackTransport>(
    () => ({ playing, toggle, publishPlaying, registerToggle }),
    [playing, toggle, publishPlaying, registerToggle],
  );

  return (
    <TransportContext.Provider value={value}>{children}</TransportContext.Provider>
  );
}

/** Read the playback transport. Returns a safe idle transport when used outside a
 * provider (e.g. an isolated component test that renders a row without the bar),
 * so consumers never need to guard for the provider's absence. */
export function usePlaybackTransport(): PlaybackTransport {
  const ctx = useContext(TransportContext);
  if (ctx) return ctx;
  return IDLE_TRANSPORT;
}

const noop = () => {};
const IDLE_TRANSPORT: PlaybackTransport = {
  playing: false,
  toggle: noop,
  publishPlaying: noop,
  registerToggle: noop,
};
