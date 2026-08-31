import type {
  MatcherApplyFile,
  MatcherFile,
  MatcherPlacement,
  SlotPosition,
} from "../api/types";

// The file matcher's model: which File sits on which Slot, as the Admin is
// currently arranging it. Pure data and pure functions — no React, no fetch —
// because every rule that matters on this screen lives here and has to be
// testable without a DOM:
//
//   * nothing commits until Apply, so the arrangement is edited locally and
//     Revert is just "go back to the snapshot taken at open";
//   * the Apply payload is SPARSE (ADR-0027's precedent) — a File that ends up
//     where its filename already says is OMITTED, which is how a correction is
//     taken BACK, and getting that backwards would silently freeze every file in
//     the Show against future scanner improvements;
//   * a merge must be stated for EVERY file on the Slot, because a Placement
//     colliding with a bare parse is settled by displacing the parsed file — so
//     "both of these, in this order" cannot be said by mentioning only one.
//
// Kind-neutral throughout: groups, slots, ordinals, paths. The Album matcher uses
// this file unchanged.

/** The Unsorted tray: Files claiming no group at all. The server puts them in
 * group -1 and so does every derivation here. */
export const UNSORTED_GROUP = -1;

export interface ArrangedFile {
  path: string;
  state: MatcherFile["state"];
  /** Where the File sits now, ordered by group then slot. */
  placements: MatcherPlacement[];
  /** What the FILENAME claims, with every decision ignored. Never edited. */
  parsed: SlotPosition[];
  titleId?: string;
  /** Whether a stored decision (rather than the parse) produced the opening
   * state. Carried for display; the Apply payload is derived from the parse. */
  decided: boolean;
  orphaned: boolean;
  /** ffprobe refused this file. Unlike every other field here it is true of files that are
   * correctly PLACED: the name numbered it and the bytes are broken, so the screen has to say
   * so or it reports the one unplayable file as fine (ADR-0047). */
  unreadable: boolean;
  reason: string;
}

/** Path → file. A Map, so iteration order is the server's order (which is the
 * order the Admin will read the columns in). */
export type Arrangement = ReadonlyMap<string, ArrangedFile>;

const sortPlacements = (ps: MatcherPlacement[]): MatcherPlacement[] =>
  [...ps].sort((a, b) => a.group - b.group || a.slot - b.slot || a.ordinal - b.ordinal);

export function arrangementFromFiles(files: readonly MatcherFile[]): Arrangement {
  const out = new Map<string, ArrangedFile>();
  for (const f of files) {
    out.set(f.path, {
      path: f.path,
      state: f.state,
      placements: sortPlacements(f.placements),
      parsed: [...f.parsed],
      titleId: f.titleId,
      decided: f.decided,
      orphaned: f.orphaned ?? false,
      unreadable: f.unreadable ?? false,
      reason: f.reason ?? "",
    });
  }
  return out;
}

function withFile(arr: Arrangement, file: ArrangedFile): Arrangement {
  const next = new Map(arr);
  next.set(file.path, file);
  return next;
}

const samePosition = (a: SlotPosition, b: SlotPosition) => a.group === b.group && a.slot === b.slot;

/** Mirror of the scanner's season-folder hint, which is what the server falls back
 * to when a File claims no position at all. The client has to agree with it, or a
 * File would render in a different column than the one its counts were tallied in. */
export function groupHintForPath(path: string): number {
  const parts = path.split(/[/\\]+/).filter((p) => p !== "");
  const dir = parts.length >= 2 ? parts[parts.length - 2] : "";
  const name = dir.trim();
  if (name.toLowerCase() === "specials") return 0;
  const m = /^season[ _]?(\d{1,4})$/i.exec(name);
  return m ? Number(m[1]) : UNSORTED_GROUP;
}

/** Which group's column a File belongs in: where it sits, else what it claims,
 * else its folder — the same order the server tallies its counts in. */
export function homeGroupOf(file: ArrangedFile): number {
  if (file.placements.length > 0) return file.placements[0].group;
  if (file.parsed.length > 0) return file.parsed[0].group;
  return groupHintForPath(file.path);
}

/** The Files on one Slot, in part order. More than one is a multi-part Edition. */
export function filesAtSlot(arr: Arrangement, target: SlotPosition): ArrangedFile[] {
  const out: ArrangedFile[] = [];
  for (const f of arr.values()) {
    if (f.state !== "placed") continue;
    if (f.placements.some((p) => samePosition(p, target))) out.push(f);
  }
  return out.sort(
    (a, b) => ordinalAt(a, target) - ordinalAt(b, target) || a.path.localeCompare(b.path),
  );
}

