import { describe, it, expect, beforeEach } from "vitest";
import {
  DEFAULT_PREFS,
  loadPlaybackPrefs,
  playbackPrefsKey,
  savePlaybackPrefs,
} from "./usePlaybackPrefs";

// The playback-prefs STORE seam (now-playing-bar/02): volume + mute persist
// per-user in localStorage, distinct from the session-scoped Queue. These pure,
// storage-injected helpers are the unit seam the bar's volume/mute controls sit
// on — the same testing philosophy as queue/persist.ts.

beforeEach(() => {
  window.localStorage.clear();
});

describe("usePlaybackPrefs — persistence (store seam)", () => {
  it("defaults to full volume, un-muted when nothing is stored", () => {
    expect(loadPlaybackPrefs(window.localStorage, "u1")).toEqual(DEFAULT_PREFS);
  });

  it("round-trips volume + muted for a user", () => {
    savePlaybackPrefs(window.localStorage, "u1", { volume: 0.4, muted: true });
    expect(loadPlaybackPrefs(window.localStorage, "u1")).toEqual({ volume: 0.4, muted: true });
  });

  it("keys prefs per user (no cross-user bleed)", () => {
    savePlaybackPrefs(window.localStorage, "u1", { volume: 0.2, muted: false });
    savePlaybackPrefs(window.localStorage, "u2", { volume: 0.9, muted: true });
    expect(loadPlaybackPrefs(window.localStorage, "u1")).toEqual({ volume: 0.2, muted: false });
    expect(loadPlaybackPrefs(window.localStorage, "u2")).toEqual({ volume: 0.9, muted: true });
    // A logged-out/anon bucket is separate again.
    expect(loadPlaybackPrefs(window.localStorage, null).volume).toBe(DEFAULT_PREFS.volume);
    expect(playbackPrefsKey(null)).toBe("juicebox.playback-prefs.anon");
  });

  it("clamps an out-of-range stored volume into [0, 1]", () => {
    window.localStorage.setItem(playbackPrefsKey("u1"), JSON.stringify({ volume: 5, muted: false }));
    expect(loadPlaybackPrefs(window.localStorage, "u1").volume).toBe(1);
    window.localStorage.setItem(playbackPrefsKey("u1"), JSON.stringify({ volume: -3, muted: false }));
    expect(loadPlaybackPrefs(window.localStorage, "u1").volume).toBe(0);
  });

  it("degrades a malformed/partial payload to the defaults", () => {
    window.localStorage.setItem(playbackPrefsKey("u1"), "{not json");
    expect(loadPlaybackPrefs(window.localStorage, "u1")).toEqual(DEFAULT_PREFS);
    // A partial payload keeps the present field and defaults the absent one.
    window.localStorage.setItem(playbackPrefsKey("u1"), JSON.stringify({ muted: true }));
    expect(loadPlaybackPrefs(window.localStorage, "u1")).toEqual({
      volume: DEFAULT_PREFS.volume,
      muted: true,
    });
  });
});
