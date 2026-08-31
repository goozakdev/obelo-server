import { describe, it, expect } from "vitest";
import {
  basename,
  compareTitles,
  comparePosition,
  comparableTitle,
  diffChars,
  elideContainerPrefix,
  splitContainerPrefix,
  titleFromFilename,
} from "./matcherCompare";

// The comparison rules, tested without a DOM.
//
// The single most important assertion in this file is the NEGATIVE one: a season
// of correctly-matched dotted filenames must light up nothing. A comparison that
// cries wolf on `Parks.and.Rec.S06E06.1080p.WEB-DL.x264-GRP` is worse than no
// comparison at all, because the Admin learns within one screenful to ignore the
// highlighting — and then misses the row that actually disagrees.

const SHOW = "Parks and Recreation";

describe("titleFromFilename", () => {
  it("keeps the episode title and drops the show prefix, the numbers and the extension", () => {
    expect(
      titleFromFilename("/tv/Parks and Recreation/Season 06/Parks and Recreation - S06E05 - Filibuster.mkv", SHOW),
    ).toBe("filibuster");
  });

  it("normalizes a scene release to NOTHING, because it carries no title at all", () => {
    // The row this produces is a correct match; the title comparison must have
    // nothing to say about it.
    expect(
      titleFromFilename("/tv/Parks and Recreation/Season 06/Parks.and.Rec.S06E06.1080p.WEB-DL.x264-GRP.mkv", SHOW),
    ).toBe("");
  });

  it("strips an ABBREVIATED show prefix, which is what filenames actually carry", () => {
    expect(titleFromFilename("Parks.and.Rec.S06E06.Filibuster.mkv", SHOW)).toBe("filibuster");
  });

  it("truncates at the first technical tag, so the release group never survives", () => {
    expect(
      titleFromFilename("Parks and Recreation - S06E05 - Filibuster [1080p][x264-SPARKS].mkv", SHOW),
    ).toBe("filibuster");
  });

  it("deletes a year in place rather than truncating at it", () => {
    // A year sits BEFORE the title often enough that truncating there would throw
    // the title away.
    expect(titleFromFilename("Show.2019.S01E01.The.Beginning.720p.mkv", "Show")).toBe(
      "the beginning",
    );
  });

  it("deletes the naming convention's part marker, which is filing and not title", () => {
    expect(
      titleFromFilename("Show - S01E01 - The Finale - part1.mkv", "Show"),
    ).toBe("the finale");
  });

  it("keeps a digit that belongs to the title", () => {
    // A bare `1` cannot be deleted as a token or `Chapter 1` loses its number; the
    // `5.1` channel layout is handled before tokenizing instead.
    expect(titleFromFilename("Show - S01E01 - Chapter 1 - 1080p DDP5.1.mkv", "Show")).toBe(
      "chapter 1",
    );
  });

  it("keeps a title when the filename carries no show prefix", () => {
    expect(titleFromFilename("S06E05 - Filibuster.mkv", SHOW)).toBe("filibuster");
  });

  it("handles Windows separators", () => {
    expect(basename("D:\\TV\\Show\\Season 01\\Show - S01E01 - Pilot.mkv")).toBe(
      "Show - S01E01 - Pilot.mkv",
    );
  });
});

describe("comparableTitle", () => {
  it("compares only on case and punctuation", () => {
    expect(comparableTitle("The Great Escape, Part 1!")).toBe("the great escape part 1");
  });
});

describe("diffChars", () => {
  it("finds a one-character disagreement with no similarity threshold", () => {
    const segments = diffChars("fillibuster", "filibuster");
    expect(segments.filter((s) => s.kind === "removed").map((s) => s.text).join("")).toBe("l");
    expect(segments.filter((s) => s.kind === "added")).toHaveLength(0);
    // Both sides still read as the whole word.
    expect(segments.filter((s) => s.kind !== "added").map((s) => s.text).join("")).toBe(
      "fillibuster",
    );
    expect(segments.filter((s) => s.kind !== "removed").map((s) => s.text).join("")).toBe(
      "filibuster",
    );
  });

  it("says nothing about identical strings", () => {
    expect(diffChars("filibuster", "filibuster")).toEqual([
      { text: "filibuster", kind: "same" },
    ]);
  });
});