export function ordinalAt(file: ArrangedFile, target: SlotPosition): number {
  return file.placements.find((p) => samePosition(p, target))?.ordinal ?? 1;
}

/** True when this File fills more than one Slot — one File across several Slots,
 * which the screen marks as shared on each. */
export const isShared = (file: ArrangedFile): boolean => file.placements.length > 1;

/** Renumber a Slot's parts 1..n in their current order, so the ordinals stay dense
 * after a removal. */
function renumber(arr: Arrangement, target: SlotPosition): Arrangement {
  let next = arr;
  filesAtSlot(arr, target).forEach((f, i) => {
    next = withFile(next, {
      ...f,
      placements: f.placements.map((p) => (samePosition(p, target) ? { ...p, ordinal: i + 1 } : p)),
    });
  });
  return next;
}

export type PlaceMode =
  /** The File leaves wherever it was and fills only this Slot. */
  | "move"
  /** The File keeps its other Slots and ALSO fills this one (one File, several
   * Slots — the `S01E01-02` case, without renaming anything). */
  | "share";

/** Put a File on a Slot. Any Files already there STAY — deciding what happens to
 * them is the caller's job (displace or merge), and it is a decision the screen
 * must make explicit rather than guess. */
export function placeFile(
  arr: Arrangement,
  path: string,
  target: SlotPosition,
  mode: PlaceMode = "move",
): Arrangement {
  const file = arr.get(path);
  if (!file) return arr;
  const already = file.placements.some((p) => samePosition(p, target));
  const kept = mode === "share" ? file.placements.filter((p) => !samePosition(p, target)) : [];
  const ordinal = already
    ? ordinalAt(file, target)
    : filesAtSlot(arr, target).length + 1;
  const vacated = mode === "share" ? [] : file.placements.filter((p) => !samePosition(p, target));
  let next = withFile(arr, {
    ...file,
    state: "placed",
    placements: sortPlacements([...kept, { ...target, ordinal }]),
  });
  // Slots the File just left may have a hole in their part numbering.
  for (const p of vacated) next = renumber(next, p);
  return renumber(next, target);
}

/** Take every OTHER File off this Slot and return it to the unassigned column —
 * the displacement half of "the drop that must not be offerable". */
export function displaceAt(arr: Arrangement, target: SlotPosition, keepPath: string): Arrangement {
  let next = arr;
  for (const f of filesAtSlot(arr, target)) {
    if (f.path === keepPath) continue;
    const rest = f.placements.filter((p) => !samePosition(p, target));
    next = withFile(next, {
      ...f,
      state: rest.length > 0 ? "placed" : "unassigned",
      placements: rest,
    });
  }
  return renumber(next, target);
}

/** Take a File off its Slots. A DECISION in its own right (Unassigned), not an
 * absence: a File the Admin deliberately took off a Slot would otherwise be
 * re-placed from its filename by the next scan. */
export function unassignFile(arr: Arrangement, path: string): Arrangement {
  const file = arr.get(path);
  if (!file) return arr;
  let next = withFile(arr, { ...file, state: "unassigned", placements: [] });
  for (const p of file.placements) next = renumber(next, p);
  return next;
}

/** Exclude a File — a sample, a stray rip, anything that is no Slot's content.
 * Nothing on disk is touched, ever. */
export function ignoreFile(arr: Arrangement, path: string): Arrangement {
  const file = arr.get(path);
  if (!file) return arr;
  let next = withFile(arr, { ...file, state: "ignored", placements: [] });
  for (const p of file.placements) next = renumber(next, p);
  return next;
}

/** Bring an Ignored File back into the working set, undecided. */
export function restoreFile(arr: Arrangement, path: string): Arrangement {
  const file = arr.get(path);
  if (!file) return arr;
  return withFile(arr, { ...file, state: "unassigned", placements: [] });
}

/** Move one part of a multi-part Slot up or down the order. `delta` is -1/+1. */
export function reorderPart(
  arr: Arrangement,
  target: SlotPosition,
  path: string,
  delta: number,
): Arrangement {
  const parts = filesAtSlot(arr, target);
  const from = parts.findIndex((f) => f.path === path);
  if (from < 0) return arr;
  const to = from + delta;
  if (to < 0 || to >= parts.length) return arr;
  const order = parts.map((f) => f.path);
  order.splice(to, 0, ...order.splice(from, 1));
  return applyPartOrder(arr, target, order);
}

