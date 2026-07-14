import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { useNavigate } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { useQueue } from "./queue/useQueue";

// The NOW PLAYING bar's VIDEO SURFACE STATE MACHINE (now-playing-bar/03): a video
// plays in the immersive `stage`, demotes to the custom `pip` window when the user
// navigates to a browse route, re-expands to the stage, and goes fullscreen on the
// STAGE WRAPPER. Driven through the same faked-apiClient seam as the sibling bar
// tests. The load-bearing assertions: the element + Playback session are NOT
// re-negotiated across a stage→pip transition (same node, one startPlayback),
// fullscreen targets the wrapper (not the bare <video>), and a `popstate` collapses
// stage→pip. requestFullscreen/exitFullscreen are stubbed in test/setup.ts.

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

function movieSummary(id: string, name: string, resumeMs = 0): TitleSummary {
  return {
    id,
    kind: "movie",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: resumeMs,
    watched: false,
    genres: [],
  };
}
function trackSummary(id: string, name: string): TitleSummary {
  return { ...movieSummary(id, name), kind: "track" };
}
function movieDetail(id: string, name: string): Partial<TitleDetail> {
  return { id, kind: "movie", title: name };
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

/** The same decision but carrying one deliverable English text track, for the
 * captions-toggle shortcut. */
const decisionWithText: PlaybackDecision = {
  ...decision,
  subtitles: [
    {
      id: "en",
      source: "sidecar",
      kind: "text",
      language: "en",
      forced: false,
      label: "English",
      url: "/api/v1/titles/t1/subtitles/en.vtt",
    },
  ],
};

/** Seed a Queue into sessionStorage (a reload-restored Queue: loads PAUSED and
 * bar-only, no surface forced) and render the bar. */
function seedAndRender(entries: QueueEntry[], currentIndex = 0) {
  const state: QueueState = { entries, currentIndex, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

/** A harness with a Play gesture (empty → playNow, so the entry auto-plays and the
 * bar opens the stage per criterion 1) and a route-navigate button (to drive the
 * stage→pip demotion off a real router navigation). */
function SurfaceHarness({ entry }: { entry: QueueEntry }) {
  const queue = useQueue();
  const navigate = useNavigate();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow([entry])}>
        play
      </button>
      <button data-testid="go-browse" onClick={() => navigate("/browse")}>
        browse
      </button>
      <NowPlayingBar />
    </>
  );
}

/** A Play gesture that plays a MULTI-entry queue (for the n/p next/prev shortcuts),
 * so the queue has a next and a previous to move between. */
function MultiHarness({ entries }: { entries: QueueEntry[] }) {
  const queue = useQueue();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow(entries)}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
}

/** Play a video via a user gesture so it auto-plays into the immersive stage, and
 * return the ready <video> element. */
async function playVideoToStage(entry: QueueEntry) {
  renderWithAuth(<SurfaceHarness entry={entry} />, { initialEntries: ["/"] });
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  // The bar's collapse-to-PiP button appears only on the stage (the bar is the stage's
  // control surface now — there are no separate on-screen overlay controls).
  await screen.findByTestId("now-playing-collapse");
  return video;
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockResolvedValue(movieDetail("t1", "Dune"));
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
  window.localStorage.clear();
});

describe("NowPlayingBar — opening the immersive stage", () => {
  it("playing a video opens the stage with the bar still docked below", async () => {
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
    // The bar (the transport) stays present below the stage.
    expect(screen.getByTestId("now-playing-bar")).toBeInTheDocument();
    expect(screen.getByTestId("now-playing-play-pause")).toBeInTheDocument();
  });

  it("a reload-restored (paused) video stays bar-only — no forced overlay", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const stage = await screen.findByTestId("now-playing-stage");
    expect(stage).toHaveAttribute("data-surface", "bar-only");
    // Not staged → the bar shows the expand affordance, not collapse-to-PiP.
    expect(screen.queryByTestId("now-playing-collapse")).toBeNull();
  });

  it("music (a Track) never shows a video surface — it's always bar-only", async () => {
    seedAndRender([entryFromTitle(trackSummary("tr1", "Song"))]);
    await screen.findByTestId("player-video");
    // Audio uses the bar-only surface (visually hidden); no video-surface chrome.
    expect(screen.queryByTestId("now-playing-pip-controls")).toBeNull();
    expect(screen.queryByTestId("now-playing-fullscreen")).toBeNull();
  });
});

