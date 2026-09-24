import { describe, it, expect } from "vitest";
import type { Marker } from "../api/types";
import { activeMarker } from "./SkipMarkerButton";

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
