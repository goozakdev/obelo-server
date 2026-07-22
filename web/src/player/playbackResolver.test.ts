import { describe, it, expect } from "vitest";
import {
  resolveBurnSubtitle,
  resolveEditionId,
  resolvePlayback,
  resolveQualityConstraints,
} from "./playbackResolver";
import type { ResolverEdition, ResolverSubtitle } from "./playbackResolver";
import type { PlaybackPreference } from "./playbackPreference";

// The Playback RESOLVER seam (appletv-web-parity §1/§3): turns a committed preference
// into the `startPlayback` override fields against a Title's Editions. Pure, so the
// Edition axis (a Show choice replays across Episodes BY NAME) and the Quality axis
// (a rung → its paired {maxResolution, maxBitrate}, offered only below source) are
// asserted here without React or a live player.

const movieEditions: ResolverEdition[] = [
  { id: "ed-4k", name: "4K" },
  { id: "ed-hd", name: "1080p" },
  { id: "ed-dc", name: "Director's Cut" },
];

// Episode 2 carries the SAME Edition names under DIFFERENT ids than the Movie /
// Episode 1 — this is exactly the drift the by-name keying survives.
const episode2Editions: ResolverEdition[] = [
  { id: "ep2-standard", name: "Theatrical" },
  { id: "ep2-dc", name: "Director's Cut" },
];

/** A full preference — the axes default to Auto / Direct Play / subtitles Off. */
function pref(p: Partial<PlaybackPreference>): PlaybackPreference {
  return { editionName: null, qualityCap: null, subtitle: null, ...p };
}

describe("playbackResolver — Edition axis", () => {
  it("Auto (null name) omits editionId", () => {
    expect(resolvePlayback(pref({ editionName: null }), movieEditions)).toEqual({});
    expect(resolveEditionId(null, movieEditions)).toBeUndefined();
  });

  it("a null/undefined preference omits editionId (backward-compatible)", () => {
    expect(resolvePlayback(null, movieEditions)).toEqual({});
    expect(resolvePlayback(undefined, movieEditions)).toEqual({});
  });

  it("an explicit Edition pick resolves the name to this Title's Edition id", () => {
    expect(resolvePlayback(pref({ editionName: "4K" }), movieEditions)).toEqual({
      editionId: "ed-4k",
    });
    expect(resolveEditionId("Director's Cut", movieEditions)).toBe("ed-dc");
  });

  it("a Show preference replays across Episodes BY NAME (different ids)", () => {
    const showPref = pref({ editionName: "Director's Cut" });
    // The same stored name resolves to each Episode's OWN id for that Edition.
    expect(resolvePlayback(showPref, movieEditions)).toEqual({ editionId: "ed-dc" });
    expect(resolvePlayback(showPref, episode2Editions)).toEqual({ editionId: "ep2-dc" });
  });

  it("a name absent from this Title's Editions degrades to Auto", () => {
    // Episode 2 has no "4K" Edition → omit rather than strand the play.
    expect(resolvePlayback(pref({ editionName: "4K" }), episode2Editions)).toEqual({});
  });

  it("an empty name is treated as Auto", () => {
    expect(resolveEditionId("", movieEditions)).toBeUndefined();
  });
});

// A 4K-source Edition (2160 lines): rungs strictly below it are 1080p / 720p / SD.
const uhdEditions: ResolverEdition[] = [
  { id: "ed-uhd", name: "4K", files: [{ height: 2160 }] },
  { id: "ed-hd", name: "1080p", files: [{ height: 1080 }] },
];

describe("playbackResolver — Quality axis", () => {
  it("Direct Play (null cap) omits the constraints override", () => {
    expect(resolvePlayback(pref({ qualityCap: null }), uhdEditions)).toEqual({});
    expect(resolveQualityConstraints(null, uhdEditions, null)).toBeUndefined();
  });

  it("a rung resolves to its paired {maxResolution, maxBitrate} from the ladder", () => {
    // Against the 4K source (Auto edition → tallest = 2160), each rung below source
    // carries BOTH its resolution token and its paired bitrate ceiling.
    expect(resolveQualityConstraints("1080p", uhdEditions, null)).toEqual({
      maxResolution: "1080p",
      maxBitrate: 8_000_000,
    });
    expect(resolveQualityConstraints("720p", uhdEditions, null)).toEqual({
      maxResolution: "720p",
      maxBitrate: 4_000_000,
    });
    expect(resolveQualityConstraints("sd", uhdEditions, null)).toEqual({
      maxResolution: "sd",
      maxBitrate: 1_500_000,
    });
  });

  it("resolvePlayback carries the rung constraints alongside the editionId", () => {
    expect(resolvePlayback(pref({ editionName: "4K", qualityCap: "720p" }), uhdEditions)).toEqual({
      editionId: "ed-uhd",
      constraints: { maxResolution: "720p", maxBitrate: 4_000_000 },
    });
  });

  it("a rung at/above the selected Edition's source degrades to Direct Play", () => {
    // The 1080p Edition's source is 1080 → a 1080p rung is not a downscale (≥ source),
    // and the 4K rung certainly isn't; both omit the override (scale never upscales).
    expect(resolveQualityConstraints("1080p", uhdEditions, "1080p")).toBeUndefined();
    expect(resolveQualityConstraints("4k", uhdEditions, "1080p")).toBeUndefined();
    // But a 720p rung IS below the 1080p source → it applies.
    expect(resolveQualityConstraints("720p", uhdEditions, "1080p")).toEqual({
      maxResolution: "720p",
      maxBitrate: 4_000_000,
    });
  });

  it("changing the Edition re-derives whether a stored rung still applies", () => {
    const p = pref({ qualityCap: "1080p" });
    // On the 4K source the 1080p rung is a genuine downscale.
    expect(resolvePlayback({ ...p, editionName: "4K" }, uhdEditions).constraints).toEqual({
      maxResolution: "1080p",
      maxBitrate: 8_000_000,
    });
    // Switch the Edition to the 1080p source → the same rung is no longer below
    // source, so it degrades to Direct Play (no override).
    expect(resolvePlayback({ ...p, editionName: "1080p" }, uhdEditions).constraints).toBeUndefined();
  });

  it("an unknown source height (no File dims) offers no rung (Direct Play)", () => {
    // Editions with no dims → source height 0 → every rung degrades to Direct Play.
    expect(resolveQualityConstraints("720p", movieEditions, null)).toBeUndefined();
  });
});

