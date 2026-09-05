import { describe, it, expect } from "vitest";
import { formatAgo, formatDate, formatDuration, formatTimecode } from "./time";

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

  // formatAgo drives the Users list's "Linked, seen 2h ago" badge, so `now` is
  // injected rather than read from the clock.
  describe("formatAgo", () => {
    const now = Date.parse("2026-09-02T12:00:00Z");
    const ago = (ms: number) => formatAgo(new Date(now - ms).toISOString(), now);

    it("steps from seconds through days", () => {
      expect(ago(5 * 1000)).toBe("just now");
      expect(ago(59 * 1000)).toBe("just now");
      expect(ago(60 * 1000)).toBe("1m ago");
      expect(ago(12 * 60 * 1000)).toBe("12m ago");
      expect(ago(59 * 60 * 1000)).toBe("59m ago");
      expect(ago(2 * 60 * 60 * 1000)).toBe("2h ago");
      expect(ago(23 * 60 * 60 * 1000)).toBe("23h ago");
      expect(ago(24 * 60 * 60 * 1000)).toBe("1d ago");
      expect(ago(9 * 24 * 60 * 60 * 1000)).toBe("9d ago");
    });

    it("reads a future timestamp as 'just now' rather than a negative age", () => {
      // Two households, two clocks: a Device last-seen written a few seconds
      // ahead of this browser must not read as "-1m ago".
      expect(formatAgo(new Date(now + 30 * 1000).toISOString(), now)).toBe(
        "just now",
      );
    });

    it("returns empty string for absent or unparseable timestamps", () => {
      expect(formatAgo(undefined, now)).toBe("");
      expect(formatAgo(null, now)).toBe("");
      expect(formatAgo("", now)).toBe("");
      expect(formatAgo("never", now)).toBe("");
    });
  });
});
