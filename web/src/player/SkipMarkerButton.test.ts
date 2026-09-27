import { describe, it, expect } from "vitest";
import type { Marker } from "../api/types";
import { activeMarker, skipTargetMs } from "./SkipMarkerButton";

const m = (kind: string, startMs: number, endMs: number): Marker => ({
  kind,
  source: "local",
  startMs,
  endMs,
});

describe("activeMarker", () => {
  const markers = [m("recap", 0, 60_000), m("intro", 50_000, 120_000), m("toString", 200_000, 300_000)];

  it("is inclusive at the start and exclusive at the end", () => {
    expect(activeMarker([m("intro", 10, 20)], 10)?.kind).toBe("intro");
    expect(activeMarker([m("intro", 10, 20)], 20)).toBeNull();
  });

  it("prefers the Marker that started last where two overlap", () => {
    expect(activeMarker(markers, 55_000)?.kind).toBe("intro");
    expect(activeMarker(markers, 40_000)?.kind).toBe("recap");
  });

  it("offers nothing for a kind the player does not recognize", () => {
    expect(activeMarker(markers, 250_000)).toBeNull();
  });
});

describe("skipTargetMs", () => {
  it("stops 500 ms short of the File's end", () => {
    expect(skipTargetMs(1_400_000, 1_400_000)).toBe(1_399_500);
    expect(skipTargetMs(60_000, 1_400_000)).toBe(60_000);
  });

  it("seeks to the Marker's end on a File too short to stop short of", () => {
    expect(skipTargetMs(300, 300)).toBe(300);
    expect(skipTargetMs(500, 500)).toBe(500);
  });
});
