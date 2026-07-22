import { describe, it, expect } from "vitest";
import { resolveEditionId, resolvePlayback } from "./playbackResolver";
import type { ResolverEdition } from "./playbackResolver";

// The Playback RESOLVER seam (appletv-web-parity §1): turns a committed preference
// into the `startPlayback` override fields against a Title's Editions. Pure, so the
// Edition axis — including the load-bearing "a Show choice replays across Episodes
// BY NAME" rule — is asserted here without React or a live player.

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

describe("playbackResolver — Edition axis", () => {
  it("Auto (null name) omits editionId", () => {
    expect(resolvePlayback({ editionName: null }, movieEditions)).toEqual({});
    expect(resolveEditionId(null, movieEditions)).toBeUndefined();
  });

  it("a null/undefined preference omits editionId (backward-compatible)", () => {
    expect(resolvePlayback(null, movieEditions)).toEqual({});
    expect(resolvePlayback(undefined, movieEditions)).toEqual({});
  });

  it("an explicit Edition pick resolves the name to this Title's Edition id", () => {
    expect(resolvePlayback({ editionName: "4K" }, movieEditions)).toEqual({ editionId: "ed-4k" });
    expect(resolveEditionId("Director's Cut", movieEditions)).toBe("ed-dc");
  });

  it("a Show preference replays across Episodes BY NAME (different ids)", () => {
    const showPref = { editionName: "Director's Cut" };
    // The same stored name resolves to each Episode's OWN id for that Edition.
    expect(resolvePlayback(showPref, movieEditions)).toEqual({ editionId: "ed-dc" });
    expect(resolvePlayback(showPref, episode2Editions)).toEqual({ editionId: "ep2-dc" });
  });

  it("a name absent from this Title's Editions degrades to Auto", () => {
    // Episode 2 has no "4K" Edition → omit rather than strand the play.
    expect(resolvePlayback({ editionName: "4K" }, episode2Editions)).toEqual({});
  });

  it("an empty name is treated as Auto", () => {
    expect(resolveEditionId("", movieEditions)).toBeUndefined();
  });
});
