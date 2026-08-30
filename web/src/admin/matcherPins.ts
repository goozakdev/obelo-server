import type {
  MatcherApplySlot,
  MatcherSlot,
  MatcherSlotRecord,
  SlotPosition,
} from "../api/types";

// The second half of a Slot, as the Admin is currently arranging it: not where a
// File sits, but which provider record decorates it (CONTEXT.md "Episode pin",
// ADR-0044).
//
// Pure data and pure functions, for the same reason matcherArrangement.ts is:
// nothing here commits until Apply, so the whole model has to be editable and
// testable without a DOM. Three rules live in this file and nowhere else.
//
//   * A POSITION IS NOT A RECORD. A record names a position in ITS OWN series and
//     stays there. Nothing in this file ever writes a borrowed group/slot onto the
//     Slot it decorates — that is the collision ADR-0044 exists to prevent, and the
//     five Batman files would land on top of the Show's real Season 1.
//   * CLEARING IS A DECISION. `null` is not "no draft": it is the Admin saying
//     "back to this series, this position", which has to reach the server as an
//     explicit entry. An untouched Slot is simply absent.
//   * A RUN IS ONE GESTURE. Borrowing a whole season pairs the group's filled
//     Slots against the foreign group's records in order — the real shape of the
//     problem — rather than five separate pins.
//
// Kind-neutral throughout: groups, slots, records. The Album matcher uses it
// unchanged.

/** Map key for a Slot's local position. */
export const slotKey = (p: SlotPosition): string => `${p.group}:${p.slot}`;

/** What the Admin has said about Slots' records this session: a record to pin, or
 * `null` where they cleared one. A Slot they have not touched is ABSENT, which is
 * what keeps an ordinary rearrangement from disturbing pins it never mentioned. */
export type PinDrafts = ReadonlyMap<string, MatcherSlotRecord | null>;

export const NO_PINS: PinDrafts = new Map<string, MatcherSlotRecord | null>();

/** Where a Slot's stored record comes from — the document the screen was last
 * handed. Passed in rather than looked up here so this module stays pure. */
export type StoredRecordAt = (position: SlotPosition) => MatcherSlotRecord | undefined;

const sameRecord = (
  a: MatcherSlotRecord | null | undefined,
  b: MatcherSlotRecord | null | undefined,
): boolean => {
  if (!a || !b) return !a && !b;
  // Only the ADDRESS is compared. The words are decoration fetched per group and
  // may be absent on one side; re-picking the same record must still read as "no
  // change" rather than as an Apply the Admin did not make.
  return (a.externalId ?? "") === (b.externalId ?? "") && a.group === b.group && a.slot === b.slot;
};

/** The record a Slot shows right now: the Admin's draft where they touched it,
 * otherwise whatever the server last read. `null` means "no record — this Slot is
 * decorated from its own position", which for a Slot the container's series does
 * not list means bare. */
export function recordAt(
  pins: PinDrafts,
  position: SlotPosition,
  stored: StoredRecordAt,
): MatcherSlotRecord | null {
  const key = slotKey(position);
  if (pins.has(key)) return pins.get(key) ?? null;
  return stored(position) ?? null;
}

function withDraft(
  pins: PinDrafts,
  position: SlotPosition,
  record: MatcherSlotRecord | null,
  stored: StoredRecordAt,
): PinDrafts {
  const next = new Map(pins);
  const key = slotKey(position);
  // A draft that lands back on what the server already has is not a change. Left
  // in, it would make Apply count work it is not doing and re-pend the Title's
  // enrichment for nothing.
  if (sameRecord(record, stored(position) ?? null)) next.delete(key);
  else next.set(key, record);
  return next;
}

/** Point one Slot's record at a provider record. The Slot's own position is not a
 * parameter of the record and never moves. */
export function repoint(
  pins: PinDrafts,
  position: SlotPosition,
  record: MatcherSlotRecord,
  stored: StoredRecordAt,
): PinDrafts {
  return withDraft(pins, position, record, stored);
}

/** Return a Slot to its default record — this series, this position. For a Slot
 * the container's series does not list, that means bare again. */
export function clearRecord(
  pins: PinDrafts,
  position: SlotPosition,
  stored: StoredRecordAt,
): PinDrafts {
  return withDraft(pins, position, null, stored);
}

/** Pair a run of Slots against a run of records, in order — the bulk gesture.
 *
 * Different lengths pair as far as they go and stop: extra Slots keep the record
 * they had (rather than being cleared, which the Admin did not ask for) and extra
 * records go unused. The caller says so out loud; guessing at an alignment the
 * Admin did not state is exactly what this screen exists to stop doing. */
export function pairRun(
  targets: readonly SlotPosition[],
  records: readonly MatcherSlot[],
): { target: SlotPosition; record: MatcherSlot }[] {
  const n = Math.min(targets.length, records.length);
  const out: { target: SlotPosition; record: MatcherSlot }[] = [];
  for (let i = 0; i < n; i++) out.push({ target: targets[i], record: records[i] });
  return out;
}

/** Fill a run of Slots' records from a foreign (or the same) series' group in one
 * gesture. `externalId` is the series the records were listed from. */
export function fillRun(
  pins: PinDrafts,
  targets: readonly SlotPosition[],
  records: readonly MatcherSlot[],
  externalId: string,
  stored: StoredRecordAt,
): PinDrafts {
  let next = pins;
  for (const { target, record } of pairRun(targets, records)) {
    next = repoint(next, target, toRecordRef(record, externalId), stored);
  }
  return next;
}

/** One listed provider record as a Slot's record reference: its position in ITS
 * series, plus the words to show. */
export function toRecordRef(record: MatcherSlot, externalId: string): MatcherSlotRecord {
  return {
    externalId,
    group: record.group,
    slot: record.slot,
    name: record.name,
    overview: record.overview,
    airDate: record.airDate,
    stillUrl: record.stillUrl,
  };
}

/** The Slots whose record the Admin changed — what Apply counts and Revert
 * undoes, alongside the Files that moved. */
export function changedSlots(pins: PinDrafts): SlotPosition[] {
  return [...pins.keys()]
    .map((key) => {
      const [group, slot] = key.split(":");
      return { group: Number(group), slot: Number(slot) };
    })
    .sort((a, b) => a.group - b.group || a.slot - b.slot);
}

/** The `slots` half of the Apply body: only the Slots the Admin decided about, in
 * a stable order. A cleared pin is sent as an explicit null record — omitting it
 * would mean "leave it alone", which is the opposite. */
export function toApplySlots(pins: PinDrafts): MatcherApplySlot[] {
  return changedSlots(pins).map((position) => ({
    group: position.group,
    slot: position.slot,
    record: pins.get(slotKey(position)) ?? null,
  }));
}
