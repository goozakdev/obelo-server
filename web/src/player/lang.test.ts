import { describe, it, expect } from "vitest";
import { kindLabel, langRank, preferredLang } from "./lang";
import { orderedAudioStreams, preferredAudioLang } from "./audio";
import { orderedImageTracks, orderedTextTracks } from "./subtitles";

describe("lang helpers (R04-19)", () => {
  it("preferredLang is the browser's primary subtag, and the audio alias is the same function", () => {
    expect(preferredLang()).toBe((navigator.language || "").split(/[-_]/)[0].toLowerCase());
    expect(preferredAudioLang).toBe(preferredLang);
  });

  it("langRank puts the preferred language first, case-insensitively, and nothing when unset", () => {
    expect(langRank("EN", "en")).toBe(0);
    expect(langRank("fr", "en")).toBe(1);
    expect(langRank(undefined, "en")).toBe(1);
    expect(langRank("en", "")).toBe(1);
    expect(langRank(undefined, "")).toBe(1);
  });

  it("kindLabel names the media kinds and passes unknown kinds through", () => {
    expect(["movie", "episode", "track", "show", "other"].map(kindLabel)).toEqual([
      "Movie",
      "Episode",
      "Track",
      "Show",
      "other",
    ]);
  });

  it("all three orderings share the preferred-language-first rule", () => {
    const sub = (id: string, language: string, kind: "text" | "image") =>
      ({ id, language, kind, forced: false, label: id, url: kind === "text" ? "/x" : undefined, source: "embedded" }) as never;
    expect(orderedTextTracks([sub("a", "fr", "text"), sub("b", "en", "text")], "en").map((t) => t.id)).toEqual(["b", "a"]);
    expect(orderedImageTracks([sub("a", "fr", "image"), sub("b", "en", "image")], "en").map((t) => t.id)).toEqual(["b", "a"]);
    const aud = (id: string, language: string) => ({ id, language, isDefault: false, label: id }) as never;
    expect(orderedAudioStreams([aud("a", "fr"), aud("b", "en")], "en").map((s) => s.id)).toEqual(["b", "a"]);
  });
});
