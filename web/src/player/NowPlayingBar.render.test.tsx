import { StrictMode } from "react";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue } from "./queue/useQueue";
import { saveQueue } from "./queue/persist";
import type { QueueState } from "./queue/model";

const { getTitle, startPlayback, reportProgress, endSession, getSessionMarkers } = vi.hoisted(
  () => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    getSessionMarkers: vi.fn(),
  }),
);

vi.mock("./hls", () => ({ attachHls: vi.fn() }));

// One call per CurrentPlayer render (the only caller), so the counter is the player
// core's render count.
const { coreRenders } = vi.hoisted(() => ({ coreRenders: { n: 0 } }));
vi.mock("./usePlaybackPrefs", async () => {
  const actual = await vi.importActual<typeof import("./usePlaybackPrefs")>("./usePlaybackPrefs");
  return {
    ...actual,
    usePlaybackPrefs: () => {
      coreRenders.n++;
      return actual.usePlaybackPrefs();
    },
  };
});
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

function summary(id: string, kind = "movie"): TitleSummary {
  return {
    id,
    kind,
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

function decisionOf(): PlaybackDecision {
  return {
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
}

beforeEach(() => {
  coreRenders.n = 0;
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockImplementation((id: string) =>
    Promise.resolve({ id, kind: "movie", title: id } as Partial<TitleDetail>),
  );
  startPlayback.mockReset().mockResolvedValue(decisionOf());
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  getSessionMarkers.mockReset().mockResolvedValue([]);
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
  vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

function Harness({ titles }: { titles: TitleSummary[] }) {
  const queue = useQueue();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow(titles.map((t) => entryFromTitle(t)))}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
}

describe("surface reconcile under StrictMode", () => {
  it("a Play opens the video stage in a real <StrictMode> render", async () => {
    renderWithAuth(
      <StrictMode>
        <Harness titles={[summary("t1")]} />
      </StrictMode>,
      { initialEntries: ["/"] },
    );
    fireEvent.click(screen.getByTestId("do-play"));
    await screen.findByTestId("player-video");
    expect(await screen.findByTestId("now-playing-collapse")).toBeTruthy();
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
  });
});

function setMedia(video: HTMLVideoElement, currentTime: number, duration: number) {
  Object.defineProperty(video, "currentTime", { value: currentTime, writable: true, configurable: true });
  Object.defineProperty(video, "duration", { value: duration, configurable: true });
  act(() => {
    fireEvent.durationChange(video);
    fireEvent.timeUpdate(video);
  });
}

describe("R04-08 — playback ticks do not re-render the player core", () => {
  it("timeupdate updates the time UI without re-rendering the core or reading storage", async () => {
    const state: QueueState = {
      entries: [entryFromTitle(summary("t1"))],
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", state);
    renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 1, 600);
    // Let the detail fetch / negotiation settle so only ticks can render from here.
    await act(async () => {});
    const rendersBefore = coreRenders.n;
    const getItem = vi.spyOn(Storage.prototype, "getItem");

    for (let t = 2; t <= 21; t++) {
      Object.defineProperty(video, "currentTime", { value: t, writable: true, configurable: true });
      act(() => {
        fireEvent.timeUpdate(video);
      });
    }

    // The time UI followed the ticks...
    expect(screen.getByTestId("now-playing-elapsed")).toHaveTextContent("0:21");
    expect(screen.getByTestId("now-playing-progress")).toHaveValue("21");
    // ...without one more render of the core (the menus, labels, icons) ...
    expect(coreRenders.n).toBe(rendersBefore);
    // ...and without a single localStorage read on the hot path.
    expect(getItem).not.toHaveBeenCalled();
  });
});

describe("R04-14 — a PiP drag does not re-render the player core", () => {
  // jsdom has no PointerEvent (so no clientX on pointer events): shim one on MouseEvent.
  beforeEach(() => {
    class PointerEventShim extends MouseEvent {
      pointerId: number;
      constructor(type: string, init: MouseEventInit & { pointerId?: number } = {}) {
        super(type, init);
        this.pointerId = init.pointerId ?? 0;
      }
    }
    vi.stubGlobal("PointerEvent", PointerEventShim);
  });
  afterEach(() => vi.unstubAllGlobals());

  it("moves the window by a ref during the drag and commits the offset on release", async () => {
    renderWithAuth(<Harness titles={[summary("t1")]} />, { initialEntries: ["/"] });
    fireEvent.click(screen.getByTestId("do-play"));
    await screen.findByTestId("player-video");
    fireEvent.click(await screen.findByTestId("now-playing-collapse"));
    const stage = await screen.findByTestId("now-playing-stage");
    // Collapsing rides history.back() (an async popstate).
    await waitFor(() => expect(stage).toHaveAttribute("data-surface", "pip"));
    await act(async () => {});
    const handle = stage.querySelector(".now-playing-surface-media")!;

    fireEvent.pointerDown(handle, { clientX: 100, clientY: 100, pointerId: 1 });
    const rendersBefore = coreRenders.n;
    fireEvent.pointerMove(handle, { clientX: 130, clientY: 90, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 160, clientY: 80, pointerId: 1 });
    expect(stage.style.transform).toBe("translate(60px, -20px)");
    expect(coreRenders.n).toBe(rendersBefore); // no render per pointermove

    fireEvent.pointerUp(handle, { clientX: 160, clientY: 80, pointerId: 1 });
    expect(stage.style.transform).toBe("translate(60px, -20px)");
    // The release committed it: it survives leaving and re-entering the pip surface.
    fireEvent.click(screen.getByTestId("now-playing-pip-expand"));
    await act(async () => {});
    expect(screen.getByTestId("now-playing-stage").style.transform).toBe("");
    fireEvent.click(await screen.findByTestId("now-playing-collapse"));
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip"),
    );
    expect(screen.getByTestId("now-playing-stage").style.transform).toBe("translate(60px, -20px)");
  });

  it("a cancelled drag commits where the box was left, so the next drag does not jump", async () => {
    renderWithAuth(<Harness titles={[summary("t1")]} />, { initialEntries: ["/"] });
    fireEvent.click(screen.getByTestId("do-play"));
    await screen.findByTestId("player-video");
    fireEvent.click(await screen.findByTestId("now-playing-collapse"));
    const stage = await screen.findByTestId("now-playing-stage");
    await waitFor(() => expect(stage).toHaveAttribute("data-surface", "pip"));
    await act(async () => {});
    const handle = stage.querySelector(".now-playing-surface-media")!;

    fireEvent.pointerDown(handle, { clientX: 100, clientY: 100, pointerId: 1 });
    fireEvent.pointerMove(handle, { clientX: 160, clientY: 80, pointerId: 1 });
    expect(stage.style.transform).toBe("translate(60px, -20px)");
    fireEvent.pointerCancel(handle, { pointerId: 1 }); // e.g. the browser took the gesture

    // The next drag starts from the box's real position (60,-20), not the stale 0,0.
    fireEvent.pointerDown(handle, { clientX: 200, clientY: 200, pointerId: 2 });
    fireEvent.pointerMove(handle, { clientX: 210, clientY: 200, pointerId: 2 });
    expect(stage.style.transform).toBe("translate(70px, -20px)");
    fireEvent.pointerUp(handle, { pointerId: 2 });
  });
});
