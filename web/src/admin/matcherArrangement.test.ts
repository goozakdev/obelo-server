import { describe, it, expect } from "vitest";
import type { MatcherFile } from "../api/types";
import {
  arrangeParts,
  arrangementFromFiles,
  changedPaths,
  displaceAt,
  filesAtSlot,
  groupHintForPath,
  homeGroupOf,
  ignoreFile,
  isShared,
  movePartBefore,
  placeFile,
  reorderPart,
  restoreFile,
  toApplyFiles,
  unassignFile,
  UNSORTED_GROUP,
} from "./matcherArrangement";

// The arrangement model. Everything here is a rule the screen depends on and the
// server enforces, so it is asserted directly rather than through a render.

function file(over: Partial<MatcherFile> & { path: string }): MatcherFile {
  return {
    state: "placed",
    parsed: [],
    placements: [],
    decided: false,
    orphaned: false,
    reason: "",
    ...over,
  };
}

const A = "/tv/Show/Season 03/Show - S03E01 - One.mkv";
const B = "/tv/Show/Season 03/Show - S03E02 - Two.mkv";
const LOOSE = "/tv/Show/extra.mkv";

const base = () =>
  arrangementFromFiles([
    file({
      path: A,
      parsed: [{ group: 3, slot: 1 }],
      placements: [{ group: 3, slot: 1, ordinal: 1 }],
    }),
    file({
      path: B,
      parsed: [{ group: 3, slot: 2 }],
      placements: [{ group: 3, slot: 2, ordinal: 1 }],
    }),
    file({ path: LOOSE, state: "unassigned" }),
  ]);

describe("group derivation", () => {
  it("mirrors the scanner's season-folder hint, including Specials", () => {
    expect(groupHintForPath("/tv/Show/Season 03/a.mkv")).toBe(3);
    expect(groupHintForPath("/tv/Show/season_4/a.mkv")).toBe(4);
    expect(groupHintForPath("/tv/Show/Specials/a.mkv")).toBe(0);
    expect(groupHintForPath("/tv/Show/a.mkv")).toBe(UNSORTED_GROUP);
  });

  it("puts a file with no position and no season folder in the Unsorted tray", () => {
    const arr = base();
    expect(homeGroupOf(arr.get(LOOSE)!)).toBe(UNSORTED_GROUP);
    expect(homeGroupOf(arr.get(A)!)).toBe(3);
  });
});

describe("placing", () => {
  it("moves a file to another group's slot and empties the one it left", () => {
    const next = placeFile(base(), A, { group: 4, slot: 1 }, "move");
    expect(filesAtSlot(next, { group: 3, slot: 1 })).toHaveLength(0);
    expect(filesAtSlot(next, { group: 4, slot: 1 }).map((f) => f.path)).toEqual([A]);
  });

  it("shares one file across two slots and marks it shared", () => {
    const next = placeFile(base(), A, { group: 3, slot: 2 }, "share");
    expect(filesAtSlot(next, { group: 3, slot: 1 }).map((f) => f.path)).toEqual([A]);
    expect(filesAtSlot(next, { group: 3, slot: 2 }).map((f) => f.path)).toEqual([B, A]);
    expect(isShared(next.get(A)!)).toBe(true);
  });

  it("leaves the sitting file in place — displace or merge is the caller's decision", () => {
    const next = placeFile(base(), A, { group: 3, slot: 2 }, "move");
    expect(filesAtSlot(next, { group: 3, slot: 2 }).map((f) => f.path)).toEqual([B, A]);
  });

  it("displaces the sitting file back to unassigned, never off the screen", () => {
    const merged = placeFile(base(), A, { group: 3, slot: 2 }, "move");
    const next = displaceAt(merged, { group: 3, slot: 2 }, A);
    expect(filesAtSlot(next, { group: 3, slot: 2 }).map((f) => f.path)).toEqual([A]);
    expect(next.get(B)!.state).toBe("unassigned");
    expect(next.get(B)!.placements).toHaveLength(0);
  });

  it("renumbers parts densely after a part leaves", () => {
    let arr = placeFile(base(), A, { group: 3, slot: 2 }, "move");
    expect(arr.get(A)!.placements[0].ordinal).toBe(2);
    arr = unassignFile(arr, B);
    expect(arr.get(A)!.placements[0].ordinal).toBe(1);
  });
});

