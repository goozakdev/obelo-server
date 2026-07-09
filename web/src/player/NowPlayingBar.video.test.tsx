import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, act, waitFor } from "@testing-library/react";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaybackDecision, TitleDetail, TitleSummary, VideoStream } from "../api/types";
import { entryFromTitle, type QueueEntry, type QueueState } from "./queue/model";
import { saveQueue } from "./queue/persist";

// Video menu (selectable-video/03, ADR-0025): the Now Playing bar lists a File's
// selectable video Streams, marks the RESOLVED (capability-then-quality) one active,
// and switches via a FRESH negotiation carrying videoStreamId — always an escalating
// switch (there is no in-band video rendition), the image-subtitle model rather than
// the instant audio flip. It preserves the audio Stream, subtitle burn, and resume
// position, and surfaces a 503 rather than swallowing it. The menu is hidden for a
// single-video File. We drive the bar through the same faked-apiClient seam as the
// audio test.

const { getTitle, startPlayback, reportProgress, endSession, searchSubtitles, fetchSubtitle } =
  vi.hoisted(() => ({
    getTitle: vi.fn(),
    startPlayback: vi.fn(),
    reportProgress: vi.fn(),
    endSession: vi.fn(),
    searchSubtitles: vi.fn(),
    fetchSubtitle: vi.fn(),
  }));

const { attachHls, setAudioTrack, setTextTrack } = vi.hoisted(() => ({
  attachHls: vi.fn(),
  setAudioTrack: vi.fn(),
  setTextTrack: vi.fn(),
}));
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
      searchSubtitles: (...a: unknown[]) => searchSubtitles(...a),
      fetchSubtitle: (...a: unknown[]) => fetchSubtitle(...a),
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

function video(p: Partial<VideoStream> & { id: string; index: number }): VideoStream {
  return {
    id: p.id,
    index: p.index,
    codec: p.codec ?? "h264",
    language: p.language,
    width: p.width ?? 1920,
    height: p.height ?? 1080,
    isDefault: p.isDefault ?? false,
    label: p.label ?? p.id,
  };
}

// A multi-video File: Colour (default, index 0) + Black & White (index 1), sharing
// audio. The decision's resolved videoStream defaults to the Colour one (index 0).
const COLOUR = video({ id: "colour", index: 0, label: "Colour", isDefault: true });
const BW = video({ id: "bw", index: 1, label: "Black & White" });

function decisionWithVideo(
  streams: VideoStream[],
  over: Partial<PlaybackDecision> = {},
): PlaybackDecision {
  return {
    sessionId: "sess-1",
    tier: "directPlay",
    streamUrl: "/api/v1/sessions/sess-1/stream",
    edition: { id: "e1", name: "1080p" },
    videoStream: { index: 0, codec: "h264", width: 1920, height: 1080 },
    videoStreams: streams,
    audioStream: { index: 1, codec: "aac", channels: 6, language: "en" },
    audioStreams: [
      {
        id: "en",
        index: 1,
        codec: "aac",
        language: "en",
        channels: 6,
        layout: "5.1",
        isDefault: true,
        label: "English 5.1",
      },
    ],
    subtitles: [],
    estimatedBitrate: 6_000_000,
    ...over,
  };
}

