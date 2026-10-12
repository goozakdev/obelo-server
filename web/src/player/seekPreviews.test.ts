import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";
import type { SeekPreviews } from "../api/types";
import { seekPreviewFrame } from "./seekPreviews";

// The checked-in wire-contract fixture, read from the repo rather than copied.
const manifest: SeekPreviews = JSON.parse(
  // vitest runs from web/, so the repo root is one level up.
  readFileSync(resolve(process.cwd(), "../internal/api/testdata/seekpreviews/manifest.json"), "utf8"),
);

describe("seekPreviewFrame (contract lookup rule)", () => {
  it("t=0 is sheet 0, cell 0", () => {
    expect(seekPreviewFrame(manifest, 0)).toEqual({ sheet: 0, index: 0, x: 0, y: 0 });
  });
  it("t=9.999 s still floors to frame 0", () => {
    expect(seekPreviewFrame(manifest, 9999).index).toBe(0);
  });
  it("t=10 s is frame 1, column 1", () => {
    expect(seekPreviewFrame(manifest, 10000)).toEqual({ sheet: 0, index: 1, x: 320, y: 0 });
  });
  it("frame 80 starts sheet 1", () => {
    expect(seekPreviewFrame(manifest, 800000)).toEqual({ sheet: 1, index: 80, x: 0, y: 0 });
  });
  it("the last frame (t=1,495,000) is sheet 1, column 9, row 6", () => {
    expect(seekPreviewFrame(manifest, 1495000)).toEqual({
      sheet: 1,
      index: 149,
      x: 9 * 320,
      y: 6 * 180,
    });
  });
  it("past the end clamps to the last frame", () => {
    expect(seekPreviewFrame(manifest, 9_999_999).index).toBe(149);
  });
  it("negative clamps to frame 0", () => {
    expect(seekPreviewFrame(manifest, -5).index).toBe(0);
  });
});
