import { describe, it, expect } from "vitest";
import type { AudioStream, DecisionStream } from "../api/types";
import {
  audioRenditionIndex,
  initialAudioId,
  orderedAudioStreams,
  preferredAudioLang,
} from "./audio";

// Pure helpers for the player's Audio menu (audio-streams/04, ADR-0022), the
// audio parallel of subtitles.ts: order the menu by the viewer's preferred audio
// language, pre-select the Stream the server actually resolved, and map a picked
// Stream to the in-band HLS AUDIO rendition index (server order = master-playlist
// order, audio-streams/03).

function stream(p: Partial<AudioStream> & { id: string }): AudioStream {
  return {
    id: p.id,
    index: p.index ?? 1,
    codec: p.codec ?? "aac",
    language: p.language,
    channels: p.channels,
    layout: p.layout,
    isDefault: p.isDefault ?? false,
    commentary: p.commentary,
    label: p.label ?? p.id,
  };
}

describe("preferredAudioLang", () => {
  it("is the ISO-639-1 primary subtag or '' when unknown", () => {
    // In jsdom navigator.language is typically "en-US" → "en". The contract we pin
    // is the shape: a lowercase primary subtag, never a region tag.
    const lang = preferredAudioLang();
    expect(lang).toMatch(/^[a-z]*$/);
    expect(lang).not.toContain("-");
  });
});

describe("orderedAudioStreams", () => {
  it("orders preferred language first, then the default disposition, then by label", () => {
    const streams = [
      stream({ id: "c", language: "en", label: "English Commentary", commentary: true }),
      stream({ id: "d", language: "de", label: "German 5.1", isDefault: true }),
      stream({ id: "j", language: "ja", label: "Japanese 5.1" }),
      stream({ id: "e", language: "en", label: "English 5.1" }),
    ];
    const ordered = orderedAudioStreams(streams, "en");
    // The two English streams float to the top (preferred), then by label; the
    // default (German) leads the rest, then the remaining by label.
    expect(ordered.map((s) => s.id)).toEqual(["e", "c", "d", "j"]);
  });

  it("with no preferred language, the default disposition leads, then by label", () => {
    const streams = [
      stream({ id: "j", language: "ja", label: "Japanese 5.1" }),
      stream({ id: "e", language: "en", label: "English 5.1", isDefault: true }),
      stream({ id: "f", language: "fr", label: "French 5.1" }),
    ];
    expect(orderedAudioStreams(streams, "").map((s) => s.id)).toEqual(["e", "f", "j"]);
  });

  it("never drops a Stream (audio is never 'off') and does not mutate the input", () => {
    const streams = [stream({ id: "a" }), stream({ id: "b" })];
    const ordered = orderedAudioStreams(streams, "en");
    expect(ordered).toHaveLength(2);
    expect(streams.map((s) => s.id)).toEqual(["a", "b"]);
  });
});

describe("initialAudioId", () => {
  const streams = [
    stream({ id: "e", index: 1, language: "en", isDefault: true }),
    stream({ id: "j", index: 2, language: "ja" }),
  ];

  it("pre-selects the Stream the server RESOLVED, matched by index", () => {
    const resolved: DecisionStream = { index: 2, codec: "aac" };
    expect(initialAudioId(streams, resolved)).toBe("j");
  });

  it("falls back to the default disposition when the resolved stream is absent", () => {
    expect(initialAudioId(streams, { index: 99, codec: "aac" })).toBe("e");
    expect(initialAudioId(streams, undefined)).toBe("e");
  });

  it("falls back to the first Stream when none is marked default", () => {
    const undef = [stream({ id: "a", index: 5 }), stream({ id: "b", index: 6 })];
    expect(initialAudioId(undef, undefined)).toBe("a");
  });

  it("is null for a silent File (no audio Streams)", () => {
    expect(initialAudioId([], undefined)).toBeNull();
  });
});

describe("audioRenditionIndex", () => {
  // The renditions are advertised in the master playlist in the File's audio-Stream
  // order (audio-streams/03), the SAME order the decision lists audioStreams — so
  // the index is a straight position lookup, not a lookup in the re-sorted menu.
  const streams = [stream({ id: "e", index: 1 }), stream({ id: "j", index: 2 }), stream({ id: "c", index: 3 })];

  it("is the position of the id among the audio Streams in server order", () => {
    expect(audioRenditionIndex(streams, "e")).toBe(0);
    expect(audioRenditionIndex(streams, "j")).toBe(1);
    expect(audioRenditionIndex(streams, "c")).toBe(2);
  });

  it("is null for an unknown id or a null selection", () => {
    expect(audioRenditionIndex(streams, "nope")).toBeNull();
    expect(audioRenditionIndex(streams, null)).toBeNull();
  });
});
