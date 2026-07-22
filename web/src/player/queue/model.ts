import type { TitleSummary } from "../../api/types";

// The PURE Queue model (Queue — see CONTEXT.md). The Queue is the ordered,
// in-session list of Titles the player walks: the entry at `currentIndex` is
// playing now, entries after it are up next, entries before it are reachable via
// prev. Every function here is a PURE transformation `QueueState -> QueueState`
// (or a selector) with NO I/O and NO DOM — so the whole model is unit-testable
// without React. The store (useQueue) and the build helpers (buildQueue) wrap
// these; the store also persists the state and the helpers resolve entries from
// the ApiClient before calling `playNow`.
//
// INVARIANT: `currentIndex` is always a valid index into a non-empty `entries`,
// or -1 iff `entries` is empty. Every operation re-establishes this.

/** One occurrence of a Title in the Queue. `entryId` is a CLIENT-generated unique
 * id so the same Title can appear more than once and a specific occurrence is
 * addressable (reorder/remove by id, mirroring playlist_items' per-item id but
 * client-side). `title` is the existing {@link TitleSummary} — no new type — so
 * an entry renders with the familiar poster/title/kind and carries the resume
 * position + watched flag the player reads. */
export interface QueueEntry {
  entryId: string;
  title: TitleSummary;
  /** The Show a queued Episode belongs to, set when the entry was built from a Show
   * walk (buildShowQueue / buildFullShowEntries). Lets the player derive the
   * per-Show Playback preference key SYNCHRONOUSLY — without waiting on the entry's
   * async detail fetch — so a no-preference Episode still plays instantly
   * (appletv-web-parity §1). Absent for Movies/Tracks and single plays. */
  showId?: string;
}

/** The three states of Repeat mode (music only — see CONTEXT.md): `off` stops
 * cleanly at the end, `all` wraps the last entry's natural end to the first, `one`
 * replays the current Title on its natural end. */
export type RepeatMode = "off" | "all" | "one";

/** The Queue's whole state: the ordered entries + the now-playing pointer, plus
 * the two music walk-order modifiers (Shuffle mode / Repeat mode). */
export interface QueueState {
  /** The LIVE play order — what next/prev/advance walk. While shuffled this is the
   * randomized order; {@link authoredOrder} remembers how to restore it. */
  entries: QueueEntry[];
  /** Index of the now-playing entry; -1 iff `entries` is empty. */
  currentIndex: number;
  /** Repeat mode (music only); `off` is the default stop-at-end. */
  repeat: RepeatMode;
  /** Shuffle mode is NON-DESTRUCTIVE: while shuffled this holds the AUTHORED
   * entryId order (a snapshot taken when shuffle turned on) so un-shuffle can
   * restore it from the current position. Non-null IFF shuffled; `null` otherwise.
   * `enqueue`/`playNext`/`removeEntry` keep it coherent (ids added/removed in both
   * the live order and here) so a later un-shuffle stays correct. */
  authoredOrder: string[] | null;
}

/** The empty Queue (nothing playing). Repeat defaults to `off`; not shuffled. */
export function emptyQueue(): QueueState {
  return { entries: [], currentIndex: -1, repeat: "off", authoredOrder: null };
}

// Client-generated entry ids. A monotonic sequence plus a time/randomness suffix
// keeps them unique even across rapid `entriesFromTitles` calls in one tick (so a
// Title queued twice gets two distinct, addressable occurrences).
let entrySeq = 0;
export function newEntryId(): string {
  entrySeq += 1;
  const rand = Math.random().toString(36).slice(2, 8);
  return `qe-${Date.now().toString(36)}-${entrySeq}-${rand}`;
}

/** Wrap a Title in a fresh Queue entry (new client-generated `entryId`). */
export function entryFromTitle(title: TitleSummary): QueueEntry {
  return { entryId: newEntryId(), title };
}

/** Wrap an ordered list of Titles as Queue entries (preserving order). The build
 * helpers feed the result to {@link playNow} / {@link enqueue}. */
export function entriesFromTitles(titles: TitleSummary[]): QueueEntry[] {
  return titles.map(entryFromTitle);
}

/** Clamp `index` into a valid pointer for a list of `length` (−1 when empty). */
function clampIndex(index: number, length: number): number {
  if (length <= 0) return -1;
  if (index < 0) return 0;
  if (index >= length) return length - 1;
  return index;
}

/** Replace the Queue with `entries` and set the now-playing pointer to
 * `startIndex` (clamped). Every build helper feeds this (story 9 — a new play
 * context replaces the Queue). An empty `entries` empties the Queue.
 *
 * A fresh play context is NEVER shuffled → `authoredOrder: null`. Repeat mode, by
 * contrast, is a persistent user preference (like a car radio's repeat toggle), so
 * it CARRIES OVER from the prior Queue rather than resetting on a new context. */
export function playNow(
  state: QueueState,
  entries: QueueEntry[],
  startIndex = 0,
): QueueState {
  if (entries.length === 0) return { ...emptyQueue(), repeat: state.repeat };
  return {
    entries: [...entries],
    currentIndex: clampIndex(startIndex, entries.length),
    repeat: state.repeat,
    authoredOrder: null,
  };
}

