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
    savePreference(window.localStorage, "u1", titleScope, {
      editionName: "Director's Cut",
      qualityCap: null,
      subtitle: null,
      aacStereo: false,
      remuxSelectedOnly: false,
    });
    expect(loadPreference(window.localStorage, "u1", titleScope)).toEqual({
      editionName: "Director's Cut",
      qualityCap: null,
      subtitle: null,
      aacStereo: false,
      remuxSelectedOnly: false,
    });
  });

  it("keys per user, per scope, and per anon bucket (no bleed)", () => {
    savePreference(window.localStorage, "u1", titleScope, { editionName: "4K", qualityCap: null, subtitle: null });
    savePreference(window.localStorage, "u2", titleScope, { editionName: "1080p", qualityCap: null, subtitle: null });
    savePreference(window.localStorage, "u1", showScope, { editionName: "Extended", qualityCap: null, subtitle: null });
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

  it("round-trips a committed Quality cap alongside the Edition", () => {
    savePreference(window.localStorage, "u1", titleScope, { editionName: "4K", qualityCap: "720p", subtitle: null, aacStereo: false, remuxSelectedOnly: false });
    expect(loadPreference(window.localStorage, "u1", titleScope)).toEqual({
      editionName: "4K",
      qualityCap: "720p",
      subtitle: null,
      aacStereo: false,
      remuxSelectedOnly: false,
    });
  });

  it("defaults a missing Quality cap to Direct Play (null)", () => {
    // A pref persisted before the Quality axis existed carries no qualityCap.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ editionName: "4K" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).qualityCap).toBeNull();
  });

  it("coerces an unknown/foreign Quality cap to Direct Play (null)", () => {
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ editionName: null, qualityCap: "8k" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).qualityCap).toBeNull();
  });

  it("round-trips a committed Subtitle track BY LANGUAGE (+ forced), not id", () => {
    savePreference(window.localStorage, "u1", titleScope, {
      editionName: null,
      qualityCap: null,
      subtitle: { language: "en", forced: false },
    });
    expect(loadPreference(window.localStorage, "u1", titleScope).subtitle).toEqual({
      language: "en",
      forced: false,
    });
    // Persisted BY LANGUAGE — no track id is smuggled into the store (that is what
    // lets a Show's choice replay across Episodes carrying different track ids).
    const raw = window.localStorage.getItem(preferenceKey("u1", titleScope));
    expect(raw).toContain('"language":"en"');
    expect(raw).not.toContain("id");
  });

  it("defaults a missing Subtitle to Off (null), and coerces a malformed one to Off", () => {
    // A pref persisted before the Subtitle axis existed carries no subtitle.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ editionName: "4K" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).subtitle).toBeNull();
    // A subtitle object without a string language is not a valid key → Off.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ subtitle: { forced: true } }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).subtitle).toBeNull();
  });

  it("round-trips the committed AAC-stereo toggle (issue 06)", () => {
    savePreference(window.localStorage, "u1", titleScope, {
      editionName: null,
      qualityCap: null,
      subtitle: null,
      aacStereo: true,
    });
    expect(loadPreference(window.localStorage, "u1", titleScope).aacStereo).toBe(true);
  });

  it("defaults a missing AAC toggle to off, and coerces a foreign truthy to off", () => {
    // A pref persisted before the AAC axis existed carries no aacStereo → off.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ editionName: "4K" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).aacStereo).toBe(false);
    // Only a strict boolean true turns it on — a foreign truthy ("yes", 1) is off.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ aacStereo: "yes" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).aacStereo).toBe(false);
  });

  it("round-trips the committed Force Remux checkbox (issue 07)", () => {
    savePreference(window.localStorage, "u1", titleScope, {
      editionName: null,
      qualityCap: null,
      subtitle: null,
      aacStereo: false,
      remuxSelectedOnly: true,
    });
    expect(loadPreference(window.localStorage, "u1", titleScope).remuxSelectedOnly).toBe(true);
  });

  it("defaults a missing Force Remux to off, and coerces a foreign truthy to off", () => {
    // A pref persisted before the Force Remux axis existed carries no flag → off.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ editionName: "4K" }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).remuxSelectedOnly).toBe(false);
    // Only a strict boolean true turns it on — a foreign truthy ("yes", 1) is off.
    window.localStorage.setItem(
      preferenceKey("u1", titleScope),
      JSON.stringify({ remuxSelectedOnly: 1 }),
    );
    expect(loadPreference(window.localStorage, "u1", titleScope).remuxSelectedOnly).toBe(false);
  });

  it("loadPreferenceForTitle derives the scope (Episode → Show)", () => {
    savePreference(window.localStorage, "u1", showScope, {
      editionName: "Director's Cut",
      qualityCap: null,
      subtitle: null,
    });
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
