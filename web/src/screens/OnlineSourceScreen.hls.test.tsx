import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import NowPlayingBar from "../player/NowPlayingBar";
import type { OnlineRow } from "../api/types";

// An Online item the server ENCODES (ADR-0068 decision 4, the ffmpeg path) reaches
// the persistent player as an HLS stream-token URL, not a file: the <video> gets no
// src, hls.js (or native HLS) is attached to the playlist, and the attachment is
// torn down with the session. A busy server is said plainly.

const { getOnlineSources, getOnlineRows, startOnlinePlayback, reportProgress, endSession, attachHls, detach } =
  vi.hoisted(() => ({
    getOnlineSources: vi.fn(),
    getOnlineRows: vi.fn(),
    startOnlinePlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    attachHls: vi.fn(),
    detach: vi.fn(),
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
    streamUrl: "/api/v1/stream/tok123/hls/index.m3u8",
    format: "hls",
  });
  reportProgress.mockReset().mockResolvedValue({});
  endSession.mockReset().mockResolvedValue(undefined);
  detach.mockReset();
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach, setTextTrack: vi.fn(), setAudioTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("OnlineSourceScreen: an encoded item", () => {
  it("attaches the HLS playlist to the player instead of setting a src, and detaches on Stop", async () => {
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));

    const video = await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(attachHls).toHaveBeenCalledWith(video, "/api/v1/stream/tok123/hls/index.m3u8", expect.anything()),
    );
    expect(video).not.toHaveAttribute("src");

    fireEvent.click(screen.getByTestId("online-player-stop"));
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("osess-1"));
    await waitFor(() => expect(detach).toHaveBeenCalled());
  });

  it("says plainly when HLS cannot be attached", async () => {
    attachHls.mockRejectedValue(new Error("This browser cannot play HLS"));
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(/cannot play HLS/);
  });

  it("says the server is busy when the transcode cap is full", async () => {
    startOnlinePlayback.mockRejectedValue(
      new ApiError(503, "SERVER_BUSY", "at capacity", { retryable: true, suggestedMaxBitrate: 600000 }),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(/busy/i);
    expect(screen.queryByTestId("player-video")).toBeNull();
  });
});