describe("NowPlayingBar — stage → pip (navigation) reuses one element + session", () => {
  it("navigating to a browse route demotes the stage to the PiP window", async () => {
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));

    await screen.findByTestId("now-playing-pip-controls");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip");
    expect(screen.queryByTestId("now-playing-collapse")).toBeNull();
    // The PiP window keeps its minimal controls (play/pause, expand, close-to-stop).
    expect(screen.getByTestId("now-playing-pip-play")).toBeInTheDocument();
    expect(screen.getByTestId("now-playing-pip-expand")).toBeInTheDocument();
    expect(screen.getByTestId("now-playing-pip-close")).toBeInTheDocument();
  });

  it("the PiP close button stops playback and dismisses the bar", async () => {
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    // Close no longer just demotes to bar-only — it empties the Queue, so the bar
    // (and its <video>) leaves the tree and the session ends.
    fireEvent.click(screen.getByTestId("now-playing-pip-close"));
    await waitFor(() => expect(screen.queryByTestId("now-playing-bar")).toBeNull());
    expect(screen.queryByTestId("player-video")).toBeNull();
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("sess-1"));
  });

  it("the transition never re-negotiates: same <video> node, one startPlayback, no endSession", async () => {
    const before = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    expect(startPlayback).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    const after = screen.getByTestId("player-video") as HTMLVideoElement;
    // The SAME element node throughout — proof it wasn't unmounted/remounted.
    expect(after).toBe(before);
    // The session was neither renegotiated nor ended by the surface change.
    expect(startPlayback).toHaveBeenCalledTimes(1);
    expect(endSession).not.toHaveBeenCalled();
  });

  it("keeps reporting progress while playing in the PiP window", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    reportProgress.mockClear();
    Object.defineProperty(video, "currentTime", { value: 12, writable: true, configurable: true });
    fireEvent.play(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith(
        "sess-1",
        expect.objectContaining({ state: "playing" }),
      ),
    );
  });
});

describe("NowPlayingBar — re-expanding to the stage", () => {
  it("re-expands from the PiP window back to the immersive stage", async () => {
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    fireEvent.click(screen.getByTestId("now-playing-pip-expand"));
    await screen.findByTestId("now-playing-collapse");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
  });

  it("re-expands from the bar's expand button", async () => {
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    // The bar exposes an expand affordance whenever the video isn't already staged.
    fireEvent.click(screen.getByTestId("now-playing-expand"));
    await screen.findByTestId("now-playing-collapse");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
  });
});

describe("NowPlayingBar — fullscreen targets the bar (the shared control surface)", () => {
  it("requests fullscreen on the bar element, not the stage wrapper or the bare <video>", async () => {
    const fsSpy = vi.spyOn(Element.prototype, "requestFullscreen").mockResolvedValue(undefined);
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));

    fireEvent.click(screen.getByTestId("now-playing-fullscreen"));
    // The bar wraps BOTH the stage and the control row, so fullscreening it keeps the
    // transport on top in fullscreen — no separate on-screen overlay controls needed.
    const bar = screen.getByTestId("now-playing-bar");
    expect(fsSpy).toHaveBeenCalledTimes(1);
    expect(fsSpy.mock.instances[0]).toBe(bar);
    expect(fsSpy.mock.instances[0]).not.toBe(screen.getByTestId("now-playing-stage"));
    expect(fsSpy.mock.instances[0]).not.toBe(video);
  });

  it("invoked from the PiP window (via the bar) promotes to the stage first", async () => {
    const fsSpy = vi.spyOn(Element.prototype, "requestFullscreen").mockResolvedValue(undefined);
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    fireEvent.click(screen.getByTestId("now-playing-fullscreen"));
    await screen.findByTestId("now-playing-collapse");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
    expect(fsSpy.mock.instances[0]).toBe(screen.getByTestId("now-playing-bar"));
  });
});

describe("NowPlayingBar — Back / popstate collapses the stage", () => {
  it("a popstate collapses stage → pip without stopping playback", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));

    // Opening the stage pushed a non-content history entry; a Back/back-gesture
    // fires popstate, which must collapse to PiP (not stop playback).
    act(() => {
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await screen.findByTestId("now-playing-pip-controls");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip");
    // Same element, session untouched.
    expect(screen.getByTestId("player-video")).toBe(video);
    expect(endSession).not.toHaveBeenCalled();
  });
});

/** A Play gesture for a MULTI-entry queue plus a route-navigate button — so we can
 * both auto-advance (fire the video's `ended`) AND demote the stage to pip. */
