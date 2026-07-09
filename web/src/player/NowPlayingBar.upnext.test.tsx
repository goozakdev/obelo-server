import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor, within } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry } from "./queue/model";
import { useQueue } from "./queue/useQueue";

// The end-of-episode "Up Next" card (now-playing-bar/upnext): in the last 30s of a
// STAGED video that has a next Queue entry, a small bottom-right card previews it
// (thumbnail + title + series) with an "Up Next:" lead and a live countdown. A click
// plays the next entry immediately; Esc dismisses the card WITHOUT collapsing the
// stage. Driven through the same faked-apiClient seam as the sibling bar tests.

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

function episodeSummary(id: string, title: string): TitleSummary {
  return {
    id,
    kind: "episode",
    title,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

/** An Episode Title detail carrying the parent-Show context the card's series line
 * reads (the lean Queue entry omits it, so the card fetches this). */
function episodeDetail(id: string, title: string, showTitle: string): Partial<TitleDetail> {
  return {
    id,
    kind: "episode",
    title,
    episode: {
      showId: "sh1",
      showTitle,
      seasonNumber: 1,
      episodeNumber: 2,
    },
  } as Partial<TitleDetail>;
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

/** A Play gesture for a two-episode queue (so there IS a next entry), plus the bar. */
function Harness({ entries }: { entries: QueueEntry[] }) {
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

/** Play a two-episode queue into the immersive stage; return the ready <video>. */
async function playTwoToStage() {
  renderWithAuth(
    <Harness
      entries={[
        entryFromTitle(episodeSummary("t1", "The Fire")),
        entryFromTitle(episodeSummary("t2", "The Rescue")),
      ]}
    />,
    { initialEntries: ["/"] },
  );
  fireEvent.click(screen.getByTestId("do-play"));
  const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
  await screen.findByTestId("now-playing-collapse"); // on the stage
  return video;
}

/** Drive the element to `remaining` seconds left of a `total`-second video. */
function setRemaining(video: HTMLVideoElement, remaining: number, total = 100) {
  Object.defineProperty(video, "duration", { value: total, configurable: true });
  Object.defineProperty(video, "currentTime", {
    value: total - remaining,
    writable: true,
    configurable: true,
  });
  act(() => {
    fireEvent.durationChange(video);
    fireEvent.timeUpdate(video);
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  getTitle.mockReset().mockImplementation((id: string) =>
    id === "t2"
      ? Promise.resolve(episodeDetail("t2", "The Rescue", "The Bear"))
      : Promise.resolve(episodeDetail("t1", "The Fire", "The Bear")),
  );
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

describe("NowPlayingBar — end-of-episode Up Next card", () => {
  it("appears in the last 30s with the next episode's title, series, and countdown", async () => {
    const video = await playTwoToStage();
    // Not near the end yet → no card.
    setRemaining(video, 45);
    expect(screen.queryByTestId("now-playing-upnext")).toBeNull();

    // Cross into the last 30s → the card appears.
    setRemaining(video, 20);
    const card = await screen.findByTestId("now-playing-upnext");
    expect(within(card).getByTestId("now-playing-upnext-countdown")).toHaveTextContent("20s");
    expect(within(card).getByTestId("now-playing-upnext-title")).toHaveTextContent("The Rescue");
    // The series line comes from the next entry's fetched detail.
    await waitFor(() => expect(within(card).getByText("The Bear")).toBeInTheDocument());
  });

  it("the countdown tracks the time remaining", async () => {
    const video = await playTwoToStage();
    setRemaining(video, 30);
    const card = await screen.findByTestId("now-playing-upnext");
    expect(within(card).getByTestId("now-playing-upnext-countdown")).toHaveTextContent("30s");
    setRemaining(video, 9);
    expect(within(card).getByTestId("now-playing-upnext-countdown")).toHaveTextContent("9s");
  });

  it("does NOT appear while the video is in the pip window (browsing)", async () => {
    const video = await playTwoToStage();
    // Collapse the stage to the corner pip, then enter the last 30s.
    fireEvent.click(screen.getByTestId("now-playing-collapse"));
    await waitFor(() =>
      expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "pip"),
    );
    setRemaining(video, 15);
    expect(screen.queryByTestId("now-playing-upnext")).toBeNull();
  });

  it("clicking the card plays the next episode immediately", async () => {
    const video = await playTwoToStage();
    setRemaining(video, 20);
    const card = await screen.findByTestId("now-playing-upnext");

    fireEvent.click(card);

    // Advanced to the last entry → nothing is 'up next' any more, so the card is gone
    // and a fresh player core is negotiating the next episode.
    await waitFor(() => expect(screen.queryByTestId("now-playing-upnext")).toBeNull());
    expect(startPlayback.mock.calls.length).toBeGreaterThan(1);
    expect(await screen.findByTestId("player-video")).toBeInTheDocument();
  });

  it("Escape dismisses the card WITHOUT collapsing the stage", async () => {
    const video = await playTwoToStage();
    setRemaining(video, 20);
    await screen.findByTestId("now-playing-upnext");

    act(() => {
      fireEvent.keyDown(document.body, { key: "Escape" });
    });

    // The card is gone…
    expect(screen.queryByTestId("now-playing-upnext")).toBeNull();
    // …but the stage stays put (Esc did NOT collapse it to pip).
    expect(screen.getByTestId("now-playing-stage")).toHaveAttribute("data-surface", "stage");
    expect(screen.getByTestId("now-playing-collapse")).toBeInTheDocument();

    // And it stays dismissed for the rest of THIS episode even as time ticks on.
    setRemaining(video, 12);
    expect(screen.queryByTestId("now-playing-upnext")).toBeNull();
  });
});
