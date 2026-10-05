import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Marker, PlaybackDecision, TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue } from "./queue/useQueue";

// The Skip button (ADR-0065): on the stage, while the playback position is inside
// a stored Marker of a kind the player recognizes, a "Skip …" button is shown;
// pressing it seeks to the Marker's end, or half a second short of the File's own
// end when the Marker runs to it. Outside every Marker there is none.

const { getTitle, startPlayback, reportProgress, endSession, getSessionMarkers } = vi.hoisted(
  () => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    getSessionMarkers: vi.fn(),
  }),
);

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
      getSessionMarkers: (...a: unknown[]) => getSessionMarkers(...a),
    },
  };
});

import NowPlayingBar from "./NowPlayingBar";

const episode: TitleSummary = {
  id: "t1",
  kind: "episode",
  title: "The Fire",
  year: 0,
  needsReview: false,
  ambiguous: false,
  resumePositionMs: 0,
  watched: false,
  genres: [],
};

const decision: PlaybackDecision = {
  sessionId: "sess-1",
  tier: "directPlay",
  streamUrl: "/api/v1/sessions/sess-1/stream",
  edition: { id: "e1", name: "1080p" },
  videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
  videoStreams: [],
  audioStream: { index: 1, codec: "aac", channels: 2 },
  audioStreams: [],
  subtitles: [],
  estimatedBitrate: 6_000_000,
};

const markers: Marker[] = [
  { kind: "intro", source: "local", startMs: 10_000, endMs: 70_000 },
  { kind: "commercial", source: "local", startMs: 200_000, endMs: 260_000 },
  { kind: "credits", source: "local", startMs: 1_300_000, endMs: 1_400_000 },
];

function Harness() {
  const queue = useQueue();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow([entryFromTitle(episode)])}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
}

async function playToStage() {
  renderWithAuth(<Harness />, { initialEntries: ["/"] });
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  await screen.findByTestId("now-playing-collapse"); // on the stage
  Object.defineProperty(video, "duration", { value: 1_400, configurable: true });
  act(() => {
    fireEvent.durationChange(video);
  });
  return video;
}

function setPosition(video: HTMLVideoElement, seconds: number) {
  Object.defineProperty(video, "currentTime", { value: seconds, writable: true, configurable: true });
  act(() => {
    fireEvent.timeUpdate(video);
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "episode", title: "The Fire" });
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  getSessionMarkers.mockReset().mockResolvedValue(markers);
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

describe("NowPlayingBar — Skip button", () => {
  it("shows Skip inside a recognized Marker and none outside", async () => {
    const video = await playToStage();
    await waitFor(() => expect(getSessionMarkers).toHaveBeenCalledWith("sess-1", expect.anything()));

    setPosition(video, 5);
    expect(screen.queryByTestId("skip-marker")).toBeNull();

    setPosition(video, 30);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Intro");

    setPosition(video, 100);
    expect(screen.queryByTestId("skip-marker")).toBeNull();

    // A kind the player does not recognize offers nothing.
    setPosition(video, 230);
    expect(screen.queryByTestId("skip-marker")).toBeNull();

    setPosition(video, 1_350);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Credits");
  });

  it("pressing Skip seeks to the Marker's end", async () => {
    const video = await playToStage();
    await waitFor(() => expect(getSessionMarkers).toHaveBeenCalled());
    setPosition(video, 30);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    expect(video.currentTime).toBe(70);
    expect(screen.queryByTestId("skip-marker")).toBeNull();
    // The progress report rides the element's `seeked` event (R04-09), not the click.
    fireEvent.seeked(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenLastCalledWith(
        "sess-1",
        expect.objectContaining({ positionMs: 70_000 }),
      ),
    );
  });

  it("pressing Skip on Credits that run to the end stops 500 ms short of it", async () => {
    const video = await playToStage();
    await waitFor(() => expect(getSessionMarkers).toHaveBeenCalled());
    setPosition(video, 1_350);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    expect(video.currentTime).toBe(1_399.5);
    fireEvent.seeked(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenLastCalledWith(
        "sess-1",
        expect.objectContaining({ positionMs: 1_399_500 }),
      ),
    );
  });

  it("shows nothing when the File has no Markers", async () => {
    getSessionMarkers.mockResolvedValue([]);
    const video = await playToStage();
    await waitFor(() => expect(getSessionMarkers).toHaveBeenCalled());
    setPosition(video, 30);
    expect(screen.queryByTestId("skip-marker")).toBeNull();
  });
});