describe("part order", () => {
  const merged = () => placeFile(base(), A, { group: 3, slot: 2 }, "move");
  const slot = { group: 3, slot: 2 };

  it("reorders with the up/down control", () => {
    const next = reorderPart(merged(), slot, A, -1);
    expect(filesAtSlot(next, slot).map((f) => f.path)).toEqual([A, B]);
  });

  it("reorders by dropping one part in front of another", () => {
    const next = movePartBefore(merged(), slot, A, B);
    expect(filesAtSlot(next, slot).map((f) => f.path)).toEqual([A, B]);
    expect(next.get(A)!.placements[0].ordinal).toBe(1);
    expect(next.get(B)!.placements[0].ordinal).toBe(2);
  });

  it("refuses to move a part past either end", () => {
    const arr = merged();
    expect(reorderPart(arr, slot, B, -1)).toBe(arr);
  });
});

describe("ignoring", () => {
  it("takes a file out of the working set and back again", () => {
    let arr = ignoreFile(base(), LOOSE);
    expect(arr.get(LOOSE)!.state).toBe("ignored");
    arr = restoreFile(arr, LOOSE);
    expect(arr.get(LOOSE)!.state).toBe("unassigned");
  });

  it("empties the slot of a file that was placed", () => {
    const arr = ignoreFile(base(), A);
    expect(filesAtSlot(arr, { group: 3, slot: 1 })).toHaveLength(0);
  });
});

describe("changedPaths", () => {
  it("counts only what actually moved", () => {
    const start = base();
    expect(changedPaths(start, start)).toEqual([]);
    expect(changedPaths(start, placeFile(start, A, { group: 4, slot: 1 }, "move"))).toEqual([A]);
  });
});

describe("toApplyFiles", () => {
  it("omits a file that ends up exactly where its filename says", () => {
    // The omission is load-bearing in both directions: it keeps an untouched file
    // open to a future scanner improvement, and it is the ONLY way to take a
    // stored correction back.
    expect(toApplyFiles(base())).toEqual([]);
  });

  it("sends a moved file with its placement", () => {
    const next = placeFile(base(), A, { group: 4, slot: 1 }, "move");
    expect(toApplyFiles(next)).toEqual([
      { path: A, state: "placed", placements: [{ group: 4, slot: 1, ordinal: 1 }] },
    ]);
  });

  it("sends BOTH parts of a merge, even the one that parses onto that slot", () => {
    // A Placement colliding with a bare parse is settled by DISPLACING the parsed
    // file. Mentioning only the moved half would therefore silently un-merge it.
    const next = placeFile(base(), A, { group: 3, slot: 2 }, "move");
    const payload = toApplyFiles(next);
    expect(payload.map((f) => f.path).sort()).toEqual([A, B].sort());
    expect(payload.find((f) => f.path === B)).toEqual({
      path: B,
      state: "placed",
      placements: [{ group: 3, slot: 2, ordinal: 1 }],
    });
    expect(payload.find((f) => f.path === A)).toEqual({
      path: A,
      state: "placed",
      placements: [{ group: 3, slot: 2, ordinal: 2 }],
    });
  });

  it("sends an unassigned file as a decision, because absence would mean the parse", () => {
    expect(toApplyFiles(unassignFile(base(), A))).toEqual([{ path: A, state: "unassigned" }]);
  });

  it("omits an unnumbered file nobody has decided about", () => {
    // LOOSE parses onto nothing and is unassigned: that IS the parse, so it
    // carries no decision.
    expect(toApplyFiles(base()).find((f) => f.path === LOOSE)).toBeUndefined();
  });

  it("always sends an ignored file", () => {
    expect(toApplyFiles(ignoreFile(base(), LOOSE))).toEqual([
      { path: LOOSE, state: "ignored" },
    ]);
  });

  it("sends both slots of a file stretched across two", () => {
    const next = placeFile(base(), A, { group: 3, slot: 5 }, "share");
    expect(toApplyFiles(next)).toEqual([
      {
        path: A,
        state: "placed",
        placements: [
          { group: 3, slot: 1, ordinal: 1 },
          { group: 3, slot: 5, ordinal: 1 },
        ],
      },
    ]);
  });
});

describe("arrangeParts", () => {
  it("merges the files a collision named onto that slot, in the order given", () => {
    // The order matters: the fix a 409 SLOT_COLLISION offers is "merge them as
    // parts", and the parts have to come out in the order the panel showed them,
    // not in whatever order their previous ordinals happened to be.
    const arr = arrangeParts(unassignFile(base(), A), { group: 3, slot: 2 }, [A, B]);
    expect(filesAtSlot(arr, { group: 3, slot: 2 }).map((f) => f.path)).toEqual([A, B]);
    expect(toApplyFiles(arr)).toEqual([
      { path: A, state: "placed", placements: [{ group: 3, slot: 2, ordinal: 1 }] },
      { path: B, state: "placed", placements: [{ group: 3, slot: 2, ordinal: 2 }] },
    ]);
  });
});
