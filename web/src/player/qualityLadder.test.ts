import { describe, it, expect } from "vitest";
import {
  QUALITY_LADDER,
  availableRungs,
  editionSourceHeight,
  isQualityCapId,
  rungById,
  rungConstraints,
  sourceHeightForSelection,
} from "./qualityLadder";

// The Quality ladder (appletv-web-parity §1/§3) — the ONE tunable constant table.
// These assert the load-bearing rules: each rung's paired {maxResolution, maxBitrate},
// and that only rungs STRICTLY BELOW the source height are ever offered (the scale
// filter never upscales).

describe("qualityLadder — the constant table", () => {
  it("pairs each rung with its resolution token and bitrate ceiling", () => {
    const asPairs = QUALITY_LADDER.map((r) => [r.id, r.maxResolution, r.maxBitrate]);
    expect(asPairs).toEqual([
      ["4k", "4k", 16_000_000],
      ["1080p", "1080p", 8_000_000],
      ["720p", "720p", 4_000_000],
      ["sd", "sd", 1_500_000],
    ]);
  });

  it("rungConstraints emits BOTH the resolution and the paired bitrate", () => {
    expect(rungConstraints(rungById("1080p")!)).toEqual({
      maxResolution: "1080p",
      maxBitrate: 8_000_000,
    });
  });

  it("isQualityCapId accepts known ids and rejects everything else", () => {
    expect(isQualityCapId("720p")).toBe(true);
    expect(isQualityCapId("8k")).toBe(false);
    expect(isQualityCapId(null)).toBe(false);
    expect(isQualityCapId(1080)).toBe(false);
  });
});

describe("qualityLadder — rungs strictly below source", () => {
  it("offers only downscales for a 4K (2160) source — never the 4K rung itself", () => {
    expect(availableRungs(2160).map((r) => r.id)).toEqual(["1080p", "720p", "sd"]);
  });

  it("offers fewer rungs as the source shrinks", () => {
    expect(availableRungs(1080).map((r) => r.id)).toEqual(["720p", "sd"]);
    expect(availableRungs(720).map((r) => r.id)).toEqual(["sd"]);
    expect(availableRungs(480).map((r) => r.id)).toEqual([]);
  });

  it("offers the 4K rung only for a source taller than 2160 (e.g. 8K)", () => {
    expect(availableRungs(4320).map((r) => r.id)).toEqual(["4k", "1080p", "720p", "sd"]);
  });

  it("offers no rung for an unknown (0) source height", () => {
    expect(availableRungs(0)).toEqual([]);
    expect(availableRungs(-5)).toEqual([]);
  });
});

describe("qualityLadder — source height derivation", () => {
  it("takes the tallest File of an Edition (0 when dims are absent)", () => {
    expect(editionSourceHeight({ files: [{ height: 1080 }, { height: 2160 }] })).toBe(2160);
    expect(editionSourceHeight({ files: [{}] })).toBe(0);
    expect(editionSourceHeight({ files: [] })).toBe(0);
    expect(editionSourceHeight(null)).toBe(0);
  });

  it("uses the named Edition's height when one is selected", () => {
    const editions = [
      { name: "4K", files: [{ height: 2160 }] },
      { name: "1080p", files: [{ height: 1080 }] },
    ];
    expect(sourceHeightForSelection(editions, "1080p")).toBe(1080);
    expect(sourceHeightForSelection(editions, "4K")).toBe(2160);
  });

  it("falls back to the tallest Edition for Auto or an unmatched name", () => {
    const editions = [
      { name: "4K", files: [{ height: 2160 }] },
      { name: "1080p", files: [{ height: 1080 }] },
    ];
    expect(sourceHeightForSelection(editions, null)).toBe(2160);
    expect(sourceHeightForSelection(editions, "Nonexistent")).toBe(2160);
  });
});