describe("compareTitles", () => {
  it("shows a provider typo against the filename", () => {
    const c = compareTitles(
      "Fillibuster",
      "/tv/Parks and Recreation/Season 06/Parks and Recreation - S06E05 - Filibuster.mkv",
      SHOW,
    );
    expect(c.differs).toBe(true);
    expect(c.segments.some((s) => s.kind === "removed")).toBe(true);
  });

  it("stays silent on a correctly-matched dotted filename", () => {
    const c = compareTitles(
      "Filibuster",
      "/tv/Parks and Recreation/Season 06/Parks.and.Rec.S06E06.1080p.WEB-DL.x264-GRP.mkv",
      SHOW,
    );
    expect(c.differs).toBe(false);
    expect(c.segments).toHaveLength(0);
  });

  it("stays silent when the Slot has no record at all (the degraded path)", () => {
    const c = compareTitles(undefined, "Show - S01E01 - Pilot.mkv", "Show");
    expect(c.differs).toBe(false);
  });

  it("stays silent when the two agree once normalized", () => {
    expect(
      compareTitles("The Great Escape", "Show.S01E01.the.great.escape.1080p.mkv", "Show").differs,
    ).toBe(false);
  });
});

describe("comparePosition", () => {
  it("compares as numbers, so leading zeroes are not a disagreement", () => {
    // The parse has already turned `S03E07` into numbers; this is the assertion
    // that nothing downstream reintroduces string comparison.
    expect(comparePosition([{ group: 3, slot: 7 }], { group: 3, slot: 7 }).differs).toBe(false);
  });

  it("flags a group disagreement and a slot disagreement separately", () => {
    const c = comparePosition([{ group: 3, slot: 61 }], { group: 4, slot: 1 });
    expect(c).toMatchObject({ groupDiffers: true, slotDiffers: true, differs: true });
    expect(c.claimed).toEqual({ group: 3, slot: 61 });
  });

  it("does not flag a file that legitimately claims two Slots", () => {
    const parsed = [
      { group: 1, slot: 1 },
      { group: 1, slot: 2 },
    ];
    expect(comparePosition(parsed, { group: 1, slot: 2 }).differs).toBe(false);
  });

  it("says nothing about a file whose name claims no position", () => {
    expect(comparePosition([], { group: 1, slot: 1 }).differs).toBe(false);
  });
});

// What a placed File is LABELLED with. The rule being tested is the one the eye
// depends on: whatever survives must start where the Slot's own label starts, so
// the position and the title line up between the two rows without being read.
describe("elideContainerPrefix", () => {
  it("replaces the container's name, keeping the separator that followed it", () => {
    expect(elideContainerPrefix("/m/Show/Show - S03E01 - Holiday Knights.mkv", "Show")).toBe(
      "… - S03E01 - Holiday Knights.mkv",
    );
  });

  it("takes an abbreviated prefix, which is the one a filename usually carries", () => {
    expect(
      elideContainerPrefix("/m/Parks.and.Rec.S06E06.1080p.mkv", "Parks and Recreation"),
    ).toBe("….S06E06.1080p.mkv");
  });

  it("leaves a filename that does not start with the container's name alone", () => {
    expect(elideContainerPrefix("/m/Show/holiday-knights.mkv", "Show")).toBe(
      "holiday-knights.mkv",
    );
  });

  it("leaves a filename that is nothing but the container's name alone", () => {
    // An ellipsis and an extension would name no file at all.
    expect(elideContainerPrefix("/m/Show/Show.mkv", "Show")).toBe("Show.mkv");
  });

  it("says nothing about the path above the filename", () => {
    // The container's name is usually the FOLDER too, and cutting the folder
    // would leave the ellipsis standing for something the Admin never saw.
    expect(elideContainerPrefix("/m/Show/Season 3/Show - S03E01.mkv", "Show")).toBe(
      "… - S03E01.mkv",
    );
  });

  it("survives a container with no title at all", () => {
    expect(elideContainerPrefix("/m/Show - S03E01.mkv", "")).toBe("Show - S03E01.mkv");
  });

  it("cuts so that what is read starts on a word, separator and all going with the ellipsis", () => {
    // The screen dims the first half. A separator left on the second half would
    // put every filename one or three characters out from the Slot code above it.
    expect(splitContainerPrefix("/m/Show - S03E01 - Holiday Knights.mkv", "Show")).toEqual({
      elided: "… - ",
      rest: "S03E01 - Holiday Knights.mkv",
    });
    expect(splitContainerPrefix("/m/holiday-knights.mkv", "Show")).toEqual({
      elided: "",
      rest: "holiday-knights.mkv",
    });
  });
});
