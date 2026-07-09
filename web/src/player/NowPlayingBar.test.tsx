import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { ApiError } from "../api/errors";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";
import { useQueue } from "./queue/useQueue";

// The NOW PLAYING bar (now-playing-bar/01): the persistent, shell-owned player.
// It reads `queue.current` from the shared store, owns the one <video> + Playback
// session, and shows the transport + now-playing label. Here we drive it through
// the same faked-apiClient seam the retired PlayerScreen used (relocated): a Queue
// is seeded into sessionStorage exactly as the QueueProvider hydrates one on
// reload. We assert bar visibility, the getTitle-sourced label, the playback
// negotiation (directPlay / HLS / busy / unsupported), progress reporting, session
// end on unmount, the reload-paused rule, and the play/pause toggle.

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
function trackDetail(id: string, name: string, artist: string): Partial<TitleDetail> {
  return {
    id,
    kind: "track",
    title: name,
    track: { artistId: "ar1", artistName: artist, albumId: "al1", albumTitle: "Album" },
  };
}
function episodeDetail(id: string, name: string): Partial<TitleDetail> {
  return {
    id,
    kind: "episode",
    title: name,
    episode: {
      showId: "sh1",
      showTitle: "The Bear",
      seasonId: "s1",
      seasonNumber: 1,
      episodeNumber: 3,
    },
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

const hlsDecision: PlaybackDecision = {
  ...decision,
  sessionId: "sess-hls",
  tier: "transcode",
  streamUrl: "/api/v1/sessions/sess-hls/hls/index.m3u8",
};

/** Seed a Queue into sessionStorage for the seeded user (u1 — renderWithAuth's
 * Admin) so the QueueProvider hydrates it on mount, then render the bar. */
function seedAndRender(entries: QueueEntry[], currentIndex = 0, initialEntries = ["/"]) {
  const state: QueueState = { entries, currentIndex, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries });
}

/** A harness that plays a Queue on demand (an empty Queue at mount → a user
 * gesture builds one), so we can assert the auto-play-on-gesture behaviour. */
function PlayHarness({ entry }: { entry: QueueEntry }) {
  const queue = useQueue();
  return (
    <>
      <button data-testid="do-play" onClick={() => queue.playNow([entry])}>
        play
      </button>
      <NowPlayingBar />
    </>
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue(movieDetail("t1", "Dune"));
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

describe("NowPlayingBar — visibility", () => {
  it("renders nothing when the Queue is empty", () => {
    renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
    expect(screen.queryByTestId("now-playing-bar")).toBeNull();
  });

  it("is present once a Queue is active", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    expect(await screen.findByTestId("now-playing-bar")).toBeInTheDocument();
  });

  it("is hidden on the login route even with an active Queue", () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))], 0, ["/login"]);
    expect(screen.queryByTestId("now-playing-bar")).toBeNull();
  });
});

describe("NowPlayingBar — now-playing label (from getTitle)", () => {
  it("shows Artist · Title for a music Track", async () => {
    getTitle.mockResolvedValue(trackDetail("tr1", "Paranoid Android", "Radiohead"));
    seedAndRender([entryFromTitle(trackSummary("tr1", "Paranoid Android"))]);
    expect(await screen.findByTestId("now-playing-title")).toHaveTextContent("Paranoid Android");
    expect(await screen.findByTestId("now-playing-context")).toHaveTextContent("Radiohead");
  });

  it("shows Show · SxxExx for a TV Episode", async () => {
    getTitle.mockResolvedValue(episodeDetail("ep1", "System"));
    seedAndRender([entryFromTitle({ ...movieSummary("ep1", "System"), kind: "episode" })]);
    expect(await screen.findByTestId("now-playing-title")).toHaveTextContent("System");
    expect(await screen.findByTestId("now-playing-context")).toHaveTextContent("The Bear · S01E03");
  });

  it("degrades to the bare title + kind when the detail fetch fails", async () => {
    getTitle.mockRejectedValue(new Error("offline"));
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    expect(await screen.findByTestId("now-playing-title")).toHaveTextContent("Dune");
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-context")).toHaveTextContent("Movie"),
    );
  });
});

