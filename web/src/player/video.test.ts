import { describe, it, expect } from "vitest";
import type { DecisionStream, VideoStream } from "../api/types";
import { initialVideoId, orderedVideoStreams } from "./video";

// Pure helpers for the player's Video menu (selectable-video/03, ADR-0025), the
// video parallel of audio.ts: order the menu (the resolved/default Stream first,
// then by label) and pre-select the Stream the server actually resolved. Unlike
// audio there is no in-band rendition index — a video switch is a full
// re-negotiation — so there is no renditionIndex helper to mirror.

function stream(p: Partial<VideoStream> & { id: string }): VideoStream {
  return {
    id: p.id,
    index: p.index ?? 0,
    codec: p.codec ?? "h264",
    language: p.language,
    width: p.width,
    height: p.height,
    isDefault: p.isDefault ?? false,
    label: p.label ?? p.id,
  };
}

describe("orderedVideoStreams", () => {
  it("puts the default disposition first, then orders by label", () => {
    const streams = [
      stream({ id: "colour", label: "Colour", isDefault: true }),
      stream({ id: "bw", label: "Black & White" }),
      stream({ id: "extra", label: "Zebra cut" }),
    ];
    // The default (Colour) leads; the rest sort by label (Black & White < Zebra).
    expect(orderedVideoStreams(streams).map((s) => s.id)).toEqual(["colour", "bw", "extra"]);
  });

  it("with no default marked, orders purely by label", () => {
    const streams = [
      stream({ id: "uhd", label: "4K" }),
      stream({ id: "hd", label: "1080p" }),
    ];
    expect(orderedVideoStreams(streams).map((s) => s.id)).toEqual(["hd", "uhd"]);
  });

  it("never drops a Stream and does not mutate the input", () => {
    const streams = [stream({ id: "a" }), stream({ id: "b" })];
    const ordered = orderedVideoStreams(streams);
    expect(ordered).toHaveLength(2);
    expect(streams.map((s) => s.id)).toEqual(["a", "b"]);
  });
});

describe("initialVideoId", () => {
  const streams = [
    stream({ id: "colour", index: 0, isDefault: true }),
    stream({ id: "bw", index: 1 }),
  ];

  it("pre-selects the Stream the server RESOLVED, matched by index", () => {
    const resolved: DecisionStream = { index: 1, codec: "h264" };
    expect(initialVideoId(streams, resolved)).toBe("bw");
  });

  it("falls back to the default disposition when the resolved stream is absent", () => {
    expect(initialVideoId(streams, { index: 99, codec: "h264" })).toBe("colour");
    expect(initialVideoId(streams, undefined)).toBe("colour");
  });

  it("falls back to the first Stream when none is marked default", () => {
    const undef = [stream({ id: "a", index: 5 }), stream({ id: "b", index: 6 })];
    expect(initialVideoId(undef, undefined)).toBe("a");
  });

  it("is null for a File with no video Streams", () => {
    expect(initialVideoId([], undefined)).toBeNull();
  });
});