/** Append `entries` to the END of the Queue (cross-Album/Show, cross-kind, and
 * duplicates are all allowed — stories 22, 29). Appending into an empty Queue
 * makes the first appended entry the now-playing one. */
export function enqueue(state: QueueState, entries: QueueEntry[]): QueueState {
  if (entries.length === 0) return state;
  const wasEmpty = state.entries.length === 0;
  return {
    ...state,
    entries: [...state.entries, ...entries],
    currentIndex: wasEmpty ? 0 : state.currentIndex,
    // While shuffled, keep the authored order coherent: the new ids append to
    // BOTH the live order and the authored snapshot (so a later un-shuffle sees
    // them). When not shuffled, `...state` already carried the null through.
    authoredOrder: state.authoredOrder
      ? [...state.authoredOrder, ...entries.map((e) => e.entryId)]
      : null,
  };
}

/** Insert `entries` immediately AFTER the now-playing entry (story 23). Into an
 * empty Queue this is just {@link playNow}. The current pointer is untouched, so
 * the playing Title is not disturbed. */
export function playNext(state: QueueState, entries: QueueEntry[]): QueueState {
  if (entries.length === 0) return state;
  if (state.entries.length === 0) return playNow(state, entries, 0);
  const at = state.currentIndex + 1;
  return {
    ...state,
    entries: [...state.entries.slice(0, at), ...entries, ...state.entries.slice(at)],
    currentIndex: state.currentIndex,
    // Coherent while shuffled: append the new ids to the authored snapshot too
    // (their live position is up-next; their restore position is the tail).
    authoredOrder: state.authoredOrder
      ? [...state.authoredOrder, ...entries.map((e) => e.entryId)]
      : null,
  };
}

/** Remove ONE occurrence (the first match) of `entryId`. If it was an upcoming or
 * already-played entry the now-playing entry is preserved (by id). If it was the
 * now-playing entry the next entry takes over (the slot is filled by what follows);
 * removing the last remaining entry empties the Queue → playback stops (stories
 * 24–26, 28). */
export function removeEntry(state: QueueState, entryId: string): QueueState {
  const i = state.entries.findIndex((e) => e.entryId === entryId);
  if (i < 0) return state;
  const entries = [...state.entries.slice(0, i), ...state.entries.slice(i + 1)];
  if (entries.length === 0) return { ...emptyQueue(), repeat: state.repeat };
  let currentIndex = state.currentIndex;
  if (i < state.currentIndex) currentIndex -= 1; // current shifted left by one
  else if (i === state.currentIndex) currentIndex = clampIndex(i, entries.length);
  // i > currentIndex: an upcoming entry — the pointer is unaffected.
  return {
    ...state,
    entries,
    currentIndex,
    // Coherent while shuffled: drop the removed id from the authored snapshot too.
    authoredOrder: state.authoredOrder
      ? state.authoredOrder.filter((id) => id !== entryId)
      : null,
  };
}

/** Apply a FULL permutation of the existing `entryId`s, PRESERVING which entry is
 * now playing (matched by id, not index) so the playing Title is untouched
 * (stories 19–20; mirrors the Playlist reorder contract). A list that is not a
 * permutation of the current ids is ignored (the Queue is returned unchanged). */
export function reorder(state: QueueState, entryIds: string[]): QueueState {
  if (entryIds.length !== state.entries.length) return state;
  const byId = new Map(state.entries.map((e) => [e.entryId, e]));
  const entries: QueueEntry[] = [];
  for (const id of entryIds) {
    const entry = byId.get(id);
    if (!entry) return state; // not a permutation of the current ids → no-op
    entries.push(entry);
  }
  const currentId =
    state.currentIndex >= 0 ? state.entries[state.currentIndex].entryId : null;
  const currentIndex =
    currentId == null ? -1 : entries.findIndex((e) => e.entryId === currentId);
  // A permutation adds/removes no ids, so the authored snapshot stays coherent
  // (`...state` carries it, and `repeat`, through unchanged).
  return { ...state, entries, currentIndex };
}

/** Empty the Queue → playback stops (stories 27–28). Resets modes to defaults. */
export function clear(): QueueState {
  return emptyQueue();
}

/** Cycle Repeat mode: `off → all → one → off` (music-only 3-state toggle). */
export function cycleRepeat(state: QueueState): QueueState {
  const repeat: RepeatMode =
    state.repeat === "off" ? "all" : state.repeat === "all" ? "one" : "off";
  return { ...state, repeat };
}

/** Fisher-Yates shuffle of a copy of `arr`, using the injected `rng` (default
 * `Math.random`). The rng is injectable so tests can pass a deterministic source
 * and assert the exact resulting order. */
