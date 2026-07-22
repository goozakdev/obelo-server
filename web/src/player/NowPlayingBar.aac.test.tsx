import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { MediaFile, PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { savePreference } from "./playbackPreference";

// AAC-stereo REPLAY through the persistent player (appletv-web-parity §7, issue 06):
// the toggle has NO contract field — a committed `aacStereo: true` must reach
// `startPlayback` as a NARROWED `deviceProfile` (`audioCodecs: ["aac"],
// maxAudioChannels: 2`), while off / no preference sends the browser-probed
// capability profile unchanged. The flag is detail-free, so an AAC-only preference
// negotiates without waiting on the Title detail.

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
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "ed1", name: "Default" },
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
  getTitle.mockReset().mockResolvedValue(movieDetail("t1", "Dune"));
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  // The probe reports mp4/h264 + several audio codecs, so the DEFAULT profile's
  // audioCodecs is more than just aac — the narrowing is observable against it.
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("NowPlayingBar — AAC-stereo replay", () => {
  it("no stored toggle → startPlayback sends the full probed capability profile", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    const profile = startPlayback.mock.calls[0][1].deviceProfile;
    // The probed profile claims more than aac (mp3/flac probe as mp4-family here) —
    // proof the off-state is NOT narrowed.
    expect(profile.audioCodecs).toContain("aac");
    expect(profile.audioCodecs.length).toBeGreaterThan(1);
  });

  it("a committed toggle reaches startPlayback as the aac/2ch profile narrowing", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      { editionName: null, qualityCap: null, subtitle: null, aacStereo: true },
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith(
        "t1",
        expect.objectContaining({
          deviceProfile: expect.objectContaining({
            audioCodecs: ["aac"],
            maxAudioChannels: 2,
          }),
        }),
        expect.anything(),
      ),
    );
    // Only the audio side is narrowed — the video/container capabilities still ride.
    const profile = startPlayback.mock.calls[0][1].deviceProfile;
    expect(profile.containers).toContain("mp4");
    expect(profile.videoCodecs.map((v: { codec: string }) => v.codec)).toContain("h264");
  });

  it("an AAC-only preference negotiates without waiting on the Title detail", async () => {
    savePreference(
      window.localStorage,
      "u1",
      { kind: "title", id: "t1" },
      { editionName: null, qualityCap: null, subtitle: null, aacStereo: true },
    );
    // The detail never resolves — the flag needs no name→id / source-height context,
    // so negotiation must not be held `pending` on it.
    getTitle.mockReset().mockReturnValue(new Promise(() => {}));
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await waitFor(() => expect(startPlayback).toHaveBeenCalled());
    const profile = startPlayback.mock.calls[0][1].deviceProfile;
    expect(profile.audioCodecs).toEqual(["aac"]);
    expect(profile.maxAudioChannels).toBe(2);
  });
});
