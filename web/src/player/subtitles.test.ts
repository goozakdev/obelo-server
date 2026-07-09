import { describe, expect, it } from "vitest";
import type { SubtitleTrack } from "../api/types";
import {
  applySubtitleSelection,
  defaultTrackId,
  deliverableTextTrackIndex,
  deliverableTextTracks,
  orderedImageTracks,
  orderedTextTracks,
} from "./subtitles";

function track(p: Partial<SubtitleTrack>): SubtitleTrack {
  return {
    id: p.id ?? "x",
    source: p.source ?? "embedded",
    kind: p.kind ?? "text",
    language: p.language,
    forced: p.forced ?? false,
    label: p.label ?? "Track",
    url: "url" in p ? p.url : "/vtt",
  };
}

describe("orderedTextTracks", () => {
  it("keeps only deliverable text tracks (drops image + url-less)", () => {
    const tracks = [
      track({ id: "t", kind: "text", label: "English" }),
      track({ id: "img", kind: "image", label: "German", url: undefined }),
      track({ id: "noUrl", kind: "text", label: "French", url: undefined }),
    ];
    const out = orderedTextTracks(tracks, "en");
    expect(out.map((t) => t.id)).toEqual(["t"]);
  });

  it("sorts the preferred language first, then forced, then by label", () => {
    const tracks = [
      track({ id: "de", language: "de", label: "German" }),
      track({ id: "en", language: "en", label: "English" }),
      track({ id: "en-forced", language: "en", forced: true, label: "English (Forced)" }),
      track({ id: "fr", language: "fr", label: "French" }),
    ];
    const out = orderedTextTracks(tracks, "fr");
    // French (preferred) leads; among the rest, the forced English precedes plain.
    expect(out[0].id).toBe("fr");
    expect(out.indexOf(out.find((t) => t.id === "en-forced")!)).toBeLessThan(
      out.indexOf(out.find((t) => t.id === "en")!),
    );
  });
});

describe("orderedImageTracks", () => {
  it("keeps only image tracks, preferred language first", () => {
    const tracks = [
      track({ id: "t", kind: "text", language: "en", label: "English" }),
      track({ id: "de", kind: "image", language: "de", label: "German", url: undefined }),
      track({ id: "it", kind: "image", language: "it", label: "Italian", url: undefined }),
    ];
    const out = orderedImageTracks(tracks, "it");
    expect(out.map((t) => t.id)).toEqual(["it", "de"]);
  });

  it("does not prioritize forced image tracks (forced image is never auto-burned)", () => {
    const tracks = [
      track({ id: "a", kind: "image", language: "de", forced: false, label: "German" }),
      track({ id: "b", kind: "image", language: "de", forced: true, label: "German (Forced)" }),
    ];
    // No preferred match → alphabetical by label; forcedness is irrelevant.
    const out = orderedImageTracks(tracks, "");
    expect(out.map((t) => t.label)).toEqual(["German", "German (Forced)"]);
  });
});

describe("applySubtitleSelection", () => {
  // A minimal fake <video> whose <track data-sub-id> elements each expose a native
  // TextTrack via `.track` — mirrors the browser, so we can assert modes without
  // jsdom's partial TextTrack support.
  function fakeVideo(ids: string[]) {
    const els = ids.map((id) => ({
      track: { mode: "disabled" as TextTrackMode },
      getAttribute: (n: string) => (n === "data-sub-id" ? id : null),
    }));
    return {
      querySelectorAll: () => els,
      _els: els,
    } as unknown as HTMLVideoElement & { _els: typeof els };
  }

  it("keys on data-sub-id so two same-language tracks never collide", () => {
    // The regression: an embedded English sub AND an English sidecar both label
    // "English"/lang "en" — matching by (label, language) would collapse them.
    const v = fakeVideo(["embedded-en", "sidecar-en"]);
    applySubtitleSelection(v, "sidecar-en");
    expect(v._els[0].track.mode).toBe("disabled"); // the embedded one stays off
    expect(v._els[1].track.mode).toBe("showing"); // exactly the picked sidecar shows
  });

  it("null selection disables every track (captions off)", () => {
    const v = fakeVideo(["en", "fr"]);
    applySubtitleSelection(v, "en");
    expect(v._els[0].track.mode).toBe("showing");
    applySubtitleSelection(v, null);
    expect(v._els.every((e) => e.track.mode === "disabled")).toBe(true);
  });
});

describe("defaultTrackId", () => {
  it("returns a forced track's id (auto-display)", () => {
    const ordered = [
      track({ id: "en", language: "en", label: "English" }),
      track({ id: "fr-forced", language: "fr", forced: true, label: "French (Forced)" }),
    ];
    expect(defaultTrackId(ordered)).toBe("fr-forced");
  });

  it("returns null when nothing is forced (subtitles default off)", () => {
    const ordered = [track({ id: "en", language: "en", label: "English" })];
    expect(defaultTrackId(ordered)).toBeNull();
  });
});

describe("deliverableTextTracks / deliverableTextTrackIndex (in-band HLS mapping)", () => {
  // Server order (unsorted) mirrors the master playlist's rendition order — the
  // index the hls.js/native subtitle track is selected by.
  const tracks = [
    track({ id: "emb-en", language: "en", label: "English" }),
    track({ id: "img-de", kind: "image", label: "German", url: undefined }),
    track({ id: "side-es", language: "es", forced: true, label: "Spanish (Forced)" }),
    track({ id: "noUrl", kind: "text", label: "French", url: undefined }),
  ];

  it("keeps only deliverable text tracks, in server order", () => {
    expect(deliverableTextTracks(tracks).map((t) => t.id)).toEqual(["emb-en", "side-es"]);
  });

  it("maps a selected id to its rendition index", () => {
    expect(deliverableTextTrackIndex(tracks, "emb-en")).toBe(0);
    expect(deliverableTextTrackIndex(tracks, "side-es")).toBe(1);
  });

  it("returns null for off, an image track, or an unknown id", () => {
    expect(deliverableTextTrackIndex(tracks, null)).toBeNull();
    expect(deliverableTextTrackIndex(tracks, "img-de")).toBeNull();
    expect(deliverableTextTrackIndex(tracks, "nope")).toBeNull();
  });
});