/** Drop one part in front of another — the drag gesture for reordering parts. */
export function movePartBefore(
  arr: Arrangement,
  target: SlotPosition,
  path: string,
  beforePath: string,
): Arrangement {
  const order = filesAtSlot(arr, target).map((f) => f.path);
  const from = order.indexOf(path);
  const at = order.indexOf(beforePath);
  if (from < 0 || at < 0 || from === at) return arr;
  order.splice(from, 1);
  order.splice(order.indexOf(beforePath), 0, path);
  return applyPartOrder(arr, target, order);
}

function applyPartOrder(arr: Arrangement, target: SlotPosition, order: string[]): Arrangement {
  let next = arr;
  order.forEach((p, i) => {
    const f = next.get(p);
    if (!f) return;
    next = withFile(next, {
      ...f,
      placements: f.placements.map((pl) =>
        samePosition(pl, target) ? { ...pl, ordinal: i + 1 } : pl,
      ),
    });
  });
  return next;
}

/** Put an exact, ordered list of Files onto one Slot as its parts — the "merge
 * them as parts" fix a `409 SLOT_COLLISION` offers, where the ORDER is the one the
 * server listed and not whatever the previous ordinals happened to be. Files
 * already on the Slot that the list does not name keep their place, after it. */
export function arrangeParts(
  arr: Arrangement,
  target: SlotPosition,
  order: readonly string[],
): Arrangement {
  const rest = filesAtSlot(arr, target)
    .map((f) => f.path)
    .filter((p) => !order.includes(p));
  const vacated: SlotPosition[] = [];
  let next = arr;
  [...order, ...rest].forEach((path, i) => {
    const f = next.get(path);
    if (!f) return;
    const elsewhere = f.placements.filter((p) => !samePosition(p, target));
    // A named File MOVES here; one that was already here keeps its other Slots.
    if (order.includes(path)) vacated.push(...elsewhere);
    next = withFile(next, {
      ...f,
      state: "placed",
      placements: sortPlacements([
        ...(order.includes(path) ? [] : elsewhere),
        { ...target, ordinal: i + 1 },
      ]),
    });
  });
  for (const p of vacated) next = renumber(next, p);
  return next;
}

/** What the parse alone would produce for this File: placed on exactly the
 * positions its filename claims, or unassigned when it claims none. */
function matchesParse(file: ArrangedFile): boolean {
  if (file.state === "ignored") return false;
  if (file.state === "unassigned") return file.parsed.length === 0;
  if (file.placements.length !== file.parsed.length) return false;
  // A part number other than 1 is a merge, which is a decision even when the
  // positions themselves agree with the filename.
  if (file.placements.some((p) => p.ordinal !== 1)) return false;
  return file.parsed.every((p) => file.placements.some((q) => samePosition(p, q)));
}

const fingerprint = (file: ArrangedFile): string =>
  `${file.state}|${sortPlacements(file.placements)
    .map((p) => `${p.group}:${p.slot}:${p.ordinal}`)
    .join(",")}`;

/** The paths whose arrangement differs from the one at open — what Apply counts
 * and what Revert undoes. */
export function changedPaths(base: Arrangement, next: Arrangement): string[] {
  const out: string[] = [];
  for (const [path, file] of next) {
    const before = base.get(path);
    if (!before || fingerprint(before) !== fingerprint(file)) out.push(path);
  }
  return out;
}

/** The Apply body: the WHOLE arrangement, expressed sparsely.
 *
 * A File is OMITTED when it ends up exactly where its filename already says, and
 * that omission is load-bearing in both directions — it is how a File that was
 * never touched stays open to a future scanner improvement, and it is the ONLY way
 * to take a correction back (a sparse store spends "no row" on "follow the
 * filename", ADR-0027).
 *
 * The one exception is a File SHARING a Slot with another: a merge has to be
 * stated for every part, because a Placement colliding with a bare parse is
 * resolved by displacing the parsed File, not by merging with it. */
export function toApplyFiles(arr: Arrangement): MatcherApplyFile[] {
  const shared = new Set<string>();
  const occupants = new Map<string, string[]>();
  for (const f of arr.values()) {
    if (f.state !== "placed") continue;
    for (const p of f.placements) {
      const key = `${p.group}:${p.slot}`;
      occupants.set(key, [...(occupants.get(key) ?? []), f.path]);
    }
  }
  for (const [, paths] of occupants) {
    if (paths.length > 1) for (const p of paths) shared.add(p);
  }

  const out: MatcherApplyFile[] = [];
  for (const f of arr.values()) {
    if (matchesParse(f) && !shared.has(f.path)) continue;
    if (f.state === "placed") {
      out.push({ path: f.path, state: "placed", placements: sortPlacements(f.placements) });
    } else {
      out.push({ path: f.path, state: f.state });
    }
  }
  return out;
}
