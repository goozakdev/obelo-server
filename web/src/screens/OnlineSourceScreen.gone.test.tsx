import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import NowPlayingBar from "../player/NowPlayingBar";
import type { OnlineRow } from "../api/types";

// A session the server ended because its media host kept refusing the URL after the
// one re-resolve (ADR-0068 decision 10): the stream just stops, and the player asks the
// keepalive endpoint why. It shows "This video is no longer available from {source}".

const { getOnlineSources, getOnlineRows, startOnlinePlayback, reportProgress, endSession, attachHls } =
  vi.hoisted(() => ({
    getOnlineSources: vi.fn(),
    getOnlineRows: vi.fn(),
    startOnlinePlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    attachHls: vi.fn(),
  }));

vi.mock("../player/hls", () => ({ attachHls: (...a: unknown[]) => attachHls(...a) }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getOnlineSources: (...a: unknown[]) => getOnlineSources(...a),
      getOnlineRows: (...a: unknown[]) => getOnlineRows(...a),
      startOnlinePlayback: (...a: unknown[]) => startOnlinePlayback(...a),
      reportProgress: (...a: unknown[]) => reportProgress(...a),
      endSession: (...a: unknown[]) => endSession(...a),
      subscribeEvents: () => () => {},
    },
  };
});

import OnlineSourceScreen from "./OnlineSourceScreen";

const rows: OnlineRow[] = [
  {
    id: "recent",
    label: "Recently added",
    nextCursor: null,
    items: [
      {
        id: "v1",
        title: "A talk",
        thumbnailUrl: "/api/v1/onlineSources/tube/items/v1/thumbnail",
        durationMs: 61000,
      },
    ],
  },
];

const gone = () =>
  new ApiError(410, "SOURCE_GONE", "This video is no longer available from Test Tube");

function renderPage() {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/online/:sourceId" element={<OnlineSourceScreen />} />
      </Routes>
      <NowPlayingBar />
    </>,
    { initialEntries: ["/online/tube"] },
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  getOnlineSources.mockReset().mockResolvedValue([{ id: "tube", name: "Test Tube", iconUrl: null }]);
  getOnlineRows.mockReset().mockResolvedValue(rows);
  startOnlinePlayback.mockReset().mockResolvedValue({
    sessionId: "osess-1",
    streamUrl: "/api/v1/stream/tok123/progressive",
    format: "progressive",
  });
  reportProgress.mockReset().mockResolvedValue({});
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn(), setAudioTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("OnlineSourceScreen: a session whose media is no longer available", () => {
  it("shows the server's message when the stream fails and the session says it is gone", async () => {
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));
    const video = await screen.findByTestId("player-video");

    reportProgress.mockRejectedValue(gone());
    fireEvent.error(video);

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(
      "This video is no longer available from Test Tube",
    );
    expect(screen.queryByTestId("player-video")).toBeNull();
  });

  it("leaves the player alone when the stream failed but the session is still live", async () => {
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));
    const video = await screen.findByTestId("player-video");

    fireEvent.error(video);

    await waitFor(() => expect(reportProgress).toHaveBeenCalled());
    expect(screen.queryByTestId("online-player-error")).toBeNull();
    expect(screen.getByTestId("player-video")).toBeInTheDocument();
  });

  it("shows the message when HLS playback stops fatally on a session that is gone", async () => {
    startOnlinePlayback.mockResolvedValue({
      sessionId: "osess-1",
      streamUrl: "/api/v1/stream/tok123/hls/index.m3u8",
      format: "hls",
    });
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));
    await waitFor(() => expect(attachHls).toHaveBeenCalled());

    reportProgress.mockRejectedValue(gone());
    const opts = attachHls.mock.calls[0][2] as { onFatal: (reason: string) => void };
    opts.onFatal("manifestLoadError");

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(
      "This video is no longer available from Test Tube",
    );
  });

  it("keeps the generic stop message for a fatal HLS error on a live session", async () => {
    startOnlinePlayback.mockResolvedValue({
      sessionId: "osess-1",
      streamUrl: "/api/v1/stream/tok123/hls/index.m3u8",
      format: "hls",
    });
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));
    await waitFor(() => expect(attachHls).toHaveBeenCalled());

    const opts = attachHls.mock.calls[0][2] as { onFatal: (reason: string) => void };
    opts.onFatal("manifestLoadError");

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(/Playback stopped: manifestLoadError/);
  });
});