function shuffleArray<T>(arr: T[], rng: () => number): T[] {
  const a = [...arr];
  for (let i = a.length - 1; i > 0; i -= 1) {
    const j = Math.floor(rng() * (i + 1));
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

/** Toggle Shuffle mode (music only), NON-DESTRUCTIVELY.
 *
 * Turning ON snapshots the authored order (`entries.map(id)`), then randomizes
 * ONLY the up-next slice (indices AFTER `currentIndex`) — the now-playing entry
 * and the already-played prefix stay exactly put. `rng` is injectable so tests can
 * pass a deterministic source (default `Math.random`).
 *
 * Turning OFF rebuilds the live order from `authoredOrder`, preserving which entry
 * is current BY entryId, and clears the snapshot. A stale/mismatched snapshot
 * (entries added/removed while shuffled) reconciles: unknown ids are dropped and
 * live entries missing from the snapshot are appended. */
export function setShuffle(
  state: QueueState,
  on: boolean,
  rng: () => number = Math.random,
): QueueState {
  if (on) {
    if (state.authoredOrder || state.currentIndex < 0) return state; // already/empty
    const authoredOrder = state.entries.map((e) => e.entryId);
    const head = state.entries.slice(0, state.currentIndex + 1); // played prefix + current
    const tail = shuffleArray(state.entries.slice(state.currentIndex + 1), rng);
    // `head` keeps `currentIndex` valid (its length is currentIndex+1), so the
    // now-playing entry never moves.
    return { ...state, entries: [...head, ...tail], authoredOrder };
  }
  if (!state.authoredOrder) return state; // not shuffled → no-op
  const currentId =
    state.currentIndex >= 0 ? state.entries[state.currentIndex].entryId : null;
  const byId = new Map(state.entries.map((e) => [e.entryId, e]));
  const restored: QueueEntry[] = [];
  const seen = new Set<string>();
  for (const id of state.authoredOrder) {
    const entry = byId.get(id);
    if (entry) {
      restored.push(entry); // drop unknown ids (removed while shuffled)
      seen.add(id);
    }
  }
  // Append entries present now but missing from the snapshot (added while shuffled
  // through a path that couldn't be reconciled) so none are lost.
  for (const e of state.entries) if (!seen.has(e.entryId)) restored.push(e);
  const currentIndex =
    currentId == null ? -1 : restored.findIndex((e) => e.entryId === currentId);
  return { ...state, entries: restored, currentIndex, authoredOrder: null };
}

/** Advance the now-playing pointer to the next entry (MANUAL skip); a no-op at the
 * last entry UNLESS Repeat mode is `all`, in which case it wraps to index 0.
 * Manual next is NEVER affected by `repeat === "one"` — the skip button always
 * moves (repeat-one only governs the natural-end advance; see {@link advance}). */
export function next(state: QueueState): QueueState {
  if (state.currentIndex < 0) return state;
  if (state.currentIndex + 1 >= state.entries.length) {
    return state.repeat === "all" ? { ...state, currentIndex: 0 } : state;
  }
  return { ...state, currentIndex: state.currentIndex + 1 };
}

/** The NATURAL-END advance (the bar's `onEnded`, music & video alike), distinct
 * from the manual {@link next}: under `one` the pointer DOES NOT move — the SAME
 * entry replays. A pure model can't re-seek the DOM <video>, so the actual replay
 * (currentTime = 0; play()) is done in the bar; here we only signal "stay put" by
 * returning the state unchanged. Under `all` the last entry wraps to the first;
 * under `off` the Queue stops cleanly at the end (no-op at the last entry). */
export function advance(state: QueueState): QueueState {
  if (state.currentIndex < 0) return state;
  if (state.repeat === "one") return state; // replay handled by the bar
  if (state.currentIndex + 1 >= state.entries.length) {
    return state.repeat === "all" ? { ...state, currentIndex: 0 } : state;
  }
  return { ...state, currentIndex: state.currentIndex + 1 };
}

/** Move the now-playing pointer to the previous entry; a no-op at the first. */
export function prev(state: QueueState): QueueState {
  if (state.currentIndex <= 0) return state;
  return { ...state, currentIndex: state.currentIndex - 1 };
}

/** Jump the now-playing pointer to `index`; a no-op if out of bounds. */
export function jumpTo(state: QueueState, index: number): QueueState {
  if (index < 0 || index >= state.entries.length) return state;
  return { ...state, currentIndex: index };
}

// --- selectors --------------------------------------------------------------

/** The now-playing entry, or null when the Queue is empty. */
export function currentEntry(state: QueueState): QueueEntry | null {
  return state.currentIndex >= 0 ? state.entries[state.currentIndex] : null;
}

/** The entries AFTER the now-playing one (the "up next" list). */
export function upNextEntries(state: QueueState): QueueEntry[] {
  if (state.currentIndex < 0) return [];
  return state.entries.slice(state.currentIndex + 1);
}

/** Is there an entry before the now-playing one (prev is meaningful)? */
export function hasPrev(state: QueueState): boolean {
  return state.currentIndex > 0;
}

/** Is there an entry after the now-playing one (next is meaningful)? */
export function hasNext(state: QueueState): boolean {
  return state.currentIndex >= 0 && state.currentIndex + 1 < state.entries.length;
}
