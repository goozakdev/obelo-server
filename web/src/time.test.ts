import { describe, it, expect } from "vitest";
import { formatDate, formatDuration, formatTimecode } from "./time";

describe("time formatting", () => {
  it("formats an RFC3339 timestamp to a local date string", () => {
    // We assert it parses to the right calendar year/components rather than an
    // exact string, since locale/zone vary across runners.
    const out = formatDate("2021-10-22T08:30:00Z");
    expect(out).toMatch(/2021/);
    // The month abbreviation is locale-dependent but should be non-empty.
    expect(out.length).toBeGreaterThan(4);
  });

  it("returns empty string for absent or unparseable timestamps", () => {
    expect(formatDate(undefined)).toBe("");
    expect(formatDate(null)).toBe("");
    expect(formatDate("not-a-date")).toBe("");
  });

  it("formats durations as h/m", () => {
    expect(formatDuration(0)).toBe("");
    expect(formatDuration(47 * 60 * 1000)).toBe("47m");
    expect(formatDuration((118 * 60 + 0) * 1000)).toBe("1h 58m");
  });

  it("formats resume timecodes as m:ss / h:mm:ss", () => {
    expect(formatTimecode(0)).toBe("0:00");
    expect(formatTimecode(95 * 1000)).toBe("1:35");
    expect(formatTimecode((1 * 3600 + 2 * 60 + 5) * 1000)).toBe("1:02:05");
  });
});
