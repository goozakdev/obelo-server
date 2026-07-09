import { describe, it, expect } from "vitest";
import type { TitleSummary } from "../../api/types";
import { emptyQueue, type QueueState } from "./model";
import { clearStoredQueue, loadQueue, queueStorageKey, saveQueue } from "./persist";

// The Queue persistence round-trip (pure, storage-injected). We use a fake
// in-memory Storage so the load/save/clear logic is tested without jsdom: a saved
// Queue restores (reload survival), it is keyed per user (scoping), an empty
// Queue / clear removes the key (logout / stop), and a corrupt payload degrades
// to the empty Queue.

class FakeStorage implements Storage {
  private map = new Map<string, string>();
  get length() {
    return this.map.size;
  }
  clear() {
    this.map.clear();
  }
  getItem(k: string) {
    return this.map.has(k) ? this.map.get(k)! : null;
  }
  key(i: number) {
    return Array.from(this.map.keys())[i] ?? null;
  }
  removeItem(k: string) {
    this.map.delete(k);
  }
  setItem(k: string, v: string) {
    this.map.set(k, v);
  }
}

function title(id: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

const state: QueueState = {
  entries: [
    { entryId: "e0", title: title("a") },
    { entryId: "e1", title: title("b") },
  ],
  currentIndex: 1,
  repeat: "off",
  authoredOrder: null,
};

describe("Queue persistence", () => {
  it("round-trips a saved Queue (reload survival)", () => {
    const storage = new FakeStorage();
    saveQueue(storage, "u1", state);
    expect(loadQueue(storage, "u1")).toEqual(state);
  });

  it("scopes the Queue per user (one browser, two users don't collide)", () => {
    const storage = new FakeStorage();
    saveQueue(storage, "u1", state);
    expect(loadQueue(storage, "u2")).toEqual(emptyQueue());
    expect(queueStorageKey("u1")).not.toBe(queueStorageKey("u2"));
  });

  it("an empty Queue removes the key; clear removes it (logout / stop)", () => {
    const storage = new FakeStorage();
    saveQueue(storage, "u1", state);
    saveQueue(storage, "u1", emptyQueue());
    expect(storage.getItem(queueStorageKey("u1"))).toBeNull();

    saveQueue(storage, "u1", state);
    clearStoredQueue(storage, "u1");
    expect(loadQueue(storage, "u1")).toEqual(emptyQueue());
  });

  it("clamps a stored out-of-range currentIndex", () => {
    const storage = new FakeStorage();
    storage.setItem(queueStorageKey("u1"), JSON.stringify({ ...state, currentIndex: 99 }));
    expect(loadQueue(storage, "u1").currentIndex).toBe(1);
  });

  it("round-trips the Repeat mode and shuffle snapshot (walk-order modifiers persist)", () => {
    const storage = new FakeStorage();
    const shuffledRepeat: QueueState = {
      ...state,
      repeat: "all",
      authoredOrder: ["e1", "e0"], // shuffled: authored order snapshot
    };
    saveQueue(storage, "u1", shuffledRepeat);
    expect(loadQueue(storage, "u1")).toEqual(shuffledRepeat);
  });

  it("loads an OLDER stored Queue (no repeat/authoredOrder) with defaults (off, not shuffled)", () => {
    const storage = new FakeStorage();
    // A pre-slice-04 payload has only entries + currentIndex.
    storage.setItem(
      queueStorageKey("u1"),
      JSON.stringify({ entries: state.entries, currentIndex: 1 }),
    );
    const loaded = loadQueue(storage, "u1");
    expect(loaded.repeat).toBe("off");
    expect(loaded.authoredOrder).toBeNull();
    expect(loaded.entries.map((e) => e.title.id)).toEqual(["a", "b"]);
  });

  it("normalizes a garbled repeat/authoredOrder to the defaults rather than rejecting", () => {
    const storage = new FakeStorage();
    storage.setItem(
      queueStorageKey("u1"),
      JSON.stringify({ ...state, repeat: "bogus", authoredOrder: 42 }),
    );
    const loaded = loadQueue(storage, "u1");
    expect(loaded.repeat).toBe("off");
    expect(loaded.authoredOrder).toBeNull();
    expect(loaded.entries.length).toBe(2); // the Queue is NOT discarded
  });

  it("degrades a corrupt payload to the empty Queue", () => {
    const storage = new FakeStorage();
    storage.setItem(queueStorageKey("u1"), "not json");
    expect(loadQueue(storage, "u1")).toEqual(emptyQueue());

    storage.setItem(queueStorageKey("u1"), JSON.stringify({ entries: "nope" }));
    expect(loadQueue(storage, "u1")).toEqual(emptyQueue());
  });
});
