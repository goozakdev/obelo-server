import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { loadQueue, saveQueue } from "./queue/persist";
import { loadPlaybackPrefs, savePlaybackPrefs, playbackPrefsKey } from "./usePlaybackPrefs";

// The NOW PLAYING bar's CONTROLS (now-playing-bar/02): seekable progress +
// elapsed/remaining, ±10s skip (video only), and volume/mute. Driven through the
// same faked-apiClient seam as NowPlayingBar.test.tsx: a Queue is seeded into
// sessionStorage so the QueueProvider hydrates it on mount. We assert seek fires
// a progress report, the skip buttons appear only for a video entry and clamp,
// and volume/mute apply to the element and round-trip through per-user
// localStorage.

const { getTitle, startPlayback, reportProgress, endSession } = vi.hoisted(() => ({
  getTitle: vi.fn(),
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

/** Seed a Queue into sessionStorage for the seeded user (u1) so the QueueProvider
 * hydrates it on mount, then render the bar. */
function seedAndRender(entries: QueueEntry[], currentIndex = 0) {
  const state: QueueState = { entries, currentIndex, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

/** Force the element's position/duration (jsdom's media props are inert) and
 * notify React via the corresponding events, so the progress bar/skip see them. */
function setMedia(video: HTMLVideoElement, currentTime: number, duration: number) {
  Object.defineProperty(video, "currentTime", {
    value: currentTime,
    writable: true,
    configurable: true,
  });
  Object.defineProperty(video, "duration", { value: duration, configurable: true });
  Object.defineProperty(video, "paused", { value: true, configurable: true });
  fireEvent.durationChange(video);
  fireEvent.timeUpdate(video);
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.removeItem(playbackPrefsKey("u1"));
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
  window.localStorage.removeItem(playbackPrefsKey("u1"));
});

describe("NowPlayingBar — progress bar + time labels", () => {
  it("reflects the current position with elapsed left and remaining right", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 83, 203); // 1:23 elapsed, 2:00 remaining
    expect(screen.getByTestId("now-playing-elapsed")).toHaveTextContent("1:23");
    expect(screen.getByTestId("now-playing-remaining")).toHaveTextContent("-2:00");
  });

  it("seeking the progress bar seeks the element and fires a progress report", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 0, 200);
    reportProgress.mockClear();
    fireEvent.change(screen.getByTestId("now-playing-progress"), { target: { value: "42" } });
    expect(video.currentTime).toBeCloseTo(42, 1);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith(
        "sess-1",
        expect.objectContaining({ positionMs: 42000 }),
      ),
    );
  });
});

describe("NowPlayingBar — ±10s skip (video only)", () => {
  it("shows the skip buttons for a video entry and jumps by ten seconds", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 50, 200);
    reportProgress.mockClear();
    fireEvent.click(screen.getByTestId("now-playing-skip-forward"));
    expect(video.currentTime).toBeCloseTo(60, 1);
    fireEvent.click(screen.getByTestId("now-playing-skip-back"));
    expect(video.currentTime).toBeCloseTo(50, 1);
    await waitFor(() => expect(reportProgress).toHaveBeenCalled());
  });

  it("clamps the skip at the start and the end of the media", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    setMedia(video, 4, 100);
    fireEvent.click(screen.getByTestId("now-playing-skip-back")); // 4 - 10 → 0
    expect(video.currentTime).toBe(0);
    setMedia(video, 96, 100);
    fireEvent.click(screen.getByTestId("now-playing-skip-forward")); // 96 + 10 → 100
    expect(video.currentTime).toBe(100);
  });

  it("hides the skip buttons for a music (track) entry", async () => {
    seedAndRender([entryFromTitle(trackSummary("tr1", "Song"))]);
    await screen.findByTestId("player-video");
    expect(screen.queryByTestId("now-playing-skip-forward")).toBeNull();
    expect(screen.queryByTestId("now-playing-skip-back")).toBeNull();
  });
});

