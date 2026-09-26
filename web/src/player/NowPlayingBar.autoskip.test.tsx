import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Marker, PlaybackDecision, TitleSummary } from "../api/types";
import { entryFromTitle } from "./queue/model";
import { useQueue } from "./queue/useQueue";

// Auto-skip and the Credits button (ADR-0065 §6). A Marker the server marks
// `autoSkip` — the viewer's own setting for its kind — is skipped without a click;
// any other Marker still offers the Skip button. With a next Episode in the Queue,
// the Credits Marker the server flags `watchedPoint` reads "Next episode" and plays
// it, after reporting a position inside the Credits so the Title counts as watched;
// any other Credits Marker, or a Queue with no next Episode, stays a plain Skip.

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
import { CREDITS_REPORT_TIMEOUT_MS } from "./SkipMarkerButton";

function episode(id: string, title: string, kind = "episode"): TitleSummary {
  return {
    id,
    kind,
    title,
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

function markers(autoSkip: { intro?: boolean; credits?: boolean }, watchedPoint = true): Marker[] {
  return [
    { kind: "intro", source: "local", startMs: 10_000, endMs: 70_000, autoSkip: !!autoSkip.intro },
    {
      kind: "credits",
      source: "local",
      startMs: 1_300_000,
      endMs: 1_380_000,
      autoSkip: !!autoSkip.credits,
      watchedPoint,
    },
  ];
}

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

async function playToStage(titles: TitleSummary[] = [episode("t1", "The Fire")]) {
  renderWithAuth(<Harness titles={titles} />, { initialEntries: ["/"] });
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  await screen.findByTestId("now-playing-collapse"); // on the stage
  Object.defineProperty(video, "duration", { value: 1_400, configurable: true });
  act(() => {
    fireEvent.durationChange(video);
  });
  await waitFor(() => expect(getSessionMarkers).toHaveBeenCalledWith("sess-1", expect.anything()));
  // Let the Markers fetch settle before the position moves.
  await act(async () => {});
  return video;
}

function setPosition(video: HTMLVideoElement, seconds: number) {
  Object.defineProperty(video, "currentTime", { value: seconds, writable: true, configurable: true });
  act(() => {
    fireEvent.timeUpdate(video);
  });
}

function startedTitles(): string[] {
  return startPlayback.mock.calls.map((c) => c[0] as string);
}

// A progress report at or past the Credits start reached the server before the
// next entry started playing.
function expectCreditsReportedBefore(titleId: string) {
  const started = startPlayback.mock.calls.findIndex((c) => c[0] === titleId);
  expect(started).toBeGreaterThanOrEqual(0);
  const startedAt = startPlayback.mock.invocationCallOrder[started];
  const reported = reportProgress.mock.calls.some(
    (c, i) =>
      c[0] === "sess-1" &&
      (c[1] as { positionMs: number }).positionMs >= 1_300_000 &&
      reportProgress.mock.invocationCallOrder[i] < startedAt,
  );
  expect(reported).toBe(true);
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockImplementation((id: string) => Promise.resolve({ id, kind: "episode", title: id }));
  startPlayback.mockReset().mockResolvedValue(decision);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  getSessionMarkers.mockReset().mockResolvedValue(markers({}));
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

describe("NowPlayingBar — auto-skip", () => {
  it("jumps past an auto-skipped Intro without a click and offers no button", async () => {
    getSessionMarkers.mockResolvedValue(markers({ intro: true }));
    const video = await playToStage();
    setPosition(video, 30);
    await waitFor(() => expect(video.currentTime).toBe(70));
    expect(screen.queryByTestId("skip-marker")).toBeNull();
    expect(reportProgress).toHaveBeenLastCalledWith("sess-1", expect.objectContaining({ positionMs: 70_000 }));
  });

  it("without the setting offers Skip and does not jump", async () => {
    const video = await playToStage();
    setPosition(video, 30);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Intro");
    expect(video.currentTime).toBe(30);
  });

  it("is per kind: an auto-skipped Intro does not auto-skip the Credits", async () => {
    getSessionMarkers.mockResolvedValue(markers({ intro: true }));
    const video = await playToStage();
    setPosition(video, 1_310);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Credits");
    expect(video.currentTime).toBe(1_310);
  });

  it("skips a Marker once: seeking back into it plays it", async () => {
    getSessionMarkers.mockResolvedValue(markers({ intro: true }));
    const video = await playToStage();
    setPosition(video, 30);
    await waitFor(() => expect(video.currentTime).toBe(70));
    setPosition(video, 20);
    expect(video.currentTime).toBe(20);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Intro");
  });

  it("an auto-skipped Credits with a next Episode in the Queue plays it", async () => {
    getSessionMarkers.mockResolvedValue(markers({ credits: true }));
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expectCreditsReportedBefore("t2");
  });

  it("an auto-skipped Credits that is not the Watched point skips within the File", async () => {
    getSessionMarkers.mockResolvedValue(markers({ credits: true }, false));
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    await waitFor(() => expect(video.currentTime).toBe(1_380));
    expect(startedTitles()).toEqual(["t1"]);
  });
});

describe("NowPlayingBar — the Credits button", () => {
  it("reads Next episode with an Episode next in the Queue and plays it when pressed", async () => {
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    const button = await screen.findByTestId("skip-marker");
    expect(button).toHaveTextContent("Next episode");
    fireEvent.click(button);
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expectCreditsReportedBefore("t2");
  });

  it("reports a whole-millisecond position from a fractional playback time", async () => {
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310.1234);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expectCreditsReportedBefore("t2");
    for (const c of reportProgress.mock.calls) {
      expect(Number.isInteger((c[1] as { positionMs: number }).positionMs)).toBe(true);
    }
  });

  it("waits for the Credits report to finish before playing the next Episode", async () => {
    let finish!: () => void;
    reportProgress.mockImplementation((_sid: string, r: { positionMs: number }) =>
      r.positionMs >= 1_300_000 && !finish
        ? new Promise((resolve) => {
            finish = () => resolve({ titleId: "t1", resumePositionMs: r.positionMs, watched: true });
          })
        : Promise.resolve({ titleId: "t1", resumePositionMs: 0, watched: false }),
    );
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    await act(async () => {});
    expect(finish).toBeDefined();
    expect(endSession).not.toHaveBeenCalled();
    expect(startedTitles()).toEqual(["t1"]);
    await act(async () => finish());
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expect(endSession).toHaveBeenCalledWith("sess-1");
  });

  it("logs a failed Credits report and still plays the next Episode", async () => {
    const failure = new Error("400 bad request");
    reportProgress.mockImplementation((_sid: string, r: { positionMs: number }) =>
      r.positionMs >= 1_300_000
        ? Promise.reject(failure)
        : Promise.resolve({ titleId: "t1", resumePositionMs: 0, watched: false }),
    );
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expect(logged).toHaveBeenCalledWith(expect.stringContaining("[player]"), failure);
  });

  it("reads Skip for a Credits that is not the Watched point, even with a next episode", async () => {
    getSessionMarkers.mockResolvedValue(markers({}, false));
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 1_310);
    const button = await screen.findByTestId("skip-marker");
    expect(button).toHaveTextContent("Skip Credits");
    fireEvent.click(button);
    expect(video.currentTime).toBe(1_380);
    expect(startedTitles()).toEqual(["t1"]);
  });

  it("reads Skip with a Movie next in the Queue: a Movie is not a next episode", async () => {
    const video = await playToStage([episode("t1", "The Fire"), episode("m1", "Dune", "movie")]);
    setPosition(video, 1_310);
    const button = await screen.findByTestId("skip-marker");
    expect(button).toHaveTextContent("Skip Credits");
    fireEvent.click(button);
    expect(video.currentTime).toBe(1_380);
    expect(startedTitles()).toEqual(["t1"]);
  });

  it("reads Skip with nothing next in the Queue and only seeks within the File", async () => {
    const video = await playToStage();
    setPosition(video, 1_310);
    const button = await screen.findByTestId("skip-marker");
    expect(button).toHaveTextContent("Skip Credits");
    fireEvent.click(button);
    expect(video.currentTime).toBe(1_380);
    expect(startedTitles()).toEqual(["t1"]);
  });

  it("the Intro button still reads Skip with an Episode next in the Queue", async () => {
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 30);
    expect(await screen.findByTestId("skip-marker")).toHaveTextContent("Skip Intro");
  });
});