describe("NowPlayingBar — playback (relocated player core)", () => {
  it("directPlay → a <video> bound to the decision streamUrl", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = await screen.findByTestId("player-video");
    expect(video).toHaveAttribute("src", decision.streamUrl);
    expect(video.tagName).toBe("VIDEO");
    expect(attachHls).not.toHaveBeenCalled();
  });

  it("passes the entry's resume position to negotiation and seeks on metadata", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune", 42000))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(startPlayback).toHaveBeenCalledWith(
      "t1",
      expect.objectContaining({ startPosition: 42000 }),
      expect.anything(),
    );
    fireEvent.loadedMetadata(video);
    expect(video.currentTime).toBeCloseTo(42, 1);
  });

  it("transcode tier → takes the hls.js path (no progressive src)", async () => {
    startPlayback.mockResolvedValue(hlsDecision);
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    expect(video).not.toHaveAttribute("src");
    await waitFor(() =>
      expect(attachHls).toHaveBeenCalledWith(
        video,
        hlsDecision.streamUrl,
        expect.objectContaining({ onSessionLost: expect.any(Function) }),
      ),
    );
  });

  it("TRANSCODE_REQUIRED → the honest unsupported state, no <video>", async () => {
    startPlayback.mockRejectedValue(
      new ApiError(501, "TRANSCODE_REQUIRED", "transcode required", { reason: "container" }),
    );
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-unsupported");
    expect(screen.queryByTestId("player-video")).toBeNull();
    expect(screen.getByTestId("player-unsupported-reason")).toHaveTextContent("container");
  });

  it("SERVER_BUSY twice → stays busy with a manual retry (no infinite loop)", async () => {
    const busy = () =>
      Promise.reject(
        new ApiError(503, "SERVER_BUSY", "server busy", { suggestedMaxBitrate: 2_000_000 }),
      );
    startPlayback.mockImplementationOnce(busy).mockImplementationOnce(busy);
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-busy-retry");
    expect(startPlayback).toHaveBeenCalledTimes(2);

    startPlayback.mockResolvedValueOnce(hlsDecision);
    fireEvent.click(screen.getByTestId("player-busy-retry"));
    await screen.findByTestId("player-video");
    expect(startPlayback).toHaveBeenCalledTimes(3);
  });

  it("reports progress on play/pause/seek and a final position on ended", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;

    fireEvent.play(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith("sess-1", expect.objectContaining({ state: "playing" })),
    );
    reportProgress.mockClear();
    fireEvent.pause(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith("sess-1", expect.objectContaining({ state: "paused" })),
    );

    Object.defineProperty(video, "currentTime", { value: 158, writable: true, configurable: true });
    reportProgress.mockClear();
    fireEvent.ended(video);
    await waitFor(() =>
      expect(reportProgress).toHaveBeenCalledWith("sess-1", expect.objectContaining({ positionMs: 158000 })),
    );
  });

  it("posts progress on the interval while playing", async () => {
    vi.useFakeTimers();
    try {
      seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
      await vi.waitFor(() => expect(screen.queryByTestId("player-video")).not.toBeNull());
      const video = screen.getByTestId("player-video") as HTMLVideoElement;
      Object.defineProperty(video, "currentTime", { value: 5, writable: true, configurable: true });
      fireEvent.play(video);
      reportProgress.mockClear();
      await act(async () => {
        vi.advanceTimersByTime(25_000);
      });
      expect(reportProgress.mock.calls.length).toBeGreaterThanOrEqual(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("ends the session on unmount (the bar leaving the tree)", async () => {
    const { unmount } = seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    await screen.findByTestId("player-video");
    unmount();
    await waitFor(() => expect(endSession).toHaveBeenCalledWith("sess-1"));
  });
});

describe("NowPlayingBar — play/pause + autoplay", () => {
  it("a restored-on-reload Queue loads PAUSED (no autoplay)", async () => {
    // Seeding the Queue then mounting the bar mirrors a page reload: the entry is
    // present on the bar's first render, so it must load paused.
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = await screen.findByTestId("player-video");
    expect(video).toHaveAttribute("data-autoplay", "false");
  });

  it("a Play gesture (empty → playNow) auto-plays the new entry", async () => {
    renderWithAuth(<PlayHarness entry={entryFromTitle(movieSummary("t9", "New"))} />, {
      initialEntries: ["/"],
    });
    // Nothing playing yet.
    expect(screen.queryByTestId("now-playing-bar")).toBeNull();
    fireEvent.click(screen.getByTestId("do-play"));
    const video = await screen.findByTestId("player-video");
    expect(video).toHaveAttribute("data-autoplay", "true");
  });

  it("the play/pause button reflects and toggles the media state", async () => {
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);
    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    const playSpy = vi.spyOn(video, "play").mockResolvedValue(undefined);
    const pauseSpy = vi.spyOn(video, "pause").mockImplementation(() => {});
    // jsdom starts paused; the button offers Play and calls play().
    const btn = screen.getByTestId("now-playing-play-pause");
    Object.defineProperty(video, "paused", { value: true, configurable: true });
    fireEvent.click(btn);
    expect(playSpy).toHaveBeenCalled();
    // Once the element reports playing, the button toggles to pause.
    fireEvent.play(video);
    Object.defineProperty(video, "paused", { value: false, configurable: true });
    fireEvent.click(btn);
    expect(pauseSpy).toHaveBeenCalled();
  });
});
