import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { MediaFile, PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { savePreference, type PlaybackPreference } from "./playbackPreference";

// Force Remux REPLAY through the persistent player (appletv-web-parity §10, issue
// 07): a committed `remuxSelectedOnly: true` reaches `startPlayback` — but ONLY when
// the resolved draft is otherwise pure direct play AND the server advertises
// `features.remuxSelectedOnly`. A Quality-cap downscale or the AAC narrowing drops
// the field (the session already leaves direct play), and an absent flag keeps a
// stored choice off the wire entirely (an older server rejects the unknown field).

const { getTitle, startPlayback, reportProgress, endSession } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  startPlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
}));

const { attachHls } = vi.hoisted(() => ({ attachHls: vi.fn() }));
vi.mock("./hls", () => ({ attachHls: (...a: unknown[]) => attachHls(...a) }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      startPlayback: (...a: unknown[]) => startPlayback(...a),
      reportProgress: (...a: unknown[]) => reportProgress(...a),
      endSession: (...a: unknown[]) => endSession(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";

function movieSummary(id: string, name: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

function file(height: number): MediaFile {
  return {
    id: `f-${height}`,
    path: "",
    container: "mp4",
    width: Math.round((height * 16) / 9),
    height,
    bitrate: 0,
    durationMs: 0,
    sizeBytes: 0,
    missing: false,
    streams: [],
    audioStreams: [],
    videoStreams: [],
  };
}

function movieDetail(id: string, name: string): Partial<TitleDetail> {
  return {
    id,
    kind: "movie",
    title: name,
    editions: [{ id: "ed1", name: "Default", files: [file(1080)] }],
  };
}

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directStream",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed1", name: "Default" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

/** A stored preference with every axis Auto/off except the overrides given. */
function pref(p: Partial<PlaybackPreference>): PlaybackPreference {
  return {
    editionName: null,
    qualityCap: null,
    subtitle: null,
    aacStereo: false,
    remuxSelectedOnly: false,
    ...p,
  };
}

function seedAndRender(entries: QueueEntry[], features?: Record<string, boolean>) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"], features });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockResolvedValue(movieDetail("t1", "Dune"));
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("NowPlayingBar — Force Remux replay", () => {
  it("no stored choice → startPlayback carries no remuxSelectedOnly", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await waitFor(() => expect(startPlayback).toHaveBeenCalled());
    expect(startPlayback.mock.calls[0][1].remuxSelectedOnly).toBeUndefined();
  });

  it("a committed checkbox on an otherwise-direct-play preference reaches startPlayback as true", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      pref({ remuxSelectedOnly: true }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({ remuxSelectedOnly: true }),
        expect.anything(),
      ),
    );
  });

  it("a Quality-cap downscale drops the field — it never rides a transcoding request", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      pref({ qualityCap: "720p", remuxSelectedOnly: true }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await waitFor(() => expect(startPlayback).toHaveBeenCalled());
    const opts = startPlayback.mock.calls[0][1];
    // The rung's constraints rode (720p is a genuine downscale of the 1080 source)…
    expect(opts.constraints.maxResolution).toBe("720p");
    // …and Force Remux was dropped: the session already leaves direct play.
    expect(opts.remuxSelectedOnly).toBeUndefined();
  });

  it("AAC-stereo on drops the field too (the audio narrowing leaves direct play)", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      pref({ aacStereo: true, remuxSelectedOnly: true }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await waitFor(() => expect(startPlayback).toHaveBeenCalled());
    const opts = startPlayback.mock.calls[0][1];
    expect(opts.deviceProfile.audioCodecs).toEqual(["aac"]);
    expect(opts.remuxSelectedOnly).toBeUndefined();
  });

  it("an absent feature flag keeps even a stored choice off the wire (older server)", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      pref({ remuxSelectedOnly: true }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))], { remuxSelectedOnly: false });
    await waitFor(() => expect(startPlayback).toHaveBeenCalled());
    expect(startPlayback.mock.calls[0][1].remuxSelectedOnly).toBeUndefined();
  });
});
