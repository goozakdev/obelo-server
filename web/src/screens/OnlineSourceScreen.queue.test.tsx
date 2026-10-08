import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { Link, Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import NowPlayingBar from "../player/NowPlayingBar";
import { useQueue, type QueueStore } from "../player/queue/useQueue";
import { entryFromOnlineItem, entryFromTitle } from "../player/queue/model";
import type { OnlineRow, TitleSummary } from "../api/types";

// An Online item in the persistent player is a single-item Queue (ADR-0068,
// ADR-0018): playing one replaces the Queue, Titles and Online items never share
// one, nothing follows it when it ends, and it carries no Skip / subtitle / lyrics
// surface (and never asks the server for Markers).

const {
  getOnlineSources,
  getOnlineRows,
  startOnlinePlayback,
  reportProgress,
  endSession,
  getSessionMarkers,
} = vi.hoisted(() => ({
  getOnlineSources: vi.fn(),
  getOnlineRows: vi.fn(),
  startOnlinePlayback: vi.fn(),
  reportProgress: vi.fn(),
  endSession: vi.fn(),
  getSessionMarkers: vi.fn(),
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
      getSessionMarkers: (...a: unknown[]) => getSessionMarkers(...a),
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
      { id: "v1", title: "A talk", thumbnailUrl: "/t1.jpg", durationMs: 61000 },
      { id: "v2", title: "Another talk", thumbnailUrl: "/t2.jpg", durationMs: 30000 },
    ],
  },
];

function title(id: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

const online = () =>
  entryFromOnlineItem({
    sourceId: "tube",
    sourceName: "Test Tube",
    itemId: "v1",
    title: "A talk",
    thumbnailUrl: "/t1.jpg",
    durationMs: 61000,
  });

let store: QueueStore;
function Probe() {
  store = useQueue();
  return null;
}

function renderPage() {
  return renderWithAuth(
    <>
      <Link to="/elsewhere">go elsewhere</Link>
      <Routes>
        <Route path="/online/:sourceId" element={<OnlineSourceScreen />} />
        <Route path="/elsewhere" element={<p>elsewhere page</p>} />
      </Routes>
      <NowPlayingBar />
      <Probe />
    </>,
    { initialEntries: ["/online/tube"] },
  );
}

async function playFirstItem() {
  fireEvent.click(await screen.findByTestId("online-item-play-v1"));
  return screen.findByTestId("player-video");
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
  getSessionMarkers.mockReset().mockResolvedValue([]);
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("Online item as a single-item Queue", () => {
  it("replaces queued Titles: exactly one Queue entry, the item", async () => {
    renderPage();
    await screen.findByTestId("online-item-play-v1");
    act(() => {
      store.playNow([entryFromTitle(title("a")), entryFromTitle(title("b"))], 1);
    });
    expect(store.length).toBe(2);

    await playFirstItem();

    expect(store.length).toBe(1);
    expect(store.current?.online?.itemId).toBe("v1");
  });

  it("refuses a Title while an Online item is queued, with an explanation", async () => {
    renderPage();
    await playFirstItem();

    let refusal: string | null = null;
    act(() => {
      refusal = store.enqueue([entryFromTitle(title("a"))]);
    });
    expect(refusal).toMatch(/online item/i);
    act(() => {
      refusal = store.playNext([entryFromTitle(title("a"))]);
    });
    expect(refusal).toMatch(/online item/i);
    expect(store.length).toBe(1);
    expect(store.current?.online).toBeTruthy();
  });

  it("refuses an Online item while Titles are queued, with an explanation", async () => {
    renderPage();
    await screen.findByTestId("online-item-play-v1");
    act(() => {
      store.playNow([entryFromTitle(title("a"))]);
    });

    let refusal: string | null = null;
    act(() => {
      refusal = store.enqueue([online()]);
    });
    expect(refusal).toMatch(/online items can't be added/i);
    act(() => {
      refusal = store.playNext([online()]);
    });
    expect(refusal).toMatch(/online items can't be added/i);
    expect(store.length).toBe(1);
    expect(store.current?.online).toBeUndefined();
  });

  it("stops when the item ends: nothing else starts", async () => {
    renderPage();
    const video = await playFirstItem();

    fireEvent.ended(video);

    expect(startOnlinePlayback).toHaveBeenCalledTimes(1);
    expect(store.length).toBe(1);
    expect(store.current?.online?.itemId).toBe("v1");
    expect(screen.getByTestId("player-video")).toBeInTheDocument();
  });

  it("offers no play-row or play-from-here control on the source page", async () => {
    renderPage();
    await screen.findByTestId("online-row-recent");

    expect(screen.queryByText(/play (row|all|from here)/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /play (row|all|from here)/i })).toBeNull();
    // The only play controls are the per-item ones.
    expect(screen.getAllByTestId(/^online-item-play-/)).toHaveLength(2);
  });

  it("shows no Skip, subtitle, lyrics or queue control and never fetches Markers", async () => {
    renderPage();
    await playFirstItem();

    for (const id of [
      "now-playing-skip-back",
      "now-playing-skip-forward",
      "now-playing-captions",
      "now-playing-queue-button",
      "player-next",
      "player-prev",
      "now-playing-upnext",
    ]) {
      expect(screen.queryByTestId(id), id).toBeNull();
    }
    expect(screen.queryByRole("button", { name: /skip/i })).toBeNull();
    expect(getSessionMarkers).not.toHaveBeenCalled();
  });

  it("keeps playing across navigation between pages", async () => {
    renderPage();
    const video = await playFirstItem();

    fireEvent.click(screen.getByText("go elsewhere"));
    expect(await screen.findByText("elsewhere page")).toBeInTheDocument();

    await waitFor(() => expect(screen.getByTestId("player-video")).toBe(video));
    expect(endSession).not.toHaveBeenCalled();
    expect(startOnlinePlayback).toHaveBeenCalledTimes(1);
  });
});