describe("NowPlayingBar — the Credits report", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  // A Credits report that holds until finished, for the Credits start only.
  function holdCreditsReport() {
    const held = { finish: undefined as undefined | (() => void), signal: undefined as AbortSignal | undefined };
    reportProgress.mockImplementation((_sid: string, r: { positionMs: number }, signal?: AbortSignal) =>
      r.positionMs >= 1_300_000 && r.positionMs < 1_380_000 && !held.finish
        ? new Promise((resolve) => {
            held.signal = signal;
            held.finish = () => resolve({ titleId: "t1", resumePositionMs: r.positionMs, watched: true });
          })
        : Promise.resolve({ titleId: "t1", resumePositionMs: 0, watched: false }),
    );
    return held;
  }

  // The Credits reports "Next episode" sent: the ones it can abort. The final
  // report of the session's end is not one of them.
  function creditsReports(): number {
    return reportProgress.mock.calls.filter((c) => {
      const p = (c[1] as { positionMs: number }).positionMs;
      return p >= 1_300_000 && p < 1_380_000 && c[2] instanceof AbortSignal;
    }).length;
  }

  it("gives up on a hung Credits report and plays the next Episode", async () => {
    const held = holdCreditsReport();
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    setPosition(video, 1_310);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    await act(async () => {});
    expect(held.finish).toBeDefined();
    expect(startedTitles()).toEqual(["t1"]);
    await act(async () => {
      vi.advanceTimersByTime(CREDITS_REPORT_TIMEOUT_MS);
    });
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    expect(held.signal?.aborted).toBe(true);
    expect(logged).toHaveBeenCalledWith(expect.stringContaining("[player]"), expect.anything());
  });

  it("sends one Credits report however often Next episode is pressed", async () => {
    const held = holdCreditsReport();
    const video = await playToStage([
      episode("t1", "The Fire"),
      episode("t2", "The Rescue"),
      episode("t3", "The Return"),
    ]);
    setPosition(video, 1_310);
    const button = await screen.findByTestId("skip-marker");
    fireEvent.click(button);
    fireEvent.click(button);
    await act(async () => {});
    expect(creditsReports()).toBe(1);
    await act(async () => held.finish?.());
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    await act(async () => {});
    expect(creditsReports()).toBe(1);
    expect(startedTitles()).toEqual(["t1", "t2"]);
  });

  it("does not advance again once the entry has changed while the report was out", async () => {
    const held = holdCreditsReport();
    const video = await playToStage([
      episode("t1", "The Fire"),
      episode("t2", "The Rescue"),
      episode("t3", "The Return"),
    ]);
    setPosition(video, 1_310);
    fireEvent.click(await screen.findByTestId("skip-marker"));
    await act(async () => {});
    act(() => {
      fireEvent.keyDown(window, { key: "n" }); // the viewer moves on by hand
    });
    await waitFor(() => expect(startedTitles()).toContain("t2"));
    await act(async () => held.finish?.());
    await act(async () => {});
    expect(startedTitles()).toEqual(["t1", "t2"]);
  });

  it("reports where playback was when the player goes away", async () => {
    const video = await playToStage([episode("t1", "The Fire"), episode("t2", "The Rescue")]);
    setPosition(video, 500.25);
    act(() => {
      fireEvent.keyDown(window, { key: "n" });
    });
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("sess-1"));
    expect(reportProgress).toHaveBeenCalledWith("sess-1", { positionMs: 500_250, state: "paused" });
  });
});