function seedAndRender(entries: QueueEntry[]) {
  const state: QueueState = { entries, currentIndex: 0, repeat: "off", authoredOrder: null };
  saveQueue(window.sessionStorage, "u1", state);
  return renderWithAuth(<NowPlayingBar />, { initialEntries: ["/"] });
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset().mockResolvedValue({ id: "t1", kind: "movie", title: "Two Cuts" } as Partial<TitleDetail>);
  reportProgress.mockReset().mockResolvedValue({ titleId: "t1", resumePositionMs: 0, watched: false });
  endSession.mockReset().mockResolvedValue(undefined);
  searchSubtitles.mockReset().mockResolvedValue([]);
  fetchSubtitle.mockReset();
  setAudioTrack.mockReset();
  setTextTrack.mockReset();
  attachHls.mockReset().mockResolvedValue({ mode: "hls.js", detach: vi.fn(), setTextTrack, setAudioTrack });
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation((mime: string) =>
    /mp4|avc1|mp4a/.test(mime) ? "probably" : "",
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("NowPlayingBar — Video menu", () => {
  it("lists every video Stream, labelled, and marks the server-resolved one active", async () => {
    startPlayback.mockResolvedValue(decisionWithVideo([COLOUR, BW]));
    seedAndRender([entryFromTitle(movieSummary("t1", "Two Cuts"))]);

    const btn = await screen.findByTestId("now-playing-video");
    expect(btn).toHaveAttribute("aria-label", "Video");
    await act(async () => {
      fireEvent.click(btn);
    });
    const menu = screen.getByTestId("now-playing-video-menu");
    expect(menu).toHaveTextContent("Colour");
    expect(menu).toHaveTextContent("Black & White");
    // The resolved (Colour) Stream is checked; the other is not.
    const colour = menu.querySelector('[data-video-id="colour"]') as HTMLElement;
    const bw = menu.querySelector('[data-video-id="bw"]') as HTMLElement;
    expect(colour).toHaveAttribute("aria-checked", "true");
    expect(bw).toHaveAttribute("aria-checked", "false");
    // CONTEXT.md: the surface is "Video", never the coined "Video track".
    expect(menu.textContent).not.toContain("Video track");
  });

  it("is hidden for a single-video File (nothing to pick)", async () => {
    startPlayback.mockResolvedValue(decisionWithVideo([COLOUR]));
    seedAndRender([entryFromTitle(movieSummary("t1", "One Cut"))]);

    // The video element renders once ready; the video menu button must not.
    await screen.findByTestId("player-video");
    expect(screen.queryByTestId("now-playing-video")).toBeNull();
  });

  it("switches via a fresh negotiation carrying videoStreamId, preserving audio + resuming", async () => {
    startPlayback
      .mockResolvedValueOnce(decisionWithVideo([COLOUR, BW]))
      .mockResolvedValueOnce(
        decisionWithVideo([COLOUR, BW], {
          sessionId: "sess-2",
          tier: "directStream",
          streamUrl: "/api/v1/sessions/sess-2/hls/master.m3u8",
          videoStream: { index: 1, codec: "h264", width: 1920, height: 1080 },
        }),
      );
    seedAndRender([entryFromTitle(movieSummary("t1", "Two Cuts"))]);

    const btn = await screen.findByTestId("now-playing-video");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-video-menu").querySelector('[data-video-id="bw"]') as HTMLElement,
      );
    });

    // The second negotiation carried videoStreamId; the mount one did not. The prior
    // session was ended cleanly (every video switch is a restart).
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));
    const firstOpts = startPlayback.mock.calls[0][1] as { videoStreamId?: string };
    const secondOpts = startPlayback.mock.calls[1][1] as {
      videoStreamId?: string;
      audioStreamId?: string;
      startPosition?: number;
    };
    expect(firstOpts.videoStreamId).toBeUndefined();
    expect(secondOpts.videoStreamId).toBe("bw");
    // The audio choice carries forward (untouched → server default), and the switch
    // resumes at (near) the captured position rather than restarting from zero.
    expect(secondOpts.startPosition).toBeGreaterThanOrEqual(0);
    expect(endSession).toHaveBeenCalledWith("sess-1");

    // After the switch lands, re-open the menu — Black & White is now the resolved
    // active Stream (re-initialized from the re-negotiated decision).
    const btn2 = await screen.findByTestId("now-playing-video");
    await act(async () => {
      fireEvent.click(btn2);
    });
    await waitFor(() =>
      expect(
        screen.getByTestId("now-playing-video-menu").querySelector('[data-video-id="bw"]'),
      ).toHaveAttribute("aria-checked", "true"),
    );
  });

  it("preserves an active subtitle burn across a video switch", async () => {
    // A burn is active on the first session (burnSubtitleId set); a video switch must
    // carry it into the re-negotiation, not drop it.
    startPlayback
      .mockResolvedValueOnce(
        decisionWithVideo([COLOUR, BW], {
          subtitles: [
            { id: "img-en", source: "embedded", kind: "image", forced: false, label: "English (image)" },
          ],
        }),
      )
      .mockResolvedValueOnce(
        decisionWithVideo([COLOUR, BW], {
          sessionId: "sess-2",
          tier: "transcode",
          streamUrl: "/api/v1/sessions/sess-2/hls/master.m3u8",
          subtitles: [
            { id: "img-en", source: "embedded", kind: "image", forced: false, label: "English (image)" },
          ],
        }),
      )
      .mockResolvedValueOnce(
        decisionWithVideo([COLOUR, BW], {
          sessionId: "sess-3",
          tier: "transcode",
          streamUrl: "/api/v1/sessions/sess-3/hls/master.m3u8",
          videoStream: { index: 1, codec: "h264", width: 1920, height: 1080 },
          subtitles: [
            { id: "img-en", source: "embedded", kind: "image", forced: false, label: "English (image)" },
          ],
        }),
      );
    seedAndRender([entryFromTitle(movieSummary("t1", "Two Cuts"))]);

    // Turn on the image subtitle (a burn → fresh negotiation).
    const cc = await screen.findByTestId("now-playing-captions");
    await act(async () => {
      fireEvent.click(cc);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-captions-menu").querySelector('[data-sub-id="img-en"]') as HTMLElement,
      );
    });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(2));

    // Now switch video — the third negotiation must still carry burnSubtitleId.
    const btn = await screen.findByTestId("now-playing-video");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-video-menu").querySelector('[data-video-id="bw"]') as HTMLElement,
      );
    });
    await waitFor(() => expect(startPlayback).toHaveBeenCalledTimes(3));
    const thirdOpts = startPlayback.mock.calls[2][1] as {
      videoStreamId?: string;
      burnSubtitleId?: string;
    };
    expect(thirdOpts.videoStreamId).toBe("bw");
    expect(thirdOpts.burnSubtitleId).toBe("img-en");
  });

  it("surfaces a 503 on a video switch (not silently dropped)", async () => {
    const { ApiError } = await import("../api/errors");
    startPlayback
      .mockResolvedValueOnce(decisionWithVideo([COLOUR, BW]))
      // The switch requires a video encode and is rejected at the transcode cap; no
      // suggested bitrate → no auto-retry, so the manual busy UX is shown.
      .mockRejectedValueOnce(new ApiError(503, "SERVER_BUSY", "server busy", { retryable: true }));
    seedAndRender([entryFromTitle(movieSummary("t1", "Two Cuts"))]);

    const btn = await screen.findByTestId("now-playing-video");
    await act(async () => {
      fireEvent.click(btn);
    });
    await act(async () => {
      fireEvent.click(
        screen.getByTestId("now-playing-video-menu").querySelector('[data-video-id="bw"]') as HTMLElement,
      );
    });

    expect(await screen.findByTestId("player-busy")).toBeInTheDocument();
    expect(screen.getByTestId("player-busy-retry")).toBeInTheDocument();
  });
});
