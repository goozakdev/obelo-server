import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Lyrics, PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { PlaybackTransportProvider } from "./transport";

// The lyrics view following the real player: the NOW PLAYING bar registers its
// element's position with the shared transport, and the lyrics view for the
// playing Track reads it from there. Nothing here fakes the transport or the
// Queue — only the apiClient and the media element's position — so the test
// fails if the bar stops handing its position to the transport.

const { getTitle, getLyrics, startPlayback, reportProgress, endSession } = vi.hoisted(() => ({
  getTitle: vi.fn(),
  getLyrics: vi.fn(),
  startPlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
}));

const { attachHls } = vi.hoisted(() => ({ attachHls: vi.fn() }));
vi.mock("./hls", () => ({
  attachHls: (...a: unknown[]) => attachHls(...a),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      getLyrics: (...a: unknown[]) => getLyrics(...a),
      startPlayback: (...a: unknown[]) => startPlayback(...a),
      reportProgress: (...a: unknown[]) => reportProgress(...a),
      endSession: (...a: unknown[]) => endSession(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";
import LyricsView from "../music/LyricsView";

function trackSummary(id: string, name: string): TitleSummary {
  return {
    id,
    kind: "track",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "e1", name: "FLAC" },
  videoStreams: [],
  audioStream: { index: 0, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 256_000,
};

const synced: Lyrics = {
  kind: "synced",
  source: "local",
  lines: [
    { startMs: 1000, text: "First line" },
    { startMs: 3000, text: "Second line" },
    { startMs: 5000, text: "Third line" },
  ],
  text: "",
};

function activeLine(): string | null {
  const active = screen
    .getAllByTestId("lyric-line")
    .filter((el) => el.getAttribute("aria-current") === "true");
  return active[0]?.textContent ?? null;
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle
    .mockReset()
    .mockResolvedValue({ id: "t1", kind: "track", title: "Song" } as Partial<TitleDetail>);
  getLyrics.mockReset().mockResolvedValue(synced);
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress
    .mockReset()
    .mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("NowPlayingBar — lyrics follow the playing element", () => {
  it("highlights the line at the bar's playback position", async () => {
    const state: QueueState = {
      entries: [entryFromTitle(trackSummary("t1", "Song"))],
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", state);
    renderWithAuth(
      <PlaybackTransportProvider>
        <NowPlayingBar />
        <LyricsView titleId="t1" />
      </PlaybackTransportProvider>,
    );

    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    Object.defineProperty(video, "currentTime", { value: 3.5, writable: true, configurable: true });
    await screen.findAllByTestId("lyric-line");
    await waitFor(() => expect(activeLine()).toBe("Second line"));

    video.currentTime = 5.2;
    await waitFor(() => expect(activeLine()).toBe("Third line"));
  });
});
