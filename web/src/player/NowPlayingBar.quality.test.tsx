import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { MediaFile, PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { savePreference } from "./playbackPreference";

// Quality-cap REPLAY through the persistent player (appletv-web-parity §1/§3): a
// committed Quality rung is resolved by the bar (from the store + the entry's detail's
// source height) and reaches `startPlayback` as the paired `constraints`
// (`maxResolution` + `maxBitrate`). Direct Play / no preference keeps the generous
// capability default (no manual cap) — the 100 Mbps hardcode is no longer the ceiling.

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

/** A Movie detail whose Edition is a 4K (2160-line) source — downscale rungs apply. */
function uhdDetail(id: string, name: string): Partial<TitleDetail> {
  return {
    id,
    kind: "movie",
    title: name,
    editions: [{ id: "ed-uhd", name: "4K", files: [file(2160)] }],
  };
}

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed-uhd", name: "4K" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockResolvedValue(uhdDetail("t1", "Dune"));
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

describe("NowPlayingBar — Quality cap replay", () => {
  it("no stored cap → startPlayback keeps the generous default (no manual 4 Mbps cap)", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    const constraints = startPlayback.mock.calls[0][1].constraints;
    expect(constraints.maxBitrate).toBe(100_000_000);
  });

  it("a committed rung reaches startPlayback as the paired maxResolution + maxBitrate", async () => {
    savePreference(window.localStorage, "u1", { kind: "title", id: "t1" }, { editionName: null, qualityCap: "720p" });
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({
          constraints: expect.objectContaining({ maxResolution: "720p", maxBitrate: 4_000_000 }),
        }),
        expect.anything(),
      ),
    );
  });
});
