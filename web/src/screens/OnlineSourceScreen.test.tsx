import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import NowPlayingBar from "../player/NowPlayingBar";
import type { OnlineRow } from "../api/types";

// An Online source's page and its hand-off to the persistent player (ADR-0068,
// ADR-0018): the page lists the source's rows and items; selecting an item plays
// it in the SHELL-OWNED bar — the same NowPlayingBar every Title plays in —
// through the Online session, with the stream-token URL on the <video>.

const {
  getOnlineSources,
  getOnlineRows,
  startOnlinePlayback,
  reportProgress,
  endSession,
} = vi.hoisted(() => ({
  getOnlineSources: vi.fn(),
  getOnlineRows: vi.fn(),
  startOnlinePlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
}));

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
        description: "About things",
        publishedAt: "2026-09-01T00:00:00Z",
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
    streamUrl: "/api/v1/stream/tok123/stream",
  });
  reportProgress.mockReset().mockResolvedValue({});
  endSession.mockReset().mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("OnlineSourceScreen", () => {
  it("lists the source's rows and items with their proxied thumbnails", async () => {
    renderPage();

    expect(await screen.findByTestId("online-row-recent")).toHaveTextContent("Recently added");
    expect(screen.getByTestId("online-source-name")).toHaveTextContent("Test Tube");
    expect(screen.getByTestId("online-item-title")).toHaveTextContent("A talk");
    expect(screen.getByText("1:01")).toBeInTheDocument();
    expect(screen.getByText("About things")).toBeInTheDocument();
    const img = document.querySelector<HTMLImageElement>("[data-testid=online-item] img");
    expect(img?.getAttribute("src")).toBe("/api/v1/onlineSources/tube/items/v1/thumbnail");
    expect(getOnlineRows).toHaveBeenCalledWith("tube", expect.anything());
  });

  it("plays the selected item in the persistent player, then ends the session on Stop", async () => {
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));

    const video = await screen.findByTestId("player-video");
    expect(video).toHaveAttribute("src", "/api/v1/stream/tok123/stream");
    expect(screen.getByTestId("now-playing-bar")).toContainElement(video);
    expect(screen.getByTestId("now-playing-title")).toHaveTextContent("A talk");
    expect(screen.getByTestId("now-playing-context")).toHaveTextContent("Test Tube");
    expect(startOnlinePlayback).toHaveBeenCalledWith(
      "tube",
      "v1",
      expect.objectContaining({ deviceProfile: expect.anything(), constraints: expect.anything() }),
      expect.anything(),
    );

    fireEvent.click(screen.getByTestId("online-player-stop"));
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("osess-1"));
    expect(screen.queryByTestId("now-playing-bar")).toBeNull();
  });

  it("is never persisted: a reload does not resurrect a dead session", async () => {
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));
    await screen.findByTestId("player-video");
    expect(window.sessionStorage.getItem("obelo.queue.u1")).toBeNull();
  });

  it("says the item can't be played here when it would need a transcode", async () => {
    startOnlinePlayback.mockRejectedValue(
      new ApiError(501, "TRANSCODE_REQUIRED", "transcode required", { reason: "container" }),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("online-item-play-v1"));

    expect(await screen.findByTestId("online-player-error")).toHaveTextContent(/Test Tube can't be played/);
    expect(screen.queryByTestId("player-video")).toBeNull();
  });

  it("names the source and offers a retry when the page fails", async () => {
    getOnlineRows
      .mockRejectedValueOnce(new ApiError(502, "SOURCE_UNAVAILABLE", "the source is not responding"))
      .mockResolvedValue(rows);
    renderPage();

    const err = await screen.findByTestId("online-source-error");
    expect(err).toHaveTextContent("Test Tube isn’t responding.");
    expect(screen.queryByTestId("online-item")).toBeNull();

    fireEvent.click(screen.getByTestId("online-source-retry"));
    expect(await screen.findByTestId("online-row-recent")).toBeInTheDocument();
    expect(getOnlineRows).toHaveBeenCalledTimes(2);
  });
});
