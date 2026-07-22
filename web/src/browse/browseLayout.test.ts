import { describe, it, expect, beforeEach } from "vitest";
import {
  DEFAULT_LAYOUT_MODE,
  layoutModeKey,
  loadLayoutMode,
  saveLayoutMode,
} from "./browseLayout";

// The browse-layout store seam (appletv-web-parity §5): load/save are pure,
// storage-injected helpers, so the per-Library round-trip is unit-testable without
// React — the same seam philosophy as playbackPreference. A real localStorage is
// used (jsdom) but cleared between tests.

beforeEach(() => {
  window.localStorage.clear();
});

describe("browse layout store", () => {
  it("defaults an unconfigured Library to the Tile wall", () => {
    expect(loadLayoutMode(window.localStorage, "lib1")).toBe("tile");
    expect(DEFAULT_LAYOUT_MODE).toBe("tile");
  });

  it("round-trips a chosen mode through localStorage, keyed per Library", () => {
    saveLayoutMode(window.localStorage, "lib1", "detail");
    expect(loadLayoutMode(window.localStorage, "lib1")).toBe("detail");
    // The raw entry lives under the per-Library key.
    expect(window.localStorage.getItem(layoutModeKey("lib1"))).toBe("detail");
  });

  it("keeps each Library's mode independent", () => {
    saveLayoutMode(window.localStorage, "lib1", "detail");
    saveLayoutMode(window.localStorage, "lib2", "list");
    expect(loadLayoutMode(window.localStorage, "lib1")).toBe("detail");
    expect(loadLayoutMode(window.localStorage, "lib2")).toBe("list");
    // A third, untouched Library still reads the default.
    expect(loadLayoutMode(window.localStorage, "lib3")).toBe("tile");
  });

  it("degrades a malformed stored value to the default rather than throwing", () => {
    window.localStorage.setItem(layoutModeKey("lib1"), "gallery");
    expect(loadLayoutMode(window.localStorage, "lib1")).toBe("tile");
  });

  it("treats an unknown Library id (empty) as the default and never writes it", () => {
    expect(loadLayoutMode(window.localStorage, "")).toBe("tile");
    saveLayoutMode(window.localStorage, "", "list");
    expect(window.localStorage.length).toBe(0);
  });
});
