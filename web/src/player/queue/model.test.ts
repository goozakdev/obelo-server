import { describe, it, expect } from "vitest";
import type { TitleSummary } from "../../api/types";
import {
  advance,
  clear,
  currentEntry,
  cycleRepeat,
  emptyQueue,
  enqueue,
  entriesFromTitles,
  hasNext,
  hasPrev,
  jumpTo,
  next,
  playNext,
  playNow,
  prev,
  removeEntry,
  reorder,
  setShuffle,
  upNextEntries,
  type QueueEntry,
  type QueueState,
} from "./model";

// PURE unit test of the Queue model (no DOM, no I/O). It pins down the operations
// and the current-pointer invariant: a new context replaces; append/playNext add
// without disturbing the current entry; remove fixes the pointer (incl. removing
// the current/last entry); reorder preserves the current entry BY id; next/prev/
// jumpTo respect bounds.

function title(id: string, name = id): TitleSummary {
  return {
    id,
    kind: "movie",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

/** Build entries with DETERMINISTIC entry ids (e0, e1, …) for assertions. */
function entries(ids: string[]): QueueEntry[] {
  return ids.map((id, i) => ({ entryId: `e${i}`, title: title(id) }));
}

const ids = (es: QueueEntry[]) => es.map((e) => e.title.id);
const entryIds = (es: QueueEntry[]) => es.map((e) => e.entryId);

describe("Queue model", () => {
  it("entriesFromTitles wraps each Title in an entry with a unique id (duplicates allowed)", () => {
    const es = entriesFromTitles([title("a"), title("a"), title("b")]);
    expect(ids(es)).toEqual(["a", "a", "b"]);
    expect(new Set(entryIds(es)).size).toBe(3); // all unique, even the duplicate Title
  });

  describe("playNow", () => {
    it("replaces the Queue and sets the current pointer to startIndex", () => {
      const s = playNow(emptyQueue(), entries(["a", "b", "c"]), 1);
      expect(ids(s.entries)).toEqual(["a", "b", "c"]);
      expect(s.currentIndex).toBe(1);
      expect(currentEntry(s)?.title.id).toBe("b");
    });

    it("defaults startIndex to 0 and clamps an out-of-range index", () => {
      expect(playNow(emptyQueue(), entries(["a", "b"])).currentIndex).toBe(0);
      expect(playNow(emptyQueue(), entries(["a", "b"]), 9).currentIndex).toBe(1);
      expect(playNow(emptyQueue(), entries(["a", "b"]), -3).currentIndex).toBe(0);
    });

    it("a new context replaces a prior Queue", () => {
      const first = playNow(emptyQueue(), entries(["a", "b"]), 1);
      const second = playNow(first, entries(["x"]), 0);
      expect(ids(second.entries)).toEqual(["x"]);
      expect(second.currentIndex).toBe(0);
    });

    it("an empty entries list empties the Queue", () => {
      expect(playNow(emptyQueue(), [])).toEqual(emptyQueue());
    });
  });

  describe("enqueue", () => {
    it("appends to the end without moving the current pointer", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 1);
      const s1 = enqueue(s0, entriesFromTitles([title("c"), title("d")]));
      expect(ids(s1.entries)).toEqual(["a", "b", "c", "d"]);
      expect(s1.currentIndex).toBe(1); // unchanged — the playing Title is untouched
    });

    it("into an empty Queue, the first appended entry becomes current", () => {
      const s = enqueue(emptyQueue(), entries(["a", "b"]));
      expect(s.currentIndex).toBe(0);
    });

    it("allows duplicates and is a no-op for an empty list", () => {
      const s0 = playNow(emptyQueue(), entries(["a"]), 0);
      const s1 = enqueue(s0, entriesFromTitles([title("a")]));
      expect(ids(s1.entries)).toEqual(["a", "a"]);
      expect(enqueue(s0, [])).toBe(s0);
    });
  });

  describe("playNext", () => {
    it("inserts immediately after the current entry, leaving current playing", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 0);
      const s1 = playNext(s0, entriesFromTitles([title("x")]));
      expect(ids(s1.entries)).toEqual(["a", "x", "b", "c"]);
      expect(s1.currentIndex).toBe(0);
      expect(upNextEntries(s1).map((e) => e.title.id)).toEqual(["x", "b", "c"]);
    });

    it("into an empty Queue behaves like playNow", () => {
      const s = playNext(emptyQueue(), entries(["a"]));
      expect(ids(s.entries)).toEqual(["a"]);
      expect(s.currentIndex).toBe(0);
    });
  });

  describe("removeEntry", () => {
    it("removes an upcoming entry, leaving the current entry current (by id)", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 0);
      const s1 = removeEntry(s0, "e2"); // remove c (upcoming)
      expect(ids(s1.entries)).toEqual(["a", "b"]);
      expect(currentEntry(s1)?.entryId).toBe("e0"); // still a
    });

    it("removing an already-played entry keeps the SAME entry current (index shifts)", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 2);
      const s1 = removeEntry(s0, "e0"); // remove a (before current)
      expect(ids(s1.entries)).toEqual(["b", "c"]);
      expect(currentEntry(s1)?.entryId).toBe("e2"); // still c, now at index 1
      expect(s1.currentIndex).toBe(1);
    });

    it("removing the current entry advances to the next entry", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 1);
      const s1 = removeEntry(s0, "e1"); // remove the current, b
      expect(ids(s1.entries)).toEqual(["a", "c"]);
      expect(currentEntry(s1)?.entryId).toBe("e2"); // c took over the slot
      expect(s1.currentIndex).toBe(1);
    });

    it("removing the current LAST entry falls back to the new last entry", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 1);
      const s1 = removeEntry(s0, "e1"); // remove the current, last entry
      expect(ids(s1.entries)).toEqual(["a"]);
      expect(s1.currentIndex).toBe(0);
    });

    it("removing the last remaining entry empties the Queue (stop)", () => {
      const s0 = playNow(emptyQueue(), entries(["a"]), 0);
      expect(removeEntry(s0, "e0")).toEqual(emptyQueue());
    });

    it("removes only the one occurrence of a duplicated Title", () => {
      const dup = entries(["a", "b", "a"]); // e0/a, e1/b, e2/a
      const s0 = playNow(emptyQueue(), dup, 0);
      const s1 = removeEntry(s0, "e2"); // the SECOND a
      expect(ids(s1.entries)).toEqual(["a", "b"]);
      expect(entryIds(s1.entries)).toEqual(["e0", "e1"]);
    });

    it("ignores an unknown entryId", () => {
      const s0 = playNow(emptyQueue(), entries(["a"]), 0);
      expect(removeEntry(s0, "nope")).toBe(s0);
    });
  });

  describe("reorder", () => {
    it("applies a full permutation, keeping the current entry current by id", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 0); // current = e0/a
      const s1 = reorder(s0, ["e2", "e0", "e1"]); // c, a, b
      expect(ids(s1.entries)).toEqual(["c", "a", "b"]);
      expect(currentEntry(s1)?.entryId).toBe("e0"); // still a
      expect(s1.currentIndex).toBe(1); // a moved to index 1
    });

    it("reordering only the up-next entries does not move the current pointer", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 0);
      const s1 = reorder(s0, ["e0", "e2", "e1"]); // a stays first; swap b/c
      expect(currentEntry(s1)?.entryId).toBe("e0");
      expect(s1.currentIndex).toBe(0);
    });

    it("ignores a list that is not a full permutation of the current ids", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 0);
      expect(reorder(s0, ["e0"])).toBe(s0); // wrong length
      expect(reorder(s0, ["e0", "zzz"])).toBe(s0); // unknown id
    });
  });

  describe("next / prev / jumpTo", () => {
    it("next advances and clamps at the last entry (clean stop)", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 0);
      const s1 = next(s0);
      expect(s1.currentIndex).toBe(1);
      expect(next(s1)).toBe(s1); // at the end → no-op
    });

    it("prev retreats and clamps at the first entry", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 1);
      expect(prev(s0).currentIndex).toBe(0);
      expect(prev(prev(s0)).currentIndex).toBe(0); // at the start → no-op
    });

    it("jumpTo moves within bounds and ignores out-of-range", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c"]), 0);
      expect(jumpTo(s0, 2).currentIndex).toBe(2);
      expect(jumpTo(s0, 9)).toBe(s0);
      expect(jumpTo(s0, -1)).toBe(s0);
    });
  });

  describe("selectors", () => {
    it("current/upNext/hasPrev/hasNext reflect the pointer", () => {
      const mid = playNow(emptyQueue(), entries(["a", "b", "c"]), 1);
      expect(currentEntry(mid)?.title.id).toBe("b");
      expect(upNextEntries(mid).map((e) => e.title.id)).toEqual(["c"]);
      expect(hasPrev(mid)).toBe(true);
      expect(hasNext(mid)).toBe(true);

      const empty = emptyQueue();
      expect(currentEntry(empty)).toBeNull();
      expect(upNextEntries(empty)).toEqual([]);
      expect(hasPrev(empty)).toBe(false);
      expect(hasNext(empty)).toBe(false);
    });
  });

  describe("clear", () => {
    it("empties the Queue", () => {
      expect(clear()).toEqual(emptyQueue());
    });
  });

  // ── Shuffle & Repeat (slice 04, music-only walk-order modifiers) ───────────

  describe("cycleRepeat", () => {
    it("cycles off → all → one → off", () => {
      const q = playNow(emptyQueue(), entries(["a"]), 0);
      expect(q.repeat).toBe("off");
      const all = cycleRepeat(q);
      expect(all.repeat).toBe("all");
      const one = cycleRepeat(all);
      expect(one.repeat).toBe("one");
      expect(cycleRepeat(one).repeat).toBe("off");
    });
  });

  describe("setShuffle", () => {
    // A deterministic rng (always 0) makes Fisher-Yates a fixed permutation, so we
    // can assert the head is untouched AND that the tail order actually changed.
    const zeroRng = () => 0;

    it("turning ON snapshots the authored order and randomizes ONLY the up-next slice", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c", "d", "e"]), 1); // current e1/b
      const s1 = setShuffle(s0, true, zeroRng);
      // The authored order is the pre-shuffle entryId order.
      expect(s1.authoredOrder).toEqual(["e0", "e1", "e2", "e3", "e4"]);
      // The already-played prefix + the now-playing entry stay exactly put.
      expect(entryIds(s1.entries.slice(0, 2))).toEqual(["e0", "e1"]);
      expect(s1.currentIndex).toBe(1);
      expect(currentEntry(s1)?.entryId).toBe("e1");
      // The up-next slice is a permutation of the original up-next (multiset kept)…
      expect(new Set(entryIds(s1.entries.slice(2)))).toEqual(new Set(["e2", "e3", "e4"]));
      // …and it was actually reordered (proof randomization touched up-next).
      expect(entryIds(s1.entries.slice(2))).not.toEqual(["e2", "e3", "e4"]);
      // The whole multiset is preserved (non-destructive).
      expect(new Set(entryIds(s1.entries))).toEqual(new Set(["e0", "e1", "e2", "e3", "e4"]));
    });

    it("turning OFF restores the authored order with the SAME entry current", () => {
      const s0 = playNow(emptyQueue(), entries(["a", "b", "c", "d", "e"]), 1);
      const s1 = setShuffle(s0, true, zeroRng);
      const s2 = setShuffle(s1, false);
      expect(entryIds(s2.entries)).toEqual(["e0", "e1", "e2", "e3", "e4"]);
      expect(currentEntry(s2)?.entryId).toBe("e1"); // still b, still current by id
      expect(s2.authoredOrder).toBeNull();
    });

    it("is a no-op on an empty Queue, when already shuffled, and when un-shuffling an unshuffled Queue", () => {
      expect(setShuffle(emptyQueue(), true)).toEqual(emptyQueue());
      const s1 = setShuffle(playNow(emptyQueue(), entries(["a", "b", "c"]), 0), true, zeroRng);
      expect(setShuffle(s1, true)).toBe(s1); // already shuffled
      const s0 = playNow(emptyQueue(), entries(["a", "b"]), 0);
      expect(setShuffle(s0, false)).toBe(s0); // not shuffled → off is a no-op
    });

    it("un-shuffle reconciles a stale/mismatched snapshot (drops unknown ids, appends new ones)", () => {
      const stale: QueueState = {
        entries: entries(["a", "b", "c"]), // e0/a, e1/b, e2/c
        currentIndex: 0, // current = e0/a
        repeat: "off",
        authoredOrder: ["e1", "gone", "e0"], // unknown "gone"; missing e2
      };
      const rec = setShuffle(stale, false);
      // "gone" dropped; e2 (present but absent from the snapshot) appended at the end.
      expect(entryIds(rec.entries)).toEqual(["e1", "e0", "e2"]);
      expect(currentEntry(rec)?.entryId).toBe("e0"); // current preserved by id
      expect(rec.authoredOrder).toBeNull();
    });
  });

  describe("authoredOrder stays coherent while shuffled", () => {
    const zeroRng = () => 0;
    const shuffled = () =>
      setShuffle(playNow(emptyQueue(), entries(["a", "b", "c"]), 0), true, zeroRng);

    it("enqueue appends the new id to BOTH the live order and the snapshot", () => {
      const s = shuffled(); // authoredOrder e0,e1,e2
      const added = enqueue(s, [{ entryId: "eX", title: title("x") }]);
      expect(added.authoredOrder).toEqual(["e0", "e1", "e2", "eX"]);
      // A later un-shuffle restores the authored order INCLUDING the appended entry.
      expect(entryIds(setShuffle(added, false).entries)).toEqual(["e0", "e1", "e2", "eX"]);
    });

    it("playNext appends the new id to the snapshot too", () => {
      const s = shuffled();
      const added = playNext(s, [{ entryId: "eY", title: title("y") }]);
      expect(added.authoredOrder).toEqual(["e0", "e1", "e2", "eY"]);
    });

    it("removeEntry drops the removed id from the snapshot", () => {
      const s = shuffled();
      const removed = removeEntry(s, "e2");
      expect(removed.authoredOrder).toEqual(["e0", "e1"]);
      expect(entryIds(setShuffle(removed, false).entries)).toEqual(["e0", "e1"]);
    });

    it("reorder leaves the snapshot unchanged (a permutation adds/removes no ids)", () => {
      const s = shuffled();
      const reordered = reorder(s, [...entryIds(s.entries)].reverse());
      expect(reordered.authoredOrder).toEqual(s.authoredOrder);
    });
  });

  describe("next / advance under Repeat mode", () => {
    const withRepeat = (s: QueueState, repeat: QueueState["repeat"]): QueueState => ({
      ...s,
      repeat,
    });

    it("next wraps to the first entry at the end ONLY under repeat-all", () => {
      const last = playNow(emptyQueue(), entries(["a", "b"]), 1);
      expect(next(last)).toBe(last); // off → clean stop (no-op)
      expect(next(withRepeat(last, "all")).currentIndex).toBe(0); // wraps
      const lastOne = withRepeat(last, "one");
      expect(next(lastOne)).toBe(lastOne); // one → no wrap either (no-op at the end)
    });

    it("manual next/prev always move and are NEVER trapped by repeat-one", () => {
      const mid = withRepeat(playNow(emptyQueue(), entries(["a", "b", "c"]), 1), "one");
      expect(next(mid).currentIndex).toBe(2); // moves despite repeat-one
      expect(prev(mid).currentIndex).toBe(0); // moves despite repeat-one
    });

    it("advance replays (stays put) under repeat-one", () => {
      const one = withRepeat(playNow(emptyQueue(), entries(["a", "b"]), 0), "one");
      expect(advance(one)).toBe(one); // pointer unchanged — the bar re-seeks the element
    });

    it("advance wraps the last entry to the first under repeat-all, advances normally mid-list", () => {
      const allLast = withRepeat(playNow(emptyQueue(), entries(["a", "b"]), 1), "all");
      expect(advance(allLast).currentIndex).toBe(0);
      const allMid = withRepeat(playNow(emptyQueue(), entries(["a", "b", "c"]), 0), "all");
      expect(advance(allMid).currentIndex).toBe(1);
    });

    it("advance stops cleanly at the end under repeat-off, advances normally mid-list", () => {
      const offLast = playNow(emptyQueue(), entries(["a", "b"]), 1);
      expect(advance(offLast)).toBe(offLast); // no-op at the end
      expect(advance(playNow(emptyQueue(), entries(["a", "b"]), 0)).currentIndex).toBe(1);
    });
  });

  describe("playNow carries repeat but resets shuffle", () => {
    it("a fresh play context keeps the repeat mode and is never shuffled", () => {
      const prior = cycleRepeat(
        setShuffle(playNow(emptyQueue(), entries(["a", "b", "c"]), 0), true, () => 0),
      ); // repeat = all, shuffled
      const fresh = playNow(prior, entries(["x", "y"]), 0);
      expect(fresh.repeat).toBe("all"); // repeat is a persistent preference
      expect(fresh.authoredOrder).toBeNull(); // a new context starts un-shuffled
    });
  });
});
