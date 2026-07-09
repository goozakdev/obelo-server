import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, act, fireEvent } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";

// Vanished-session recovery (the pause-then-freeze bug): the server reaps a
// Playback session after a long pause (or drops it on a restart), deleting its
// scratch dir, so the HLS playlist + every segment 404. The player then plays out
// its buffer and freezes. The fix: the HLS layer reports the persistent failure
// via onSessionLost, and the bar re-negotiates a FRESH session from the live
// position and resumes there (CONTEXT.md: a client whose session vanished simply
// re-negotiates). We drive the bar through the same faked-apiClient + mocked-hls
// seam as the other NowPlayingBar tests, and invoke the onSessionLost the bar
// hands attachHls to simulate the reaped stream.

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

function movieSummary(id: string, name: string): TitleSummary {
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

/** An HLS-tier decision (transcode), so the bar attaches via the mocked hls seam
 * and hands it an onSessionLost. `sessionId`/`streamUrl` vary per negotiation so a
 * recovery is observable as a distinct second stream. */
function hlsDecision(sessionId: string): PlaybackDecision {
  return {
    sessionId,
    tier: "transcode",
    streamUrl: `/api/v1/sessions/${sessionId}/hls/master.m3u8`,
    edition: { id: "e1", name: "1080p" },
    videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
    videoStreams: [],
    audioStream: { index: 1, codec: "aac", channels: 2 },
    audioStreams: [],
    subtitles: [],
    estimatedBitrate: 6_000_000,
  };
}

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

/** Read the onSessionLost the bar passed to the Nth attachHls call. */
function onSessionLostFor(callIndex: number): () => void {
  const opts = attachHls.mock.calls[callIndex]?.[2] as { onSessionLost?: () => void } | undefined;
  expect(opts?.onSessionLost, "the bar wires onSessionLost into attachHls").toBeTypeOf("function");
  return opts!.onSessionLost!;
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "movie", title: "Dune" } as Partial<TitleDetail>);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack: vi.fn(), setAudioTrack: vi.fn() });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("NowPlayingBar — vanished-session recovery", () => {
  it("re-negotiates from the live position when the HLS session vanishes", async () => {
    startPlayback.mockResolvedValueOnce(hlsDecision("sess-1")).mockResolvedValueOnce(hlsDecision("sess-2"));
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);

    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() => expect(attachHls).toHaveBeenCalledTimes(1));
    // The stream was attached from sess-1's playlist.
    expect(attachHls.mock.calls[0][1]).toBe("/api/v1/sessions/sess-1/hls/master.m3u8");

    // Viewer is 100s in when the session gets reaped mid-pause.
    Object.defineProperty(video, "currentTime", { value: 100, writable: true, configurable: true });
    Object.defineProperty(video, "paused", { value: true, configurable: true });

    // The HLS layer reports the vanished session (playlist + segments all 404).
    await act(async () => {
      onSessionLostFor(0)();
    });

    // The bar re-negotiates a FRESH session from where the viewer was...
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    expect(startPlayback.mock.calls[1][1]).toEqual(
      expect.objectContaining({ startPosition: 100_000 }),
    );
    // ...ending the reaped one cleanly (best-effort DELETE)...
    expect(endSession).toHaveBeenCalledWith("sess-1");
    // ...and re-attaches to the new stream so playback can continue.
    await waitFor(() =>
      expect(attachHls).toHaveBeenLastCalledWith(
        video,
        "/api/v1/sessions/sess-2/hls/master.m3u8",
        expect.objectContaining({ onSessionLost: expect.any(Function) }),
      ),
    );
  });

  it("recovers at most once per window (a fatal-error storm re-negotiates once)", async () => {
    startPlayback.mockResolvedValueOnce(hlsDecision("sess-1")).mockResolvedValueOnce(hlsDecision("sess-2"));
    seedAndRender([entryFromTitle(movieSummary("t1", "Dune"))]);

    const video = (await screen.findByTestId("player-video")) as HTMLVideoElement;
    await waitFor(() => expect(attachHls).toHaveBeenCalledTimes(1));
    Object.defineProperty(video, "currentTime", { value: 100, writable: true, configurable: true });
    Object.defineProperty(video, "paused", { value: true, configurable: true });

    // A burst of session-lost reports (hls.js can raise several) must not spin the
    // negotiate path — exactly one re-negotiation.
    const lost = onSessionLostFor(0);
    await act(async () => {
      lost();
      lost();
      lost();
    });

    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    // Give any errant extra re-negotiation a chance to land, then confirm none did.
    await Promise.resolve();
    expect(startPlayback).toHaveBeenCalledTimes(2);
    expect(endSession).toHaveBeenCalledTimes(1);
  });
});
