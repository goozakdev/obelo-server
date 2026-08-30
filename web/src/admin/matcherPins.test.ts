import { describe, it, expect } from "vitest";
import type { MatcherSlot, MatcherSlotRecord, SlotPosition } from "../api/types";
import {
  NO_PINS,
  changedSlots,
  clearRecord,
  fillRun,
  pairRun,
  recordAt,
  repoint,
  slotKey,
  toApplySlots,
  type PinDrafts,
} from "./matcherPins";

// The record half of a Slot, as the Admin arranges it before Apply.
//
// The Batman case is the whole reason this model exists: five files placed into a
// Season 4 the Show's provider does not have, whose records live in season 1 of a
// re-numbered continuation series. Those records are numbered 1..5 and the Show
// has a real Season 1, so the single most important property here is a negative
// one — borrowing a record must never write its numbering onto the Slot.

const TNBA = "77777";

/** The foreign series' season 1, as `listSeriesSlots` returns it. */
const foreignSeason1: MatcherSlot[] = [
  { group: 1, slot: 1, name: "Holiday Knights" },
  { group: 1, slot: 2, name: "Sins of the Father" },
  { group: 1, slot: 3, name: "Cold Comfort" },
  { group: 1, slot: 4, name: "Double Talk" },
  { group: 1, slot: 5, name: "You Scratch My Back" },
];

/** The five Slots the files were placed on: S04E01..E05, local numbering. */
const season4: SlotPosition[] = [1, 2, 3, 4, 5].map((slot) => ({ group: 4, slot }));

/** No Slot has a stored record — the state right after the files were placed. */
const nothingStored = () => undefined;

const storedOf =
  (map: Record<string, MatcherSlotRecord>) =>
  (position: SlotPosition): MatcherSlotRecord | undefined =>
    map[slotKey(position)];

describe("matcherPins", () => {
  it("fills a whole group's records from a foreign group in one gesture", () => {
    const pins = fillRun(NO_PINS, season4, foreignSeason1, TNBA, nothingStored);

    expect(changedSlots(pins)).toHaveLength(5);
    for (let i = 0; i < 5; i++) {
      const position = season4[i];
      const record = recordAt(pins, position, nothingStored);
      expect(record).not.toBeNull();
      // The record names its OWN series' position. The Slot keeps S04E0n.
      expect(record).toMatchObject({ externalId: TNBA, group: 1, slot: i + 1 });
      expect(position).toEqual({ group: 4, slot: i + 1 });
      // The borrowed words come with it, so the Slot can show them before Apply.
      expect(record?.name).toBe(foreignSeason1[i].name);
    }
  });

  it("never lets a borrowed record's numbering become a Slot's position", () => {
    // The collision ADR-0044 exists to prevent: the borrowed run is numbered 1..5
    // and the Show has a real Season 1. Nothing this module produces may address a
    // Slot by the record's numbers.
    const pins = fillRun(NO_PINS, season4, foreignSeason1, TNBA, nothingStored);
    const addressed = toApplySlots(pins).map((s) => `${s.group}:${s.slot}`);
    expect(addressed).toEqual(["4:1", "4:2", "4:3", "4:4", "4:5"]);
    expect(addressed).not.toContain("1:1");
  });

  it("pairs runs of different lengths as far as they go, and no further", () => {
    // Seven records offered for five Slots: the extra two are simply unused.
    const long = [...foreignSeason1, { group: 1, slot: 6 }, { group: 1, slot: 7 }];
    expect(pairRun(season4, long)).toHaveLength(5);

    // Three records for five Slots: the last two Slots are left exactly as they
    // were — clearing them is a decision the Admin did not make.
    const short = foreignSeason1.slice(0, 3);
    const pins = fillRun(NO_PINS, season4, short, TNBA, nothingStored);
    expect(changedSlots(pins)).toEqual([
      { group: 4, slot: 1 },
      { group: 4, slot: 2 },
      { group: 4, slot: 3 },
    ]);
    expect(recordAt(pins, { group: 4, slot: 5 }, nothingStored)).toBeNull();
  });

  it("sends a cleared pin as an explicit null, not as an omission", () => {
    // Omitting it would mean "leave the record alone", which is the opposite of
    // what the Admin said. A stored pin is needed for the clear to be a change.
    const stored = storedOf({ "4:1": { externalId: TNBA, group: 1, slot: 1 } });
    const pins = clearRecord(NO_PINS, { group: 4, slot: 1 }, stored);

    expect(recordAt(pins, { group: 4, slot: 1 }, stored)).toBeNull();
    expect(toApplySlots(pins)).toEqual([{ group: 4, slot: 1, record: null }]);
  });

  it("treats an untouched Slot as absent so an ordinary Apply cannot clear it", () => {
    const stored = storedOf({ "4:1": { externalId: TNBA, group: 1, slot: 1 } });
    const pins: PinDrafts = NO_PINS;

    expect(toApplySlots(pins)).toEqual([]);
    // ...and it still shows the record the server last read.
    expect(recordAt(pins, { group: 4, slot: 1 }, stored)).toMatchObject({ slot: 1 });
  });

  it("counts re-picking the record a Slot already has as no change", () => {
    const stored = storedOf({ "4:1": { externalId: TNBA, group: 1, slot: 1 } });
    const pins = repoint(
      NO_PINS,
      { group: 4, slot: 1 },
      { externalId: TNBA, group: 1, slot: 1, name: "Holiday Knights" },
      stored,
    );
    // Otherwise Apply would report work it is not doing and re-pend the Title's
    // enrichment for nothing.
    expect(toApplySlots(pins)).toEqual([]);
  });

  it("offers a same-series record exactly like a foreign one", () => {
    // The common case, not the exotic one: the provider counts a run in the NEXT
    // season of the same series. Nothing about the shape changes.
    const own = "1438";
    const pins = repoint(
      NO_PINS,
      { group: 3, slot: 61 },
      { externalId: own, group: 4, slot: 1, name: "Holiday Knights" },
      nothingStored,
    );
    expect(toApplySlots(pins)).toEqual([
      { group: 3, slot: 61, record: { externalId: own, group: 4, slot: 1, name: "Holiday Knights" } },
    ]);
  });

  it("undoes a re-pointing when the draft is dropped, exactly like a placement", () => {
    // Revert is "go back to the snapshot", which for records is "drop the drafts".
    const stored = storedOf({ "4:1": { externalId: TNBA, group: 1, slot: 1 } });
    const pins = clearRecord(NO_PINS, { group: 4, slot: 1 }, stored);
    expect(toApplySlots(pins)).toHaveLength(1);
    expect(toApplySlots(NO_PINS)).toEqual([]);
    expect(recordAt(NO_PINS, { group: 4, slot: 1 }, stored)).toMatchObject({ slot: 1 });
  });
});
