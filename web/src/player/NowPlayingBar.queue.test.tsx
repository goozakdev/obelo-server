import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent, within } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { ApiError } from "../api/errors";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { useQueue } from "./queue/useQueue";

// The NOW PLAYING bar as a QUEUE-driven player (relocated from the retired
// PlayerScreen's playlist + queue-panel tests). The bar walks `queue.current`:
// auto-advancing on onEnded, exposing prev/next, skipping an unplayable entry, and
// stopping cleanly at the end. The queue button opens a drawer with the QueuePanel,
// through which the Queue is reshaped while it plays; reshaping UP-NEXT never
// disturbs the current session, and Clear queue stops playback and dismisses the
// bar. We assert which Title the bar NEGOTIATES (startPlayback) as the pointer moves.

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

function titleSummary(id: string, name: string): TitleSummary {
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

function threeEntries(): QueueEntry[] {
  return [
    entryFromTitle(titleSummary("t1", "Dune")),
    entryFromTitle(titleSummary("t2", "Arrival")),
    entryFromTitle(titleSummary("t3", "Sicario")),
  ];
}

function seedAndRender(entries: QueueEntry[], currentIndex: number) {
  const state: QueueState = { entries, currentIndex, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
  return entries;
}

function negotiatedTitleIds() {
  return startPlayback.mock.calls.map((c) => c[0]);
}

/** Open the queue drawer and return the entry <li> for a given entryId. */
async function openDrawer() {
  fireEvent.click(await screen.findByTestId("now-playing-queue-button"));
  await screen.findByTestId("queue-panel");
}
function panelTitleIds() {
  return screen.getAllByTestId("queue-entry").map((li) => li.getAttribute("data-title-id"));
}
function entryRow(entryId: string) {
  return screen.getAllByTestId("queue-entry").find((li) => li.getAttribute("data-entry-id") === entryId)!;
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockImplementation((id: string) =>
    Promise.resolve({ id, kind: "movie", title: id } as Partial<TitleDetail>),
  );
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
});

describe("NowPlayingBar — queue play-through", () => {
  it("advances to the next entry on onEnded (re-negotiates the next Title)", async () => {
    seedAndRender(threeEntries(), 0);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(startPlayback).toHaveBeenCalledWith("t1", expect.anything(), expect.anything());

    fireEvent.ended(video);
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("t2", expect.anything(), expect.anything()),
    );
    // Advancing re-keyed the player, ending the prior entry's session.
    await waitFor(() => expect(endSession).toHaveBeenCalled());
  });

  it("stops cleanly at the end of the Queue (no advance, no error)", async () => {
    seedAndRender(threeEntries(), 2);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(screen.getByTestId("player-next")).toBeDisabled();

    fireEvent.ended(video);
    await Promise.resolve();
    expect(negotiatedTitleIds()).toEqual(["t3"]);
    expect(screen.getByTestId("player-video")).toBeInTheDocument();
    expect(screen.queryByTestId("player-negotiate-error")).toBeNull();
  });

  it("prev/next move within the Queue and disable at the ends", async () => {
    seedAndRender(threeEntries(), 1);
    await screen.findByTestId("player-video");
    expect(screen.getByTestId("player-prev")).toBeEnabled();
    expect(screen.getByTestId("player-next")).toBeEnabled();

    fireEvent.click(screen.getByTestId("player-next"));
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("t3", expect.anything(), expect.anything()),
    );
    // Now at the last entry → next disabled.
    await waitFor(() => expect(screen.getByTestId("player-next")).toBeDisabled());
  });

  it("skips an entry whose negotiation fails (Missing/unplayable) and plays the next", async () => {
    startPlayback.mockImplementation((id: string) =>
      id === "t1"
        ? Promise.reject(new ApiError(404, "NOT_FOUND", "title not found"))
        : Promise.resolve(decision),
    );
    seedAndRender(
      [entryFromTitle(titleSummary("t1", "Dune")), entryFromTitle(titleSummary("t2", "Arrival"))],
      0,
    );
    await screen.findByTestId("player-video");
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("t2", expect.anything(), expect.anything()),
    );
    expect(negotiatedTitleIds()).toContain("t1");
    expect(screen.queryByTestId("player-negotiate-error")).toBeNull();
  });
});