describe("NowPlayingBar — shuffle + repeat (music only)", () => {
  it("shows the shuffle + repeat controls for a music (track) entry and NOT for video", async () => {
    seedAndRender([entryFromTitle(trackSummary("tr1", "Song"))]);
    await screen.findByTestId("player-video");
    expect(screen.getByTestId("now-playing-shuffle")).toBeInTheDocument();
    expect(screen.getByTestId("now-playing-repeat")).toBeInTheDocument();
  });

  it("hides the shuffle + repeat controls for a video (movie) entry", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    expect(screen.queryByTestId("now-playing-shuffle")).toBeNull();
    expect(screen.queryByTestId("now-playing-repeat")).toBeNull();
  });

  it("clicking shuffle drives the store (aria-pressed + the persisted snapshot)", async () => {
    seedAndRender([entryFromTitle(trackSummary("tr1", "Song")), entryFromTitle(trackSummary("tr2", "Song 2"))]);
    await screen.findByTestId("player-video");
    const shuffle = screen.getByTestId("now-playing-shuffle");
    expect(shuffle).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(shuffle);
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-shuffle")).toHaveAttribute("aria-pressed", "true"),
    );
    // The store committed Shuffle mode: the persisted Queue now holds a snapshot.
    expect(loadQueue(window.sessionStorage, "u1").authoredOrder).not.toBeNull();
  });

  it("clicking repeat cycles off → all → one and reflects each state", async () => {
    seedAndRender([entryFromTitle(trackSummary("tr1", "Song"))]);
    await screen.findByTestId("player-video");
    const repeat = screen.getByTestId("now-playing-repeat");
    expect(repeat).toHaveAttribute("aria-label", "Repeat off");
    expect(repeat).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(repeat);
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-repeat")).toHaveAttribute("aria-label", "Repeat all"),
    );
    fireEvent.click(screen.getByTestId("now-playing-repeat"));
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-repeat")).toHaveAttribute("aria-label", "Repeat one"),
    );
    // The store committed Repeat mode.
    expect(loadQueue(window.sessionStorage, "u1").repeat).toBe("one");
  });
});

describe("NowPlayingBar — volume + mute", () => {
  it("the volume slider changes playback volume", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    fireEvent.change(screen.getByTestId("now-playing-volume-slider"), {
      target: { value: "0.3" },
    });
    await waitFor(() => expect(video.volume).toBeCloseTo(0.3, 2));
  });

  it("clicking the volume icon mutes and unmutes", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    const mute = screen.getByTestId("now-playing-mute");
    expect(mute).toHaveAttribute("aria-label", "Mute");
    fireEvent.click(mute);
    await waitFor(() => expect(video.muted).toBe(true));
    expect(screen.getByTestId("now-playing-mute")).toHaveAttribute("aria-label", "Unmute");
    fireEvent.click(screen.getByTestId("now-playing-mute"));
    await waitFor(() => expect(video.muted).toBe(false));
  });

  it("persists volume + mute to per-user localStorage", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    fireEvent.change(screen.getByTestId("now-playing-volume-slider"), {
      target: { value: "0.5" },
    });
    fireEvent.click(screen.getByTestId("now-playing-mute"));
    await waitFor(() => {
      const stored = loadPlaybackPrefs(window.localStorage, "u1");
      expect(stored.volume).toBeCloseTo(0.5, 2);
      expect(stored.muted).toBe(true);
    });
  });

  it("applies the persisted volume + mute to the element on load", async () => {
    savePlaybackPrefs(window.localStorage, "u1", { volume: 0.25, muted: true });
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() => {
      expect(video.volume).toBeCloseTo(0.25, 2);
      expect(video.muted).toBe(true);
    });
    // The mute icon reflects the restored muted state.
    expect(screen.getByTestId("now-playing-mute")).toHaveAttribute("aria-label", "Unmute");
  });
});