// ── Subtitle axis (issue 05, ADR-0020) ───────────────────────────────────────────
// The resolver's ONLY subtitle output is burnSubtitleId, and it emits it under ONE
// condition: the stored language(+forced) matches an IMAGE track on THIS Title AND
// the resolved tier transcodes/remuxes. Text tracks (selectable WebVTT) and direct-
// play image tracks render locally → no field. Matching is BY LANGUAGE(+forced), not
// id, so a Show choice replays across Episodes whose tracks carry different ids.

// Episode 1's tracks and Episode 2's carry the SAME languages under DIFFERENT ids —
// the drift the by-language keying survives. Each has an English IMAGE (PGS) track.
const ep1Subs: ResolverSubtitle[] = [
  { id: "ep1-en-img", kind: "image", language: "en", forced: false },
  { id: "ep1-en-txt", kind: "text", language: "en", forced: false },
  { id: "ep1-fr-forced", kind: "text", language: "fr", forced: true },
];
const ep2Subs: ResolverSubtitle[] = [
  { id: "ep2-en-img", kind: "image", language: "en", forced: false },
  { id: "ep2-fr-forced", kind: "text", language: "fr", forced: true },
];

describe("playbackResolver — Subtitle axis (burnSubtitleId)", () => {
  it("Off (null subtitle) emits no burnSubtitleId", () => {
    expect(resolveBurnSubtitle(null, ep1Subs, true)).toBeUndefined();
    expect(resolveBurnSubtitle(null, ep1Subs, false)).toBeUndefined();
  });

  it("an IMAGE track on a TRANSCODE tier resolves to burnSubtitleId", () => {
    expect(
      resolveBurnSubtitle({ language: "en", forced: false }, ep1Subs, true),
    ).toBe("ep1-en-img");
  });

  it("an IMAGE track on a DIRECT-PLAY tier is left to local render (omitted)", () => {
    // Same pick, tier does NOT transcode → no burn (the player renders it locally).
    expect(
      resolveBurnSubtitle({ language: "en", forced: false }, ep1Subs, false),
    ).toBeUndefined();
  });

  it("a TEXT track never sets burnSubtitleId, even on a transcode tier", () => {
    // The fr forced track is text — a selectable WebVTT rendition, never a burn.
    expect(
      resolveBurnSubtitle({ language: "fr", forced: true }, ep1Subs, true),
    ).toBeUndefined();
  });

  it("a language absent from THIS Title emits no burnSubtitleId", () => {
    expect(
      resolveBurnSubtitle({ language: "de", forced: false }, ep1Subs, true),
    ).toBeUndefined();
  });

  it("matches by language+forced, not id — a Show choice replays across Episodes", () => {
    const pick: { language: string; forced: boolean } = { language: "en", forced: false };
    // The SAME stored key resolves to EACH Episode's own image-track id.
    expect(resolveBurnSubtitle(pick, ep1Subs, true)).toBe("ep1-en-img");
    expect(resolveBurnSubtitle(pick, ep2Subs, true)).toBe("ep2-en-img");
  });

  it("resolvePlayback emits burnSubtitleId only when a Quality cap makes it transcode", () => {
    // The stored image sub + a downscale rung (constraints present = transcode) → burn.
    expect(
      resolvePlayback(
        pref({ editionName: "4K", qualityCap: "720p", subtitle: { language: "en", forced: false } }),
        uhdEditions,
        ep1Subs,
      ),
    ).toEqual({
      editionId: "ed-uhd",
      constraints: { maxResolution: "720p", maxBitrate: 4_000_000 },
      burnSubtitleId: "ep1-en-img",
    });
    // Same image sub but Direct Play (no cap) → no transcode signal → no burn.
    expect(
      resolvePlayback(
        pref({ subtitle: { language: "en", forced: false } }),
        uhdEditions,
        ep1Subs,
      ),
    ).toEqual({});
  });

  it("defaults subtitles to [] so a caller without Subtitle context is unaffected", () => {
    expect(resolvePlayback(pref({ subtitle: { language: "en", forced: false } }), uhdEditions)).toEqual(
      {},
    );
  });
});
