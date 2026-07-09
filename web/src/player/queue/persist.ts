import { emptyQueue, type QueueState } from "./model";

// Queue persistence (PRD "Persistence", stories 36–38). The Queue is held in
// `sessionStorage` so it survives a within-app navigation and a page reload in
// the SAME browser-tab session, but is NOT shared across tabs/Devices and is
// gone when the tab closes — matching "client-side and per playback session."
// It is keyed PER logged-in user so one browser's two users don't see each
// other's Queue, and it is removed on logout (see useQueue). It is never sent to
// the server.
//
// These are pure, storage-injected helpers (the store passes `sessionStorage`,
// tests pass a fake/real Storage), so the load/save round-trip is unit-testable.

const STORAGE_PREFIX = "juicebox.queue";

/** The per-user storage key (a logged-out/anon session gets its own bucket). */
export function queueStorageKey(userId: string | null): string {
  return `${STORAGE_PREFIX}.${userId ?? "anon"}`;
}

/** Load a user's persisted Queue, defensively. Any malformed/partial payload (a
 * hand-edited store, a schema drift) degrades to the empty Queue rather than
 * throwing — a corrupt Queue must never break the player. */
export function loadQueue(storage: Storage, userId: string | null): QueueState {
  try {
    const raw = storage.getItem(queueStorageKey(userId));
    if (!raw) return emptyQueue();
    const parsed: unknown = JSON.parse(raw);
    if (
      !parsed ||
      typeof parsed !== "object" ||
      !Array.isArray((parsed as QueueState).entries) ||
      typeof (parsed as QueueState).currentIndex !== "number"
    ) {
      return emptyQueue();
    }
    const candidate = parsed as QueueState;
    const valid = candidate.entries.every(
      (e) =>
        e &&
        typeof e.entryId === "string" &&
        e.title &&
        typeof e.title.id === "string",
    );
    if (!valid) return emptyQueue();
    const length = candidate.entries.length;
    const currentIndex =
      length === 0 ? -1 : Math.min(Math.max(candidate.currentIndex, 0), length - 1);
    // Normalize the walk-order modifiers (slice 04). An OLDER stored Queue lacking
    // these fields — or one with a garbled value — loads with the safe defaults
    // (repeat off, not shuffled) rather than being rejected as corrupt.
    const repeat =
      candidate.repeat === "all" || candidate.repeat === "one" ? candidate.repeat : "off";
    const authoredOrder =
      Array.isArray(candidate.authoredOrder) &&
      candidate.authoredOrder.every((id) => typeof id === "string")
        ? candidate.authoredOrder
        : null;
    return { entries: candidate.entries, currentIndex, repeat, authoredOrder };
  } catch {
    return emptyQueue();
  }
}

/** Persist a user's Queue. An empty Queue removes the key (nothing playing → no
 * stored state to restore). Storage failures (quota/private mode) are swallowed:
 * the in-memory Queue still works, it just won't survive a reload. */
export function saveQueue(storage: Storage, userId: string | null, state: QueueState): void {
  try {
    if (state.entries.length === 0) storage.removeItem(queueStorageKey(userId));
    else storage.setItem(queueStorageKey(userId), JSON.stringify(state));
  } catch {
    // ignore — persistence is best-effort.
  }
}

/** Remove a user's persisted Queue (on logout). */
export function clearStoredQueue(storage: Storage, userId: string | null): void {
  try {
    storage.removeItem(queueStorageKey(userId));
  } catch {
    // ignore.
  }
}