function MultiNavHarness({ entries }: { entries: QueueEntry[] }) {
  const queue = useQueue();
  const navigate = useNavigate();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow(entries)}>
        play
      </button>
      <button data-testid="go-browse" onClick={() => navigate("/browse")}>
        browse
      </button>
      <NowPlayingBar />
    </>
  );
}

describe("NowPlayingBar — a Queue advance continues in the SAME view", () => {
  const twoEpisodes = () => [
    entryFromTitle(movieSummary("t1", "Ep 1")),
    entryFromTitle(movieSummary("t2", "Ep 2")),
  ];

  it("auto-advancing to the next episode stays on the stage (not pip)", async () => {
    renderWithAuth(<MultiNavHarness entries={twoEpisodes()} />, { initialEntries: ["/"] });
    fireEvent.click(screen.getByTestId("do-play"));
    const first = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await screen.findByTestId("now-playing-collapse"); // proves we're on the stage
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");

    // The episode ends → onEnded → queue.advance(): the player core re-keys to Ep 2.
    act(() => {
      fireEvent.ended(first);
    });

    // The next episode negotiates and plays — and CONTINUES on the stage. (Before the
    // surface was lifted to the persistent bar, the re-key reset it and a stray
    // history popstate collapsed the fresh stage to pip.)
    const second = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(second).not.toBe(first); // it really did advance to a new element
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
    await screen.findByTestId("now-playing-collapse");
  });

  it("auto-advancing while in the pip window stays in pip", async () => {
    renderWithAuth(<MultiNavHarness entries={twoEpisodes()} />, { initialEntries: ["/"] });
    fireEvent.click(screen.getByTestId("do-play"));
    const first = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    // Demote to the corner pip by navigating to a browse route (keeps playing).
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip");

    act(() => {
      fireEvent.ended(first);
    });

    // The next episode continues in the pip window, not popped back to the stage.
    const second = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(second).not.toBe(first);
    await screen.findByTestId("now-playing-pip-controls");
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip");
  });
});