describe("NowPlayingBar — queue drawer reshaping", () => {
  it("renders the ordered entries with the current one marked", async () => {
    const entries = seedAndRender(threeEntries(), 0);
    await openDrawer();
    // The current entry sits in the Now Playing card, the rest in the Up Next list;
    // in document order that's the full Queue order.
    expect(panelTitleIds()).toEqual(["t1", "t2", "t3"]);
    const current = screen
      .getAllByTestId("queue-entry")
      .filter((li) => li.getAttribute("aria-current") === "true");
    expect(current).toHaveLength(1);
    expect(entryRow(entries[0].entryId)).toHaveAttribute("aria-current", "true");
  });

  it("drag-and-drop reordering of up-next does NOT re-negotiate the current Title", async () => {
    const entries = seedAndRender(threeEntries(), 0);
    await screen.findByTestId("player-video");
    expect(negotiatedTitleIds()).toEqual(["t1"]);
    await openDrawer();

    // Drag the last up-next entry (t3) onto t2 → it's inserted before t2: [t1, t3, t2].
    fireEvent.dragStart(entryRow(entries[2].entryId));
    fireEvent.dragOver(entryRow(entries[1].entryId));
    fireEvent.drop(entryRow(entries[1].entryId));
    await waitFor(() => expect(panelTitleIds()).toEqual(["t1", "t3", "t2"]));
    // Current session untouched.
    expect(negotiatedTitleIds()).toEqual(["t1"]);
    expect(endSession).not.toHaveBeenCalled();
    expect(entryRow(entries[0].entryId)).toHaveAttribute("aria-current", "true");
  });

  it("removing the CURRENT entry advances to the next (ends session, negotiates next)", async () => {
    const entries = seedAndRender(
      [entryFromTitle(titleSummary("t1", "Dune")), entryFromTitle(titleSummary("t2", "Arrival"))],
      0,
    );
    await screen.findByTestId("player-video");
    await openDrawer();

    fireEvent.click(within(entryRow(entries[0].entryId)).getByTestId("queue-remove"));
    await waitFor(() =>
      expect(startPlayback).toHaveBeenCalledWith("t2", expect.anything(), expect.anything()),
    );
    await waitFor(() => expect(endSession).toHaveBeenCalled());
  });

  it("Clear queue stops playback and dismisses the bar", async () => {
    seedAndRender(threeEntries(), 0);
    await screen.findByTestId("player-video");
    await openDrawer();

    fireEvent.click(screen.getByTestId("queue-clear"));
    await waitFor(() => expect(screen.queryByTestId("now-playing-bar")).toBeNull());
    expect(screen.queryByTestId("player-video")).toBeNull();
    // Clearing emptied the Queue → the session ended as the bar left the tree.
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("sess-1"));
  });

  it("clearing with the drawer OPEN does not re-open it when new media starts", async () => {
    // A harness with a Play affordance so we can start fresh media after clearing.
    function Harness() {
      const queue = useQueue();
      return (
        <>
          <button
            data-testid="play-new"
            onClick={() => queue.playNow([entryFromTitle(titleSummary("t9", "New Movie"))])}
          >
            play new
          </button>
          <NowPlayingBar />
        </>
      );
    }
    saveQueue(window.sessionStorage, "u1", {
      entries: threeEntries(),
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    });
    renderWithAuth(<Harness />, { initialEntries: ["/"] });

    await screen.findByTestId("player-video");
    await openDrawer();
    expect(screen.getByTestId("queue-panel")).toBeInTheDocument();

    // Clear queue → the bar (and with it the drawer) leaves the tree.
    fireEvent.click(screen.getByTestId("queue-clear"));
    await waitFor(() => expect(screen.queryByTestId("now-playing-bar")).toBeNull());

    // Play new media → the bar returns, but the stale drawer must NOT re-appear.
    fireEvent.click(screen.getByTestId("play-new"));
    await screen.findByTestId("now-playing-bar");
    expect(screen.queryByTestId("now-playing-drawer")).toBeNull();
    expect(screen.queryByTestId("queue-panel")).toBeNull();
  });
});
