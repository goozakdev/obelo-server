import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useAuth } from "../../auth/session";
import * as model from "./model";
import type { QueueEntry, QueueState, RepeatMode } from "./model";
import { clearStoredQueue, loadQueue, saveQueue } from "./persist";

// The `useQueue` store: the ONE client-side seam holding the Queue for the
// current playback session (PRD "The useQueue store"). It wraps the pure model
// (./model) in a React context, adds `sessionStorage` persistence scoped to the
// logged-in user (./persist), and clears the Queue on logout. The player and the
// play affordances read/drive it; its logic is pure (the model) so it is
// unit-tested without React, while this provider is exercised through the
// component test seam.

/** The Queue as the player + affordances consume it: the ordered entries and the
 * now-playing selectors, plus the pure operations (each commits a new model
 * state). `playNow`/`enqueue`/`playNext` take already-resolved {@link QueueEntry}s
 * (the build helpers produce them). */
export interface QueueStore {
  entries: QueueEntry[];
  /** The now-playing entry, or null when the Queue is empty. */
  current: QueueEntry | null;
  /** The entries after the now-playing one ("up next"). */
  upNext: QueueEntry[];
  /** Index of the now-playing entry (−1 when empty). */
  index: number;
  /** Number of entries. */
  length: number;
  hasPrev: boolean;
  hasNext: boolean;
  /** Whether Shuffle mode is on (derived: the Queue is shuffled iff it holds an
   * authored-order snapshot). Music-only in the UI. */
  shuffle: boolean;
  /** Repeat mode: `off` (default stop-at-end) / `all` / `one`. Music-only in UI. */
  repeat: RepeatMode;
  playNow: (entries: QueueEntry[], startIndex?: number) => void;
  enqueue: (entries: QueueEntry[]) => void;
  playNext: (entries: QueueEntry[]) => void;
  removeEntry: (entryId: string) => void;
  reorder: (entryIds: string[]) => void;
  clear: () => void;
  next: () => void;
  prev: () => void;
  jumpTo: (index: number) => void;
  /** Toggle Shuffle mode (non-destructive: un-shuffle restores the authored order). */
  setShuffle: (on: boolean) => void;
  /** Cycle Repeat mode: off → all → one → off. */
  cycleRepeat: () => void;
  /** The NATURAL-END advance (the player's `onEnded`): wraps under repeat-all,
   * stays put under repeat-one (the bar replays the element), stops under off. */
  advance: () => void;
}

const QueueContext = createContext<QueueStore | null>(null);

/** Read the logged-in user's id synchronously from the SAME storage the auth
 * layer hydrates from, so the provider can scope/load the persisted Queue on its
 * first render WITHOUT waiting for the (async) auth hydrate. The auth `ready`
 * gate (below) then reconciles login/logout. */
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

export interface QueueProviderProps {
  children: ReactNode;
  /** Test seam: seed the Queue directly, bypassing storage. Production mounts
   * without this and hydrates from `sessionStorage`. */
  initialState?: QueueState;
}

export function QueueProvider({ children, initialState }: QueueProviderProps) {
  const { session, ready } = useAuth();

  // The persistence-scope user id. Initialized synchronously (from storage) so
  // the Queue is hydrated on the first render; reconciled against the auth
  // session below once auth has hydrated (login/logout/switch).
  const [userId, setUserId] = useState<string | null>(() =>
    initialState ? null : persistedUserId(),
  );
  const [state, setState] = useState<QueueState>(
    () => initialState ?? loadQueue(window.sessionStorage, userId),
  );

  // Reconcile the Queue with the auth session, but ONLY after auth has hydrated
  // (`ready`) — before that, `session` is transiently null and must NOT be read
  // as a logout (which would wipe a just-restored Queue). On a real logout we
  // clear the persisted + in-memory Queue (story 38); on login/switch we load
  // that user's Queue.
  useEffect(() => {
    if (!ready || initialState) return;
    const sid = session?.user.id ?? null;
    if (sid === userId) return;
    if (sid === null) {
      clearStoredQueue(window.sessionStorage, userId);
      setState(model.emptyQueue());
    } else {
      setState(loadQueue(window.sessionStorage, sid));
    }
    setUserId(sid);
  }, [ready, session, userId, initialState]);

  // Persist on every change (within-app navigation + reload survival, stories
  // 36–37). Skipped while seeded with an explicit `initialState` (tests).
  useEffect(() => {
    if (initialState) return;
    saveQueue(window.sessionStorage, userId, state);
  }, [state, userId, initialState]);

  // The imperative operations commit a new model state via a functional update,
  // so they read the latest state without being re-created on every change
  // (stable identities for consumers' effects).
  const ops = useMemo(() => {
    const playNow = (entries: QueueEntry[], startIndex = 0) =>
      setState((s) => model.playNow(s, entries, startIndex));
    const enqueue = (entries: QueueEntry[]) =>
      setState((s) => model.enqueue(s, entries));
    const playNext = (entries: QueueEntry[]) =>
      setState((s) => model.playNext(s, entries));
    const removeEntry = (entryId: string) =>
      setState((s) => model.removeEntry(s, entryId));
    const reorder = (entryIds: string[]) =>
      setState((s) => model.reorder(s, entryIds));
    const clear = () => setState(() => model.clear());
    const next = () => setState((s) => model.next(s));
    const prev = () => setState((s) => model.prev(s));
    const jumpTo = (index: number) => setState((s) => model.jumpTo(s, index));
    const setShuffle = (on: boolean) => setState((s) => model.setShuffle(s, on));
    const cycleRepeat = () => setState((s) => model.cycleRepeat(s));
    const advance = () => setState((s) => model.advance(s));
    return {
      playNow, enqueue, playNext, removeEntry, reorder, clear, next, prev, jumpTo,
      setShuffle, cycleRepeat, advance,
    };
  }, []);

  const value = useMemo<QueueStore>(
    () => ({
      entries: state.entries,
      current: model.currentEntry(state),
      upNext: model.upNextEntries(state),
      index: state.currentIndex,
      length: state.entries.length,
      hasPrev: model.hasPrev(state),
      hasNext: model.hasNext(state),
      shuffle: state.authoredOrder !== null,
      repeat: state.repeat,
      ...ops,
    }),
    [state, ops],
  );

  return <QueueContext.Provider value={value}>{children}</QueueContext.Provider>;
}

/** Read the Queue store. Throws if used outside a {@link QueueProvider} (a wiring
 * bug). */
export function useQueue(): QueueStore {
  const ctx = useContext(QueueContext);
  if (!ctx) throw new Error("useQueue must be used within a QueueProvider");
  return ctx;
}
