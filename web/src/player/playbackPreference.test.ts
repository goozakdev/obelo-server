import { describe, it, expect, beforeEach } from "vitest";
import {
  AUTO_PREFERENCE,
  loadPreference,
  loadPreferenceForTitle,
  preferenceKey,
  preferenceScopeForTitle,
  savePreference,
  type PreferenceScope,
} from "./playbackPreference";

// The Playback preference STORE seam (appletv-web-parity §1/§2): the committed
// pre-play Edition choice persists per user + per Title (Movie) / per Show (TV) in
// localStorage. These pure, storage-injected helpers are the unit seam the sheet's
// commit and the player's replay sit on — same philosophy as usePlaybackPrefs.

const titleScope: PreferenceScope = { kind: "title", id: "t1" };
const showScope: PreferenceScope = { kind: "show", id: "sh1" };

beforeEach(() => {
  window.localStorage.clear();
});

describe("playbackPreference — scope derivation", () => {
  it("keys a Movie on its own Title id", () => {
    expect(preferenceScopeForTitle({ id: "t1", kind: "movie" })).toEqual({
      kind: "title",
      id: "t1",
    });
  });

  it("keys a TV Episode on its Show id (ports across the Show's Episodes)", () => {
    expect(
      preferenceScopeForTitle({ id: "ep1", kind: "episode", episode: { showId: "sh1" } }),
    ).toEqual({ kind: "show", id: "sh1" });
  });

  it("returns null for an Episode missing Show context (skip the store)", () => {
    expect(preferenceScopeForTitle({ id: "ep1", kind: "episode" })).toBeNull();
  });
});

describe("playbackPreference — persistence", () => {
  it("defaults to all-Auto when nothing is stored", () => {
    expect(loadPreference(window.localStorage, "u1", titleScope)).toEqual(AUTO_PREFERENCE);
  });

  it("round-trips a committed Edition name for a user + Title", () => {
    savePreference(window.localStorage, "u1", titleScope, { editionName: "Director's Cut" });
    expect(loadPreference(window.localStorage, "u1", titleScope)).toEqual({
      editionName: "Director's Cut",
    });
  });

  it("keys per user, per scope, and per anon bucket (no bleed)", () => {
    savePreference(window.localStorage, "u1", titleScope, { editionName: "4K" });
    savePreference(window.localStorage, "u2", titleScope, { editionName: "1080p" });
    savePreference(window.localStorage, "u1", showScope, { editionName: "Extended" });
    expect(loadPreference(window.localStorage, "u1", titleScope).editionName).toBe("4K");
    expect(loadPreference(window.localStorage, "u2", titleScope).editionName).toBe("1080p");
    expect(loadPreference(window.localStorage, "u1", showScope).editionName).toBe("Extended");
    // A different Title / anon are separate again.
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t9" }).editionName).toBeNull();
    expect(preferenceKey(null, titleScope)).toBe("juicebox.playback-pref.anon.title.t1");
    expect(preferenceKey("u1", showScope)).toBe("juicebox.playback-pref.u1.show.sh1");
  });

  it("degrades a malformed/partial payload to Auto rather than throwing", () => {
    window.localStorage.setItem(preferenceKey("u1", titleScope), "{not json");
    expect(loadPreference(window.localStorage, "u1", titleScope)).toEqual(AUTO_PREFERENCE);
    window.localStorage.setItem(preferenceKey("u1", titleScope), JSON.stringify({ editionName: 7 }));
    expect(loadPreference(window.localStorage, "u1", titleScope).editionName).toBeNull();
  });

  it("loadPreferenceForTitle derives the scope (Episode → Show)", () => {
    savePreference(window.localStorage, "u1", showScope, { editionName: "Director's Cut" });
    const pref = loadPreferenceForTitle(window.localStorage, "u1", {
      id: "ep1",
      kind: "episode",
      episode: {
        showId: "sh1",
        showTitle: "The Bear",
        seasonId: "s1",
        seasonNumber: 1,
      },
    });
    expect(pref.editionName).toBe("Director's Cut");
  });
});
