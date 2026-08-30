import type { SlotsUnavailableReason } from "../api/types";

// The kind-neutral seam of the file matcher.
//
// `FileMatcher.tsx` renders groups, slots, files and ordinals — the same four
// things the API puts on the wire — and knows no other vocabulary. Every word an
// Admin actually reads comes from here, so the Album matcher is an adapter
// (`albumMatcherLabels` + a route) rather than a second screen that looks like
// this one (ADR-0044).
//
// The rule this file enforces: if a string says "season" or "episode", it belongs
// in the TV adapter, not in the component.

export interface MatcherLabels {
  /** One group's name: "Season 4", "Specials", "Disc 2". */
  groupName(group: number): string;
  /** A Slot's compact code: "S06E05", "2-04". Shown wherever the position itself
   * is the subject, which is most of this screen. */
  slotCode(group: number, slot: number): string;
  /** Lower-case nouns for prose: "episode" / "episodes", "season". */
  slotNoun: string;
  slotNounPlural: string;
  groupNoun: string;
  /** What a provider's whole record set is called — "series" for TV, "release"
   * for music. Read when a Slot's record was borrowed from another one, which is
   * the only place this screen names the provider's side of the world. */
  seriesNoun: string;
  /** The pinned tray of Files claiming no group at all (group -1). */
  unsortedName: string;
  unsortedHint: string;
  /** One sentence per `slotsUnavailable` reason. Four reasons, four DIFFERENT
   * things to go and fix — "no titles available" would tell the Admin none of
   * them, which is the whole point of the field being an enum. */
  unavailableSentence(reason: SlotsUnavailableReason): string;
}