describe("NowPlayingBar — keyboard shortcuts (immersive stage)", () => {
  it("Space toggles play/pause; a focused form control keeps its own Space", async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    const pause = vi.spyOn(HTMLMediaElement.prototype, "pause").mockImplementation(() => {});
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    play.mockClear();

    // Default paused → Space plays.
    fireEvent.keyDown(document.body, { key: " " });
    expect(play).toHaveBeenCalledTimes(1);

    // Now "playing" → Space pauses.
    Object.defineProperty(video, "paused", { value: false, configurable: true });
    fireEvent.keyDown(document.body, { key: " " });
    expect(pause).toHaveBeenCalledTimes(1);

    // Space while the volume range slider is focused is left to the input.
    play.mockClear();
    fireEvent.keyDown(screen.getByTestId("now-playing-volume-slider"), { key: " " });
    expect(play).not.toHaveBeenCalled();
  });

  it("←/→ skip ∓10 seconds", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    Object.defineProperty(video, "duration", { value: 100, configurable: true });
    Object.defineProperty(video, "currentTime", { value: 5, writable: true, configurable: true });
    fireEvent.durationChange(video);

    fireEvent.keyDown(document.body, { key: "ArrowRight" });
    expect(video.currentTime).toBe(15);
    fireEvent.keyDown(document.body, { key: "ArrowLeft" });
    expect(video.currentTime).toBe(5);
  });

  it("↑/↓ nudge the volume", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    await waitFor(() => expect(video.volume).toBe(1));
    fireEvent.keyDown(document.body, { key: "ArrowDown" });
    await waitFor(() => expect(video.volume).toBeCloseTo(0.95));
    fireEvent.keyDown(document.body, { key: "ArrowUp" });
    await waitFor(() => expect(video.volume).toBeCloseTo(1));
  });

  it("f requests fullscreen on the bar; m mutes", async () => {
    const fsSpy = vi.spyOn(Element.prototype, "requestFullscreen").mockResolvedValue(undefined);
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));

    fireEvent.keyDown(document.body, { key: "f" });
    expect(fsSpy).toHaveBeenCalledTimes(1);
    expect(fsSpy.mock.instances[0]).toBe(screen.getByTestId("now-playing-bar"));

    fireEvent.keyDown(document.body, { key: "m" });
    await waitFor(() => expect(video.muted).toBe(true));
  });

  it("Esc: fullscreen defers to the browser's exit; windowed collapses to pip", async () => {
    const back = vi.spyOn(window.history, "back").mockImplementation(() => {});
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    const bar = screen.getByTestId("now-playing-bar");

    // In fullscreen, Esc must NOT run our collapse (the browser exits fullscreen).
    Object.defineProperty(document, "fullscreenElement", { value: bar, configurable: true });
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(back).not.toHaveBeenCalled();

    // Windowed, Esc collapses via Back (the pushed stage history entry).
    Object.defineProperty(document, "fullscreenElement", { value: null, configurable: true });
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(back).toHaveBeenCalledTimes(1);
  });

  it("stays inert while the video is in the pip window (keys belong to the page)", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    fireEvent.click(screen.getByTestId("go-browse"));
    await screen.findByTestId("now-playing-pip-controls");

    Object.defineProperty(video, "duration", { value: 100, configurable: true });
    Object.defineProperty(video, "currentTime", { value: 5, writable: true, configurable: true });
    fireEvent.durationChange(video);
    fireEvent.keyDown(document.body, { key: "ArrowRight" });
    expect(video.currentTime).toBe(5); // unchanged — the shortcut is inert in pip
  });

  it("k play/pause and j / l skip mirror Space and ← / →", async () => {
    const play = vi.spyOn(HTMLMediaElement.prototype, "play").mockResolvedValue(undefined);
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    play.mockClear();
    fireEvent.keyDown(document.body, { key: "k" });
    expect(play).toHaveBeenCalledTimes(1);

    Object.defineProperty(video, "duration", { value: 100, configurable: true });
    Object.defineProperty(video, "currentTime", { value: 50, writable: true, configurable: true });
    fireEvent.durationChange(video);
    fireEvent.keyDown(document.body, { key: "l" });
    expect(video.currentTime).toBe(60);
    fireEvent.keyDown(document.body, { key: "j" });
    expect(video.currentTime).toBe(50);
  });

  it("0–9 seek to N0% of the duration", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    Object.defineProperty(video, "duration", { value: 200, configurable: true });
    Object.defineProperty(video, "currentTime", { value: 0, writable: true, configurable: true });
    fireEvent.durationChange(video);

    fireEvent.keyDown(document.body, { key: "5" });
    expect(video.currentTime).toBe(100); // 50%
    fireEvent.keyDown(document.body, { key: "9" });
    expect(video.currentTime).toBe(180); // 90%
    fireEvent.keyDown(document.body, { key: "0" });
    expect(video.currentTime).toBe(0);
  });

  it(", / . step one frame — only while paused", async () => {
    const video = await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    Object.defineProperty(video, "duration", { value: 200, configurable: true });
    Object.defineProperty(video, "currentTime", { value: 50, writable: true, configurable: true });
    fireEvent.durationChange(video);

    // Playing → frame step is a no-op.
    Object.defineProperty(video, "paused", { value: false, configurable: true });
    fireEvent.keyDown(document.body, { key: "." });
    expect(video.currentTime).toBe(50);

    // Paused → steps ~1/30s forward, then back.
    Object.defineProperty(video, "paused", { value: true, configurable: true });
    fireEvent.keyDown(document.body, { key: "." });
    expect(video.currentTime).toBeCloseTo(50 + 1 / 30);
    fireEvent.keyDown(document.body, { key: "," });
    expect(video.currentTime).toBeCloseTo(50);
  });

  it("c toggles captions on (first text track) then off", async () => {
    startPlayback.mockResolvedValue(decisionWithText);
    await playVideoToStage(entryFromTitle(movieSummary("t1", "Dune")));
    const cc = await screen.findByTestId("now-playing-captions");
    expect(cc).toHaveAttribute("aria-pressed", "false");

    fireEvent.keyDown(document.body, { key: "c" });
    await waitFor(() => expect(cc).toHaveAttribute("aria-pressed", "true"));
    fireEvent.keyDown(document.body, { key: "c" });
    await waitFor(() => expect(cc).toHaveAttribute("aria-pressed", "false"));
  });

  it("n / p advance and rewind the queue", async () => {
    const e1 = entryFromTitle(movieSummary("t1", "One"));
    const e2 = entryFromTitle(movieSummary("t2", "Two"));
    renderWithAuth(<MultiHarness entries={[e1, e2]} />, { initialEntries: ["/"] });
    fireEvent.click(screen.getByTestId("do-play"));
    await screen.findByTestId("now-playing-collapse"); // staged on the first entry
    expect(startPlayback).toHaveBeenCalledTimes(1);

    // n advances (re-keys the player → a fresh negotiation for the next entry).
    fireEvent.keyDown(document.body, { key: "n" });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    // p rewinds back to the first.
    fireEvent.keyDown(document.body, { key: "p" });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(3));
  });
});
